// Package store owns the SQLite connection discipline: WAL, full sync for the
// event log, and a single-writer connection so SQLITE_BUSY storms cannot happen.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

type DB struct {
	W *sql.DB // single-writer connection: all mutations
	R *sql.DB // read pool: WAL allows concurrent readers
}

func Open(path string) (*DB, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)&_pragma=foreign_keys(1)"
	w, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	r, err := sql.Open("sqlite", dsn)
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4)
	db := &DB{W: w, R: r}
	if err := db.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

func (db *DB) migrate() error {
	var version int
	if err := db.W.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for i, name := range names {
		v := i + 1
		if v <= version {
			continue
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		if _, err := db.W.Exec(string(body)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if _, err := db.W.Exec(fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
			return err
		}
	}
	return nil
}

// Write runs fn inside a transaction on the single-writer connection.
func (db *DB) Write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (db *DB) Close() error {
	db.R.Close()
	return db.W.Close()
}
