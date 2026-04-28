package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Cidan/memmy/internal/clock"
	"github.com/Cidan/memmy/internal/config"
	"github.com/Cidan/memmy/internal/embed/fake"
	"github.com/Cidan/memmy/internal/service"
	bboltstore "github.com/Cidan/memmy/internal/storage/bbolt"
	mcpadapter "github.com/Cidan/memmy/internal/transport/mcp"
)

// connect builds a real bbolt-backed MemoryService, wraps it in an MCP
// adapter, and returns an in-process MCP client session.
func connect(t *testing.T) *mcpsdk.ClientSession {
	t.Helper()
	store, err := bboltstore.Open(bboltstore.Options{
		Path: filepath.Join(t.TempDir(), "memmy.db"),
		Dim:  32, RandSeed: 42,
		FlatScanThreshold: 100000,
	})
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	svc, err := service.New(
		store.Graph(), store.VectorIndex(),
		fake.New(32),
		clock.NewFake(time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)),
		service.DefaultConfig(),
		nil,
	)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	adapter := mcpadapter.New(svc, nil)

	t1, t2 := mcpsdk.NewInMemoryTransports()
	if _, err := adapter.Server().Connect(context.Background(), t1, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "v0.0.1"}, nil)
	cs, err := client.Connect(context.Background(), t2, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestMCP_ToolList(t *testing.T) {
	cs := connect(t)
	ctx := context.Background()

	got := map[string]bool{}
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		got[tool.Name] = true
	}
	for _, want := range []string{"memory.write", "memory.recall", "memory.forget", "memory.stats"} {
		if !got[want] {
			t.Errorf("missing tool: %s", want)
		}
	}
}

func TestMCP_WriteThenRecall(t *testing.T) {
	cs := connect(t)
	ctx := context.Background()

	// memory.write
	wargs := map[string]any{
		"tenant":  map[string]string{"agent": "ada"},
		"message": "the quick brown fox jumps. it is a sunny morning. the fox sees a rabbit.",
	}
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory.write",
		Arguments: wargs,
	})
	if err != nil {
		t.Fatalf("CallTool write: %v", err)
	}
	if res.IsError {
		t.Fatalf("write result is error: %+v", res)
	}
	var write struct {
		MessageID string   `json:"message_id"`
		NodeIDs   []string `json:"node_ids"`
	}
	if err := decodeStructured(res, &write); err != nil {
		t.Fatalf("decode write: %v", err)
	}
	if write.MessageID == "" || len(write.NodeIDs) == 0 {
		t.Fatalf("write: empty result %+v", write)
	}

	// memory.recall
	rres, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory.recall",
		Arguments: map[string]any{
			"tenant": map[string]string{"agent": "ada"},
			"query":  "the quick brown fox jumps. it is a sunny morning. the fox sees a rabbit.",
			"k":      3,
		},
	})
	if err != nil {
		t.Fatalf("CallTool recall: %v", err)
	}
	if rres.IsError {
		t.Fatalf("recall result is error: %+v", rres)
	}
	var recall struct {
		Results []struct {
			NodeID string  `json:"node_id"`
			Text   string  `json:"text"`
			Score  float64 `json:"score"`
		} `json:"results"`
	}
	if err := decodeStructured(rres, &recall); err != nil {
		t.Fatalf("decode recall: %v", err)
	}
	if len(recall.Results) == 0 {
		t.Fatalf("recall returned no hits: %s", textPayload(rres))
	}
}

