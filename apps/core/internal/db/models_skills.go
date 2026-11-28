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

// SocialAccount 是 social 多渠道账号池的账号行（对应表 social_accounts，由 apps/social
// 服务建立；core 侧只读 + 会话保活降级写，见 internal/migration）。
//
// ⚠️ SessionRef 存的是 AES-256-GCM 密文（v1.<base64url>），json tag 用 "-"：
// core 侧任何直接序列化本结构的响应都不会泄漏登录态；core 也绝不解密。
type SocialAccount struct {
	ID             int64      `gorm:"primaryKey" json:"id"`
	Channel        string     `gorm:"size:32;uniqueIndex:uk_social_account;not null" json:"channel"`
	AccountID      string     `gorm:"size:128;uniqueIndex:uk_social_account;not null" json:"account_id"`
	MachineID      string     `gorm:"size:64;not null;default:''" json:"machine_id"`
	Owner          string     `gorm:"size:64;not null;default:''" json:"owner"`
	LoginName      string     `gorm:"size:128;not null;default:''" json:"login_name"`
	SessionRef     string     `gorm:"type:text" json:"-"`
	CookiesPath    string     `gorm:"size:255;not null;default:''" json:"cookies_path"`
	Status         string     `gorm:"size:16;not null;default:active;index" json:"status"`
	CooldownUntil  *time.Time `json:"cooldown_until"`
	LastUsedAt     *time.Time `json:"last_used_at"`
	UsedToday      int        `gorm:"not null;default:0" json:"used_today"`
	DailyLimit     int        `gorm:"not null;default:0" json:"daily_limit"`
	LastError      string     `gorm:"type:text" json:"last_error"`
	LastLoginAt    *time.Time `json:"last_login_at"`
	LastLoginError string     `gorm:"type:text" json:"last_login_error"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

func (SocialAccount) TableName() string { return "social_accounts" }

// SocialAccount 的状态机取值（与 apps/social/internal/store 的 Account* 常量逐字一致）。
const (
	SocialAccountActive    = "active"
	SocialAccountNeedLogin = "need_login"
	SocialAccountBanned    = "banned"
	SocialAccountCooldown  = "cooldown"
)

// skill_cutover.status 的取值（状态机定义在 internal/migration 的 CutoverService）。
//
// 常量放在 db 包（表所属层）而不是 migration 包：跨包读这张表的状态机（如 approval 的
// TickGray 灰度推进）必须用同一套取值判「切流是否活跃」。一旦两侧各写各的字面量，
// migration 侧改名/加状态时 approval 侧的守卫会静默失效，而两侧各自的单测都发现不了。
const (
	CutoverStatusPending    = "pending"     // 已登记目标，未放量
	CutoverStatusRunning    = "running"     // 放量中（traffic ∈ 档位）
	CutoverStatusCompleted  = "completed"   // 已全量并收尾（skills.addr 已收敛）
	CutoverStatusRolledBack = "rolled_back" // 已回滚
)

// CutoverStatusActive 是「切流状态机还会动这些行」的状态集合：跨包守卫（approval 灰度推进
// 让路、S5/S3 巡检让路）一律用它，不要各自罗列状态字面量。
//
// completed / rolled_back 都是终态：收尾之后切流状态机不再动这些行；回滚时目标版本行已被
// 置 retired/traffic=0，该切流不可能再产生该版本的 gray 行（真要重新放量走
// CutoverService.Stage，它在写流量之前就把 status 写成 running），所以两者都不该让路。
var CutoverStatusActive = []string{CutoverStatusPending, CutoverStatusRunning}

// SkillCutover 是 skill 的「跨服务切流」状态（对应表 skill_cutover，M4-1）。
// SkillCutover 是 skill 的「跨服务切流」状态（对应表 skill_cutover，M4-1）。
//
// 与 skill_versions（版本灰度）分表记录的原因：切流的主体是「一个能力从旧实现迁到
// 新实现」，新旧两版共用同一个 skill 名与同一套 schema，只有 addr 不同。把切流的
// 档位 / 目标 addr / 判定阈值单独记一行，审计与 board 运维中心才能一眼看清「现在切到
// 第几档、达标没有、谁批的」，而不用从多个版本行的 traffic_split 里反推意图。
type SkillCutover struct {
	ID         uint   `gorm:"primaryKey" json:"id"`
	SkillName  string `gorm:"size:128;uniqueIndex;not null" json:"skill_name"`
	TargetAddr string `gorm:"size:512;not null" json:"target_addr"`
	// 列名必须显式钉住（不是靠 GORM 的 snake_case 推导）：字段名 TargetVer / LegacyVer 会被
	// 推导成 target_ver / legacy_ver，而 hacks/sql/m4_1_cutover.sql 建的是 target_version /
	// legacy_version。两条建表路径（bootstrap.sh 建表 vs 服务启动 EnsureSchema）必须产出同一张表，
	// 否则「先跑 bootstrap 的环境」里 GORM 按 target_ver 生成 SQL → MySQL 1054 Unknown column，
	// 切流 API 全挂；而 EnsureSchema 有 HasTable 守卫，表已存在时也不会补列，故障只在运行时暴露。
	TargetVer   string     `gorm:"column:target_version;size:32;not null;default:''" json:"target_version"` // 切流目标版本号
	LegacyVer   string     `gorm:"column:legacy_version;size:32;not null;default:''" json:"legacy_version"` // 被切流的旧版本号
	Traffic     int        `gorm:"not null;default:0" json:"traffic"`                                       // 当前档位 0/10/50/100
	Status      string     `gorm:"size:16;not null;default:pending" json:"status"`                          // pending|running|completed|rolled_back
	StageAt     *time.Time `json:"stage_at"`                                                                // 当前档位起始时间（时长护栏）
	CompletedAt *time.Time `json:"completed_at"`                                                            // 全量收尾时刻（下线稳定观察窗起点）
	ParityMin   float64    `gorm:"not null;default:0" json:"parity_min"`                                    // 一致率阈值（0 = 缺省）
	SuccessMin  float64    `gorm:"not null;default:0" json:"success_min"`                                   // 成功率阈值（0 = 缺省）
	MinSamples  int        `gorm:"not null;default:0" json:"min_samples"`                                   // 每档最少样本（0 = 缺省）
	Actor       string     `gorm:"size:64;not null;default:''" json:"actor"`                                // 最近一次操作者
	Note        string     `gorm:"type:text" json:"note"`                                                   // 人读说明/原因
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

func (SkillCutover) TableName() string { return "skill_cutover" }
