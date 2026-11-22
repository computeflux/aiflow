package errorsink

import (
	"log"
	"time"

	"github.com/computeflux/mailer/models"
)

type Entry struct {
	System     string
	Level      string
	Source     string
	WorkflowID string
	RunID      string
	TraceID    string
	Message    string
	Stack      string
	Context    string
}

func Record(database *models.DB, entry Entry) {
	if database == nil {
		log.Printf("[%s] [%s] %s (context=%s)", entry.System, entry.Source, entry.Message, entry.Context)
		return
	}
	if entry.Level == "" {
		entry.Level = "error"
	}
	errLog := models.ErrorLog{
		System:     entry.System,
		Level:      entry.Level,
		Source:     entry.Source,
		WorkflowID: entry.WorkflowID,
		RunID:      entry.RunID,
		TraceID:    entry.TraceID,
		Message:    entry.Message,
		Stack:      entry.Stack,
		Context:    entry.Context,
		CreatedAt:  time.Now(),
	}
	if err := database.Create(&errLog).Error; err != nil {
		log.Printf("[errorsink] record error failed: %v", err)
	}
}
