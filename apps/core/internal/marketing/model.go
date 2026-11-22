// Package marketing 是营销域模型（docs/marketing/plan/P2-判断服务地基.md MK-10）：
// 判断服务（mk-judge）的地基，定义 region / product / kind 枚举与
// Judgment / Lead / Action 三个核心结构体，并提供合法性校验。
//
// 本包只做域模型与校验：不碰数据库、不调 LLM、不改 internal/db。
// 表建模（mk_judgments / mk_leads / mk_actions）由后续卡（MK-11 / MK-19）
// 在本包内以 ensureDDL + SQL 双轨落地，不塞进 internal/db 的 AutoMigrate。
package marketing

import (
	"fmt"
	"time"
)

// region 枚举（★ 所有判断/线索/动作都必须带，规格 §2 / §7）。
const (
	RegionIntl = "intl" // 海外
	RegionCN   = "cn"   // 国内
)

// product 枚举（★ 双产品线，规格 §7）。
const (
	ProductNetwork = "network" // 网络产品（TEE 中转 / 节点）
	ProductShop    = "shop"    // 算力店铺（minishop 店主招募 / 分享文案）
)

// kind 枚举（判断点类型，J1–J18 共用一套校验与落库，规格 §2）。
const (
	KindJudgeLead          = "judge_lead"          // J1–J4/J18 线索判断（A1）
	KindCommentSlot        = "comment_slot"        // J18 评论区可插入性（选场地）
	KindDraftReply         = "draft_reply"         // J5–J7 草稿生成（A2/A3/A5）
	KindCritiqueReply      = "critique_reply"      // 自审（spam_risk / hard_sell / overclaim / region_fit / platform_rule_ok）
	KindAnswerOrEscalate   = "answer_or_escalate"  // J8–J10 答疑（A4，含引用核对）
	KindComplianceCheck    = "compliance_check"    // J13 合规（规则包）
	KindStorefrontCopy     = "storefront_copy"     // A6 店主分享文案
	KindAnalyzePerformance = "analyze_performance" // 归因 / 效果分析（分 region）
)

// ---- action kind（mk_actions.kind，规格 §7）----
// comment 含回复 / 答疑 / 截流，不是单独的第三种动作（规格 §1.5）。
const (
	ActionKindPublish = "publish" // 自有账号发内容
	ActionKindComment = "comment" // 在他人内容下评论（含回复/答疑/截流）
	ActionKindDM      = "dm"      // 私聊
	ActionKindRecruit = "recruit" // 店主招募（A5）
)

// verdict 枚举（判断结论，规格 §2.2 self_critique.verdict）。
const (
	VerdictPass   = "pass"
	VerdictRevise = "revise"
	VerdictReject = "reject"
)

// lead status 枚举（mk_leads.status，MK-19）。
const (
	LeadStatusNew       = "new"       // 新线索（默认）
	LeadStatusReviewed  = "reviewed"  // 人工复核过
	LeadStatusConverted = "converted" // 已转动作
	LeadStatusDiscarded = "discarded" // 丢弃（负样本）
)

// action status 枚举（mk_actions.status，规格 §6.2：draft→reviewing 强制人工）。
const (
	ActionStatusDraft     = "draft"     // 草稿（默认）
	ActionStatusReviewing = "reviewing" // 人工审核中
	ActionStatusApproved  = "approved"  // 审核通过
	ActionStatusRejected  = "rejected"  // 审核驳回（进负样本）
	ActionStatusSent      = "sent"      // 已发送（M6）
	ActionStatusFailed    = "failed"    // 发送失败（M6）
)

// ValidLeadStatus 判断 mk_leads.status 是否合法。
func ValidLeadStatus(s string) bool {
	switch s {
	case LeadStatusNew, LeadStatusReviewed, LeadStatusConverted, LeadStatusDiscarded:
		return true
	}
	return false
}

// ValidActionStatus 判断 mk_actions.status 是否合法。
func ValidActionStatus(s string) bool {
	switch s {
	case ActionStatusDraft, ActionStatusReviewing, ActionStatusApproved,
		ActionStatusRejected, ActionStatusSent, ActionStatusFailed:
		return true
	}
	return false
}

// ---- 合法性校验 ----

