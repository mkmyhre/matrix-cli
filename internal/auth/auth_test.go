package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPasswordLoginUsesConfiguredAuthURL(t *testing.T) {
	var path string
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"access","refresh_token":"refresh","user_id":"@alice:test","device_id":"DEV","expires_in_ms":60000}`))
	}))
	defer server.Close()
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	creds, err := (PasswordAuthenticator{BaseURL: server.URL + "/", Client: server.Client(), Now: func() time.Time { return now }}).Login(context.Background(), "alice", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/_matrix/client/v3/login" {
		t.Fatalf("path = %q", path)
	}
	if payload["type"] != "m.login.password" || payload["password"] != "secret" {
		t.Fatalf("unexpected payload: %#v", payload)
	}
	identifier := payload["identifier"].(map[string]any)
	if identifier["type"] != "m.id.user" || identifier["user"] != "alice" {
		t.Fatalf("unexpected identifier: %#v", identifier)
	}
	if creds.UserID != "@alice:test" || creds.AccessToken != "access" || creds.DeviceID != "DEV" {
		t.Fatalf("unexpected credentials: %#v", creds)
	}
	if want := now.Add(time.Minute); !creds.ExpiresAt.Equal(want) {
		t.Fatalf("expiry = %s, want %s", creds.ExpiresAt, want)
	}
}

func TestPasswordLoginReturnsMatrixError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errcode":"M_FORBIDDEN","error":"bad login"}`))
	}))
	defer server.Close()
	_, err := (PasswordAuthenticator{BaseURL: server.URL, Client: server.Client()}).Login(context.Background(), "alice", "bad")
	if err == nil {
		t.Fatal("expected login error")
	}
}
