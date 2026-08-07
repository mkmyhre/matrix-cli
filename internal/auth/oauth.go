package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	legacyClientScope = "urn:matrix:org.matrix.msc2967.client:api:*"
	stableClientScope = "urn:matrix:client:api:*"
)

type delegatedAuth struct {
	Issuer    string
	Legacy    bool
	Authorize string
	Token     string
	Register  string
}

type oauthClientRegistration struct {
	ClientID string `json:"client_id"`
}

// loginDelegatedOAuth performs Matrix delegated authentication using OAuth 2.0
// authorization code flow, dynamic client registration, and PKCE.
func (a SSOAuthenticator) loginDelegatedOAuth(ctx context.Context) (Credentials, error) {
	metadata, err := a.discoverDelegatedAuth(ctx)
	if err != nil {
		return Credentials{}, err
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Credentials{}, fmt.Errorf("start OAuth callback listener: %w", err)
	}
	defer listener.Close()
	nonce, err := randomURLToken(32)
	if err != nil {
		return Credentials{}, fmt.Errorf("create OAuth callback nonce: %w", err)
	}
	callbackPath := "/_matrix-cli/oauth/" + nonce
	callbackURL := "http://" + listener.Addr().String() + callbackPath

	clientID, err := a.registerOAuthClient(ctx, metadata.Register, callbackURL)
	if err != nil {
		return Credentials{}, err
	}
	state, err := randomURLToken(32)
	if err != nil {
		return Credentials{}, err
	}
	verifier, err := randomURLToken(48)
	if err != nil {
		return Credentials{}, err
	}
	challengeBytes := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeBytes[:])
	requestedDeviceID, err := randomURLToken(12)
	if err != nil {
		return Credentials{}, err
	}

	result := make(chan oauthCallback, 1)
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		callback := oauthCallback{
			code:        r.URL.Query().Get("code"),
			state:       r.URL.Query().Get("state"),
			errorCode:   r.URL.Query().Get("error"),
			description: r.URL.Query().Get("error_description"),
		}
		once.Do(func() { result <- callback })
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if callback.code != "" && callback.state == state {
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

	scopePrefix := "urn:matrix:client:device:"
	clientScope := stableClientScope
	if metadata.Legacy {
		scopePrefix = "urn:matrix:org.matrix.msc2967.client:device:"
		clientScope = legacyClientScope
	}
	authorizeURL, err := url.Parse(metadata.Authorize)
	if err != nil {
		return Credentials{}, fmt.Errorf("parse OAuth authorization endpoint: %w", err)
	}
	query := authorizeURL.Query()
	query.Set("response_type", "code")
	query.Set("response_mode", "query")
	query.Set("client_id", clientID)
	query.Set("redirect_uri", callbackURL)
	query.Set("scope", strings.Join([]string{"openid", clientScope, scopePrefix + requestedDeviceID}, " "))
	query.Set("state", state)
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	authorizeURL.RawQuery = query.Encode()

	out := a.Output
	if out != nil {
		fmt.Fprintf(out, "Open this URL to sign in:\n%s\n", authorizeURL.String())
	}
	opener := a.OpenBrowser
	if opener == nil {
		opener = openBrowser
	}
	if err = opener(authorizeURL.String()); err != nil && out != nil {
		fmt.Fprintf(out, "Could not open a browser automatically: %v\n", err)
	}

	timeout := a.Timeout
	if timeout <= 0 {
		timeout = defaultSSOTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var callback oauthCallback
	select {
	case callback = <-result:
	case <-ctx.Done():
		return Credentials{}, ctx.Err()
	case <-timer.C:
		return Credentials{}, errors.New("timed out waiting for browser OAuth login")
	}
	if callback.state != state {
		return Credentials{}, errors.New("OAuth callback state did not match")
	}
	if callback.code == "" {
		if callback.description == "" {
			callback.description = "authorization server rejected the login"
		}
		return Credentials{}, fmt.Errorf("OAuth login failed (%s): %s", callback.errorCode, callback.description)
	}

	creds, err := a.exchangeOAuthCode(ctx, metadata.Token, clientID, callbackURL, verifier, callback.code)
	if err != nil {
		return Credentials{}, err
	}
	creds.OAuthClientID = clientID
	creds.OAuthTokenEndpoint = metadata.Token
	if err = a.loadOAuthIdentity(ctx, &creds); err != nil {
		return Credentials{}, err
	}
	return creds, nil
}

type oauthCallback struct {
	code        string
	state       string
	errorCode   string
	description string
}

func (a SSOAuthenticator) discoverDelegatedAuth(ctx context.Context) (delegatedAuth, error) {
	homeserver := strings.TrimRight(a.HomeserverURL, "/")
	if homeserver == "" {
		homeserver = strings.TrimRight(a.BaseURL, "/")
	}
	var wellKnown struct {
		Stable struct {
			Issuer string `json:"issuer"`
		} `json:"m.authentication"`
		Legacy struct {
			Issuer string `json:"issuer"`
		} `json:"org.matrix.msc2965.authentication"`
	}
	if err := a.getJSON(ctx, homeserver+"/.well-known/matrix/client", &wellKnown); err != nil {
		return delegatedAuth{}, fmt.Errorf("discover delegated Matrix authentication: %w", err)
	}
	issuer, legacy := wellKnown.Stable.Issuer, false
	if issuer == "" {
		issuer, legacy = wellKnown.Legacy.Issuer, true
	}
	if issuer == "" {
		return delegatedAuth{}, errors.New("homeserver supports neither m.login.sso nor delegated Matrix authentication")
	}
	var oidc struct {
		Issuer                string `json:"issuer"`
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
		RegistrationEndpoint  string `json:"registration_endpoint"`
	}
	discoveryURL := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	if err := a.getJSON(ctx, discoveryURL, &oidc); err != nil {
		return delegatedAuth{}, fmt.Errorf("discover OAuth server metadata: %w", err)
	}
	if strings.TrimRight(oidc.Issuer, "/") != strings.TrimRight(issuer, "/") {
		return delegatedAuth{}, errors.New("OAuth metadata issuer does not match the Matrix well-known issuer")
	}
	if oidc.AuthorizationEndpoint == "" || oidc.TokenEndpoint == "" || oidc.RegistrationEndpoint == "" {
		return delegatedAuth{}, errors.New("OAuth metadata is missing authorization, token, or registration endpoint")
	}
	return delegatedAuth{Issuer: issuer, Legacy: legacy, Authorize: oidc.AuthorizationEndpoint, Token: oidc.TokenEndpoint, Register: oidc.RegistrationEndpoint}, nil
}

func (a SSOAuthenticator) registerOAuthClient(ctx context.Context, endpoint, callbackURL string) (string, error) {
	payload := map[string]any{
		"application_type":           "native",
		"client_name":                "matrix-cli",
		"client_uri":                 "https://github.com/mkmyhre/matrix.tui",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"redirect_uris":              []string{callbackURL},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	}
	var response oauthClientRegistration
	if err := a.postJSON(ctx, endpoint, payload, &response); err != nil {
		return "", fmt.Errorf("register OAuth client: %w", err)
	}
	if response.ClientID == "" {
		return "", errors.New("OAuth registration returned no client ID")
	}
	return response.ClientID, nil
}

func (a SSOAuthenticator) exchangeOAuthCode(ctx context.Context, endpoint, clientID, callbackURL, verifier, code string) (Credentials, error) {
	values := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"redirect_uri":  {callbackURL},
		"code_verifier": {verifier},
		"code":          {code},
	}
	var response struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := a.postForm(ctx, endpoint, values, &response); err != nil {
		return Credentials{}, fmt.Errorf("exchange OAuth authorization code: %w", err)
	}
	if response.AccessToken == "" {
		return Credentials{}, errors.New("OAuth token response contained no access token")
	}
	creds := Credentials{AccessToken: response.AccessToken, RefreshToken: response.RefreshToken}
	if response.ExpiresIn > 0 {
		now := time.Now
		if a.Now != nil {
			now = a.Now
		}
		creds.ExpiresAt = now().Add(time.Duration(response.ExpiresIn) * time.Second)
	}
	return creds, nil
}

