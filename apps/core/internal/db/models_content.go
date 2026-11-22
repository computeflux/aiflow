package db

import "time"

// models_content.go：内容生产域表模型（抓取/发布/邮件/社交/X 订阅点赞/错误日志）。
// 自 models.go 拆出（批次 F，纯机械搬迁，行为无变更）。

// RawItem 是采集原文（raw_items 表）：source+external_id 唯一去重，入 RAG 前清洗。
type RawItem struct {
	ID          uint       `gorm:"primaryKey" json:"id"`
	Source      string     `gorm:"uniqueIndex:idx_source_external;index;not null" json:"source"`
	ExternalID  string     `gorm:"uniqueIndex:idx_source_external;index" json:"external_id"`
	Keyword     string     `gorm:"index" json:"keyword"`
	URL         string     `json:"url"`
	Content     string     `gorm:"type:text" json:"content"`
	PublishedAt *time.Time `json:"published_at"`
	FetchedAt   time.Time  `json:"fetched_at"`
	RawJSON     string     `gorm:"type:longtext" json:"raw_json"`
	Liked       bool       `gorm:"index;default:false" json:"liked"`
	LikedAt     *time.Time `json:"liked_at"`
}

// PublishJob 是发布任务（publish_jobs 表）：审批通过后由工作流投递到平台。
type PublishJob struct {
	ID          uint       `gorm:"primaryKey" json:"id"`
	Platform    string     `gorm:"index;not null" json:"platform"`
	AccountID   string     `gorm:"index;not null" json:"account_id"`
	Content     string     `gorm:"type:text" json:"content"`
	Sources     string     `gorm:"type:text" json:"sources"`
	Status      string     `gorm:"index;not null;default:pending" json:"status"`
	ErrorMsg    string     `json:"error_msg"`
	CreatedAt   time.Time  `json:"created_at"`
	ApprovedAt  *time.Time `json:"approved_at"`
	PublishedAt *time.Time `json:"published_at"`
}

