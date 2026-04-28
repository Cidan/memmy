// Package mcp adapts the MemoryService onto an MCP server using the
// official github.com/modelcontextprotocol/go-sdk. Adapters NEVER touch
// Embedder/VectorIndex/Graph directly — they only call MemoryService
// (DESIGN.md §0 #4 / §10.5).
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Cidan/memmy/internal/service"
	"github.com/Cidan/memmy/internal/types"
)

// ServerVersion is the version string memmy advertises.
const (
	ServerName    = "memmy"
	ServerVersion = "v0.1.0"
)

// Adapter wraps a MemoryService and exposes it as an MCP Server with the
// four memory.* tools.
type Adapter struct {
	svc          service.MemoryService
	srv          *mcpsdk.Server
	tenantSchema *service.TenantSchema // nil when no schema is configured
}

// New constructs a new Adapter. Tool registration happens here so
// subsequent calls to Server() return a fully-wired MCP server.
//
// When tenantSchema is non-nil, every tool's InputSchema has its
// `tenant` property replaced with the schema-rendered JSON Schema —
// the LLM sees the constraints directly in the tool listing, and the
// SDK validates incoming arguments against them. Handlers also
// surface ErrTenantInvalid as a structured CallToolResult error so
// the LLM has actionable retry information.
func New(svc service.MemoryService, tenantSchema *service.TenantSchema) *Adapter {
	a := &Adapter{
		svc: svc,
		srv: mcpsdk.NewServer(&mcpsdk.Implementation{
			Name:    ServerName,
			Version: ServerVersion,
		}, nil),
		tenantSchema: tenantSchema,
	}
	a.registerTools()
	return a
}

// Server returns the underlying MCP server (used by transport listeners).
func (a *Adapter) Server() *mcpsdk.Server { return a.srv }

// HTTPHandler returns an http.Handler that implements the streamable
// MCP transport for this adapter's server.
func (a *Adapter) HTTPHandler() http.Handler {
	return mcpsdk.NewStreamableHTTPHandler(func(r *http.Request) *mcpsdk.Server {
		return a.srv
	}, nil)
}

// RunStdio runs the adapter's MCP server against the SDK's stdio
// transport (process stdin/stdout). Blocks until ctx is cancelled or
// the underlying transport returns. Used when memmy is launched as a
// child process by an MCP-aware tool.
//
// Logs MUST be routed to stderr by the caller; stdio mode keeps stdout
// clean for JSON-RPC framing.
func (a *Adapter) RunStdio(ctx context.Context) error {
	return a.srv.Run(ctx, &mcpsdk.StdioTransport{})
}

// RunTransport runs the adapter's MCP server against an arbitrary
// transport. Used by tests to wire the same tool surface through an
// in-memory transport pair.
func (a *Adapter) RunTransport(ctx context.Context, t mcpsdk.Transport) error {
	return a.srv.Run(ctx, t)
}

// ListenAndServe runs an HTTP server bound to addr. Blocks until the
// underlying server returns. Use Shutdown to stop gracefully.
func (a *Adapter) ListenAndServe(addr string) error {
	server := &http.Server{Addr: addr, Handler: a.HTTPHandler(), ReadHeaderTimeout: 10 * time.Second}
	return server.ListenAndServe()
}

// ----- tool args / results (JSON-shaped) -----