// ValidRegion 判断 region 是否合法（intl | cn）。
func ValidRegion(s string) bool {
	return s == RegionIntl || s == RegionCN
}

// ValidProduct 判断 product 是否合法（network | shop）。
func ValidProduct(s string) bool {
	return s == ProductNetwork || s == ProductShop
}

// ValidKind 判断判断点 kind 是否合法（8 种，见上）。
func ValidKind(s string) bool {
	switch s {
	case KindJudgeLead, KindCommentSlot, KindDraftReply, KindCritiqueReply,
		KindAnswerOrEscalate, KindComplianceCheck, KindStorefrontCopy,
		KindAnalyzePerformance:
		return true
	}
	return false
}

// ValidActionKind 判断 mk_actions.kind 是否合法（publish | comment | dm | recruit）。
func ValidActionKind(s string) bool {
	switch s {
	case ActionKindPublish, ActionKindComment, ActionKindDM, ActionKindRecruit:
		return true
	}
	return false
}

// ---- 核心结构体 ----

// Judgment 是一条 LLM 判断记录（mk_judgments 表，MK-11 落库）。
// 可回放硬要求：ContextHash + ContextSnapshot 同表，
// 任一条历史判断都能取出「当时模型看到了什么」+ 模型原始输出。
//
// GORM 标签与 TableName 由 MK-11 的 store 复用（一卡一组文件：
// store_judgment.go 只保留 ensureDDL + Store CRUD，结构体收敛在本文件）。
type Judgment struct {
	ID              uint      `gorm:"primaryKey" json:"id"`
	Region          string    `gorm:"size:16;not null" json:"region"`                  // intl | cn
	Product         string    `gorm:"size:16;not null;default:network" json:"product"` // network | shop
	Kind            string    `gorm:"size:32;not null" json:"kind"`                    // judge_lead | comment_slot | draft_reply | critique_reply | answer_or_escalate | compliance_check | storefront_copy | analyze_performance
	Source          string    `gorm:"size:32;not null" json:"source"`                  // 判断输入来源（github|reddit|x|zhihu|juejin|v2ex|telegram|discord|manual）
	SourceRef       string    `gorm:"size:255" json:"source_ref"`                      // 来源引用（issue 号 / 帖子 URL / 线索 id 等）
	Model           string    `gorm:"size:64" json:"model"`                            // 模型名（回放时确认「哪个模型判的」）
	PromptVersion   string    `gorm:"size:32" json:"prompt_version"`                   // prompt 版本（换 prompt 必跑金标回归）
	ContextHash     string    `gorm:"size:64;index" json:"context_hash"`               // 上下文包 sha256（回放用，建索引）
	ContextSnapshot string    `gorm:"type:longtext" json:"context_snapshot"`           // 上下文包完整快照（JSON，落库可回放）
	RawOutput       string    `gorm:"type:longtext" json:"raw_output"`                 // 模型原始输出（未解析）
	Verdict         string    `gorm:"size:32" json:"verdict"`                          // pass | revise | reject
	Reason          string    `gorm:"type:text" json:"reason"`                         // 一句话判断理由
	CreatedAt       time.Time `json:"created_at"`
}

// TableName 固定表名（不依赖 GORM 命名策略）。
func (Judgment) TableName() string { return "mk_judgments" }

// Validate 拒绝非法 region / product / kind（MK-10 验收：三类非法枚举）。
func (j *Judgment) Validate() error {
	if !ValidRegion(j.Region) {
		return fmt.Errorf("marketing: 非法 region %q（合法值：intl | cn）", j.Region)
	}
	if !ValidProduct(j.Product) {
		return fmt.Errorf("marketing: 非法 product %q（合法值：network | shop）", j.Product)
	}
	if !ValidKind(j.Kind) {
		return fmt.Errorf("marketing: 非法 kind %q（合法值：judge_lead | comment_slot | draft_reply | critique_reply | answer_or_escalate | compliance_check | storefront_copy | analyze_performance）", j.Kind)
	}
	return nil
}

