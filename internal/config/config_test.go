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
