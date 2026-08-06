//go:build goolm

package matrix

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestEncryptedClientInitializesPersistentStore(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/_matrix/client/v3/keys/query":
			_ = json.NewEncoder(w).Encode(map[string]any{"device_keys": map[string]any{}})
		case r.URL.Path == "/_matrix/client/v3/keys/upload":
			_ = json.NewEncoder(w).Encode(map[string]any{"one_time_key_counts": map[string]int{"signed_curve25519": 0}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewEncrypted(server.URL, "@alice:test", "DEVICE", "token", filepath.Join(t.TempDir(), "crypto.db"), []byte("test-pickle-key"))
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if encrypted, ok := client.crypto.(*encryptedSupport); ok {
		defer encrypted.helper.Close()
	}
}
