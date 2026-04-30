package queries

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Store is the per-dataset queries.sqlite handle.
type Store struct {
	db   *sql.DB
	path string
}

// OpenStore opens or creates the queries database at path.
func OpenStore(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("queries: store path required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("queries: mkdir: %w", err)
	}
	v := url.Values{}
	v.Set("_journal_mode", "WAL")
	v.Set("_synchronous", "NORMAL")
	v.Set("_busy_timeout", "5000")
	dsn := "file:" + path + "?" + v.Encode()
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("queries: open: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("queries: ping: %w", err)
	}
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS queries (
  id                   TEXT PRIMARY KEY,
  category             TEXT NOT NULL,
  text                 TEXT NOT NULL,
  gold_turn_uuids_json TEXT NOT NULL,
  notes                TEXT NOT NULL,
  generated_at_unix_ms INTEGER NOT NULL,
  generator_version    TEXT NOT NULL,
  corpus_snapshot_hash TEXT NOT NULL,
  embedding            BLOB
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS idx_queries_category ON queries(category);
CREATE INDEX IF NOT EXISTS idx_queries_gen_corpus ON queries(generator_version, corpus_snapshot_hash, category);

CREATE TABLE IF NOT EXISTS judgments (
  query_id           TEXT NOT NULL,
  candidate_set_hash TEXT NOT NULL,
  judge_version      TEXT NOT NULL,
  verdict_json       TEXT NOT NULL,
  judged_at_unix_ms  INTEGER NOT NULL,
  PRIMARY KEY (query_id, candidate_set_hash, judge_version)
) WITHOUT ROWID;
`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("queries: schema: %w", err)
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

// CountForGeneration returns the number of queries already stored that
// match the (generator_version, corpus_snapshot_hash, category) tuple.
// Used by the queries subcommand to decide whether to re-run the
// generator at all.
func (s *Store) CountForGeneration(ctx context.Context, generatorVersion, corpusSnapshotHash string, category Category) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM queries
		WHERE generator_version=? AND corpus_snapshot_hash=? AND category=?
	`, generatorVersion, corpusSnapshotHash, string(category)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("queries: count for generation: %w", err)
	}
	return n, nil
}

// Put inserts a labeled query. Idempotent: same ID is a no-op (preserves
// the original generated_at and embedding so re-running the generator
// after data appears does not blow away the cached vector).
func (s *Store) Put(ctx context.Context, q LabeledQuery, generatorVersion, corpusSnapshotHash string) error {
	if q.ID == "" {
		q.ID = QueryID(q.Text, q.Category)
	}
	gold, err := json.Marshal(q.GoldTurnUUIDs)
	if err != nil {
		return fmt.Errorf("queries: marshal gold: %w", err)
	}
	if q.GeneratedAt.IsZero() {
		q.GeneratedAt = time.Now().UTC()
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO queries(
		  id, category, text, gold_turn_uuids_json, notes, generated_at_unix_ms,
		  generator_version, corpus_snapshot_hash, embedding
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)
	`, q.ID, string(q.Category), q.Text, gold, q.Notes, q.GeneratedAt.UnixMilli(), generatorVersion, corpusSnapshotHash)
	if err != nil {
		return fmt.Errorf("queries: put: %w", err)
	}
	return nil
}

// PutEmbedding stores the query embedding. Idempotent.
func (s *Store) PutEmbedding(ctx context.Context, queryID string, vec []float32) error {
	if queryID == "" {
		return errors.New("queries: empty queryID")
	}
	raw := encodeVector(vec)
	_, err := s.db.ExecContext(ctx, `UPDATE queries SET embedding=? WHERE id=?`, raw, queryID)
	if err != nil {
		return fmt.Errorf("queries: put embedding: %w", err)
	}
	return nil
}

// Embedding returns the stored embedding, or (nil, false) when none.
func (s *Store) Embedding(ctx context.Context, queryID string, dim int) ([]float32, bool, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT embedding FROM queries WHERE id=?`, queryID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("queries: get embedding: %w", err)
	}
	if len(raw) == 0 {
		return nil, false, nil
	}
	if len(raw) != dim*4 {
		return nil, false, fmt.Errorf("queries: stored embedding has %d bytes, want %d", len(raw), dim*4)
	}
	out := make([]float32, dim)
	decodeVector(raw, out)
	return out, true, nil
}

// All returns every labeled query in storage order (by category then ID).
func (s *Store) All(ctx context.Context) ([]LabeledQuery, error) {
	return s.list(ctx, `SELECT id, category, text, gold_turn_uuids_json, notes, generated_at_unix_ms FROM queries ORDER BY category ASC, id ASC`)
}

// ByCategory returns labeled queries filtered to one category.
func (s *Store) ByCategory(ctx context.Context, c Category) ([]LabeledQuery, error) {
	return s.list(ctx, `SELECT id, category, text, gold_turn_uuids_json, notes, generated_at_unix_ms FROM queries WHERE category=? ORDER BY id ASC`, string(c))
}

func (s *Store) list(ctx context.Context, query string, args ...any) ([]LabeledQuery, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("queries: list: %w", err)
	}
	defer rows.Close()
	var out []LabeledQuery
	for rows.Next() {
		var (
			lq      LabeledQuery
			cat     string
			goldRaw []byte
			tsMs    int64
		)
		if err := rows.Scan(&lq.ID, &cat, &lq.Text, &goldRaw, &lq.Notes, &tsMs); err != nil {
			return nil, err
		}
		lq.Category = Category(cat)
		lq.GeneratedAt = time.UnixMilli(tsMs).UTC()
		if err := json.Unmarshal(goldRaw, &lq.GoldTurnUUIDs); err != nil {
			return nil, fmt.Errorf("queries: decode gold: %w", err)
		}
		out = append(out, lq)
	}
	return out, rows.Err()
}

// Count returns the total number of queries.
func (s *Store) Count(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM queries`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("queries: count: %w", err)
	}
	return n, nil
}

func encodeVector(v []float32) []byte {
	out := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(f))
	}
	return out
}

func decodeVector(b []byte, out []float32) {
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
}