// IngestTask 是采集任务（ingest_tasks 表）：cron 表达式驱动 ScheduledFetchWorkflow。
type IngestTask struct {
	ID             uint       `gorm:"primaryKey" json:"id"`
	Source         string     `gorm:"index;not null" json:"source"`
	Keyword        string     `gorm:"index;not null" json:"keyword"`
	Limit          int        `gorm:"default:0" json:"limit"`
	Mode           string     `json:"mode"`
	ParallelCount  int        `gorm:"default:0" json:"parallel_count"`
	Schedule       string     `json:"schedule"`
	Enabled        bool       `gorm:"default:true" json:"enabled"`
	LastRunAt      *time.Time `json:"last_run_at"`
	LastStatus     string     `json:"last_status"`
	LastWorkflowID string     `json:"last_workflow_id"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// EmailBatch 是邮件批次（email_batches 表）：一次 AI 分析 + 群发的批次头。
type EmailBatch struct {
	ID            uint       `gorm:"primaryKey" json:"id"`
	OrgName       string     `gorm:"index;not null" json:"org_name"`
	Status        string     `gorm:"index;not null;default:pending" json:"status"`
	WorkflowID    string     `json:"workflow_id"`
	Total         int        `json:"total"`
	Sent          int        `json:"sent"`
	Analysis      string     `gorm:"type:text" json:"analysis"`
	ErrorMsg      string     `gorm:"type:text" json:"error_msg"`
	CcList        string     `gorm:"type:text" json:"cc_list"`
	CustomSubject string     `gorm:"type:text" json:"custom_subject"`
	CustomContent string     `gorm:"type:text" json:"custom_content"`
	EmailSenderID *uint      `gorm:"index" json:"email_sender_id"`
	CreatedAt     time.Time  `json:"created_at"`
	ApprovedAt    *time.Time `json:"approved_at"`
	SentAt        *time.Time `json:"sent_at"`
}

// EmailRecipient 是邮件收件人（email_recipients 表）：逐人发送状态与退信记录。
type EmailRecipient struct {
	ID        uint       `gorm:"primaryKey" json:"id"`
	BatchID   uint       `gorm:"index;not null" json:"batch_id"`
	Email     string     `gorm:"index;not null" json:"email"`
	Name      string     `json:"name"`
	Repo      string     `json:"repo"`
	Reason    string     `json:"reason"`
	Selected  bool       `gorm:"default:true" json:"selected"`
	Status    string     `gorm:"default:pending" json:"status"`
	SentAt    *time.Time `json:"sent_at"`
	ErrorMsg  string     `json:"error_msg"`
	CreatedAt time.Time  `json:"created_at"`
}

// EmailSendLog tracks how many times an email has been sent globally.
type EmailSendLog struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Email     string    `gorm:"uniqueIndex;not null;type:varchar(320)" json:"email"`
	Count     int       `gorm:"default:0" json:"count"`
	LastSent  time.Time `json:"last_sent"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// EmailTemplate 是邮件模板（email_templates 表）：AI 生成 + 人工编辑的正文骨架。
type EmailTemplate struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	Name        string    `gorm:"not null" json:"name"`
	Subject     string    `gorm:"not null" json:"subject"`
	Body        string    `gorm:"type:text;not null" json:"body"`
	ContentType string    `gorm:"default:text" json:"content_type"`
	IsDefault   bool      `gorm:"default:false" json:"is_default"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// EmailSender 存储在数据库中的多邮件服务商/多发件邮箱配置
type EmailSender struct {
	ID          uint       `gorm:"primaryKey" json:"id"`
	Name        string     `gorm:"size:128;not null" json:"name"`         // 配置别名或服务商名称 (如 Resend-主号 / 阿里云企业邮)
	Host        string     `gorm:"size:255;not null" json:"host"`         // SMTP 服务器地址 (如 smtp.resend.com)
	Port        int        `gorm:"default:465" json:"port"`               // SMTP 端口 (465 或 587)
	User        string     `gorm:"size:255;not null" json:"user"`         // SMTP 认证用户名/API Key
	Password    string     `gorm:"size:512;not null" json:"password"`     // SMTP 认证密码/密钥
	FromAddress string     `gorm:"size:320;not null" json:"from_address"` // 发件人邮箱
	FromName    string     `gorm:"size:128" json:"from_name"`             // 发件人名称 (如 ComputeFlux)
	DailyLimit  int        `gorm:"default:0" json:"daily_limit"`          // 每日发信配额限制 (0 为不限)
	UsedToday   int        `gorm:"default:0" json:"used_today"`           // 今日已发数量
	LastUsedAt  *time.Time `json:"last_used_at"`                          // 最后发信时间
	Enabled     bool       `gorm:"default:true" json:"enabled"`           // 是否启用
	IsDefault   bool       `gorm:"default:false" json:"is_default"`       // 是否为默认发信邮箱
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

func (EmailSender) TableName() string { return "email_senders" }

// SocialSchedule 是社交排程（social_schedules 表）：定时抓取关键词并生成待审内容。
type SocialSchedule struct {
	ID        uint       `gorm:"primaryKey" json:"id"`
	Hours     int        `gorm:"default:24" json:"hours"`
	Count     int        `gorm:"default:5" json:"count"`
	Sources   string     `gorm:"default:x" json:"sources"`
	Schedule  string     `gorm:"not null" json:"schedule"`
	Enabled   bool       `gorm:"default:true" json:"enabled"`
	LastRunAt *time.Time `json:"last_run_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// SocialTweet 是生成的推文（social_tweets 表）：关联排程，人工确认后发布。
type SocialTweet struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	ScheduleID uint      `gorm:"index" json:"schedule_id"`
	Content    string    `gorm:"type:text;not null" json:"content"`
	SourceURL  string    `json:"source_url"`
	Keyword    string    `json:"keyword"`
	Score      float64   `json:"score"`
	Reasoning  string    `gorm:"type:text" json:"reasoning"`
	CreatedAt  time.Time `json:"created_at"`
}

