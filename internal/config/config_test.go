package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestAccountPathAndList(t *testing.T) {
	root := t.TempDir()
	defaultPath := filepath.Join(root, "config.json")
	if got, err := AccountPath(defaultPath, "default"); err != nil || got != defaultPath {
		t.Fatalf("default path = %q, %v", got, err)
	}
	workPath, err := AccountPath(defaultPath, "work-eu")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "accounts", "work-eu", "config.json"); workPath != want {
		t.Fatalf("work path = %q, want %q", workPath, want)
	}
	if _, err := AccountPath(defaultPath, "../escape"); err == nil {
		t.Fatal("unsafe account name was accepted")
	}

	cfg := Config{HomeserverURL: "https://matrix.test", AuthURL: "https://matrix.test", Username: "alice"}
	if err := (FileStore{Path: defaultPath}).Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := (FileStore{Path: workPath}).Save(cfg); err != nil {
		t.Fatal(err)
	}
	names, err := ListAccounts(defaultPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"default", "work-eu"}) {
		t.Fatalf("accounts = %#v", names)
	}
}

func TestAccountPreferencesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "accounts.json")
	store := AccountPreferencesStore{Path: path}
	defaults, err := store.Load()
	if err != nil || defaults.DefaultAccount != "default" {
		t.Fatalf("missing preferences = %#v, %v", defaults, err)
	}
	if err = store.Save(AccountPreferences{DefaultAccount: "work-dev"}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil || got.DefaultAccount != "work-dev" {
		t.Fatalf("preferences = %#v, %v", got, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("preferences mode = %o", info.Mode().Perm())
	}
}

func TestThreadViewDefaultsToFocusedAndValidates(t *testing.T) {
	cfg := Config{}
	if got := cfg.EffectiveThreadView(); got != ThreadViewFocused {
		t.Fatalf("default thread view = %q", got)
	}
	cfg.ThreadView = ThreadViewSplit
	if got := cfg.EffectiveThreadView(); got != ThreadViewSplit {
		t.Fatalf("configured thread view = %q", got)
	}
	cfg = Config{HomeserverURL: "https://matrix.test", AuthURL: "https://matrix.test", Username: "alice", ThreadView: "tiles"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("invalid thread view was accepted")
	}
	if !ValidAccountColor("#12aBcF") || ValidAccountColor("red") || ValidAccountColor("#12345x") {
		t.Fatal("account color validation is incorrect")
	}
	if got := (Config{}).EffectiveTheme(); got != ThemeMinimal {
		t.Fatalf("default theme = %q", got)
	}
	if got := (Config{Theme: ThemeBoxed}).EffectiveTheme(); got != ThemeBoxed {
		t.Fatalf("configured theme = %q", got)
	}
}

func TestConfigValidatesBaseURLs(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{name: "relative", url: "matrix.example"},
		{name: "unsupported scheme", url: "file:///tmp/matrix"},
		{name: "missing host", url: "https:///matrix"},
		{name: "credentials", url: "https://alice:secret@matrix.example"},
		{name: "query", url: "https://matrix.example?tenant=one"},
		{name: "fragment", url: "https://matrix.example/#client"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{HomeserverURL: test.url, AuthURL: "https://auth.example", Username: "alice"}
			if err := cfg.Validate(); err == nil {
				t.Fatalf("URL %q was accepted", test.url)
			}
		})
	}
	for _, valid := range []string{"https://matrix.example", "http://localhost:8008", "https://example.test/matrix"} {
		cfg := Config{HomeserverURL: valid, AuthURL: valid, Username: "alice"}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("valid URL %q rejected: %v", valid, err)
		}
	}
}

func TestConfigValidatesKeybindingsAndConflicts(t *testing.T) {
	base := Config{HomeserverURL: "https://matrix.test", AuthURL: "https://matrix.test", Username: "alice"}
	tests := []struct {
		name     string
		bindings map[string]string
	}{
		{name: "unknown action", bindings: map[string]string{"launch_missiles": "x"}},
		{name: "empty key", bindings: map[string]string{"quit": ""}},
		{name: "whitespace", bindings: map[string]string{"quit": " q "}},
		{name: "conflict", bindings: map[string]string{"quit": "j"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			cfg.Keybindings = test.bindings
			if err := cfg.Validate(); err == nil {
				t.Fatalf("bindings %#v were accepted", test.bindings)
			}
		})
	}

	base.Keybindings = map[string]string{"normal_mode": "esc", "send": "enter", "move_down": "down"}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid contextual/default bindings rejected: %v", err)
	}
}

func TestFileStoreRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	store := FileStore{Path: path}
	want := Config{HomeserverURL: "https://matrix.test", AuthURL: "https://auth.test", ServerName: "test", Username: "alice"}
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	if err := store.Delete(); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}