func TestMCP_Stats(t *testing.T) {
	cs := connect(t)
	ctx := context.Background()
	if _, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory.write",
		Arguments: map[string]any{
			"tenant":  map[string]string{"agent": "ada"},
			"message": "first sentence. second sentence. third sentence.",
		},
	}); err != nil {
		t.Fatal(err)
	}
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory.stats",
		Arguments: map[string]any{"tenant": map[string]string{"agent": "ada"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var stats struct {
		NodeCount int `json:"node_count"`
		HNSWSize  int `json:"hnsw_size"`
	}
	if err := decodeStructured(res, &stats); err != nil {
		t.Fatal(err)
	}
	if stats.NodeCount == 0 || stats.HNSWSize == 0 {
		t.Fatalf("stats unexpectedly zero: %+v", stats)
	}
}

// TestMCP_RunTransport_StdioCodePath exercises the Server.Run code path
// (the same path RunStdio uses against StdioTransport) by wiring the
// adapter through an in-memory transport pair. This proves the tool
// surface is identical regardless of transport — the only thing that
// changes between HTTP and stdio is the bytes-on-the-wire layer.
// connectWithSchema is a variant of connect() that wires a TenantSchema
// into both the service and the adapter so end-to-end tests can verify
// schema rendering and corrective error round-tripping.
func connectWithSchema(t *testing.T, schema *service.TenantSchema) *mcpsdk.ClientSession {
	t.Helper()
	store, err := bboltstore.Open(bboltstore.Options{
		Path: filepath.Join(t.TempDir(), "memmy.db"),
		Dim:  32, RandSeed: 42,
		FlatScanThreshold: 100000,
	})
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	svc, err := service.New(
		store.Graph(), store.VectorIndex(),
		fake.New(32),
		clock.NewFake(time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)),
		service.DefaultConfig(),
		schema,
	)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	adapter := mcpadapter.New(svc, schema)

	t1, t2 := mcpsdk.NewInMemoryTransports()
	if _, err := adapter.Server().Connect(context.Background(), t1, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "v0.0.1"}, nil)
	cs, err := client.Connect(context.Background(), t2, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func projectScopeSchemaForMCP(t *testing.T) *service.TenantSchema {
	t.Helper()
	cfg := config.TenantSchemaConfig{
		Description: "Identity for this memory. project (absolute path) for project memory; scope=global for cross-project.",
		Keys: map[string]config.TenantKeyConfig{
			"project": {Description: "Absolute path of the working directory.", Pattern: "^/"},
			"scope":   {Description: "Memory scope.", Enum: []string{"global"}},
		},
		OneOf: [][]string{{"project"}, {"scope"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	s, err := service.NewTenantSchemaFromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMCP_TenantSchema_ValidProjectAccepted(t *testing.T) {
	cs := connectWithSchema(t, projectScopeSchemaForMCP(t))
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "memory.write",
		Arguments: map[string]any{
			"tenant":  map[string]string{"project": "/home/me/repo"},
			"message": "first sentence. second sentence. third sentence.",
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success; got error result %+v", res)
	}
	var write struct {
		MessageID string `json:"message_id"`
	}
	if err := decodeStructured(res, &write); err != nil {
		t.Fatal(err)
	}
	if write.MessageID == "" {
		t.Fatal("expected message_id")
	}
}

func TestMCP_TenantSchema_ValidScopeAccepted(t *testing.T) {
	cs := connectWithSchema(t, projectScopeSchemaForMCP(t))
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "memory.write",
		Arguments: map[string]any{
			"tenant":  map[string]string{"scope": "global"},
			"message": "global note.",
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success; got %+v", res)
	}
}

func TestMCP_TenantSchema_InvalidSurfacesError(t *testing.T) {
	cs := connectWithSchema(t, projectScopeSchemaForMCP(t))
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "memory.write",
		Arguments: map[string]any{
			"tenant":  map[string]string{"project": "relative/path"},
			"message": "hello.",
		},
	})
	// With the schema attached to the tool's InputSchema, the SDK
	// pre-validates and surfaces the failure either as a JSON-RPC
	// error (err != nil) or as an IsError result with the SDK's
	// validation diagnostic in TextContent. Both paths give the LLM
	// enough information to retry with a valid tenant. We accept
	// either surface here.
	if err != nil {
		// SDK pre-validation rejected the call as a JSON-RPC error;
		// the LLM would see the message in the JSON-RPC response.
		if !strings.Contains(err.Error(), "tenant") && !strings.Contains(err.Error(), "pattern") {
			t.Fatalf("err didn't mention the tenant/pattern constraint: %v", err)
		}
		return
	}
	if !res.IsError {
		t.Fatalf("expected error result; got %+v", res)
	}
	text := textPayload(res)
	if text == "" {
		t.Fatal("missing error payload content")
	}
	// Either: (a) SDK validation message, mentions "tenant"/"pattern";
	//         (b) our service-layer JSON payload with error_code etc.
	var jsonPayload struct {
		ErrorCode      string          `json:"error_code"`
		Field          string          `json:"field"`
		Got            string          `json:"got"`
		Message        string          `json:"message"`
		ExpectedSchema json.RawMessage `json:"expected_schema"`
	}
	if err := json.Unmarshal([]byte(text), &jsonPayload); err == nil && jsonPayload.ErrorCode != "" {
		// Handler-validation path — verify the structured payload.
		if len(jsonPayload.ExpectedSchema) == 0 {
			t.Error("expected_schema missing in handler payload")
		}
		return
	}
	// SDK-validation path — message should reference the tenant property.
	if !strings.Contains(text, "tenant") && !strings.Contains(text, "pattern") {
		t.Fatalf("error text didn't reference the tenant/pattern constraint: %s", text)
	}
}

// TestMCP_TenantSchema_HandlerCatchesUnknownKey covers the
// service-layer-validation path explicitly. The SDK is configured
// with our rendered schema (which has additionalProperties: false),
// but we send the call through the handler directly via a service-
// level test elsewhere — at the MCP boundary, an unknown key passes
// through SDK validation only if the SDK's encoding of "no
// additional properties" is loose. We assert SOME error surfaces.
func TestMCP_TenantSchema_UnknownKeyRejected(t *testing.T) {
	cs := connectWithSchema(t, projectScopeSchemaForMCP(t))
	_, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "memory.write",
		Arguments: map[string]any{
			"tenant":  map[string]string{"project": "/x", "agent": "claude"},
			"message": "hello.",
		},
	})
	// Either err is non-nil (SDK rejected) or the result was an error
	// content; we don't care which surface, only that the call did
	// NOT silently succeed with an unknown tenant key.
	if err == nil {
		// Re-issue and inspect the result.
		res, err2 := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
			Name: "memory.write",
			Arguments: map[string]any{
				"tenant":  map[string]string{"project": "/x", "agent": "claude"},
				"message": "hello.",
			},
		})
		if err2 != nil {
			return
		}
		if !res.IsError {
			t.Fatalf("unknown tenant key was silently accepted: %+v", res)
		}
	}
}

