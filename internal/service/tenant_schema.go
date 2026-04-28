package service

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/Cidan/memmy/internal/config"
)

// TenantSchema validates a tenant tuple against a configured shape and
// renders the equivalent JSON Schema for MCP InputSchema patching.
//
// The schema only validates incoming tuples — it does NOT transform
// them. TenantID is derived from the validated tuple as today
// (see DESIGN.md §3). Stored memories are not migrated when the schema
// changes; data written under one schema remains addressable if the
// schema is later changed back to one that accepts the original
// tuple shape.
type TenantSchema struct {
	description string
	keys        map[string]TenantKey
	oneOf       [][]string
}

// TenantKey is one declared tuple key.
type TenantKey struct {
	Description string
	Pattern     *regexp.Regexp // nil if no pattern
	PatternRaw  string
	Enum        []string
	Required    bool
}

// NewTenantSchemaFromConfig compiles a TenantSchema from config. Returns
// (nil, nil) when no schema is configured — callers treat nil as
// "accept any tuple."
func NewTenantSchemaFromConfig(cfg config.TenantSchemaConfig) (*TenantSchema, error) {
	if !cfg.IsConfigured() {
		return nil, nil
	}
	keys := make(map[string]TenantKey, len(cfg.Keys))
	for name, kc := range cfg.Keys {
		k := TenantKey{
			Description: kc.Description,
			Enum:        append([]string(nil), kc.Enum...),
			Required:    kc.Required,
			PatternRaw:  kc.Pattern,
		}
		if kc.Pattern != "" {
			re, err := regexp.Compile(kc.Pattern)
			if err != nil {
				return nil, fmt.Errorf("tenant.keys.%s.pattern: %w", name, err)
			}
			k.Pattern = re
		}
		keys[name] = k
	}
	oneOf := make([][]string, len(cfg.OneOf))
	for i, set := range cfg.OneOf {
		oneOf[i] = append([]string(nil), set...)
	}
	return &TenantSchema{
		description: cfg.Description,
		keys:        keys,
		oneOf:       oneOf,
	}, nil
}

// Description returns the top-level description string for use in the
// rendered JSON Schema and in MCP tool descriptions.
func (s *TenantSchema) Description() string {
	if s == nil {
		return ""
	}
	return s.description
}

// Validate enforces the schema against a tuple. Returns *ErrTenantInvalid
// when the tuple is rejected. nil-receiver = accept anything.
func (s *TenantSchema) Validate(tuple map[string]string) error {
	if s == nil {
		return nil
	}
	// 1. Reject unknown keys (additionalProperties: false).
	for name := range tuple {
		if _, ok := s.keys[name]; !ok {
			return &ErrTenantInvalid{
				Code:    "tenant_unknown_key",
				Field:   name,
				Message: fmt.Sprintf("unknown tenant key %q", name),
			}
		}
	}
	// 2. Per-key constraints (pattern, enum) for keys present in the tuple.
	for name, key := range s.keys {
		v, present := tuple[name]
		if !present {
			continue
		}
		if len(key.Enum) > 0 && !slices.Contains(key.Enum, v) {
			return &ErrTenantInvalid{
				Code:    "tenant_enum_mismatch",
				Field:   name,
				Got:     v,
				Message: fmt.Sprintf("tenant.%s = %q is not in enum %v", name, v, key.Enum),
			}
		}
		if key.Pattern != nil && !key.Pattern.MatchString(v) {
			return &ErrTenantInvalid{
				Code:    "tenant_pattern_mismatch",
				Field:   name,
				Got:     v,
				Message: fmt.Sprintf("tenant.%s = %q does not match pattern %q", name, v, key.PatternRaw),
			}
		}
	}
	// 3. Required keys must be present.
	for name, key := range s.keys {
		if !key.Required {
			continue
		}
		if _, ok := tuple[name]; !ok {
			return &ErrTenantInvalid{
				Code:    "tenant_missing_required",
				Field:   name,
				Message: fmt.Sprintf("tenant key %q is required", name),
			}
		}
	}
	// 4. one_of: exactly one set must have all its keys present.
	if len(s.oneOf) > 0 {
		matched := 0
		for _, set := range s.oneOf {
			present := true
			for _, k := range set {
				if _, ok := tuple[k]; !ok {
					present = false
					break
				}
			}
			if present {
				matched++
			}
		}
		if matched == 0 {
			return &ErrTenantInvalid{
				Code:    "tenant_one_of_no_match",
				Message: fmt.Sprintf("tenant must contain exactly one of these key sets: %v", s.oneOf),
			}
		}
		if matched > 1 {
			return &ErrTenantInvalid{
				Code:    "tenant_one_of_multiple_match",
				Message: fmt.Sprintf("tenant matched multiple of these key sets (must be exactly one): %v", s.oneOf),
			}
		}
	}
	return nil
}

