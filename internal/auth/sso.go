package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

const defaultSSOTimeout = 5 * time.Minute

// SSOAuthenticator implements the standard Matrix m.login.sso browser flow.
// The homeserver may use any upstream identity provider (Keycloak, Okta, etc.).
type SSOAuthenticator struct {
	BaseURL       string
	HomeserverURL string
	Client        *http.Client
	Now           func() time.Time
	DeviceName    string
	OpenBrowser   func(string) error
	Output        io.Writer
	Timeout       time.Duration
}

type loginFlow struct {
	Type              string `json:"type"`
	IdentityProviders []struct {
		ID string `json:"id"`
	} `json:"identity_providers"`
}

func (a SSOAuthenticator) Login(ctx context.Context, identityProvider string) (Credentials, error) {
	if strings.TrimSpace(a.BaseURL) == "" {
		return Credentials{}, errors.New("authentication URL is required")
	}
	if err := a.checkFlow(ctx, identityProvider); err != nil {
		creds, oauthErr := a.loginDelegatedOAuth(ctx)
		if oauthErr == nil {
			return creds, nil
		}
		return Credentials{}, fmt.Errorf("legacy Matrix SSO unavailable (%v); delegated OAuth unavailable: %w", err, oauthErr)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Credentials{}, fmt.Errorf("start SSO callback listener: %w", err)
	}
	defer listener.Close()

	nonce, err := randomURLToken(32)
	if err != nil {
		return Credentials{}, fmt.Errorf("create SSO callback nonce: %w", err)
	}
	callbackPath := "/_matrix-cli/sso/" + nonce
	callbackURL := "http://" + listener.Addr().String() + callbackPath
	result := make(chan ssoCallback, 1)
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		callback := ssoCallback{
			token:       r.URL.Query().Get("loginToken"),
			code:        r.URL.Query().Get("error"),
			description: r.URL.Query().Get("error_description"),
		}
		if callback.token == "" && callback.code == "" {
			callback.code = "missing_login_token"
			callback.description = "the homeserver callback contained no loginToken"
		}
		once.Do(func() { result <- callback })
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if callback.token != "" {
			_, _ = io.WriteString(w, "<!doctype html><title>Matrix login complete</title><p>Login complete. You can close this window and return to matrix-cli.</p>")
		} else {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "<!doctype html><title>Matrix login failed</title><p>Login failed. Return to matrix-cli for details.</p>")
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serveDone := make(chan struct{})
	go func() {
		_ = server.Serve(listener)
		close(serveDone)
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		<-serveDone
	}()

	loginURL, err := a.loginURL(callbackURL, identityProvider)
	if err != nil {
		return Credentials{}, err
	}
	out := a.Output
	if out != nil {
		fmt.Fprintf(out, "Open this URL to sign in:\n%s\n", loginURL)
	}
	opener := a.OpenBrowser
	if opener == nil {
		opener = openBrowser
	}
	if err = opener(loginURL); err != nil && out != nil {
		fmt.Fprintf(out, "Could not open a browser automatically: %v\n", err)
	}

	timeout := a.Timeout
	if timeout <= 0 {
		timeout = defaultSSOTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var callback ssoCallback
	select {
	case callback = <-result:
	case <-ctx.Done():
		return Credentials{}, ctx.Err()
	case <-timer.C:
		return Credentials{}, errors.New("timed out waiting for browser SSO login")
	}
	if callback.token == "" {
		if callback.description == "" {
			callback.description = "identity provider rejected the login"
		}
		return Credentials{}, fmt.Errorf("SSO login failed (%s): %s", callback.code, callback.description)
	}

	payload := map[string]any{
		"type":          "m.login.token",
		"token":         callback.token,
		"refresh_token": true,
	}
	if a.DeviceName != "" {
		payload["initial_device_display_name"] = a.DeviceName
	}
	creds, err := (PasswordAuthenticator{BaseURL: a.BaseURL, Client: a.Client, Now: a.Now}).request(ctx, "/_matrix/client/v3/login", payload)
	if err == nil && creds.UserID == "" {
		return Credentials{}, errors.New("authentication response contained no user ID")
	}
	return creds, err
}

type ssoCallback struct {
	token       string
	code        string
	description string
}

func (a SSOAuthenticator) checkFlow(ctx context.Context, identityProvider string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(a.BaseURL, "/")+"/_matrix/client/v3/login", nil)
	if err != nil {
		return err
	}
	client := a.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("discover Matrix login flows: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("discover Matrix login flows: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Flows []loginFlow `json:"flows"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return fmt.Errorf("decode Matrix login flows: %w", err)
	}
	for _, flow := range body.Flows {
		if flow.Type != "m.login.sso" {
			continue
		}
		if identityProvider == "" || len(flow.IdentityProviders) == 0 {
			return nil
		}
		for _, provider := range flow.IdentityProviders {
			if provider.ID == identityProvider {
				return nil
			}
		}
		return fmt.Errorf("SSO identity provider %q is not advertised by the homeserver", identityProvider)
	}
	return errors.New("homeserver does not advertise the m.login.sso login flow")
}

func (a SSOAuthenticator) loginURL(callbackURL, identityProvider string) (string, error) {
	base, err := url.Parse(strings.TrimRight(a.BaseURL, "/"))
	if err != nil {
		return "", fmt.Errorf("parse authentication URL: %w", err)
	}
	if (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return "", errors.New("authentication URL must be an absolute HTTP(S) URL")
	}
	path := "/_matrix/client/v3/login/sso/redirect"
	escapedBasePath := strings.TrimRight(base.EscapedPath(), "/")
	base.Path = strings.TrimRight(base.Path, "/") + path
	base.RawPath = escapedBasePath + path
	if identityProvider != "" {
		base.Path += "/" + identityProvider
		base.RawPath += "/" + url.PathEscape(identityProvider)
	}
	base.RawQuery = url.Values{"redirectUrl": []string{callbackURL}}.Encode()
	base.Fragment = ""
	return base.String(), nil
}

func randomURLToken(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func openBrowser(target string) error {
	var command string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		command, args = "open", []string{target}
	case "windows":
		command, args = "rundll32", []string{"url.dll,FileProtocolHandler", target}
	default:
		command, args = "xdg-open", []string{target}
	}
	cmd := exec.Command(command, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
