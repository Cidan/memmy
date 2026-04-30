// Package embedcache caches embedding vectors keyed by
// (model_id, dim, sha256(text)) so repeated ingest runs over the same
// corpus do not re-embed unchanged chunks. The store is its own SQLite
// file; it shares no schema with memmy's reference backend.
package embedcache

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/Cidan/memmy/internal/embed"
)

// Cache is a content-addressed embedding cache backed by SQLite.
type Cache struct {
	db   *sql.DB
	path string
}

// Open opens or creates the cache file at path.
func Open(path string) (*Cache, error) {
	if path == "" {
		return nil, errors.New("embedcache: path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("embedcache: mkdir: %w", err)
	}
	v := url.Values{}
	v.Set("_journal_mode", "WAL")
	v.Set("_synchronous", "NORMAL")
	v.Set("_busy_timeout", "5000")
	dsn := "file:" + path + "?" + v.Encode()
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("embedcache: open: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("embedcache: ping: %w", err)
	}
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS embeddings (
  model_id    TEXT NOT NULL,
  dim         INTEGER NOT NULL,
  text_sha256 TEXT NOT NULL,
  vector      BLOB NOT NULL,
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (model_id, dim, text_sha256)
) WITHOUT ROWID;
`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("embedcache: schema: %w", err)
	}
	return &Cache{db: db, path: path}, nil
}

// Close releases the database handle.
func (c *Cache) Close() error {
	if c.db == nil {
		return nil
	}
	err := c.db.Close()
	c.db = nil
	return err
}

// HashText returns the sha256 hex digest used for the cache key.
func HashText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// Get returns (vec, true) when a vector for (modelID, dim, text) is
// cached. The vector is freshly allocated on each call so callers
// cannot accidentally mutate the stored row.
func (c *Cache) Get(ctx context.Context, modelID string, dim int, text string) ([]float32, bool, error) {
	var raw []byte
	err := c.db.QueryRowContext(ctx, `
		SELECT vector FROM embeddings WHERE model_id=? AND dim=? AND text_sha256=?
	`, modelID, dim, HashText(text)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("embedcache: get: %w", err)
	}
	if len(raw) != dim*4 {
		return nil, false, fmt.Errorf("embedcache: stored vector has %d bytes, want %d", len(raw), dim*4)
	}
	out := make([]float32, dim)
	decodeVector(raw, out)
	return out, true, nil
}

// Put stores a vector under (modelID, dim, text). Idempotent.
func (c *Cache) Put(ctx context.Context, modelID string, dim int, text string, vec []float32) error {
	if len(vec) != dim {
		return fmt.Errorf("embedcache: vec has %d components, want %d", len(vec), dim)
	}
	raw := encodeVector(vec)
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO embeddings(model_id, dim, text_sha256, vector, created_at)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(model_id, dim, text_sha256) DO UPDATE SET vector=excluded.vector
	`, modelID, dim, HashText(text), raw, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("embedcache: put: %w", err)
	}
	return nil
}

// EmbedBatch returns vectors aligned with `texts`, hitting the cache
// for known content and calling embedder for the misses. The result
// preserves input order regardless of cache hit pattern.
func (c *Cache) EmbedBatch(ctx context.Context, e embed.Embedder, modelID string, task embed.EmbedTask, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	dim := e.Dim()
	out := make([][]float32, len(texts))
	var (
		missTexts []string
		missIdx   []int
	)
	for i, t := range texts {
		v, hit, err := c.Get(ctx, modelID, dim, t)
		if err != nil {
			return nil, err
		}
		if hit {
			out[i] = v
			continue
		}
		missTexts = append(missTexts, t)
		missIdx = append(missIdx, i)
	}
	if len(missTexts) == 0 {
		return out, nil
	}
	vecs, err := e.Embed(ctx, task, missTexts)
	if err != nil {
		return nil, fmt.Errorf("embedcache: embed misses: %w", err)
	}
	if len(vecs) != len(missTexts) {
		return nil, fmt.Errorf("embedcache: embedder returned %d vecs for %d texts", len(vecs), len(missTexts))
	}
	for j, idx := range missIdx {
		out[idx] = vecs[j]
		if err := c.Put(ctx, modelID, dim, missTexts[j], vecs[j]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Count returns the total number of cached vectors.
func (c *Cache) Count(ctx context.Context) (int, error) {
	var n int
	err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM embeddings`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("embedcache: count: %w", err)
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
