// Package generate implements the tweet generation skill:
// an LLM writes a Chinese social-media post from retrieved knowledge.
package activities

import (
	"context"
	"fmt"
	"strings"

	"github.com/wetee/ai-workflow/internal/llm"
)

// GenerateSkillInput is the payload for tweet generation.
type GenerateSkillInput struct {
	Topic    string   `json:"topic"`
	Platform string   `json:"platform"`
	Query    string   `json:"query"`
	Docs     []string `json:"docs"`
	Sources  []string `json:"sources"`
}

// GenerateSkillResult contains the generated tweet and source references.
type GenerateSkillResult struct {
	Content string   `json:"content"`
	Sources []string `json:"sources"`
}

// GenerateRun creates a tweet from retrieved knowledge and recent industry news.
func GenerateRun(ctx context.Context, client *llm.Client, input GenerateSkillInput) (GenerateSkillResult, error) {
	knowledge := strings.Join(input.Docs, "\n\n---\n\n")
	system := "You are a Chinese social media content editor. Write a concise, engaging tweet in Chinese. Only output the tweet text, no explanation."
	user := fmt.Sprintf(`Topic: %s
Platform: %s
Query: %s

Reference materials:
%s

Generate a tweet based on the above.`, input.Topic, input.Platform, input.Query, knowledge)

	content, err := client.ChatCompletion(ctx, system, user)
	if err != nil {
		return GenerateSkillResult{}, fmt.Errorf("llm generate: %w", err)
	}
	return GenerateSkillResult{
		Content: content,
		Sources: input.Sources,
	}, nil
}
