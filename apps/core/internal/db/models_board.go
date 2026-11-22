package db

import "time"

// models_board.go：产品看板域表模型（Board/List/Card/评论/思维导图/统一工作项）。
// 自 models.go 拆出（批次 F，纯机械搬迁，行为无变更）。

// Board is a kanban-style project board.
type Board struct {
	ID          uint        `gorm:"primaryKey" json:"id"`
	Name        string      `gorm:"not null" json:"name"`
	Description string      `gorm:"type:text" json:"description"`
	ViewMode    string      `gorm:"default:week" json:"view_mode"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
	Lists       []BoardList `gorm:"foreignKey:BoardID" json:"lists,omitempty"`
}

// BoardList 是看板内的列（board_lists 表），按 Position 排序。
type BoardList struct {
	ID        uint        `gorm:"primaryKey" json:"id"`
	BoardID   uint        `gorm:"index;not null" json:"board_id"`
	Name      string      `gorm:"not null" json:"name"`
	Position  int         `gorm:"default:0" json:"position"`
	CreatedAt time.Time   `json:"created_at"`
	Cards     []BoardCard `gorm:"foreignKey:ListID" json:"cards,omitempty"`
}

// CardDependency 是卡片的前置依赖状态（**不落库**，读取看板时实时计算）：
// 依赖卡未合入主分支 → 当前卡不可开始。
type CardDependency struct {
	CardID uint   `json:"card_id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	// Merged 依赖卡的改动是否已合入主分支（false = 仍在阻塞）
	Merged bool `json:"merged"`
}

// BoardCard 是看板卡片（board_cards 表）：状态机 + S9 工作项关联字段（work_item_no）。
type BoardCard struct {
	ID          uint       `gorm:"primaryKey" json:"id"`
	ListID      uint       `gorm:"index;not null" json:"list_id"`
	Title       string     `gorm:"not null" json:"title"`
	Description string     `gorm:"type:text" json:"description"`
	Position    int        `gorm:"default:0" json:"position"`
	Assignee    string     `json:"assignee"`
	Labels      string     `json:"labels"`
	DueDate     *time.Time `json:"due_date"`
	Priority    string     `gorm:"default:medium" json:"priority"` // low, medium, high, urgent
	CoverColor  string     `json:"cover_color"`                    // hex color for card top border
	Checklist   string     `gorm:"type:text" json:"checklist"`     // JSON: [{text,done}]
	Source      string     `json:"source"`
	// S9.8（§7.10）：目标项目名（projects.name，可多选逗号分隔），即"本卡允许 S1 修改的 apps/<dir> 项目"。
	// 只按项目识别，不再识别技能；可空：空 = 无目标 → rejected（回 Backlog，不回退 labels）。labels 是纯展示标签。
	TargetName string `gorm:"size:128" json:"target_name"`
	// S7 产品情报：任务卡状态（默认 todo）。
	// ready_for_evolve = 已交给工作流（可被 S1 evolve-driver 领取，目标为已注册启用项目）。
	Status     string `gorm:"size:32;default:todo" json:"status"`
	WorkflowID string `json:"workflow_id"`
	WorkItemNo string `gorm:"size:32" json:"work_item_no"` // S9 关联统一工作项（可空）；分诊阶段为 "card-<id>"（卡关联，hand-off 后回填工作项号）
	// DependsOn 显式前置卡 ID（JSON 数组，如 "[137]"）。
	// 语义：这些卡的改动**合入主分支后**本卡才能开始执行。
	// 为空时退回 LLM 推断依赖（见 internal/workitem/deps.go）。
	DependsOn string `gorm:"size:255" json:"depends_on"`
	// BlockedBy 读取时计算的阻塞项（不落库）：依赖卡的合并状态。
	BlockedBy []CardDependency `gorm:"-" json:"blocked_by,omitempty"`
	// EvolveStatus / FailReason：读取时从关联 evolve_task 投影（**不落库**）。
	// 看板用它展示「已失败」徐标 + 失败原因 + 「忽略审核 / 续接执行」按钮。
	EvolveStatus string `gorm:"-" json:"evolve_status,omitempty"`
	FailReason   string `gorm:"-" json:"fail_reason,omitempty"`
	// MergeState：合入阻塞态投影（不落库）：paused_base_dirty = 分支/主干有未提交改动无法合并；
	// conflict = 合并冲突重试中。卡面用它提示「无法完成合并」。
	MergeState string `gorm:"-" json:"merge_state,omitempty"`
	// S9 Bug 看板（§7.4 重设：Bug 队列 = 工作看板里的 Bug 看板）：
	SourceRef string         `gorm:"size:255" json:"source_ref"` // Bug 卡：错误簇键（system|source|归一化消息，去重键）；需求卡为空
	Frequency int            `gorm:"default:0" json:"frequency"` // Bug 卡：近 24h 出现次数（分诊归并累加）；0 = 非 Bug 卡
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	Comments  []BoardComment `gorm:"foreignKey:CardID" json:"comments,omitempty"`
}

