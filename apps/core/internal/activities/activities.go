package activities

import (
	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/llm"
	"github.com/wetee/ai-workflow/internal/qdrant"
)

// Activities holds the dependencies for all Go Temporal activities.
type Activities struct {
	Qdrant *qdrant.Client
	LLM    *llm.Client
	DB     *db.DB
}

// New creates an Activities instance.
func New(qc *qdrant.Client, llm *llm.Client, database *db.DB) *Activities {
	return &Activities{
		Qdrant: qc,
		LLM:    llm,
		DB:     database,
	}
}
