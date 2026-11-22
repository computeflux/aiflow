package db

import "time"

// models_skills.go：技能与进化闭环域表模型（Skill 注册表/版本/运行、S1 审批与 Agent、
// 进化任务、S3 熔断与干预）。自 models.go 拆出（批次 F，纯机械搬迁，行为无变更）。

// AuditLog 统一审计日志（表 audit_logs，月分区，只增不删）。
// 老列 job_id / note 承载 publish job 审计；S1 新增 target / detail（审批、灰度、
// 进化链全部动作留痕，见 docs/plan/S1-进化闭环.md）。
// 表结构由 apps/core/hacks/sql/approvals.sql 管理（复合主键 (id, created_at) 为月分区必需），
// 不加入 AutoMigrate；“只增不删”由应用层保证（无删除 API/入口 + 仅 DROP 过期分区）。
type AuditLog struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	JobID     *uint     `gorm:"index" json:"job_id"`
	Action    string    `json:"action"`
	Actor     string    `json:"actor"`
	Target    string    `gorm:"size:255;default:''" json:"target"`
	Note      string    `json:"note"`
	Detail    string    `gorm:"type:json" json:"detail"` // JSON 上下文（证据/参数快照）
	CreatedAt time.Time `json:"created_at"`
}

// Skill 是 Skill 注册表的主表（对应表 skills，P1 改造见 docs/plan/transform-plan.md §6.3）。
// 表结构由 apps/core/hacks/sql/skills.sql 或 skillgateway.EnsureSchema() 建立，
// 不加入 AutoMigrate（避免 GORM 改动 JSON 列定义）。
// input_schema / output_schema / limits / permissions 均以 JSON 字符串存储。
type Skill struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	Name         string    `gorm:"size:128;uniqueIndex;not null" json:"name"`
	Owner        string    `gorm:"size:64;not null" json:"owner"`
	Runtime      string    `gorm:"size:16;not null" json:"runtime"` // go / python / rust
	Type         string    `gorm:"size:16;not null" json:"type"`    // service / cli
	Entrypoint   string    `gorm:"size:512;default:''" json:"entrypoint"`
	Addr         string    `gorm:"size:512;default:''" json:"addr"`
	Container    string    `gorm:"size:64;default:''" json:"container"`
	Limits       string    `gorm:"type:json;not null" json:"limits"`
	Permissions  string    `gorm:"type:json" json:"permissions"`
	InputSchema  string    `gorm:"type:json;not null" json:"input_schema"`
	OutputSchema string    `gorm:"type:json;not null" json:"output_schema"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (Skill) TableName() string { return "skills" }

// SkillVersion 是 Skill 的版本记录（对应表 skill_versions）。
// traffic_split 0~100；status：draft / testing / gray / live / retired。
// GrayStage / GrayStageStartedAt / ApprovalID 为 S1 灰度状态机追加列
// （状态全部持久化在本表，api 重启后自动续推，docs/plan/S1-进化闭环.md §3.4）。
type SkillVersion struct {
	ID                 uint       `gorm:"primaryKey" json:"id"`
	SkillID            uint       `gorm:"uniqueIndex:uk_skill_version" json:"skill_id"`
	Version            string     `gorm:"size:32;uniqueIndex:uk_skill_version" json:"version"`
	Image              string     `gorm:"size:255" json:"image"`
	TrafficSplit       int        `gorm:"default:100" json:"traffic_split"`
	Status             string     `gorm:"size:16;default:draft" json:"status"`
	RollbackTo         string     `gorm:"size:32" json:"rollback_to"`
	Config             string     `gorm:"type:json" json:"config"` // 完整 spec JSON 快照（P1 追加）
	GrayStage          *int       `json:"gray_stage"`              // 当前灰度档位 5/50/100（S1）
	GrayStageStartedAt *time.Time `json:"gray_stage_started_at"`   // 当前档位窗口起点（S1）
	ApprovalID         *uint      `json:"approval_id"`             // 关联审批单（S1 灰度收尾定位进化单）
	CreatedAt          time.Time  `json:"created_at"`
}

func (SkillVersion) TableName() string { return "skill_versions" }

// SkillRun 是每次 Skill 调用的执行记录（对应表 skill_runs）。
// status：ok / fail / timeout / rejected。
type SkillRun struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	SkillName  string    `gorm:"size:128;index:idx_skill_time,priority:1;not null" json:"skill_name"`
	Version    string    `gorm:"size:32;not null" json:"version"`
	TraceID    string    `gorm:"size:64;index;not null" json:"trace_id"`
	WorkflowID string    `gorm:"size:64" json:"workflow_id"`
	RunID      string    `gorm:"size:64" json:"run_id"`
	InputHash  string    `gorm:"size:64;not null" json:"input_hash"`
	Status     string    `gorm:"size:16;not null" json:"status"`
	LatencyMs  *int64    `json:"latency_ms"`
	Error      string    `gorm:"type:text" json:"error"`
	CreatedAt  time.Time `gorm:"index:idx_skill_time,priority:2" json:"created_at"`
}

func (SkillRun) TableName() string { return "skill_runs" }

// Approval 是 S1 审批单（对应表 approvals，docs/plan/S1-进化闭环.md §3.2）。
// kind：skill_release / rollback / takeover；status：pending / approved / rejected / expired。
// payload 为 JSON 字符串（skill_release 含 diff/review/test_report；rollback 含 rollback_to）。
type Approval struct {
	ID         uint       `gorm:"primaryKey" json:"id"`
	ApprovalNo string     `gorm:"size:32;uniqueIndex;not null" json:"approval_no"`
	Kind       string     `gorm:"size:32;not null" json:"kind"`
	Subject    string     `gorm:"size:255;not null" json:"subject"`
	Payload    string     `gorm:"type:json;not null" json:"payload"`
	Status     string     `gorm:"size:16;index:idx_approvals_status,priority:1;not null;default:pending" json:"status"`
	Proposer   string     `gorm:"size:64;not null" json:"proposer"`
	Approver   string     `gorm:"size:64" json:"approver"`
	Reason     string     `gorm:"type:text" json:"reason"`
	ExpiresAt  *time.Time `json:"expires_at"`
	CreatedAt  time.Time  `gorm:"index:idx_approvals_status,priority:2" json:"created_at"`
	DecidedAt  *time.Time `json:"decided_at"`
}

func (Approval) TableName() string { return "approvals" }

// AgentBinding 是 Agent 角色绑定（对应表 agent_bindings，transform-plan §4.4）：
// 运行时可切换（agentctl bind / board Agents 页），evolve-driver 先查表、缺失回退 env。
type AgentBinding struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Role      string    `gorm:"size:32;uniqueIndex;not null" json:"role"` // coder | reviewer
	AgentName string    `gorm:"size:128;not null" json:"agent_name"`
	Version   string    `gorm:"size:32;default:latest" json:"version"`
	UpdatedBy string    `gorm:"size:64" json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (AgentBinding) TableName() string { return "agent_bindings" }

