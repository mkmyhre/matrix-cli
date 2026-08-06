package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"matrix-cli/internal/auth"
)

type failingStore struct{}

type memoryStore struct {
	creds   auth.Credentials
	present bool
}

func (s *memoryStore) Load() (auth.Credentials, error) {
	if !s.present {
		return auth.Credentials{}, errors.New("missing")
	}
	return s.creds, nil
}
func (s *memoryStore) Save(creds auth.Credentials) error {
	s.creds, s.present = creds, true
	return nil
}
func (s *memoryStore) Delete() error { s.present = false; return nil }

func (failingStore) Load() (auth.Credentials, error) {
	return auth.Credentials{}, errors.New("unavailable")
}
func (failingStore) Save(auth.Credentials) error { return errors.New("unavailable") }
func (failingStore) Delete() error               { return errors.New("unavailable") }

func TestFallbackStoreMigratesNewerFallbackOverStaleKeyring(t *testing.T) {
	primary := &memoryStore{creds: auth.Credentials{AccessToken: "stale", UserID: "@alice:test"}, present: true}
	fallback := &memoryStore{creds: auth.Credentials{AccessToken: "current", UserID: "@alice:test"}, present: true}
	store := FallbackStore{Primary: primary, Fallback: fallback}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "current" || primary.creds.AccessToken != "current" {
		t.Fatalf("loaded %#v, keyring %#v", got, primary.creds)
	}
	if fallback.present {
		t.Fatal("fallback was not removed after migration")
	}
}

func TestFallbackStoreUsesProtectedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "session.json")
	store := FallbackStore{Primary: failingStore{}, Fallback: FileStore{Path: path}}
	want := auth.Credentials{AccessToken: "secret", UserID: "@alice:test"}
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("session mode = %o", info.Mode().Perm())
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != want.AccessToken || got.UserID != want.UserID {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	if err := store.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fallback file remains after delete: %v", err)
	}
}