func (a SSOAuthenticator) loadOAuthIdentity(ctx context.Context, creds *Credentials) error {
	homeserver := strings.TrimRight(a.HomeserverURL, "/")
	if homeserver == "" {
		homeserver = strings.TrimRight(a.BaseURL, "/")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, homeserver+"/_matrix/client/v3/account/whoami", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	var response struct {
		UserID   string `json:"user_id"`
		DeviceID string `json:"device_id"`
	}
	if err = a.doJSON(req, &response); err != nil {
		return fmt.Errorf("load Matrix identity after OAuth login: %w", err)
	}
	if response.UserID == "" || response.DeviceID == "" {
		return errors.New("Matrix identity response contained no user or device ID")
	}
	creds.UserID, creds.DeviceID = response.UserID, response.DeviceID
	return nil
}

func (a SSOAuthenticator) getJSON(ctx context.Context, endpoint string, output any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	return a.doJSON(req, output)
}

func (a SSOAuthenticator) postJSON(ctx context.Context, endpoint string, payload, output any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return a.doJSON(req, output)
}

func (a SSOAuthenticator) postForm(ctx context.Context, endpoint string, values url.Values, output any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return a.doJSON(req, output)
}

func (a SSOAuthenticator) doJSON(req *http.Request, output any) error {
	resp, err := httpClient(a.Client).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(output); err != nil {
		return err
	}
	return nil
}

func RefreshOAuth(ctx context.Context, client *http.Client, now func() time.Time, creds Credentials) (Credentials, error) {
	values := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {creds.OAuthClientID},
		"refresh_token": {creds.RefreshToken},
	}
	authenticator := SSOAuthenticator{Client: client, Now: now}
	refreshed, err := authenticator.exchangeOAuthRefresh(ctx, creds.OAuthTokenEndpoint, values)
	if err != nil {
		return Credentials{}, err
	}
	return refreshed, nil
}

func (a SSOAuthenticator) exchangeOAuthRefresh(ctx context.Context, endpoint string, values url.Values) (Credentials, error) {
	var response struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := a.postForm(ctx, endpoint, values, &response); err != nil {
		return Credentials{}, err
	}
	if response.AccessToken == "" {
		return Credentials{}, errors.New("OAuth refresh response contained no access token")
	}
	result := Credentials{AccessToken: response.AccessToken, RefreshToken: response.RefreshToken}
	if response.ExpiresIn > 0 {
		now := time.Now
		if a.Now != nil {
			now = a.Now
		}
		result.ExpiresAt = now().Add(time.Duration(response.ExpiresIn) * time.Second)
	}
	return result, nil
}