// AgentRegistry 是 Agent manifest 注册表（对应表 agents_registry）。
// entrypoint / container / workdir_prefix 是 evolve-driver 经 docker exec 调用
// adapter 所需的位置信息（容器内绝对路径）。
type AgentRegistry struct {
	Name          string    `gorm:"primaryKey;size:128" json:"name"`
	Role          string    `gorm:"size:32;not null" json:"role"`
	Version       string    `gorm:"size:32;default:latest" json:"version"`
	Entrypoint    string    `gorm:"size:512;not null" json:"entrypoint"`
	Container     string    `gorm:"size:64;not null" json:"container"`
	WorkdirPrefix string    `gorm:"size:64;default:/work" json:"workdir_prefix"`
	Limits        string    `gorm:"type:json" json:"limits"`
	Permissions   string    `gorm:"type:json" json:"permissions"`
	Config        string    `gorm:"type:json" json:"config"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (AgentRegistry) TableName() string { return "agents_registry" }

// EvolveTask 是进化单（对应表 evolve_tasks，docs/plan/S1-进化闭环.md §3.8）。
// 状态机只向前：open→coding→reviewing→sandboxing→testing→approving→released，
// 失败分支 rolled_back / cancelled；审计/人工可建（source=patrol|manual|card）。
// 表结构由 approval.EnsureSchema() 建立，不加入 AutoMigrate。
type EvolveTask struct {
	ID         uint   `gorm:"primaryKey" json:"id"`
	TaskNo     string `gorm:"size:32;uniqueIndex;not null" json:"task_no"`
	Source     string `gorm:"size:32;not null" json:"source"`
	Evidence   string `gorm:"type:json" json:"evidence"`
	TargetName string `gorm:"size:128;not null" json:"target_name"`
	Workdir    string `gorm:"size:255" json:"workdir"`
	Status     string `gorm:"size:32;index:idx_evolve_status,priority:1;not null;default:open" json:"status"`
	Diff       string `gorm:"type:longtext" json:"diff"`
	Review     string `gorm:"type:json" json:"review"`
	TestReport string `gorm:"type:json" json:"test_report"`
	ApprovalID *uint  `json:"approval_id"`
	// 人工 PR（自动化，2026-10-08）：审批通过后由 evolve-driver 推分支 + 建 GitHub PR；
	// PRUrl 非空 = 已建；PRError 非空 = 建 PR/本地合并失败或跳过原因（不再重试，人工处理后可清空重试）。
	PRUrl   string `gorm:"size:255;default:''" json:"pr_url"`
	PRError string `gorm:"type:text" json:"pr_error"`
	// 本地合并（EVOLVE_MERGE_MODE=local，2026-10-08）：released 后把 evolve 分支**快进**到本地 main，
	// 只动 ref（CAS update-ref）不碰工作区文件；MergeCommit = 合入 main 的提交 sha（空 = 未合并）。
	MergeCommit string `gorm:"size:64;default:''" json:"merge_commit"`
	// MergeState（2026-10-25）：合入 main 的阻塞态。"" = 正常；"paused_base_dirty" = main 工作区有未提交
	// 改动 → 合入暂停（看板显示为阻塞，driver 每 EVOLVE_MERGE_RETRY_SEC 秒重试检查，干净后自动补合）。
	MergeState string `gorm:"size:32;default:''" json:"merge_state"`
	// pi-web 会话 ID（多轮对话续接）：任务重新执行时复用上一轮的对话上下文，避免重复发送任务描述。
	SessionID string `gorm:"size:128;default:''" json:"session_id"`
	// SkipReview：看板「跳过审核」标记（2026-11-03）。
	// 置 1 后本轮直接跳过 AI reviewer（ocr），继续沙箱/CI → 审批进 Review 列，
	// 由人工看 diff。用于大 diff（一次 vendor 上万行）或 reviewer 反复超时的场景：
	// 逐行自动审这类改动只能耗满 MaxRounds 再 cancel，不如直接交人。
	SkipReview bool `gorm:"not null;default:false" json:"skip_review"`
	// FailReason：失败原因（status=failed 时由 evolve-driver 写入，看板直接展示）。
	// 2026-11-03：失败不再打回 Backlog —— status=failed 时工作项保持 in_progress、卡片留在 In Progress，
	// 由人工在看板选「忽略审核 / 续接执行 / 终止」。
	FailReason string    `gorm:"type:text" json:"fail_reason"`
	CreatedAt  time.Time `gorm:"index:idx_evolve_status,priority:2" json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (EvolveTask) TableName() string { return "evolve_tasks" }

// ModelProfile 是模型管理（S8）的模型档案（对应表 model_profiles）。
// active=1 的档案是全局生效模型：应用（PUT /model-config/apply）时应用层
// 保证单 active；Go 核心服务（api/audit/ops-report/...）启动 + 周期 ticker
// 读活动档热切 LLM；ai-flow 经 /model-config/sync-aiflow 推 model_list。
// api_key 仅服务端消费，列表接口必须脱敏（见 routes_models.go）。
type ModelProfile struct {
	ID             uint      `gorm:"primaryKey" json:"id"`
	Name           string    `gorm:"size:64;uniqueIndex;not null" json:"name"`
	Provider       string    `gorm:"size:32;not null;default:openai" json:"provider"`
	Model          string    `gorm:"size:128;not null" json:"model"`
	BaseURL        string    `gorm:"size:255;not null;default:''" json:"base_url"`
	APIKey         string    `gorm:"size:255;not null;default:''" json:"api_key"`
	EmbeddingModel string    `gorm:"size:128;not null;default:''" json:"embedding_model"`
	Consumers      string    `gorm:"size:128;not null;default:''" json:"consumers"`
	SyncAgents     int       `gorm:"not null;default:1" json:"sync_agents"` // 1=纳入 agent 框架模型目录（S8.1）
	Active         int       `gorm:"not null;default:0" json:"active"`
	Note           string    `gorm:"size:255;not null;default:''" json:"note"`
	UpdatedBy      string    `gorm:"size:64;not null;default:''" json:"updated_by"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func (ModelProfile) TableName() string { return "model_profiles" }

// SkillCircuit 是熔断状态持久化（对应表 skill_circuit，S3 熔断器启用，S1 先建表）。
type SkillCircuit struct {
	SkillName           string     `gorm:"primaryKey;size:128" json:"skill_name"`
	State               string     `gorm:"size:16;not null;default:close" json:"state"` // close | open | half
	ConsecutiveFailures int        `gorm:"not null;default:0" json:"consecutive_failures"`
	OpenedAt            *time.Time `json:"opened_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

func (SkillCircuit) TableName() string { return "skill_circuit" }

// Intervention 是干预记录（对应表 interventions，transform-plan §9.3；
// S1 提前建表：越界打回 / 灰度回滚写入 kind=manual）。
type Intervention struct {
	ID         uint       `gorm:"primaryKey" json:"id"`
	WorkflowID string     `gorm:"size:64;not null" json:"workflow_id"` // 无 workflow 时填 skill 名等标识
	RunID      string     `gorm:"size:64" json:"run_id"`
	Kind       string     `gorm:"size:32;not null" json:"kind"`                // circuit_open | loop_detect | manual
	Status     string     `gorm:"size:16;not null;default:open" json:"status"` // open | resolved
	Note       string     `gorm:"type:text" json:"note"`
	CreatedAt  time.Time  `json:"created_at"`
	ResolvedAt *time.Time `json:"resolved_at"`
}

func (Intervention) TableName() string { return "interventions" }
