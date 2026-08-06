//go:build goolm && !cgo

package matrix

import (
	"database/sql"

	"modernc.org/sqlite"
)

// cryptohelper normally registers this driver through go-sqlite3. Register a
// pure-Go equivalent when CGO is disabled.
func init() {
	sql.Register("sqlite3-fk-wal", &sqlite.Driver{})
}
