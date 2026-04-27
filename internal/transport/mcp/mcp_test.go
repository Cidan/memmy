package mcp_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Cidan/memmy/internal/clock"
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
	)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	adapter := mcpadapter.New(svc)

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
