package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	messages           <-chan matrix.Message
	streamErrors       <-chan error
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
	if f.messages != nil || f.streamErrors != nil {
		return f.messages, f.streamErrors
	}
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

func TestLoginReusesSavedAccountDetails(t *testing.T) {
	var loginPayload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			if err := json.NewDecoder(r.Body).Decode(&loginPayload); err != nil {
				t.Error(err)
			}
			_, _ = w.Write([]byte(`{"access_token":"new-token","refresh_token":"refresh","user_id":"@alice:test","device_id":"DEV"}`))
		case "/_matrix/client/v3/account/whoami":
			_, _ = w.Write([]byte(`{"user_id":"@alice:test","device_id":"DEV"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg := &memoryConfig{value: config.Config{HomeserverURL: server.URL, AuthURL: server.URL, Username: "alice", AuthMethod: config.AuthMethodPassword}, present: true}
	sess := &memorySession{}
	app := &App{Config: cfg, Session: sess, HTTP: server.Client()}
	root := app.Root()
	root.SetArgs([]string{"login", "--password", "secret"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if loginPayload["type"] != "m.login.password" || cfg.value.HomeserverURL != server.URL || cfg.value.AuthMethod != config.AuthMethodPassword {
		t.Fatalf("login payload=%#v config=%#v", loginPayload, cfg.value)
	}
}

func TestSSOLoginRejectsPasswordFlag(t *testing.T) {
	root := testApp(&fakeMatrix{}).Root()
	root.SetArgs([]string{"login", "--homeserver", "https://hs", "--sso", "--password", "secret"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "cannot be used with --sso") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoginIDPRequiresSSO(t *testing.T) {
	root := testApp(&fakeMatrix{}).Root()
	root.SetArgs([]string{"login", "--homeserver", "https://hs", "--idp", "work"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "--idp requires --sso") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConfigCommandSetsThreadView(t *testing.T) {
	app := testApp(&fakeMatrix{})
	root := app.Root()
	root.SetArgs([]string{"config", "set", "thread_view", "split"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	cfg, err := app.Config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ThreadView != config.ThreadViewSplit {
		t.Fatalf("thread view = %q", cfg.ThreadView)
	}
}

func TestConfigCommandSetsAccountColor(t *testing.T) {
	app := testApp(&fakeMatrix{})
	root := app.Root()
	root.SetArgs([]string{"config", "set", "color", "#12ABef"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	cfg, err := app.Config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Color != "#12abef" {
		t.Fatalf("account color = %q", cfg.Color)
	}
}

func TestConfigCommandSetsTheme(t *testing.T) {
	app := testApp(&fakeMatrix{})
	root := app.Root()
	root.SetArgs([]string{"config", "set", "theme", "boxed"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	cfg, err := app.Config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Theme != config.ThemeBoxed {
		t.Fatalf("theme = %q", cfg.Theme)
	}
}

func TestAccountsDefaultPersistsPreference(t *testing.T) {
	app := testApp(&fakeMatrix{})
	app.ListAccounts = func() ([]string, error) { return []string{"default", "work"}, nil }
	var saved string
	app.SetDefaultAccount = func(name string) error { saved = name; return nil }
	root := app.Root()
	root.SetArgs([]string{"accounts", "default", "work"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if saved != "work" || app.DefaultAccount != "work" {
		t.Fatalf("default account saved=%q app=%q", saved, app.DefaultAccount)
	}
}

func TestPrepareTUIAccountsKeepsBackgroundAccountsLive(t *testing.T) {
	prodMessages := make(chan matrix.Message, 1)
	prodMessages <- matrix.Message{RoomID: "!incident:prod", Sender: "@alice:prod", Body: "failed"}
	close(prodMessages)
	clients := map[string]*fakeMatrix{
		"dev":  {},
		"prod": {rooms: []matrix.Room{{ID: "!incident:prod", Name: "Incidents"}}, messages: prodMessages, streamErrors: closedErrors()},
	}
	configs := map[string]*memoryConfig{}
	sessions := map[string]*memorySession{}
	for _, name := range []string{"dev", "prod"} {
		configs[name] = &memoryConfig{value: config.Config{HomeserverURL: "https://" + name, AuthURL: "https://" + name, Username: name}, present: true}
		sessions[name] = &memorySession{value: auth.Credentials{AccessToken: name, UserID: "@me:" + name, CryptoPickleKey: "pickle"}, present: true}
	}
	app := &App{
		ListAccounts:  func() ([]string, error) { return []string{"dev", "prod"}, nil },
		SelectAccount: func(name string) (config.Store, session.Store, error) { return configs[name], sessions[name], nil },
		NewMatrix: func(_ config.Config, creds auth.Credentials) (matrix.API, error) {
			return clients[strings.TrimPrefix(creds.UserID, "@me:")], nil
		},
		DefaultAccount: "dev", Account: "dev",
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prepared, err := app.prepareTUIAccounts(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if prepared.activeName != "dev" || len(prepared.options) != 2 {
		t.Fatalf("prepared accounts = %#v", prepared)
	}
	select {
	case notification := <-prepared.notifications:
		if notification.Account != "prod" || notification.RoomName != "Incidents" || notification.Message.Body != "failed" {
			t.Fatalf("notification = %#v", notification)
		}
	case <-time.After(time.Second):
		t.Fatal("background account notification was not forwarded")
	}
}

func TestMergeRefreshedCredentialsPreservesDeviceState(t *testing.T) {
	previous := auth.Credentials{AccessToken: "old", RefreshToken: "refresh", UserID: "@alice:test", DeviceID: "DEV", CryptoPickleKey: "pickle", OAuthClientID: "client", OAuthTokenEndpoint: "https://auth/token"}
	got := mergeRefreshedCredentials(previous, auth.Credentials{AccessToken: "new"})
	if got.AccessToken != "new" || got.RefreshToken != "refresh" || got.UserID != "@alice:test" || got.DeviceID != "DEV" || got.CryptoPickleKey != "pickle" || got.OAuthClientID != "client" || got.OAuthTokenEndpoint != "https://auth/token" {
		t.Fatalf("merged credentials = %#v", got)
	}
}

func TestLoadClientReturnsAccountAwareSignInHint(t *testing.T) {
	cfg := &memoryConfig{value: config.Config{HomeserverURL: "https://hs", AuthURL: "https://auth", Username: "alice"}, present: true}
	sess := &memorySession{value: auth.Credentials{AccessToken: "old-token", UserID: "@alice:test", CryptoPickleKey: "pickle"}, present: true}
	app := &App{Config: cfg, Session: sess, Account: "work-dev", NewMatrix: func(config.Config, auth.Credentials) (matrix.API, error) {
		return &validatingMatrix{fakeMatrix: &fakeMatrix{}, err: matrix.ErrInvalidSession}, nil
	}}
	_, err := app.loadClient(context.Background())
	if err == nil {
		t.Fatal("expected sign-in error")
	}
	for _, expected := range []string{`account "work-dev"`, "homeserver rejected", "matrix --ac work-dev login"} {
		if !strings.Contains(err.Error(), expected) {
			t.Fatalf("error missing %q: %v", expected, err)
		}
	}
	if !errors.Is(err, matrix.ErrInvalidSession) {
		t.Fatalf("error does not preserve invalid-session cause: %v", err)
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
