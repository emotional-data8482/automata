// Package sqlite provides the persistent local Store for core.Runtime.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/emotional-data8482/automata/core"
	_ "modernc.org/sqlite"
)

const schemaVersion = 1

// userVersionPragma is the fixed statement that stamps the schema version.
// SQLite cannot bind PRAGMA arguments as query parameters, so the statement is
// a literal rather than formatted; a test pins it to schemaVersion.
const userVersionPragma = "PRAGMA user_version=1"

var ErrOwned = errors.New("sqlite runtime store already has a local owner")

type Store struct {
	db       *sql.DB
	lockFile *os.File

	mu     sync.Mutex
	closed bool
}

func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("sqlite runtime store path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create sqlite runtime directory: %w", err)
	}
	lockFile, err := os.OpenFile(path+".owner.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open sqlite owner lock: %w", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lockFile.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrOwned
		}
		return nil, fmt.Errorf("claim sqlite owner lock: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		_ = unlockClose(lockFile)
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, lockFile: lockFile}
	if err := store.initialize(ctx); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) initialize(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != 0 && version != schemaVersion {
		return fmt.Errorf("unsupported sqlite runtime schema version %d", version)
	}
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=0",
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply %q: %w", statement, err)
		}
	}
	if version == schemaVersion {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE runtime_kv (
		bucket TEXT NOT NULL,
		key TEXT NOT NULL,
		value BLOB NOT NULL,
		PRIMARY KEY(bucket, key)
	)`); err != nil {
		return err
	}
	// SQLite cannot bind PRAGMA arguments as query parameters; the statement
	// is the fixed literal above, never assembled from input.
	if _, err := tx.ExecContext(ctx, userVersionPragma); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Transaction(ctx context.Context, writable bool, fn func(core.StoreTransaction) error) error {
	if fn == nil {
		return fmt.Errorf("nil sqlite store transaction")
	}
	s.mu.Lock()
	closed, db := s.closed, s.db
	s.mu.Unlock()
	if closed || db == nil {
		return errors.New("sqlite runtime store is closed")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: !writable})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	view := &transaction{tx: tx, writable: writable}
	if err := fn(view); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Close() error {
	s.mu.Lock()
	s.closed = true
	db, lockFile := s.db, s.lockFile
	s.db, s.lockFile = nil, nil
	s.mu.Unlock()
	var errs []error
	if db != nil {
		errs = append(errs, db.Close())
	}
	if lockFile != nil {
		errs = append(errs, unlockClose(lockFile))
	}
	return errors.Join(errs...)
}

func unlockClose(file *os.File) error {
	return errors.Join(syscall.Flock(int(file.Fd()), syscall.LOCK_UN), file.Close())
}

type transaction struct {
	tx       *sql.Tx
	writable bool
}

func (tx *transaction) Get(bucket, key string) ([]byte, error) {
	var value []byte
	err := tx.tx.QueryRow(`SELECT value FROM runtime_kv WHERE bucket=? AND key=?`, bucket, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrStoreKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), value...), nil
}

func (tx *transaction) Put(bucket, key string, value []byte) error {
	if !tx.writable {
		return fmt.Errorf("read-only sqlite store transaction")
	}
	// A nil Go slice is a valid zero-length value in the core contract; bind it
	// as an empty blob, not SQL NULL, so the NOT NULL column accepts it.
	if value == nil {
		value = []byte{}
	}
	_, err := tx.tx.Exec(`INSERT INTO runtime_kv(bucket, key, value) VALUES (?, ?, ?)
		ON CONFLICT(bucket, key) DO UPDATE SET value=excluded.value`, bucket, key, value)
	return err
}

func (tx *transaction) Delete(bucket, key string) error {
	if !tx.writable {
		return fmt.Errorf("read-only sqlite store transaction")
	}
	_, err := tx.tx.Exec(`DELETE FROM runtime_kv WHERE bucket=? AND key=?`, bucket, key)
	return err
}

func (tx *transaction) Scan(bucket, prefix string, visit func(string, []byte) error) error {
	_, err := tx.ScanPage(bucket, prefix, "", 0, visit)
	return err
}

func (tx *transaction) ScanPage(bucket, prefix, after string, limit int, visit func(string, []byte) error) (string, error) {
	if visit == nil {
		return "", fmt.Errorf("nil scan visitor")
	}
	// Seek straight to the prefix range with the (bucket, key) primary key and
	// stream it in bounded pages. Keys sharing a prefix are contiguous in
	// ascending order, so the first key outside the prefix ends the range
	// without reading the rest of the bucket.
	pageSize := 256
	if limit > 0 && limit < pageSize {
		pageSize = limit
	}
	var last string
	visited := 0
	cursor := after
	for limit <= 0 || visited < limit {
		// Give SQLite a single lower bound so it seeks the index to the range
		// start instead of choosing between two constraints.
		query, bound := `SELECT key, value FROM runtime_kv WHERE bucket=? AND key>? ORDER BY key LIMIT ?`, cursor
		if cursor < prefix {
			query, bound = `SELECT key, value FROM runtime_kv WHERE bucket=? AND key>=? ORDER BY key LIMIT ?`, prefix
		}
		rows, err := tx.tx.Query(query, bucket, bound, pageSize)
		if err != nil {
			return last, err
		}
		exhausted := true
		done := false
		for rows.Next() {
			exhausted = false
			var key string
			var value []byte
			if err := rows.Scan(&key, &value); err != nil {
				rows.Close()
				return last, err
			}
			cursor = key
			if !strings.HasPrefix(key, prefix) {
				done = true
				break
			}
			if err := visit(key, append([]byte(nil), value...)); err != nil {
				rows.Close()
				return last, err
			}
			last = key
			visited++
			if limit > 0 && visited >= limit {
				done = true
				break
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return last, err
		}
		if done || exhausted {
			return last, nil
		}
	}
	return last, nil
}
