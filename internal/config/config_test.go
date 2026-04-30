package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cidan/memmy/internal/config"
)

func TestConfig_LoadDefaultsAndOverlay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memmy.yaml")
	contents := `
server:
  transports:
    mcp:
      enabled: true
      addr: "127.0.0.1:9999"
storage:
  backend: neo4j
  neo4j:
    uri: bolt://localhost:7687
    user: neo4j
    password: secret
    database: neo4j
embedder:
  backend: fake
  fake:
    dim: 32
memory:
  chunk_window_size: 3
  chunk_stride: 2
  weight_cap: 100.0
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Transports["mcp"].Addr != "127.0.0.1:9999" {
		t.Fatalf("addr=%q", cfg.Server.Transports["mcp"].Addr)
	}
	if cfg.Storage.Neo4j.URI != "bolt://localhost:7687" {
		t.Fatalf("uri=%q", cfg.Storage.Neo4j.URI)
	}
	if cfg.Storage.Neo4j.Password != "secret" {
		t.Fatalf("password=%q", cfg.Storage.Neo4j.Password)
	}
	if cfg.Embedder.Backend != "fake" || cfg.EmbedderDim() != 32 {
		t.Fatalf("embedder %+v", cfg.Embedder)
	}
}

func TestConfig_PasswordEnvExpansion(t *testing.T) {
	t.Setenv("TEST_NEO4J_PW", "shh-it-is-a-secret")
	dir := t.TempDir()
	path := filepath.Join(dir, "memmy.yaml")
	contents := `
server:
  transports:
    mcp:
      enabled: true
      addr: "127.0.0.1:9999"
storage:
  backend: neo4j
  neo4j:
    uri: bolt://localhost:7687
    user: neo4j
    password: ${TEST_NEO4J_PW}
embedder:
  backend: fake
  fake:
    dim: 32
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Storage.Neo4j.Password != "shh-it-is-a-secret" {
		t.Errorf("env not expanded: %q", cfg.Storage.Neo4j.Password)
	}
}

func TestConfig_RejectsUnknownStorage(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Backend = "mongodb"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for unknown storage backend")
	}
}

func TestConfig_RejectsNoTransport(t *testing.T) {
	cfg := config.Default()
	cfg.Server.Transports = map[string]config.TransportConfig{
		"mcp": {Enabled: false, Addr: ""},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when no transport enabled")
	}
}

func TestConfig_RejectsZeroDim(t *testing.T) {
	cfg := config.Default()
	cfg.Embedder.Fake.Dim = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for fake.dim == 0")
	}
}

// stdio mutual-exclusivity tests (Round 3 / US-202).

func stdioOnlyConfig() config.Config {
	cfg := config.Default()
	cfg.Storage.Neo4j.Password = "test-password" // Default() leaves this empty
	cfg.Server.Transports = map[string]config.TransportConfig{
		config.TransportStdio: {Enabled: true}, // no Addr
	}
	return cfg
}

func TestConfig_StdioOnly_Accepted(t *testing.T) {
	cfg := stdioOnlyConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("stdio-only should validate: %v", err)
	}
}

func TestConfig_StdioPlusMCP_Rejected(t *testing.T) {
	cfg := stdioOnlyConfig()
	cfg.Server.Transports[config.TransportMCP] = config.TransportConfig{
		Enabled: true, Addr: "127.0.0.1:8765",
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("stdio + mcp should be rejected")
	}
	for _, want := range []string{"stdio", "mutually exclusive", "mcp"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message %q missing %q", err.Error(), want)
		}
	}
}

func TestConfig_StdioPlusGRPC_Rejected(t *testing.T) {
	cfg := stdioOnlyConfig()
	cfg.Server.Transports[config.TransportGRPC] = config.TransportConfig{
		Enabled: true, Addr: "127.0.0.1:8766",
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("stdio + grpc should be rejected")
	}
}

func TestConfig_StdioPlusHTTP_Rejected(t *testing.T) {
	cfg := stdioOnlyConfig()
	cfg.Server.Transports[config.TransportHTTP] = config.TransportConfig{
		Enabled: true, Addr: "127.0.0.1:8767",
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("stdio + http should be rejected")
	}
}

func TestConfig_StdioPlusDisabledOther_Accepted(t *testing.T) {
	cfg := stdioOnlyConfig()
	cfg.Server.Transports[config.TransportMCP] = config.TransportConfig{
		Enabled: false, Addr: "127.0.0.1:8765",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("stdio + disabled mcp should validate: %v", err)
	}
}

