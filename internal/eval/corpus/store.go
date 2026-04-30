package corpus

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Store wraps corpus.sqlite, the per-dataset record of which source
// JSONL files have been seen and which turns we extracted from them.
type Store struct {
	db   *sql.DB
	path string
}

// SourceFile is the dedup unit for ingest: the same path+mtime+sha is
// skipped on a re-run.
type SourceFile struct {
	Path        string
	ModTime     time.Time
	SizeBytes   int64
	ContentHash string
	IngestedAt  time.Time
}

// StoredTurn mirrors corpus.Turn but adds a stable per-dataset ID.
type StoredTurn struct {
	ID        int64
	UUID      string
	SessionID string
	Role      string
	Text      string
	Timestamp time.Time
	Source    string
}

// OpenStore opens or creates the corpus database at path.
func OpenStore(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("corpus: store path required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("corpus: mkdir: %w", err)
	}
	v := url.Values{}
	v.Set("_journal_mode", "WAL")
	v.Set("_synchronous", "NORMAL")
	v.Set("_busy_timeout", "5000")
	dsn := "file:" + path + "?" + v.Encode()
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("corpus: open: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("corpus: ping: %w", err)
	}
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS source_files (
  path         TEXT PRIMARY KEY,
  mtime_unix   INTEGER NOT NULL,
  size_bytes   INTEGER NOT NULL,
  content_hash TEXT NOT NULL,
  ingested_at  INTEGER NOT NULL
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS turns (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  uuid         TEXT NOT NULL,
  session_id   TEXT NOT NULL,
  role         TEXT NOT NULL,
  text         TEXT NOT NULL,
  ts_unix_ms   INTEGER NOT NULL,
  source_file  TEXT NOT NULL,
  UNIQUE (uuid, source_file)
);

CREATE INDEX IF NOT EXISTS idx_turns_ts ON turns(ts_unix_ms);
CREATE INDEX IF NOT EXISTS idx_turns_session ON turns(session_id);
`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("corpus: schema: %w", err)
	}
	return &Store{db: db, path: path}, nil
}

// Close releases the handle.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// HasSourceFile reports whether the (path, mtime, sha) tuple has
// already been ingested. Used to short-circuit re-ingest of unchanged
// JSONL files.
func (s *Store) HasSourceFile(ctx context.Context, sf SourceFile) (bool, error) {
	var hash string
	err := s.db.QueryRowContext(ctx, `
		SELECT content_hash FROM source_files WHERE path=? AND mtime_unix=?
	`, sf.Path, sf.ModTime.UnixMilli()).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("corpus: source check: %w", err)
	}
	return hash == sf.ContentHash, nil
}

// PutSourceFile records that a file was ingested.
func (s *Store) PutSourceFile(ctx context.Context, sf SourceFile) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO source_files(path, mtime_unix, size_bytes, content_hash, ingested_at)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
		  mtime_unix=excluded.mtime_unix,
		  size_bytes=excluded.size_bytes,
		  content_hash=excluded.content_hash,
		  ingested_at=excluded.ingested_at
	`, sf.Path, sf.ModTime.UnixMilli(), sf.SizeBytes, sf.ContentHash, sf.IngestedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("corpus: put source: %w", err)
	}
	return nil
}

// PutTurn inserts a turn record. Duplicate (uuid, source_file) is a no-op.
func (s *Store) PutTurn(ctx context.Context, t Turn) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO turns(uuid, session_id, role, text, ts_unix_ms, source_file)
		VALUES(?, ?, ?, ?, ?, ?)
	`, t.UUID, t.SessionID, t.Role, t.Text, t.Timestamp.UnixMilli(), t.SourceFile)
	if err != nil {
		return fmt.Errorf("corpus: put turn: %w", err)
	}
	return nil
}

// CountTurns returns the total number of turn rows.
func (s *Store) CountTurns(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM turns`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("corpus: count: %w", err)
	}
	return n, nil
}

// IterateTurns calls fn for every turn in chronological (ts) order.
// Streamed; safe for large corpora.
func (s *Store) IterateTurns(ctx context.Context, fn func(StoredTurn) error) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, uuid, session_id, role, text, ts_unix_ms, source_file
		FROM turns
		ORDER BY ts_unix_ms ASC, id ASC
	`)
	if err != nil {
		return fmt.Errorf("corpus: iterate: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			t   StoredTurn
			ts  int64
		)
		if err := rows.Scan(&t.ID, &t.UUID, &t.SessionID, &t.Role, &t.Text, &ts, &t.Source); err != nil {
			return fmt.Errorf("corpus: scan: %w", err)
		}
		t.Timestamp = time.UnixMilli(ts).UTC()
		if err := fn(t); err != nil {
			return err
		}
	}
	return rows.Err()
}

// SnapshotHash returns a stable hash of the corpus contents (sorted
// turn UUIDs). Used in manifests to detect corpus drift.
func (s *Store) SnapshotHash(ctx context.Context) (string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT uuid FROM turns`)
	if err != nil {
		return "", fmt.Errorf("corpus: snapshot: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return "", err
		}
		ids = append(ids, u)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	sort.Strings(ids)
	h := sha256.New()
	for _, id := range ids {
		_, _ = h.Write([]byte(id))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16]), nil
}

// HashFile returns the sha256 hex digest of a file's full contents.
// Helper for the SourceFile dedup key.
func HashFile(path string) (string, int64, time.Time, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", 0, time.Time{}, fmt.Errorf("corpus: stat %q: %w", path, err)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, time.Time{}, fmt.Errorf("corpus: open %q: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", 0, time.Time{}, fmt.Errorf("corpus: hash %q: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), st.Size(), st.ModTime(), nil
}