func TestMCP_TenantSchema_DescriptionRendersIntoToolListing(t *testing.T) {
	cs := connectWithSchema(t, projectScopeSchemaForMCP(t))
	ctx := context.Background()
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		if tool.Name != "memory.write" {
			continue
		}
		// InputSchema is *jsonschema.Schema rendered to JSON; just
		// inspect that the tenant property carries our description.
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "Identity for this memory.") {
			t.Fatalf("schema description not visible in InputSchema: %s", raw)
		}
		if !strings.Contains(string(raw), `"oneOf"`) {
			t.Fatalf("oneOf constraint not visible in InputSchema: %s", raw)
		}
		return
	}
	t.Fatal("memory.write tool not listed")
}

func TestMCP_RunTransport_StdioCodePath(t *testing.T) {
	store, err := bboltstore.Open(bboltstore.Options{
		Path: filepath.Join(t.TempDir(), "memmy.db"),
		Dim:  32, RandSeed: 42,
		FlatScanThreshold: 100000,
	})
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	svc, err := service.New(
		store.Graph(), store.VectorIndex(),
		fake.New(32),
		clock.NewFake(time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)),
		service.DefaultConfig(),
		nil,
	)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	adapter := mcpadapter.New(svc, nil)

	serverT, clientT := mcpsdk.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	serverDone := make(chan error, 1)
	go func() { serverDone <- adapter.RunTransport(ctx, serverT) }()

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "v0.0.1"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	// Round-trip a write through the same RunTransport path stdio uses.
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory.write",
		Arguments: map[string]any{
			"tenant":  map[string]string{"agent": "ada"},
			"message": "stdio path test. second sentence.",
		},
	})
	if err != nil {
		t.Fatalf("CallTool write: %v", err)
	}
	if res.IsError {
		t.Fatalf("write result is error: %+v", res)
	}

	// Cancel and confirm RunTransport unwinds cleanly.
	_ = cs.Close()
	cancel()
	select {
	case err := <-serverDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("RunTransport returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunTransport did not return within 2s of context cancellation")
	}
}

func TestMCP_ForgetByMessageID(t *testing.T) {
	cs := connect(t)
	ctx := context.Background()
	wres, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory.write",
		Arguments: map[string]any{
			"tenant":  map[string]string{"agent": "ada"},
			"message": "alpha sentence. beta sentence. gamma sentence.",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var write struct {
		MessageID string `json:"message_id"`
	}
	if err := decodeStructured(wres, &write); err != nil {
		t.Fatal(err)
	}

	fres, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory.forget",
		Arguments: map[string]any{
			"tenant":     map[string]string{"agent": "ada"},
			"message_id": write.MessageID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var fout struct {
		DeletedNodes   int `json:"deleted_nodes"`
		DeletedVectors int `json:"deleted_vectors"`
	}
	if err := decodeStructured(fres, &fout); err != nil {
		t.Fatal(err)
	}
	if fout.DeletedNodes == 0 || fout.DeletedVectors == 0 {
		t.Fatalf("forget reported zero deletions: %+v", fout)
	}
}

// decodeStructured pulls structuredContent off the result, falling back
// to the textual content rendering. The SDK delivers typed Out values as
// structuredContent; the SDK doesn't surface it on the result struct
// directly via the public types, so we read the JSON from the text
// content rendered by our adapter.
func decodeStructured(res *mcpsdk.CallToolResult, out any) error {
	if res.StructuredContent != nil {
		raw, err := json.Marshal(res.StructuredContent)
		if err == nil {
			if err := json.Unmarshal(raw, out); err == nil {
				return nil
			}
		}
	}
	t := textPayload(res)
	if t == "" {
		return nil
	}
	return json.Unmarshal([]byte(t), out)
}

func textPayload(res *mcpsdk.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}
