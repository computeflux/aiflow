package activities

import (
	"context"
)

// Evaluation represents a relevance score for a retrieved document.
type Evaluation = EvaluateSkillEvaluation

// EvaluateInput is the payload for the CRAG evaluation activity.
type EvaluateInput = EvaluateSkillInput

// EvaluateResult returns per-document scores and an overall sufficiency flag.
type EvaluateResult = EvaluateSkillResult

// Evaluate uses an LLM to judge whether retrieved documents can answer the query.
func (a *Activities) Evaluate(ctx context.Context, input EvaluateInput) (EvaluateResult, error) {
	return EvaluateRun(ctx, a.LLM, input)
}