// BoardComment 是卡片评论（board_comments 表）。
type BoardComment struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CardID    uint      `gorm:"index;not null" json:"card_id"`
	Author    string    `json:"author"`
	Body      string    `gorm:"type:text;not null" json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// MindMap 是思维导图（mind_maps 表）；Data 存画布 JSON。
type MindMap struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Name      string    `gorm:"not null" json:"name"`
	Data      string    `gorm:"type:longtext;not null" json:"data"`
	Layout    string    `gorm:"default:mindMap" json:"layout"`
	Theme     string    `gorm:"default:dark" json:"theme"`
	ViewX     float64   `gorm:"default:0" json:"view_x"`
	ViewY     float64   `gorm:"default:0" json:"view_y"`
	Scale     float64   `gorm:"default:1" json:"scale"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// WorkItem 是 S9 统一工作项（对应表 work_items，docs/plan/S9-统一工作项流水线.md §5.1）：
// 需求 / Bug / 自进化提案统一成一种货币，进同一条带轻重缓急（score + P0-P3）的队列，
// 由 work-dispatch 调度器按 score 取最高分自动领取并驱动 S1 进化链，人只在审批处放行。
// kind：requirement（需求）| bug（缺陷）；source：product | error_log | audit | manual。
// 状态机见 internal/workitem（open→triaged→queued→in_progress→in_review→approved→shipped；
// rejected / expired 为终止/旁路态）。
type WorkItem struct {
	ID             uint       `gorm:"primaryKey" json:"id"`
	No             string     `gorm:"size:32;uniqueIndex;not null" json:"no"`
	Kind           string     `gorm:"size:16;not null" json:"kind"`
	Title          string     `gorm:"size:255;not null" json:"title"`
	Description    string     `gorm:"type:text" json:"description"`
	Source         string     `gorm:"size:32;not null" json:"source"`
	SourceRef      string     `gorm:"size:255" json:"source_ref"`  // card_id / error_log_id / audit_report_id / manual / 错误簇键（system|source|归一化消息，≤230 字符）
	TargetName     string     `gorm:"size:128" json:"target_name"` // 目标项目（可多选，逗号分隔；core 前缀 = 核心框架红线，S9.8 §7.10）
	Score          float64    `gorm:"default:0" json:"score"`
	Priority       string     `gorm:"size:8;default:P2" json:"priority"` // P0 | P1 | P2 | P3
	Severity       int        `gorm:"default:0" json:"severity"`         // 0-4
	Urgency        int        `gorm:"default:0" json:"urgency"`          // 0-4
	Impact         int        `gorm:"default:0" json:"impact"`           // 0-4
	Frequency      int        `gorm:"default:0" json:"frequency"`        // 0-4
	Status         string     `gorm:"size:32;index;not null;default:open" json:"status"`
	DueAt          *time.Time `json:"due_at"`
	Checklist      string     `gorm:"type:text" json:"checklist"` // JSON [{text,done}]
	Assignee       string     `gorm:"size:128" json:"assignee"`
	Locked         bool       `gorm:"default:false" json:"locked"`
	TriageDegraded bool       `gorm:"default:false" json:"triage_degraded"` // LLM 不可用降级标注
	CardID         *uint      `json:"card_id"`
	EvolveTaskNo   string     `gorm:"size:32" json:"evolve_task_no"`
	RollbackCount  int        `gorm:"default:0" json:"rollback_count"` // S1 灰度回滚次数（重修上限，超限转 rejected；见 workitem.feedback）
	CreatedAt      time.Time  `json:"created_at"`
	TriagedAt      *time.Time `json:"triaged_at"`
	PickedAt       *time.Time `json:"picked_at"`
	ShippedAt      *time.Time `json:"shipped_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

func (WorkItem) TableName() string { return "work_items" }
