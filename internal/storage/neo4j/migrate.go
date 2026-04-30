package neo4jstore

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// migrationFS embeds every Cypher migration shipped with the binary.
// File names MUST match `NNN_description.cypher` and are applied in
// numeric order. Each file may contain multiple statements separated
// by `;` — each statement runs in its own auto-commit transaction
// because Neo4j schema operations cannot share a transaction with
// data operations.
//
//go:embed migrations/*.cypher
var migrationFS embed.FS

const migrationDir = "migrations"

// migration is one parsed migration file: numeric version + the
// Cypher statements it contains.
type migration struct {
	Version    int
	Name       string
	Statements []string
}

// loadMigrations parses the embedded migrationFS into a sorted slice
// of migration records. Statements within a file are split on `;` at
// the top level (Neo4j Cypher does not nest semicolons in our
// scripts; comments are stripped).
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, migrationDir)
	if err != nil {
		return nil, fmt.Errorf("neo4jstore: read migrationFS: %w", err)
	}
	out := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".cypher") {
			continue
		}
		ver, err := parseMigrationVersion(e.Name())
		if err != nil {
			return nil, err
		}
		raw, err := fs.ReadFile(migrationFS, path.Join(migrationDir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("neo4jstore: read %q: %w", e.Name(), err)
		}
		stmts := splitCypherStatements(string(raw))
		out = append(out, migration{
			Version:    ver,
			Name:       e.Name(),
			Statements: stmts,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i := range out {
		if out[i].Version != i+1 {
			return nil, fmt.Errorf("neo4jstore: migration version gap at %s (got %d, want %d)", out[i].Name, out[i].Version, i+1)
		}
	}
	return out, nil
}

// parseMigrationVersion extracts the leading integer from a migration
// filename like "001_description.cypher".
func parseMigrationVersion(name string) (int, error) {
	cut := strings.Index(name, "_")
	if cut < 1 {
		return 0, fmt.Errorf("neo4jstore: bad migration filename %q (need NNN_name.cypher)", name)
	}
	v, err := strconv.Atoi(name[:cut])
	if err != nil {
		return 0, fmt.Errorf("neo4jstore: parse migration version in %q: %w", name, err)
	}
	return v, nil
}

// splitCypherStatements splits a multi-statement script on top-level
// semicolons. Comments (lines starting with `//`) and blank lines are
// stripped. Inline comments are not handled — keep migration files
// clean.
func splitCypherStatements(raw string) []string {
	var out []string
	var b strings.Builder
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
		if strings.HasSuffix(trimmed, ";") {
			stmt := strings.TrimRight(strings.TrimSpace(b.String()), ";")
			if stmt != "" {
				out = append(out, stmt)
			}
			b.Reset()
		}
	}
	if rest := strings.TrimSpace(b.String()); rest != "" {
		out = append(out, rest)
	}
	return out
}

// RequiredSchemaVersion returns the highest migration version
// embedded in the binary. Used to detect schema skew at Open time.
func RequiredSchemaVersion() (int, error) {
	migs, err := loadMigrations()
	if err != nil {
		return 0, err
	}
	if len(migs) == 0 {
		return 0, nil
	}
	return migs[len(migs)-1].Version, nil
}

// CurrentSchemaVersion reads the highest applied migration version
// from the database. Returns 0 when no migrations have been recorded.
func (s *Storage) CurrentSchemaVersion(ctx context.Context) (int, error) {
	res, err := s.withReadSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		r, err := tx.Run(ctx, "MATCH (m:Migration) RETURN max(m.version) AS v", nil)
		if err != nil {
			return 0, err
		}
		rec, err := r.Single(ctx)
		if err != nil {
			return 0, nil // no rows = 0 applied
		}
		raw, ok := rec.Get("v")
		if !ok || raw == nil {
			return 0, nil
		}
		switch v := raw.(type) {
		case int64:
			return int(v), nil
		case int:
			return v, nil
		default:
			return 0, fmt.Errorf("neo4jstore: unexpected version type %T", raw)
		}
	})
	if err != nil {
		return 0, fmt.Errorf("neo4jstore: read schema version: %w", err)
	}
	return res.(int), nil
}

// Migrate applies every embedded migration whose version is greater
// than the current applied version. Idempotent: re-running on an up-
// to-date database is a no-op. Each migration file's statements are
// executed in order; the Migration node recording the version is
// written only after every statement in the file succeeds.
func (s *Storage) Migrate(ctx context.Context) error {
	migs, err := loadMigrations()
	if err != nil {
		return err
	}
	current, err := s.CurrentSchemaVersion(ctx)
	if err != nil {
		return err
	}
	for _, m := range migs {
		if m.Version <= current {
			continue
		}
		for _, stmt := range m.Statements {
			params := map[string]any{"dim": s.dim}
			if _, err := s.execAutoCommit(ctx, stmt, params); err != nil {
				return fmt.Errorf("neo4jstore: migration %s statement: %w\nstatement: %s", m.Name, err, stmt)
			}
		}
		if err := s.recordMigration(ctx, m); err != nil {
			return fmt.Errorf("neo4jstore: record migration %s: %w", m.Name, err)
		}
	}
	return nil
}

func (s *Storage) recordMigration(ctx context.Context, m migration) error {
	_, err := s.withWriteSession(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		_, err := tx.Run(ctx, `
			CREATE (m:Migration {
				version: $version,
				name: $name,
				applied_at: $applied_at
			})
		`, map[string]any{
			"version":    m.Version,
			"name":       m.Name,
			"applied_at": time.Now().UTC().Format(time.RFC3339Nano),
		})
		return nil, err
	})
	return err
}

// execAutoCommit runs a statement outside of a managed transaction.
// Schema operations (CREATE CONSTRAINT, CREATE INDEX) cannot share a
// transaction with data writes in Neo4j, so they go via auto-commit.
func (s *Storage) execAutoCommit(ctx context.Context, stmt string, params map[string]any) (neo4j.ResultWithContext, error) {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: s.database})
	defer sess.Close(ctx)
	return sess.Run(ctx, stmt, params)
}
