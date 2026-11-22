package activities

import (
	"context"
	"encoding/json"
	"testing"
)

var agentTestSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"topic": {"type": "string", "minLength": 1},
		"limit": {"type": "integer", "minimum": 1}
	},
	"required": ["topic", "limit"]
}`)

func TestExtractParamsValid(t *testing.T) {
	a := &Activities{}
	params := map[string]any{"topic": "AI", "limit": 5}
	out, err := a.ExtractParamsActivity(context.Background(), params, agentTestSchema)
	if err != nil {
		t.Fatalf("ExtractParamsActivity: %v", err)
	}
	if out["topic"] != "AI" {
		t.Fatalf("output = %v", out)
	}
}

func TestExtractParamsSchemaFail(t *testing.T) {
	a := &Activities{}
	if _, err := a.ExtractParamsActivity(context.Background(), map[string]any{"topic": "AI"}, agentTestSchema); err == nil {
		t.Fatal("expected schema validation error (missing limit)")
	}
	if _, err := a.ExtractParamsActivity(context.Background(), map[string]any{"topic": "", "limit": 5}, agentTestSchema); err == nil {
		t.Fatal("expected schema validation error (empty topic)")
	}
}

func TestExtractParamsNil(t *testing.T) {
	a := &Activities{}
	if _, err := a.ExtractParamsActivity(context.Background(), nil, agentTestSchema); err == nil {
		t.Fatal("expected nil-params error")
	}
}

func TestExtractParamsNoSchema(t *testing.T) {
	a := &Activities{}
	out, err := a.ExtractParamsActivity(context.Background(), map[string]any{"anything": 1}, nil)
	if err != nil {
		t.Fatalf("empty schema should skip validation: %v", err)
	}
	if out["anything"] != 1 {
		t.Fatalf("output = %v", out)
	}
}

func TestReportStatusNormal(t *testing.T) {
	a := &Activities{} // DB nil：不落库
	out, err := a.ReportStatusActivity(context.Background(), "progress", "half done")
	if err != nil {
		t.Fatalf("ReportStatusActivity: %v", err)
	}
	if out["status"] != "progress" {
		t.Fatalf("output = %v", out)
	}
}

func TestReportStatusFailing(t *testing.T) {
	a := &Activities{} // DB nil：otel.Record 被跳过，不 panic
	if _, err := a.ReportStatusActivity(context.Background(), "failed", "boom"); err != nil {
		t.Fatalf("ReportStatusActivity: %v", err)
	}
}

func TestReportStatusEmpty(t *testing.T) {
	a := &Activities{}
	if _, err := a.ReportStatusActivity(context.Background(), "", "x"); err == nil {
		t.Fatal("expected empty-status error")
	}
}