// Lead 是一条营销线索（mk_leads 表，MK-19 落库）。
type Lead struct {
	ID         int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	Region     string    `gorm:"size:16;not null" json:"region"`                  // intl | cn
	Product    string    `gorm:"size:16;not null;default:network" json:"product"` // network | shop
	Source     string    `gorm:"size:32;not null" json:"source"`                  // github|reddit|x|zhihu|juejin|v2ex|telegram|discord
	SourceRef  string    `gorm:"size:255;not null" json:"source_ref"`
	URL        string    `gorm:"size:512;not null;default:''" json:"url"`
	Author     string    `gorm:"size:128;not null;default:''" json:"author"`
	Excerpt    string    `gorm:"type:text" json:"excerpt"`
	IsRealNeed bool      `gorm:"not null;default:false" json:"is_real_need"`
	NeedType   string    `gorm:"size:32;not null;default:noise" json:"need_type"`   // demand|supply|complaint|question|noise（国内无 supply）
	Intervene  string    `gorm:"size:16;not null;default:monitor" json:"intervene"` // yes|no|monitor
	Pain       string    `gorm:"size:32;not null;default:''" json:"pain"`           // 按 region 分支的枚举（规格 §2.1 J3）
	Score      int       `gorm:"not null;default:0" json:"score"`
	Confidence float64   `gorm:"type:decimal(4,3);not null;default:0" json:"confidence"`
	JudgmentID int64     `gorm:"not null;default:0" json:"judgment_id"`
	IdemKey    string    `gorm:"size:128;not null;uniqueIndex:uk_idem" json:"idem_key"` // 幂等键（uk_idem）
	Status     string    `gorm:"size:32;not null;default:new" json:"status"`            // new | reviewed | converted | discarded
	FoundAt    time.Time `gorm:"not null" json:"found_at"`
}

// Validate 拒绝非法 region / product（线索无 kind 维度）。
func (l *Lead) Validate() error {
	if !ValidRegion(l.Region) {
		return fmt.Errorf("marketing: 非法 region %q（合法值：intl | cn）", l.Region)
	}
	if !ValidProduct(l.Product) {
		return fmt.Errorf("marketing: 非法 product %q（合法值：network | shop）", l.Product)
	}
	return nil
}

// Action 是一个对外动作（mk_actions 表，MK-19 落库）。
// 本阶段（P2）只写不读；发送 / 限流属 M6。
type Action struct {
	ID                   int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	Region               string    `gorm:"size:16;not null" json:"region"`                  // intl | cn
	Product              string    `gorm:"size:16;not null;default:network" json:"product"` // network | shop
	LeadID               int64     `gorm:"not null;default:0" json:"lead_id"`
	Agent                string    `gorm:"size:8;not null" json:"agent"`    // a1..a6
	Kind                 string    `gorm:"size:16;not null" json:"kind"`    // publish | comment | dm | recruit
	Channel              string    `gorm:"size:48;not null" json:"channel"` // github|x|telegram|wechat-mp|manual-zhihu...
	Account              string    `gorm:"size:64;not null" json:"account"`
	TargetURL            string    `gorm:"size:512;not null;default:''" json:"target_url"`
	Body                 string    `gorm:"type:longtext;not null" json:"body"`
	DraftJudgmentID      int64     `gorm:"not null;default:0" json:"draft_judgment_id"`
	CritiqueJudgmentID   int64     `gorm:"not null;default:0" json:"critique_judgment_id"`
	ComplianceJudgmentID int64     `gorm:"not null;default:0" json:"compliance_judgment_id"`
	Status               string    `gorm:"size:32;not null;default:draft" json:"status"`          // draft | reviewing | approved | rejected | sent | failed
	IdemKey              string    `gorm:"size:128;not null;uniqueIndex:uk_idem" json:"idem_key"` // 幂等键（uk_idem）
	CreatedAt            time.Time `gorm:"not null;default:CURRENT_TIMESTAMP" json:"created_at"`
}

// Validate 拒绝非法 region / product / kind（action kind 四值）。
func (a *Action) Validate() error {
	if !ValidRegion(a.Region) {
		return fmt.Errorf("marketing: 非法 region %q（合法值：intl | cn）", a.Region)
	}
	if !ValidProduct(a.Product) {
		return fmt.Errorf("marketing: 非法 product %q（合法值：network | shop）", a.Product)
	}
	if !ValidActionKind(a.Kind) {
		return fmt.Errorf("marketing: 非法 action kind %q（合法值：publish | comment | dm | recruit）", a.Kind)
	}
	return nil
}
