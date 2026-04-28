package service_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/Cidan/memmy/internal/config"
	"github.com/Cidan/memmy/internal/service"
)

func projectScopeSchema(t *testing.T) *service.TenantSchema {
	t.Helper()
	cfg := config.TenantSchemaConfig{
		Description: "Identity for this memory.",
		Keys: map[string]config.TenantKeyConfig{
			"project": {Description: "Absolute path.", Pattern: "^/"},
			"scope":   {Description: "'global' for cross-project.", Enum: []string{"global"}},
		},
		OneOf: [][]string{
			{"project"},
			{"scope"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config.Validate: %v", err)
	}
	s, err := service.NewTenantSchemaFromConfig(cfg)
	if err != nil {
		t.Fatalf("NewTenantSchemaFromConfig: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil schema")
	}
	return s
}

func TestTenantSchema_NilSchemaAcceptsAnything(t *testing.T) {
	var s *service.TenantSchema
	for _, tuple := range []map[string]string{
		{"agent": "claude-code"},
		{"foo": "bar", "baz": "qux"},
		{},
	} {
		if err := s.Validate(tuple); err != nil {
			t.Fatalf("nil schema rejected tuple %v: %v", tuple, err)
		}
	}
}

func TestTenantSchema_UnconfiguredReturnsNil(t *testing.T) {
	s, err := service.NewTenantSchemaFromConfig(config.TenantSchemaConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if s != nil {
		t.Fatal("expected nil schema for empty config")
	}
}

func TestTenantSchema_AcceptsValidProjectTuple(t *testing.T) {
	s := projectScopeSchema(t)
	if err := s.Validate(map[string]string{"project": "/home/me/repo"}); err != nil {
		t.Fatalf("expected accept: %v", err)
	}
}

func TestTenantSchema_AcceptsValidScopeTuple(t *testing.T) {
	s := projectScopeSchema(t)
	if err := s.Validate(map[string]string{"scope": "global"}); err != nil {
		t.Fatalf("expected accept: %v", err)
	}
}

func TestTenantSchema_RejectsUnknownKey(t *testing.T) {
	s := projectScopeSchema(t)
	err := s.Validate(map[string]string{"project": "/x", "agent": "claude"})
	var te *service.ErrTenantInvalid
	if !errors.As(err, &te) {
		t.Fatalf("err = %v, want ErrTenantInvalid", err)
	}
	if te.Code != "tenant_unknown_key" || te.Field != "agent" {
		t.Fatalf("got %+v", te)
	}
}

func TestTenantSchema_RejectsPatternMismatch(t *testing.T) {
	s := projectScopeSchema(t)
	err := s.Validate(map[string]string{"project": "relative/path"})
	var te *service.ErrTenantInvalid
	if !errors.As(err, &te) {
		t.Fatalf("err = %v, want ErrTenantInvalid", err)
	}
	if te.Code != "tenant_pattern_mismatch" || te.Field != "project" {
		t.Fatalf("got %+v", te)
	}
}

func TestTenantSchema_RejectsEnumMismatch(t *testing.T) {
	s := projectScopeSchema(t)
	err := s.Validate(map[string]string{"scope": "personal"})
	var te *service.ErrTenantInvalid
	if !errors.As(err, &te) {
		t.Fatalf("err = %v, want ErrTenantInvalid", err)
	}
	if te.Code != "tenant_enum_mismatch" || te.Field != "scope" {
		t.Fatalf("got %+v", te)
	}
}

func TestTenantSchema_RejectsNoOneOfMatch(t *testing.T) {
	s := projectScopeSchema(t)
	err := s.Validate(map[string]string{}) // empty tuple — no one_of set is satisfied
	var te *service.ErrTenantInvalid
	if !errors.As(err, &te) {
		t.Fatalf("err = %v, want ErrTenantInvalid", err)
	}
	if te.Code != "tenant_one_of_no_match" {
		t.Fatalf("got %+v", te)
	}
}

func TestTenantSchema_RejectsMultipleOneOfMatches(t *testing.T) {
	s := projectScopeSchema(t)
	err := s.Validate(map[string]string{"project": "/x", "scope": "global"})
	var te *service.ErrTenantInvalid
	if !errors.As(err, &te) {
		t.Fatalf("err = %v, want ErrTenantInvalid", err)
	}
	if te.Code != "tenant_one_of_multiple_match" {
		t.Fatalf("got %+v", te)
	}
}

func TestTenantSchema_RejectsMissingRequired(t *testing.T) {
	cfg := config.TenantSchemaConfig{
		Keys: map[string]config.TenantKeyConfig{
			"region":  {Required: true, Enum: []string{"us", "eu"}},
			"project": {},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	s, _ := service.NewTenantSchemaFromConfig(cfg)
	err := s.Validate(map[string]string{"project": "/x"})
	var te *service.ErrTenantInvalid
	if !errors.As(err, &te) {
		t.Fatalf("err = %v, want ErrTenantInvalid", err)
	}
	if te.Code != "tenant_missing_required" || te.Field != "region" {
		t.Fatalf("got %+v", te)
	}
}

func TestTenantSchema_JSONSchemaShape(t *testing.T) {
	s := projectScopeSchema(t)
	js := s.JSONSchema()
	if js == nil {
		t.Fatal("nil JSON schema")
	}
	if js.Type != "object" {
		t.Errorf("type=%q, want object", js.Type)
	}
	if js.Properties == nil || js.Properties["project"] == nil || js.Properties["scope"] == nil {
		t.Fatalf("properties missing: %+v", js.Properties)
	}
	if js.Properties["project"].Pattern != "^/" {
		t.Errorf("project pattern=%q", js.Properties["project"].Pattern)
	}
	if got := js.Properties["scope"].Enum; len(got) != 1 || got[0] != "global" {
		t.Errorf("scope enum=%v", got)
	}
	if len(js.OneOf) != 2 {
		t.Fatalf("oneOf len=%d, want 2", len(js.OneOf))
	}
	if js.AdditionalProperties == nil {
		t.Errorf("expected additionalProperties=false (encoded as Not: {})")
	}
	// Round-trip via JSON to make sure marshalling produces valid output.
	if _, err := json.Marshal(js); err != nil {
		t.Fatalf("marshal: %v", err)
	}
}

func TestTenantSchema_PayloadIncludesExpectedSchema(t *testing.T) {
	s := projectScopeSchema(t)
	err := s.Validate(map[string]string{"project": "relative"})
	var te *service.ErrTenantInvalid
	if !errors.As(err, &te) {
		t.Fatal("expected ErrTenantInvalid")
	}
	payload, perr := te.Payload(s.JSONSchema())
	if perr != nil {
		t.Fatal(perr)
	}
	var got struct {
		ErrorCode      string          `json:"error_code"`
		Field          string          `json:"field"`
		Got            string          `json:"got"`
		Message        string          `json:"message"`
		ExpectedSchema json.RawMessage `json:"expected_schema"`
	}
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatal(err)
	}
	if got.ErrorCode != "tenant_pattern_mismatch" {
		t.Errorf("error_code=%q", got.ErrorCode)
	}
	if got.Field != "project" {
		t.Errorf("field=%q", got.Field)
	}
	if got.Got != "relative" {
		t.Errorf("got=%q", got.Got)
	}
	if len(got.ExpectedSchema) == 0 {
		t.Error("expected_schema missing")
	}
}
