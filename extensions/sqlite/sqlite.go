// Package sqlite provides the persistent local Store for core.Runtime.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/emotional-data8482/automata/core"
	_ "modernc.org/sqlite"
)

const schemaVersion = 1

var ErrOwned = errors.New("sqlite runtime store already has a local owner")

type Store struct {
	db       *sql.DB
	lockFile *os.File
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
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Transaction(ctx context.Context, writable bool, fn func(core.StoreTransaction) error) error {
	if fn == nil {
		return fmt.Errorf("nil sqlite store transaction")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: !writable})
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
	var errs []error
	if s.db != nil {
		errs = append(errs, s.db.Close())
		s.db = nil
	}
	if s.lockFile != nil {
		errs = append(errs, unlockClose(s.lockFile))
		s.lockFile = nil
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
	_, err := tx.tx.Exec(`INSERT INTO runtime_kv(bucket, key, value) VALUES (?, ?, ?)
		ON CONFLICT(bucket, key) DO UPDATE SET value=excluded.value`, bucket, key, value)
	return err
}

func (tx *transaction) Scan(bucket, prefix string, visit func(string, []byte) error) error {
	if visit == nil {
		return nil
	}
	rows, err := tx.tx.Query(`SELECT key, value FROM runtime_kv WHERE bucket=?`, bucket)
	if err != nil {
		return err
	}
	defer rows.Close()
	type item struct {
		key   string
		value []byte
	}
	var items []item
	for rows.Next() {
		var entry item
		if err := rows.Scan(&entry.key, &entry.value); err != nil {
			return err
		}
		if strings.HasPrefix(entry.key, prefix) {
			items = append(items, entry)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].key < items[j].key })
	for _, entry := range items {
		if err := visit(entry.key, append([]byte(nil), entry.value...)); err != nil {
			return err
		}
	}
	return nil
}
