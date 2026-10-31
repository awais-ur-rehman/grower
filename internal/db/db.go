// Package db opens the SQLite database, applies embedded migrations and exposes the
// sqlc-generated queries (package dbgen).
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/reilandeubank/grower/internal/db/dbgen"

	"modernc.org/sqlite" // also registers the pure-Go "sqlite" driver
	sqlite3 "modernc.org/sqlite/lib"
)

// FileName is the database file inside GROWER_DATA_DIR.
const FileName = "grower.db"

// readPoolSize bounds concurrent readers; WAL lets them run alongside the writer.
const readPoolSize = 4

// DB holds two pools over the same file. SQLite allows one writer at a time, so all
// writes go through a single connection (no SQLITE_BUSY between our own goroutines);
// reads use a small query_only pool.
type DB struct {
	Write *sql.DB
	Read  *sql.DB
	// Path is the database file.
	Path string
}

// Open opens (creating if needed) <dataDir>/grower.db. It does not run migrations.
func Open(dataDir string) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir %q: %w", dataDir, err)
	}
	path := filepath.Join(dataDir, FileName)

	write, err := openPool(path, false)
	if err != nil {
		return nil, err
	}
	write.SetMaxOpenConns(1)

	read, err := openPool(path, true)
	if err != nil {
		_ = write.Close()
		return nil, err
	}
	read.SetMaxOpenConns(readPoolSize)

	return &DB{Write: write, Read: read, Path: path}, nil
}

func openPool(path string, readOnly bool) (*sql.DB, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	if readOnly {
		q.Add("_pragma", "query_only(1)")
	} else {
		// Take the write lock at BEGIN so a transaction never fails half-way on upgrade.
		q.Set("_txlock", "immediate")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve sqlite path %q: %w", path, err)
	}
	uriPath := filepath.ToSlash(absPath)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath // A Windows drive letter belongs in the path, not the URI host.
	}
	dsn := (&url.URL{Scheme: "file", Path: uriPath, RawQuery: q.Encode()}).String()

	pool, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	if err := pool.Ping(); err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("connect sqlite %q: %w", path, err)
	}
	return pool, nil
}

// Close closes both pools.
func (d *DB) Close() error {
	return errors.Join(d.Read.Close(), d.Write.Close())
}

// Ping checks that the database answers on the read pool.
func (d *DB) Ping(ctx context.Context) error {
	return d.Read.PingContext(ctx)
}

// Reader returns queries bound to the read pool.
func (d *DB) Reader() *dbgen.Queries { return dbgen.New(d.Read) }

// Writer returns queries bound to the single writer connection.
func (d *DB) Writer() *dbgen.Queries { return dbgen.New(d.Write) }

// InTx runs fn inside a write transaction, committing if fn returns nil.
func (d *DB) InTx(ctx context.Context, fn func(q *dbgen.Queries) error) error {
	tx, err := d.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if err := fn(dbgen.New(tx)); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// IsUniqueViolation reports whether err is a UNIQUE constraint failure.
func IsUniqueViolation(err error) bool { return hasCode(err, sqlite3.SQLITE_CONSTRAINT_UNIQUE) }

// IsForeignKeyViolation reports whether err is a FOREIGN KEY constraint failure.
func IsForeignKeyViolation(err error) bool { return hasCode(err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY) }

func hasCode(err error, code int) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && e.Code() == code
}

// Size is the bytes on disk of the database file plus its write-ahead log, which
// holds recent writes until the next checkpoint.
func (d *DB) Size() (int64, error) {
	var total int64
	for _, p := range []string{d.Path, d.Path + "-wal"} {
		fi, err := os.Stat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		total += fi.Size()
	}
	return total, nil
}
