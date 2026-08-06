package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Credentials are the result of a Matrix login. They must be kept in secret storage.
type Credentials struct {
	AccessToken     string    `json:"access_token"`
	RefreshToken    string    `json:"refresh_token,omitempty"`
	UserID          string    `json:"user_id"`
	DeviceID        string    `json:"device_id,omitempty"`
	ExpiresAt       time.Time `json:"expires_at,omitempty"`
	CryptoPickleKey string    `json:"crypto_pickle_key,omitempty"`
}

func (c Credentials) Valid() bool { return c.AccessToken != "" && c.UserID != "" }
func (c Credentials) Expiring(now time.Time) bool {
	return !c.ExpiresAt.IsZero() && !c.ExpiresAt.After(now.Add(30*time.Second))
}

type PasswordAuthenticator struct {
	BaseURL    string
	Client     *http.Client
	Now        func() time.Time
	DeviceName string
}

func (a PasswordAuthenticator) Login(ctx context.Context, username, password string) (Credentials, error) {
	payload := map[string]any{
		"type":          "m.login.password",
		"identifier":    map[string]any{"type": "m.id.user", "user": username},
		"password":      password,
		"refresh_token": true,
	}
	if a.DeviceName != "" {
		payload["initial_device_display_name"] = a.DeviceName
	}
	creds, err := a.request(ctx, "/_matrix/client/v3/login", payload)
	if err == nil && creds.UserID == "" {
		return Credentials{}, fmt.Errorf("authentication response contained no user ID")
	}
	return creds, err
}

func (a PasswordAuthenticator) Refresh(ctx context.Context, token string) (Credentials, error) {
	if token == "" {
		return Credentials{}, fmt.Errorf("no refresh token available")
	}
	return a.request(ctx, "/_matrix/client/v3/refresh", map[string]any{"refresh_token": token})
}

func (a PasswordAuthenticator) request(ctx context.Context, path string, payload any) (Credentials, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Credentials{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.BaseURL, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return Credentials{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	client := a.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Credentials{}, fmt.Errorf("authentication request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Credentials{}, fmt.Errorf("read authentication response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var matrixErr struct {
			Code    string `json:"errcode"`
			Message string `json:"error"`
		}
		_ = json.Unmarshal(body, &matrixErr)
		if matrixErr.Message == "" {
			matrixErr.Message = strings.TrimSpace(string(body))
		}
		return Credentials{}, fmt.Errorf("authentication failed (HTTP %d, %s): %s", resp.StatusCode, matrixErr.Code, matrixErr.Message)
	}
	var wire struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		UserID       string `json:"user_id"`
		DeviceID     string `json:"device_id"`
		ExpiresInMS  int64  `json:"expires_in_ms"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return Credentials{}, fmt.Errorf("decode authentication response: %w", err)
	}
	creds := Credentials{AccessToken: wire.AccessToken, RefreshToken: wire.RefreshToken, UserID: wire.UserID, DeviceID: wire.DeviceID}
	if wire.ExpiresInMS > 0 {
		now := time.Now
		if a.Now != nil {
			now = a.Now
		}
		creds.ExpiresAt = now().Add(time.Duration(wire.ExpiresInMS) * time.Millisecond)
	}
	if creds.AccessToken == "" {
		return Credentials{}, fmt.Errorf("authentication response contained no access token")
	}
	return creds, nil
}