// XAccount 是 pyreplay worker 的点赞账号池（对应表 x_accounts）。
// 注意：auth_token / ct0 为敏感字段，接口返回前必须脱敏。
// machine_id 用于多机器人部署：每台机器人只使用属于自己的账号。
type XAccount struct {
	ID              uint       `gorm:"primaryKey" json:"id"`
	MachineID       string     `gorm:"size:64;index;default:''" json:"machine_id"`
	Username        string     `gorm:"size:128;not null" json:"username"`
	AuthToken       string     `gorm:"size:512;not null" json:"auth_token"`
	Ct0             string     `gorm:"size:512;not null" json:"ct0"`
	Enabled         bool       `gorm:"default:true" json:"enabled"`
	DailyLikeLimit  int        `gorm:"default:50" json:"daily_like_limit"`
	CooldownMinutes int        `gorm:"default:10" json:"cooldown_minutes"`
	UsedToday       int        `gorm:"default:0" json:"used_today"`
	LastUsedAt      *time.Time `json:"last_used_at"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// ErrorLog 统一错误日志：各系统（API / Go Worker / Python Worker）把错误写入该表，
// board 的“错误日志”页可以查看、筛选并标记处理。
type ErrorLog struct {
	ID         uint       `gorm:"primaryKey" json:"id"`
	System     string     `gorm:"size:64;index;not null" json:"system"`     // api / go-worker / pycraper / pyreplay
	Level      string     `gorm:"size:16;index;default:error" json:"level"` // error / warning / fatal
	Source     string     `gorm:"size:128;index" json:"source"`             // activity 名 / 路由 / 平台名
	WorkflowID string     `gorm:"size:255;index" json:"workflow_id"`        // Temporal workflow id
	RunID      string     `gorm:"size:255" json:"run_id"`                   // Temporal run id
	TraceID    string     `gorm:"size:64;index;default:''" json:"trace_id"` // OpenTelemetry trace id（打通错误与链路）
	Message    string     `gorm:"type:text;not null" json:"message"`        // 错误信息
	Stack      string     `gorm:"type:longtext" json:"stack"`               // 堆栈（可选）
	Context    string     `gorm:"type:longtext" json:"context"`             // JSON 上下文：attempt / 请求体等
	Resolved   bool       `gorm:"index;default:false" json:"resolved"`      // 是否已处理
	CreatedAt  time.Time  `gorm:"index" json:"created_at"`
	ResolvedAt *time.Time `json:"resolved_at"`
	WorkItemNo string     `gorm:"size:32;index" json:"work_item_no"` // S9 归并到的 Bug 工作项号（可空）
}

func (XAccount) TableName() string { return "x_accounts" }

// FollowTarget 是订阅的 X 用户（对应表 follow_targets），由 pyreplay
// worker 的 FollowActivities.scan_and_like 定期扫描点赞。
// machine_id 用于多机器人部署：每台机器人只扫描属于自己的订阅目标。
type FollowTarget struct {
	ID                  uint       `gorm:"primaryKey" json:"id"`
	MachineID           string     `gorm:"size:64;index;uniqueIndex:uk_handle_machine;default:''" json:"machine_id"`
	Handle              string     `gorm:"size:128;uniqueIndex:uk_handle_machine;not null" json:"handle"`
	Enabled             bool       `gorm:"default:true" json:"enabled"`
	ScanIntervalMinutes int        `gorm:"default:15" json:"scan_interval_minutes"`
	MaxLikePerScan      int        `gorm:"default:5" json:"max_like_per_scan"`
	LastCheckedAt       *time.Time `json:"last_checked_at"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

func (FollowTarget) TableName() string { return "follow_targets" }

// LikeJob 是分发到各机器的点赞任务（对应表 like_jobs）。
// 唯一键 (tweet_id, machine_id)：每台机器对每条推文最多一个任务，
// 不同机器可重复（每台机器用自己的账号池各点一次）。
// 状态机：pending（待执行）→ claimed（被某台机器抢占执行中）→ done / failed，
// 失败后回退 pending 并顺延 scheduled_at 重试，超过尝试次数标记 failed。
type LikeJob struct {
	ID              uint       `gorm:"primaryKey" json:"id"`
	FollowTargetID  uint       `gorm:"index" json:"follow_target_id"`
	Handle          string     `gorm:"size:128" json:"handle"`
	TweetID         string     `gorm:"size:64;uniqueIndex:uk_tweet_machine" json:"tweet_id"`
	MachineID       string     `gorm:"size:64;uniqueIndex:uk_tweet_machine;index:idx_machine_status_time,priority:1" json:"machine_id"`
	TweetURL        string     `gorm:"size:512" json:"tweet_url"`
	Content         string     `gorm:"type:text" json:"content"`
	ScheduledAt     time.Time  `gorm:"index:idx_machine_status_time,priority:3" json:"scheduled_at"`
	Status          string     `gorm:"size:16;index:idx_machine_status_time,priority:2;default:pending" json:"status"` // pending/claimed/done/failed
	Attempts        int        `gorm:"default:0" json:"attempts"`
	AccountID       *uint      `json:"account_id"`
	AccountUsername string     `gorm:"size:128" json:"account_username"`
	Error           string     `gorm:"type:text" json:"error"`
	ClaimedAt       *time.Time `json:"claimed_at"`
	DoneAt          *time.Time `json:"done_at"`
	CreatedAt       time.Time  `json:"created_at"`
}

func (LikeJob) TableName() string { return "like_jobs" }
