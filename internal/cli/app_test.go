package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"matrix-cli/internal/auth"
	"matrix-cli/internal/config"
	"matrix-cli/internal/matrix"
	"matrix-cli/internal/session"
)

type memoryConfig struct {
	value   config.Config
	present bool
}

func (s *memoryConfig) Load() (config.Config, error) {
	if !s.present {
		return config.Config{}, errors.New("missing")
	}
	return s.value, nil
}
func (s *memoryConfig) Save(v config.Config) error { s.value = v; s.present = true; return nil }
func (s *memoryConfig) Delete() error              { s.present = false; return nil }

type memorySession struct {
	value   auth.Credentials
	present bool
}

func (s *memorySession) Load() (auth.Credentials, error) {
	if !s.present {
		return auth.Credentials{}, errors.New("missing")
	}
	return s.value, nil
}
func (s *memorySession) Save(v auth.Credentials) error { s.value = v; s.present = true; return nil }
func (s *memorySession) Delete() error                 { s.present = false; return nil }

type fakeMatrix struct {
	sentRoom, sentBody string
	rooms              []matrix.Room
}

func (f *fakeMatrix) Rooms(context.Context) ([]matrix.Room, error)   { return f.rooms, nil }
func (f *fakeMatrix) Spaces(context.Context) ([]matrix.Space, error) { return nil, nil }
func (f *fakeMatrix) RoomInfo(context.Context, string) (matrix.RoomInfo, error) {
	return matrix.RoomInfo{}, nil
}
func (f *fakeMatrix) RecentMessages(context.Context, string, string, int) (matrix.MessagePage, error) {
	return matrix.MessagePage{}, nil
}
func (f *fakeMatrix) Send(_ context.Context, room, body string) error {
	f.sentRoom, f.sentBody = room, body
	return nil
}
func (f *fakeMatrix) SendThread(_ context.Context, room, _ string, body string) error {
	f.sentRoom, f.sentBody = room, body
	return nil
}
func (f *fakeMatrix) Subscribe(context.Context, string) (<-chan matrix.Message, <-chan error) {
	return closedMessages(), closedErrors()
}
func (f *fakeMatrix) Logout(context.Context) error { return nil }

type validatingMatrix struct {
	*fakeMatrix
	err error
}

func (f *validatingMatrix) ValidateSession(context.Context) error { return f.err }

func closedMessages() <-chan matrix.Message { ch := make(chan matrix.Message); close(ch); return ch }
func closedErrors() <-chan error            { ch := make(chan error); close(ch); return ch }

func testApp(fake *fakeMatrix) *App {
	cfg := &memoryConfig{value: config.Config{HomeserverURL: "https://hs", AuthURL: "https://auth", Username: "alice"}, present: true}
	sess := &memorySession{value: auth.Credentials{AccessToken: "token", UserID: "@alice:test"}, present: true}
	return &App{Config: cfg, Session: sess, NewMatrix: func(config.Config, auth.Credentials) (matrix.API, error) { return fake, nil }}
}

func TestAccountFlagSelectsNamedAccount(t *testing.T) {
	fake := &fakeMatrix{}
	selected := ""
	app := testApp(fake)
	app.SelectAccount = func(name string) (config.Store, session.Store, error) {
		selected = name
		return app.Config, app.Session, nil
	}
	root := app.Root()
	root.SetArgs([]string{"--ac", "work", "rooms"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if selected != "work" {
		t.Fatalf("selected account = %q", selected)
	}
}

func TestSendCommandJoinsMessageArguments(t *testing.T) {
	fake := &fakeMatrix{}
	root := testApp(fake).Root()
	root.SetArgs([]string{"send", "!room:test", "hello", "world"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if fake.sentRoom != "!room:test" || fake.sentBody != "hello world" {
		t.Fatalf("sent %q to %q", fake.sentBody, fake.sentRoom)
	}
}

func TestRoomsCommandSortsAndPrintsNames(t *testing.T) {
	fake := &fakeMatrix{rooms: []matrix.Room{{ID: "!z:test"}, {ID: "!a:test", Name: "Alpha"}}}
	root := testApp(fake).Root()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"rooms"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "!a:test\tAlpha\n!z:test\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestSendRequiresMessage(t *testing.T) {
	root := testApp(&fakeMatrix{}).Root()
	root.SetArgs([]string{"send", "!room:test"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "requires at least 2") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMergeRefreshedCredentialsPreservesDeviceState(t *testing.T) {
	previous := auth.Credentials{AccessToken: "old", RefreshToken: "refresh", UserID: "@alice:test", DeviceID: "DEV", CryptoPickleKey: "pickle"}
	got := mergeRefreshedCredentials(previous, auth.Credentials{AccessToken: "new"})
	if got.AccessToken != "new" || got.RefreshToken != "refresh" || got.UserID != "@alice:test" || got.DeviceID != "DEV" || got.CryptoPickleKey != "pickle" {
		t.Fatalf("merged credentials = %#v", got)
	}
}

func TestLoadClientRefreshesRejectedSessionBeforeCryptoInitialization(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/client/v3/refresh" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-token"}`))
	}))
	defer server.Close()

	cfg := &memoryConfig{value: config.Config{HomeserverURL: "https://hs", AuthURL: server.URL, Username: "alice"}, present: true}
	sess := &memorySession{value: auth.Credentials{AccessToken: "old-token", RefreshToken: "refresh", UserID: "@alice:test", DeviceID: "DEV", CryptoPickleKey: "pickle"}, present: true}
	var tokens []string
	app := &App{Config: cfg, Session: sess, NewMatrix: func(_ config.Config, creds auth.Credentials) (matrix.API, error) {
		tokens = append(tokens, creds.AccessToken)
		var validationErr error
		if creds.AccessToken == "old-token" {
			validationErr = matrix.ErrInvalidSession
		}
		return &validatingMatrix{fakeMatrix: &fakeMatrix{}, err: validationErr}, nil
	}}
	if _, err := app.loadClient(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(tokens, ",") != "old-token,new-token" {
		t.Fatalf("factory tokens = %v", tokens)
	}
	if sess.value.AccessToken != "new-token" || sess.value.CryptoPickleKey != "pickle" || sess.value.DeviceID != "DEV" {
		t.Fatalf("saved credentials = %#v", sess.value)
	}
}
