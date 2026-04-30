package neo4jstore_test

import (
	"context"
	"testing"

	neo4jstore "github.com/Cidan/memmy/internal/storage/neo4j"
	"github.com/Cidan/memmy/internal/storage/neo4j/neo4jtest"
)

func TestRequiredSchemaVersion(t *testing.T) {
	v, err := neo4jstore.RequiredSchemaVersion()
	if err != nil {
		t.Fatalf("RequiredSchemaVersion: %v", err)
	}
	if v < 1 {
		t.Errorf("RequiredSchemaVersion=%d, want >= 1", v)
	}
}

func TestMigrate_AppliedVersionMatchesRequired(t *testing.T) {
	storage, _, _ := neo4jtest.Open(t, 32)
	ctx := context.Background()

	want, err := neo4jstore.RequiredSchemaVersion()
	if err != nil {
		t.Fatalf("RequiredSchemaVersion: %v", err)
	}

	got, err := storage.CurrentSchemaVersion(ctx)
	if err != nil {
		t.Fatalf("CurrentSchemaVersion: %v", err)
	}
	if got != want {
		t.Errorf("CurrentSchemaVersion=%d, want %d (helper Migrate must bring db to required)", got, want)
	}

	// Idempotent: re-applying must not break or duplicate.
	if err := storage.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate: %v", err)
	}
	got2, err := storage.CurrentSchemaVersion(ctx)
	if err != nil {
		t.Fatalf("CurrentSchemaVersion 2: %v", err)
	}
	if got2 != want {
		t.Errorf("CurrentSchemaVersion after re-Migrate=%d, want %d", got2, want)
	}
}

func TestOpen_RejectsZeroDim(t *testing.T) {
	conn := neo4jtest.SkipIfUnset(t)
	_, err := neo4jstore.Open(context.Background(), neo4jstore.Options{
		URI:      conn.URI,
		Username: conn.User,
		Password: conn.Password,
		Database: conn.Database,
		Dim:      0,
	})
	if err == nil {
		t.Fatal("expected error for Dim=0")
	}
}