// writeArgs is the JSON request shape for memory.write.
type writeArgs struct {
	Tenant   map[string]string `json:"tenant"`
	Message  string            `json:"message"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// writeResult is the JSON response shape for memory.write.
type writeResult struct {
	MessageID string   `json:"message_id"`
	NodeIDs   []string `json:"node_ids"`
}

// recallArgs is the JSON request shape for memory.recall.
type recallArgs struct {
	Tenant     map[string]string `json:"tenant"`
	Query      string            `json:"query"`
	K          int               `json:"k,omitempty"`
	Hops       int               `json:"hops,omitempty"`
	Oversample int               `json:"oversample,omitempty"`
}

// recallHit mirrors types.RecallHit on the wire.
type recallHit struct {
	NodeID         string                  `json:"node_id"`
	Text           string                  `json:"text"`
	SourceMsgID    string                  `json:"source_msg_id"`
	SourceText     string                  `json:"source_text"`
	Score          float64                 `json:"score"`
	ScoreBreakdown recallScoreBreakdown    `json:"score_breakdown"`
	Path           []string                `json:"path"`
}

type recallScoreBreakdown struct {
	Sim        float64 `json:"sim"`
	NodeWeight float64 `json:"node_weight"`
	GraphMult  float64 `json:"graph_mult"`
	Depth      int     `json:"depth"`
}

type recallResult struct {
	Results []recallHit `json:"results"`
}

// forgetArgs is the JSON request shape for memory.forget.
type forgetArgs struct {
	Tenant    map[string]string `json:"tenant"`
	MessageID string            `json:"message_id,omitempty"`
	Before    string            `json:"before,omitempty"` // RFC3339; optional
}

type forgetResult struct {
	DeletedNodes   int `json:"deleted_nodes"`
	DeletedEdges   int `json:"deleted_edges"`
	DeletedVectors int `json:"deleted_vectors"`
}

type statsArgs struct {
	Tenant map[string]string `json:"tenant,omitempty"`
}

type statsResult struct {
	NodeCount       int     `json:"node_count"`
	MemoryEdgeCount int     `json:"memory_edge_count"`
	HNSWSize        int     `json:"hnsw_size"`
	AvgNodeWeight   float64 `json:"avg_node_weight"`
	AvgEdgeWeight   float64 `json:"avg_edge_weight"`
}

// ----- registration -----

func (a *Adapter) registerTools() {
	registerToolWithTenantSchema(a.srv, a.tenantSchema, &mcpsdk.Tool{
		Name:        "memory.write",
		Description: "Ingest a message into memory. Splits into chunks, embeds, persists nodes/vectors/HNSW links and structural memory edges.",
	}, writeArgs{}, a.handleWrite)

	registerToolWithTenantSchema(a.srv, a.tenantSchema, &mcpsdk.Tool{
		Name:        "memory.recall",
		Description: "Retrieve memories by semantic similarity with weight-aware re-ranking and graph expansion. Returns ranked results with provenance.",
	}, recallArgs{}, a.handleRecall)

	registerToolWithTenantSchema(a.srv, a.tenantSchema, &mcpsdk.Tool{
		Name:        "memory.forget",
		Description: "Hard-delete a message and all derived nodes/vectors/edges, or all messages older than a timestamp.",
	}, forgetArgs{}, a.handleForget)

	registerToolWithTenantSchema(a.srv, a.tenantSchema, &mcpsdk.Tool{
		Name:        "memory.stats",
		Description: "Return per-tenant or aggregate memory counts and weight statistics.",
	}, statsArgs{}, a.handleStats)
}

// registerToolWithTenantSchema is a small wrapper around mcp.AddTool
// that auto-derives the InputSchema from the typed argument value and,
// if a TenantSchema is configured, replaces the `tenant` property's
// schema with the rendered one.
//
// We can't use generics here trivially because Go's type inference
// won't carry the In type through both jsonschema.For[In] and the
// handler signature without an explicit type parameter; the helper
// is generic across both.
func registerToolWithTenantSchema[In, Out any](
	srv *mcpsdk.Server,
	schema *service.TenantSchema,
	tool *mcpsdk.Tool,
	_ In,
	handler func(context.Context, *mcpsdk.CallToolRequest, In) (*mcpsdk.CallToolResult, Out, error),
) {
	in, err := jsonschema.For[In](nil)
	if err != nil {
		// Fall back to SDK auto-derivation if reflection fails — this
		// is an internal programming error rather than a runtime one.
		mcpsdk.AddTool(srv, tool, handler)
		return
	}
	if schema != nil && in.Properties != nil {
		if _, ok := in.Properties["tenant"]; ok {
			in.Properties["tenant"] = schema.JSONSchema()
		}
	}
	tool.InputSchema = in
	mcpsdk.AddTool(srv, tool, handler)
}

// tenantErrorResult inspects err and, if it's an *ErrTenantInvalid,
// returns a CallToolResult{IsError: true} with a structured corrective
// payload (error_code, field, got, message, expected_schema) so the
// LLM can self-correct on retry. Returns nil for non-tenant errors.
func (a *Adapter) tenantErrorResult(err error) *mcpsdk.CallToolResult {
	var te *service.ErrTenantInvalid
	if !errors.As(err, &te) {
		return nil
	}
	var expected *jsonschema.Schema
	if a.tenantSchema != nil {
		expected = a.tenantSchema.JSONSchema()
	}
	payload, perr := te.Payload(expected)
	if perr != nil {
		// Fall back to a minimal envelope if marshalling the schema
		// somehow fails. Same shape, just without expected_schema.
		fallback := struct {
			ErrorCode string `json:"error_code"`
			Message   string `json:"message"`
		}{ErrorCode: "tenant_invalid", Message: te.Message}
		payload, _ = json.Marshal(fallback)
	}
	return &mcpsdk.CallToolResult{
		IsError: true,
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: string(payload)}},
	}
}

// ----- handlers -----

func (a *Adapter) handleWrite(ctx context.Context, _ *mcpsdk.CallToolRequest, args writeArgs) (*mcpsdk.CallToolResult, writeResult, error) {
	if args.Message == "" {
		return nil, writeResult{}, fmt.Errorf("memory.write: message required")
	}
	out, err := a.svc.Write(ctx, types.WriteRequest{
		Tenant:   args.Tenant,
		Message:  args.Message,
		Metadata: args.Metadata,
	})
	if err != nil {
		if r := a.tenantErrorResult(err); r != nil {
			return r, writeResult{}, nil
		}
		return nil, writeResult{}, err
	}
	return summary(out), writeResult{MessageID: out.MessageID, NodeIDs: out.NodeIDs}, nil
}

func (a *Adapter) handleRecall(ctx context.Context, _ *mcpsdk.CallToolRequest, args recallArgs) (*mcpsdk.CallToolResult, recallResult, error) {
	if args.Query == "" {
		return nil, recallResult{}, fmt.Errorf("memory.recall: query required")
	}
	out, err := a.svc.Recall(ctx, types.RecallRequest{
		Tenant:      args.Tenant,
		Query:       args.Query,
		K:           args.K,
		Hops:        args.Hops,
		OversampleN: args.Oversample,
	})
	if err != nil {
		if r := a.tenantErrorResult(err); r != nil {
			return r, recallResult{}, nil
		}
		return nil, recallResult{}, err
	}
	hits := make([]recallHit, len(out.Results))
	for i, r := range out.Results {
		hits[i] = recallHit{
			NodeID:      r.NodeID,
			Text:        r.Text,
			SourceMsgID: r.SourceMsgID,
			SourceText:  r.SourceText,
			Score:       r.Score,
			ScoreBreakdown: recallScoreBreakdown{
				Sim:        r.ScoreBreakdown.Sim,
				NodeWeight: r.ScoreBreakdown.NodeWeight,
				GraphMult:  r.ScoreBreakdown.GraphMult,
				Depth:      r.ScoreBreakdown.Depth,
			},
			Path: r.Path,
		}
	}
	return summary(out), recallResult{Results: hits}, nil
}

func (a *Adapter) handleForget(ctx context.Context, _ *mcpsdk.CallToolRequest, args forgetArgs) (*mcpsdk.CallToolResult, forgetResult, error) {
	req := types.ForgetRequest{Tenant: args.Tenant, MessageID: args.MessageID}
	if args.Before != "" {
		t, err := time.Parse(time.RFC3339, args.Before)
		if err != nil {
			return nil, forgetResult{}, fmt.Errorf("memory.forget: invalid before timestamp: %w", err)
		}
		req.Before = t
	}
	out, err := a.svc.Forget(ctx, req)
	if err != nil {
		if r := a.tenantErrorResult(err); r != nil {
			return r, forgetResult{}, nil
		}
		return nil, forgetResult{}, err
	}
	return summary(out), forgetResult{
		DeletedNodes:   out.DeletedNodes,
		DeletedEdges:   out.DeletedEdges,
		DeletedVectors: out.DeletedVectors,
	}, nil
}

func (a *Adapter) handleStats(ctx context.Context, _ *mcpsdk.CallToolRequest, args statsArgs) (*mcpsdk.CallToolResult, statsResult, error) {
	out, err := a.svc.Stats(ctx, types.StatsRequest{Tenant: args.Tenant})
	if err != nil {
		if r := a.tenantErrorResult(err); r != nil {
			return r, statsResult{}, nil
		}
		return nil, statsResult{}, err
	}
	return summary(out), statsResult{
		NodeCount:       out.NodeCount,
		MemoryEdgeCount: out.MemoryEdgeCount,
		HNSWSize:        out.HNSWSize,
		AvgNodeWeight:   out.AvgNodeWeight,
		AvgEdgeWeight:   out.AvgEdgeWeight,
	}, nil
}

// summary builds a CallToolResult containing a one-line text rendering of
// the structured output. The structured payload is the typed Out value
// returned alongside; the SDK will surface it as `structuredContent`.
func summary(v any) *mcpsdk.CallToolResult {
	b, _ := json.Marshal(v)
	return &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: string(b)}},
	}
}
