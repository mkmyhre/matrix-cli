package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"matrix-cli/internal/auth"
)

// FileStore is a fallback for systems without an OS keyring (for example,
// headless Linux development environments). The file and its directory are
// restricted to the current user.
type FileStore struct{ Path string }

func (s FileStore) Load() (auth.Credentials, error) {
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return auth.Credentials{}, err
	}
	var creds auth.Credentials
	if err := json.Unmarshal(raw, &creds); err != nil {
		return auth.Credentials{}, fmt.Errorf("decode session file: %w", err)
	}
	if !creds.Valid() {
		return auth.Credentials{}, errors.New("invalid session file")
	}
	return creds, nil
}

func (s FileStore) Save(creds auth.Credentials) error {
	if !creds.Valid() {
		return errors.New("refusing to save invalid session")
	}
	raw, err := json.Marshal(creds)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return fmt.Errorf("create session directory: %w", err)
	}
	if err := os.WriteFile(s.Path, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("write session file: %w", err)
	}
	return os.Chmod(s.Path, 0o600)
}

func (s FileStore) Delete() error {
	err := os.Remove(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// FallbackStore prefers the OS keyring and uses a user-only file when the
// keyring service is unavailable. A successful keyring save removes stale
// fallback data.
type FallbackStore struct {
	Primary  Store
	Fallback Store
}

func (s FallbackStore) Load() (auth.Credentials, error) {
	// A fallback file is created only when the latest keyring write failed, so
	// it is authoritative while present. This avoids resurrecting an older
	// keyring token when a temporarily unavailable keyring comes back.
	creds, fallbackErr := s.Fallback.Load()
	if fallbackErr == nil {
		if err := s.Primary.Save(creds); err == nil {
			_ = s.Fallback.Delete()
		}
		return creds, nil
	}
	creds, primaryErr := s.Primary.Load()
	if primaryErr == nil {
		return creds, nil
	}
	return auth.Credentials{}, fmt.Errorf("keyring: %v; fallback file: %w", primaryErr, fallbackErr)
}

func (s FallbackStore) Save(creds auth.Credentials) error {
	if err := s.Primary.Save(creds); err == nil {
		_ = s.Fallback.Delete()
		return nil
	}
	if err := s.Fallback.Save(creds); err != nil {
		return fmt.Errorf("save fallback session: %w", err)
	}
	return nil
}

func (s FallbackStore) Delete() error {
	primaryErr := s.Primary.Delete()
	fallbackErr := s.Fallback.Delete()
	if fallbackErr != nil {
		return fallbackErr
	}
	// If the fallback was in use, an unavailable keyring is expected and there
	// is no keyring secret to remove. Otherwise surface primary deletion errors.
	if primaryErr != nil {
		if _, err := s.Primary.Load(); err == nil {
			return primaryErr
		}
	}
	return nil
}