func TestConfig_StdioRequiresNoAddr(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Neo4j.Password = "test-password"
	cfg.Server.Transports = map[string]config.TransportConfig{
		config.TransportStdio: {Enabled: true, Addr: ""},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("stdio with empty Addr should be valid: %v", err)
	}
}

// Tenant-schema config tests (Round 4 / US-301).

func TestTenantSchemaConfig_EmptyAccepted(t *testing.T) {
	cfg := config.Default()
	if cfg.Tenant.IsConfigured() {
		t.Fatal("default config should have an unconfigured tenant schema")
	}
	// Default() ships with NO transports enabled and NO Neo4j password
	// — the operator must declare both explicitly. Set both here so
	// the rest of Validate() can run.
	cfg.Storage.Neo4j.Password = "test-password"
	cfg.Server.Transports = map[string]config.TransportConfig{
		config.TransportStdio: {Enabled: true},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default + stdio should validate: %v", err)
	}
}

func TestTenantSchemaConfig_ValidParse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memmy.yaml")
	contents := `
server:
  transports:
    stdio:
      enabled: true
storage:
  backend: neo4j
  neo4j:
    uri: bolt://localhost:7687
    user: neo4j
    password: secret
embedder:
  backend: fake
  fake:
    dim: 32
tenant:
  description: "Identity for this memory."
  keys:
    project:
      description: "Absolute path."
      pattern: "^/"
    scope:
      description: "global for cross-project"
      enum: ["global"]
  one_of:
    - [project]
    - [scope]
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Tenant.IsConfigured() {
		t.Fatal("expected tenant configured")
	}
	if cfg.Tenant.Keys["project"].Pattern != "^/" {
		t.Errorf("project.pattern=%q", cfg.Tenant.Keys["project"].Pattern)
	}
	if got := cfg.Tenant.Keys["scope"].Enum; len(got) != 1 || got[0] != "global" {
		t.Errorf("scope.enum=%v", got)
	}
	if len(cfg.Tenant.OneOf) != 2 {
		t.Errorf("one_of len=%d, want 2", len(cfg.Tenant.OneOf))
	}
}

func TestTenantSchemaConfig_RejectsInvalidPattern(t *testing.T) {
	cfg := config.Default()
	cfg.Tenant = config.TenantSchemaConfig{
		Keys: map[string]config.TenantKeyConfig{
			"project": {Pattern: "([invalid"},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for invalid regex pattern")
	}
}

func TestTenantSchemaConfig_RejectsUnknownOneOfKey(t *testing.T) {
	cfg := config.Default()
	cfg.Tenant = config.TenantSchemaConfig{
		Keys: map[string]config.TenantKeyConfig{
			"project": {},
		},
		OneOf: [][]string{{"project"}, {"missing"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for unknown one_of key")
	}
}

func TestTenantSchemaConfig_RejectsEmptyOneOfSet(t *testing.T) {
	cfg := config.Default()
	cfg.Tenant = config.TenantSchemaConfig{
		Keys: map[string]config.TenantKeyConfig{
			"project": {},
		},
		OneOf: [][]string{{}, {"project"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for empty one_of set")
	}
}

// TestConfig_NoTransportsByDefault asserts that Default() ships with
// zero transports enabled — operators must explicitly declare one,
// and a YAML config that omits server.transports fails validation
// rather than silently bringing up an unwanted listener.
func TestConfig_NoTransportsByDefault(t *testing.T) {
	cfg := config.Default()
	if len(cfg.Server.Transports) != 0 {
		t.Fatalf("Default() should have empty transports; got %v", cfg.Server.Transports)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Default() should fail Validate (no transport enabled)")
	}
}

// TestConfig_LoadOmittedServerSectionFails covers the original user
// concern: a YAML without server.transports should NOT silently bring
// up an mcp listener.
func TestConfig_LoadOmittedServerSectionFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memmy.yaml")
	contents := `
storage:
  backend: neo4j
  neo4j:
    uri: bolt://localhost:7687
    user: neo4j
    password: secret
embedder:
  backend: fake
  fake:
    dim: 32
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path); err == nil {
		t.Fatal("Load() should reject a config that omits server.transports")
	}
}

func TestConfig_HTTPRequiresAddr(t *testing.T) {
	cfg := config.Default()
	cfg.Server.Transports = map[string]config.TransportConfig{
		config.TransportMCP: {Enabled: true, Addr: ""},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("mcp transport with empty addr should be rejected")
	}
}

