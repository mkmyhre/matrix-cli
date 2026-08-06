//go:build goolm && !cgo

package matrix

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestPureGoCryptoSQLiteDriver(t *testing.T) {
	db, err := sql.Open("sqlite3-fk-wal", "file:"+filepath.Join(t.TempDir(), "crypto.db")+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Ping(); err != nil {
		t.Fatal(err)
	}
}