// JSONSchema renders the schema as a *jsonschema.Schema suitable for
// patching into an MCP tool's InputSchema (the `tenant` property).
func (s *TenantSchema) JSONSchema() *jsonschema.Schema {
	if s == nil {
		return nil
	}
	props := make(map[string]*jsonschema.Schema, len(s.keys))
	keyNames := make([]string, 0, len(s.keys))
	for name := range s.keys {
		keyNames = append(keyNames, name)
	}
	sort.Strings(keyNames)
	var required []string
	for _, name := range keyNames {
		k := s.keys[name]
		ks := &jsonschema.Schema{
			Type:        "string",
			Description: k.Description,
		}
		if k.PatternRaw != "" {
			ks.Pattern = k.PatternRaw
		}
		if len(k.Enum) > 0 {
			enumAny := make([]any, len(k.Enum))
			for i, e := range k.Enum {
				enumAny[i] = e
			}
			ks.Enum = enumAny
		}
		props[name] = ks
		if k.Required {
			required = append(required, name)
		}
	}
	out := &jsonschema.Schema{
		Type:                 "object",
		Description:          s.description,
		Properties:           props,
		Required:             required,
		AdditionalProperties: falseSchema(),
	}
	if len(s.oneOf) > 0 {
		oneOf := make([]*jsonschema.Schema, len(s.oneOf))
		for i, set := range s.oneOf {
			req := append([]string(nil), set...)
			sort.Strings(req)
			oneOf[i] = &jsonschema.Schema{Required: req}
		}
		out.OneOf = oneOf
	}
	return out
}

// falseSchema returns a *jsonschema.Schema that disallows any
// additional properties. The library represents this as a schema
// with Not set to an empty schema (i.e., nothing matches).
func falseSchema() *jsonschema.Schema {
	// jsonschema.Schema's AdditionalProperties accepts a *Schema. The
	// idiomatic "false" is encoded by the library when serialized via
	// json.Marshal of a nil-Type Schema with no other constraints
	// being treated as "everything matches" — which is the OPPOSITE
	// of what we want. We therefore use Not: {} which matches nothing.
	return &jsonschema.Schema{Not: &jsonschema.Schema{}}
}

// ErrTenantInvalid is the typed error returned when a tuple is
// rejected. It carries enough structured detail that MCP handlers can
// surface a corrective payload back to the LLM.
type ErrTenantInvalid struct {
	Code    string // tenant_unknown_key | tenant_enum_mismatch | ...
	Field   string // empty when the error isn't field-specific
	Got     string // the offending value (when applicable)
	Message string
}

func (e *ErrTenantInvalid) Error() string { return e.Message }

// Payload returns a JSON-serializable detail object suitable for
// inclusion in an MCP CallToolResult error payload alongside the
// expected JSON Schema.
func (e *ErrTenantInvalid) Payload(expected *jsonschema.Schema) ([]byte, error) {
	out := struct {
		ErrorCode      string             `json:"error_code"`
		Field          string             `json:"field,omitempty"`
		Got            string             `json:"got,omitempty"`
		Message        string             `json:"message"`
		ExpectedSchema *jsonschema.Schema `json:"expected_schema,omitempty"`
	}{
		ErrorCode:      e.Code,
		Field:          e.Field,
		Got:            e.Got,
		Message:        e.Message,
		ExpectedSchema: expected,
	}
	return json.Marshal(out)
}
