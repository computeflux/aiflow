// Package evaluate implements the CRAG relevance evaluation skill:
// an LLM judges whether retrieved documents can answer the query.
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/wetee/ai-workflow/internal/llm"
	"github.com/wetee/ai-workflow/internal/qdrant"
)

// EvaluateSkillEvaluation represents a relevance score for a retrieved document.
type EvaluateSkillEvaluation struct {
	Score     float64 `json:"score"` // 0-1
	Reasoning string  `json:"reasoning"`
}

// EvaluateSkillInput is the payload for the CRAG evaluation skill.
type EvaluateSkillInput struct {
	Query string                `json:"query"`
	Docs  []qdrant.SearchResult `json:"docs"`
}

// EvaluateSkillResult returns per-document scores and an overall sufficiency flag.
type EvaluateSkillResult struct {
	Evaluations []EvaluateSkillEvaluation `json:"evaluations"`
	Sufficient  bool                      `json:"sufficient"`
}

// EvaluateRun uses an LLM to judge whether retrieved documents can answer the query.
func EvaluateRun(ctx context.Context, client *llm.Client, input EvaluateSkillInput) (EvaluateSkillResult, error) {
	if len(input.Docs) == 0 {
		return EvaluateSkillResult{Sufficient: false}, nil
	}

	var sb strings.Builder
	for i, d := range input.Docs {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, d.Chunk.Content))
	}
	system := "You evaluate document relevance. Return a JSON object with fields: score (0-1 float), reasoning (short string), sufficient (boolean)."
	user := fmt.Sprintf(`Query: %s

Documents:
%s

Is this information sufficient to answer the query?`, input.Query, sb.String())

	resp, err := client.ChatCompletion(ctx, system, user)
	if err != nil {
		return EvaluateSkillResult{}, fmt.Errorf("llm evaluate: %w", err)
	}

	score := 0.0
	sufficient := false
	fmt.Sscanf(resp, "{\"score\":%f", &score)
	if strings.Contains(strings.ToLower(resp), "\"sufficient\":true") {
		sufficient = true
	}
	if !sufficient && score >= 0.7 {
		sufficient = true
	}

	return EvaluateSkillResult{
		Evaluations: []EvaluateSkillEvaluation{{Score: score, Reasoning: resp}},
		Sufficient:  sufficient,
	}, nil
}
