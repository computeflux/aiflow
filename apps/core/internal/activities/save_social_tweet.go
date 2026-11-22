package activities

import (
	"context"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
)

// SaveSocialTweetInput 是保存生成推文 activity 的输入：关联排程 + 内容 + 来源。
type SaveSocialTweetInput struct {
	ScheduleID uint    `json:"schedule_id"`
	Content    string  `json:"content"`
	SourceURL  string  `json:"source_url"`
	Keyword    string  `json:"keyword"`
	Score      float64 `json:"score"`
	Reasoning  string  `json:"reasoning"`
}

func (a *Activities) SaveSocialTweet(ctx context.Context, input SaveSocialTweetInput) error {
	tweet := db.SocialTweet{
		ScheduleID: input.ScheduleID,
		Content:    input.Content,
		SourceURL:  input.SourceURL,
		Keyword:    input.Keyword,
		Score:      input.Score,
		Reasoning:  input.Reasoning,
		CreatedAt:  time.Now(),
	}
	return a.DB.Create(&tweet).Error
}
