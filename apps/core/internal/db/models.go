package db

import (
	"time"

	"gorm.io/gorm"
)

// 发布平台标识（publish_jobs.platform / accounts.platform）。
const (
	PlatformX           = "x"
	PlatformXiaohongshu = "xiaohongshu"
	PlatformGitHub      = "github"
)

// 发布任务状态机（publish_jobs.status；queued/sending 为发送链路中间态）。
const (
	StatusPending   = "pending"
	StatusApproved  = "approved"
	StatusRejected  = "rejected"
	StatusPublished = "published"
	StatusFailed    = "failed"
	StatusQueued    = "queued"
	StatusSending   = "sending"
	StatusSkipped   = "skipped"
)

// Account 是发布账号（accounts 表）；Credentials 存平台登录凭据 JSON（接口侧脱敏）。
type Account struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	Platform    string    `gorm:"index;not null" json:"platform"`
	AccountID   string    `gorm:"index;not null" json:"account_id"`
	Name        string    `json:"name"`
	Credentials string    `json:"credentials"`
	Active      bool      `gorm:"default:true" json:"active"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// FetchCache 是抓取内容缓存（fetch_cache 表）：按 source+external_id 去重，供工作流复用。
type FetchCache struct {
	ID          uint       `gorm:"primaryKey" json:"id"`
	WorkflowID  string     `gorm:"index;not null" json:"workflow_id"`
	Source      string     `gorm:"index;not null" json:"source"`
	ExternalID  string     `gorm:"index" json:"external_id"`
	Keyword     string     `gorm:"index" json:"keyword"`
	URL         string     `json:"url"`
	Content     string     `gorm:"type:longtext" json:"content"`
	PublishedAt *time.Time `json:"published_at"`
	FetchedAt   time.Time  `json:"fetched_at"`
	RawJSON     string     `gorm:"type:longtext" json:"raw_json"`
	CreatedAt   time.Time  `json:"created_at"`
}

// BeforeSave 把空的 JSON 列归一为合法 JSON：MySQL 的 JSON 列不接受空串，
// 否则 `store.Log(ctx, ..., nil)` 这类调用会静默丢失审计留痕（而 S1 的红线就是留痕）。
func (a *AuditLog) BeforeSave(*gorm.DB) error {
	if a.Detail == "" {
		a.Detail = "null"
	}
	return nil
}

// BeforeSave 把空的 JSON 列归一为合法 JSON（MySQL JSON 列不接受空串；
// limits/input_schema/output_schema 为 NOT NULL，更不能写空串）。
func (s *Skill) BeforeSave(*gorm.DB) error {
	for p, v := range map[*string]string{&s.Limits: "{}", &s.Permissions: "{}", &s.InputSchema: "{}", &s.OutputSchema: "{}"} {
		if *p == "" {
			*p = v
		}
	}
	return nil
}

// BeforeSave 把空的 JSON 列归一为合法 JSON（MySQL JSON 列不接受空串）。
func (v *SkillVersion) BeforeSave(*gorm.DB) error {
	if v.Config == "" {
		v.Config = "{}"
	}
	return nil
}

// BeforeSave 把空的 JSON 列归一为合法 JSON（MySQL 的 JSON 列不接受空串，
// 2026-09-30 实测：config 缺省时 register 会踩 Error 3140）。
func (a *AgentRegistry) BeforeSave(*gorm.DB) error {
	if a.Limits == "" {
		a.Limits = "{}"
	}
	if a.Permissions == "" {
		a.Permissions = "{}"
	}
	if a.Config == "" {
		a.Config = "{}"
	}
	return nil
}

// BeforeSave 把空的 JSON 列归一为合法 JSON：MySQL 的 JSON 列**不接受空串**
// （2026-09-30 实测：patrol 建单与 driver 早期保存均踩 Error 3140 "The document is empty"）。
// 语义：未填写 = JSON null（列表/详情接口原样返回 null）。
func (t *EvolveTask) BeforeSave(*gorm.DB) error {
	if t.Evidence == "" {
		t.Evidence = "{}"
	}
	if t.Review == "" {
		t.Review = "null"
	}
	if t.TestReport == "" {
		t.TestReport = "null"
	}
	return nil
}
