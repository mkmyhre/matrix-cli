package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNewHTTPClientHasBoundedTimeout(t *testing.T) {
	client := NewHTTPClient()
	if client.Timeout != defaultHTTPTimeout || client.Timeout <= 0 {
		t.Fatalf("timeout = %s", client.Timeout)
	}
	if httpClient(client) != client || httpClient(nil) != sharedHTTPClient {
		t.Fatal("HTTP client selection did not preserve configured/shared clients")
	}
}

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

func TestSSOLoginUsesBrowserCallbackAndTokenExchange(t *testing.T) {
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/_matrix/client/v3/login":
			_, _ = w.Write([]byte(`{"flows":[{"type":"m.login.sso","identity_providers":[{"id":"work"}]}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/_matrix/client/v3/login":
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			_, _ = w.Write([]byte(`{"access_token":"access","refresh_token":"refresh","user_id":"@alice:test","device_id":"DEV"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var opened string
	opener := func(target string) error {
		opened = target
		parsed, err := url.Parse(target)
		if err != nil {
			return err
		}
		callback := parsed.Query().Get("redirectUrl")
		resp, err := http.Get(callback + "?loginToken=one-time-token")
		if resp != nil {
			_ = resp.Body.Close()
		}
		return err
	}
	creds, err := (SSOAuthenticator{
		BaseURL: server.URL, Client: server.Client(), DeviceName: "matrix-cli (work)",
		OpenBrowser: opener, Timeout: time.Second,
	}).Login(context.Background(), "work")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(opened, "/_matrix/client/v3/login/sso/redirect/work?") {
		t.Fatalf("opened URL = %q", opened)
	}
	if payload["type"] != "m.login.token" || payload["token"] != "one-time-token" || payload["refresh_token"] != true {
		t.Fatalf("token payload = %#v", payload)
	}
	if payload["initial_device_display_name"] != "matrix-cli (work)" {
		t.Fatalf("device name missing from payload: %#v", payload)
	}
	if creds.AccessToken != "access" || creds.RefreshToken != "refresh" || creds.DeviceID != "DEV" {
		t.Fatalf("credentials = %#v", creds)
	}
}

func TestSSOLoginRejectsUnsupportedFlow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"flows":[{"type":"m.login.password"}]}`))
	}))
	defer server.Close()
	_, err := (SSOAuthenticator{BaseURL: server.URL, Client: server.Client()}).Login(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "m.login.sso") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestOAuthRefresh(t *testing.T) {
	var form url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		form = r.Form
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":120}`))
	}))
	defer server.Close()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	creds, err := RefreshOAuth(context.Background(), server.Client(), func() time.Time { return now }, Credentials{
		RefreshToken: "old-refresh", OAuthClientID: "client", OAuthTokenEndpoint: server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if form.Get("grant_type") != "refresh_token" || form.Get("client_id") != "client" || form.Get("refresh_token") != "old-refresh" {
		t.Fatalf("form = %#v", form)
	}
	if creds.AccessToken != "new-access" || creds.RefreshToken != "new-refresh" || !creds.ExpiresAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("credentials = %#v", creds)
	}
}

func TestSSOFallsBackToDelegatedOAuth(t *testing.T) {
	var server *httptest.Server
	var registration map[string]any
	var tokenForm url.Values
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/_matrix/client/v3/login":
			http.NotFound(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/.well-known/matrix/client":
			_, _ = w.Write([]byte(`{"org.matrix.msc2965.authentication":{"issuer":"` + server.URL + `/"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/.well-known/openid-configuration":
			_, _ = w.Write([]byte(`{"issuer":"` + server.URL + `/","authorization_endpoint":"` + server.URL + `/authorize","token_endpoint":"` + server.URL + `/token","registration_endpoint":"` + server.URL + `/register"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/register":
			if err := json.NewDecoder(r.Body).Decode(&registration); err != nil {
				t.Error(err)
			}
			_, _ = w.Write([]byte(`{"client_id":"dynamic-client"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/token":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			tokenForm = r.Form
			_, _ = w.Write([]byte(`{"access_token":"oauth-access","refresh_token":"oauth-refresh","expires_in":3600}`))
		case r.Method == http.MethodGet && r.URL.Path == "/_matrix/client/v3/account/whoami":
			if r.Header.Get("Authorization") != "Bearer oauth-access" {
				t.Errorf("authorization = %q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"user_id":"@alice:test","device_id":"OAUTHDEV"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var authorizationURL *url.URL
	opener := func(target string) error {
		var err error
		authorizationURL, err = url.Parse(target)
		if err != nil {
			return err
		}
		callback := authorizationURL.Query().Get("redirect_uri")
		values := url.Values{"code": {"authorization-code"}, "state": {authorizationURL.Query().Get("state")}}
		resp, err := http.Get(callback + "?" + values.Encode())
		if resp != nil {
			_ = resp.Body.Close()
		}
		return err
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	creds, err := (SSOAuthenticator{
		BaseURL: server.URL, HomeserverURL: server.URL, Client: server.Client(),
		OpenBrowser: opener, Timeout: time.Second, Now: func() time.Time { return now },
	}).Login(context.Background(), "keycloak")
	if err != nil {
		t.Fatal(err)
	}
	if registration["token_endpoint_auth_method"] != "none" || registration["application_type"] != "native" {
		t.Fatalf("registration = %#v", registration)
	}
	scope := authorizationURL.Query().Get("scope")
	if !strings.Contains(scope, legacyClientScope) || !strings.Contains(scope, "urn:matrix:org.matrix.msc2967.client:device:") {
		t.Fatalf("scope = %q", scope)
	}
	if tokenForm.Get("code_verifier") == "" || tokenForm.Get("client_id") != "dynamic-client" {
		t.Fatalf("token form = %#v", tokenForm)
	}
	if creds.UserID != "@alice:test" || creds.DeviceID != "OAUTHDEV" || creds.OAuthClientID != "dynamic-client" || creds.OAuthTokenEndpoint != server.URL+"/token" {
		t.Fatalf("credentials = %#v", creds)
	}
	if want := now.Add(time.Hour); !creds.ExpiresAt.Equal(want) {
		t.Fatalf("expiry = %s, want %s", creds.ExpiresAt, want)
	}
}
