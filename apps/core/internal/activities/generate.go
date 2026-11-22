package activities

import (
	"context"
)

// GenerateInput is the payload for tweet generation activity.
type GenerateInput = GenerateSkillInput

// GenerateResult contains the generated tweet and source references.
type GenerateResult = GenerateSkillResult

// Generate creates a tweet from retrieved knowledge and recent industry news.
func (a *Activities) Generate(ctx context.Context, input GenerateInput) (GenerateResult, error) {
	return GenerateRun(ctx, a.LLM, input)
}
