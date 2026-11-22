// Package marketing 是营销判断服务（docs/marketing/implementation-spec.md）：
// 本文件是 MK-13（docs/marketing/plan/P2-判断服务地基.md）：上下文包组装 + snapshot/hash。
//
// 上下文包回答「当时模型看到了什么」：按 (kind, region, product) 组装规格 §5 的
// 「必须给」与「明确不给」，产出 ContextSnapshot（JSON，落 mk_judgments.context_snapshot
// 可回放）+ ContextHash（snapshot 的 sha256，落 context_hash 建索引）。
//
// 硬要求（上下文串味是最常见的翻车方式）：
//   - region=cn 的上下文不得出现海外产品话术（crypto 词表）；
//   - region=intl 的上下文不得出现国内发票/采购口径。
//
// 组装时服务端硬拦：命中即报错，不产出上下文包（不靠模型自觉）。
//
// 边界：不接 Qdrant（J10 的 RAG Top-5 属 A4，后续卡；RAGTop5 字段预留，当前为空）。
// 一卡一组文件（P2 串行红线）：本卡只动 context.go / context_test.go；
// MK-22 负样本回流只在本文件加注入函数，不改已有签名。
package marketing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ---- 上下文包输入 / 输出 ----

// RAGHit 是 RAG Top-5 的一条命中（J10，带段落来源）。
// A4 后续卡才接 Qdrant；本卡字段预留，当前恒为空。
type RAGHit struct {
	Source string `json:"source"` // 段落来源（文档 / 章节）
	Text   string `json:"text"`   // 段落内容
}

// ContextInput 是上下文包组装的原料（内容字段均可选，空 = 未提供）。
// 字段按规格 §5 各判断点的「必须给」分组。
type ContextInput struct {
	Kind    string // 判断点 kind（见 Kind* 常量）
	Region  string // intl | cn
	Product string // network | shop

	// J1–J4 线索判断（judge_lead）
	PostText    string   // 原帖全文
	AuthorPosts []string // 作者近 5 条
	Replies     []string // 已有回复

	// J18 评论区可插入性（comment_slot）
	OPStyle      string // 楼主历史风格
	CommentRules string // 该平台评论区规则

	// J5–J7 草稿生成（draft_reply）
	Persona         string   // 人群画像（按 region 取 09/10）
	SuccessSamples  []string // 历史成功话术（3 条）
	RejectedSamples []string // 历史被驳回话术（3 条）
	PlatformRules   string   // 平台自推规则

	// J8–J10 答疑（answer_or_escalate）
	Question      string   // 问题
	RAGTop5       []RAGHit // RAG Top-5（带段落来源；A4 后续卡，当前为空）
	Dialogue      string   // 对话上下文（J16）
	BusinessTerms string   // 国内商务口径（发票/合同/对公流程，J16，仅 cn 有意义）

	// J13 合规（compliance_check）/ 自审（critique_reply）
	DraftText      string // 待发文本 / 待自审文本
	RulePack       string // 该 region 的规则包
	RecentRejected string // 近期被驳回样本

	// J17 店主分享文案（storefront_copy）
	StoreInfo        string // 店主店铺信息
	StoreProfile     string // 其私域画像
	ComplianceBounds string // 合规边界

	// 归因 / 效果分析（analyze_performance）
	Performance string // 效果数据
}

// ContextSnapshot 是上下文包快照（落 mk_judgments.context_snapshot，可回放）。
// 「当时模型看到了什么」= Sections；MustGive / NotGive 是规格 §5 的组装依据
// （回放审计用，说明「该给什么 / 明确不给什么」）。
type ContextSnapshot struct {
	Kind     string            `json:"kind"`
	Region   string            `json:"region"`
	Product  string            `json:"product"`
	MustGive []string          `json:"must_give"` // 规格 §5「必须给」
	NotGive  []string          `json:"not_give"`  // 规格 §5「明确不给」
	Sections map[string]string `json:"sections"`  // 实际上下文内容（key = 段落名）
}

// SourceText 从 snapshot sections 提取「原文」（MK-12 规则3 quote 子串匹配用）。
// 与 sourceText(ContextInput) 对应：按 kind 取组装时写入的原文 section。
// 回放（MK-16 /replay）只有 snapshot、没有 ContextInput 原料，靠它还原原文做校验。
func (s ContextSnapshot) SourceText() string {
	switch s.Kind {
	case KindJudgeLead, KindCommentSlot, KindDraftReply:
		return s.Sections["post"]
	case KindAnswerOrEscalate:
		return s.Sections["question"]
	case KindCritiqueReply, KindComplianceCheck:
		return s.Sections["draft"]
	case KindStorefrontCopy:
		return s.Sections["store_info"]
	case KindAnalyzePerformance:
		return s.Sections["performance"]
	}
	return ""
}

// ContentText 返回归一化后的上下文内容文本（sections 按 key 排序拼接、
// 小写、去空白），供词表扫描（纯度检查 / MK-12 校验复用）。
// 只含 Sections（模型真正看到的内容），不含 MustGive/NotGive 元数据。
func (s ContextSnapshot) ContentText() string {
	keys := make([]string, 0, len(s.Sections))
	for k := range s.Sections {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(s.Sections[k])
		b.WriteByte('\n')
	}
	text := strings.ToLower(b.String())
	return strings.ReplaceAll(text, " ", "")
}

// ---- 词表（内置常量最小集，规则包文件化留给后续卡）----

// cnForbiddenTerms 是海外产品话术词表：region=cn 的上下文内容不得命中其中任何词。
var cnForbiddenTerms = []string{
	"挖矿", "矿工", "空投", "年化", "虚拟币", "代币", "交易所", "稳定币",
	"钱包登录", "链上", "多链", "无kyc", "usdt",
	"crypto", "airdrop", "mining", "wallet", "on-chain", "defi", "nft",
}

// intlForbiddenTerms 是国内发票/采购口径词表：region=intl 的上下文内容不得命中其中任何词。
var intlForbiddenTerms = []string{
	"发票", "开票", "对公", "采购", "人民币", "支付宝", "微信",
	"增值税", "专票", "普票",
}

// CheckRegionPurity 执行双向词表互斥硬要求（只扫 Sections，即模型真正看到的内容）：
//   - region=cn：不得命中任何海外产品话术（crypto）词；
//   - region=intl：不得命中任何国内发票/采购口径词。
//
// 命中即返回错误（组装侧硬拦，不产出上下文包）。
func CheckRegionPurity(s ContextSnapshot) error {
	var terms []string
	switch s.Region {
	case RegionCN:
		terms = cnForbiddenTerms
	case RegionIntl:
		terms = intlForbiddenTerms
	default:
		return fmt.Errorf("marketing: 上下文包纯度检查：非法 region %q（合法值：intl | cn）", s.Region)
	}
	text := s.ContentText()
	for _, term := range terms {
		norm := strings.ToLower(strings.ReplaceAll(term, " ", ""))
		if strings.Contains(text, norm) {
			return fmt.Errorf("marketing: 上下文包串味：region=%s 命中禁词 %q", s.Region, term)
		}
	}
	return nil
}

// ---- 产品一句话（规格 §5：J1–J4 必须给「产品一句话」）----
// 硬要求：cn 版不得含海外产品话术；intl 版不得含国内发票/采购口径。

func productBrief(region, product string) (string, error) {
	switch {
	case region == RegionCN && product == ProductNetwork:
		return "AI 网关 + TEE 隐私计算，人民币（支付宝/微信），可开票，数据不出域", nil
	case region == RegionCN && product == ProductShop:
		return "算力店铺（minishop）：店主上架算力资源，人民币结算，可开票", nil
	case region == RegionIntl && product == ProductNetwork:
		return "Crypto-native 算力网络：钱包登录、链上结算、无 KYC、多链、按量付费", nil
	case region == RegionIntl && product == ProductShop:
		return "算力店铺（minishop）：店主上架算力资源，分享赚佣金", nil
	}
	return "", fmt.Errorf("marketing: 无产品一句话：region=%s product=%s", region, product)
}

// ---- 规格 §5 组装依据（「必须给」/「明确不给」）----

func contextSpec(kind, region string) (mustGive, notGive []string) {
	switch kind {
	case KindJudgeLead:
		if region == RegionCN {
			mustGive = []string{"原帖全文", "作者近 5 条", "已有回复", "国内产品一句话（AI 网关 + TEE + 人民币可开票）"}
			notGive = []string{"任何 crypto 相关词汇（海外产品话术，会串味）"}
		} else {
			mustGive = []string{"原帖全文", "作者近 5 条", "已有回复", "海外产品一句话"}
			notGive = []string{"国内产品信息（会串味）"}
		}
	case KindCommentSlot:
		mustGive = []string{"目标帖子全文", "已有回复列表", "楼主历史风格", "该平台评论区规则"}
		notGive = []string{"我们的产品信息（判「场地」不需要，给了反而会诱导硬推）"}
	case KindDraftReply:
		mustGive = []string{"原帖", "人群画像（按 region 取 09/10）", "历史成功话术 3 条", "历史被驳回话术 3 条", "平台自推规则"}
		notGive = []string{"全部产品文档（太长）"}
	case KindCritiqueReply:
		mustGive = []string{"待自审文本", "该 region 的规则包"}
		notGive = []string{"全部产品文档（太长）"}
	case KindAnswerOrEscalate:
		mustGive = []string{"问题", "RAG Top-5（带段落来源）", "没覆盖就说没覆盖"}
		notGive = []string{"营销话术（答疑场景不推销）"}
	case KindComplianceCheck:
		mustGive = []string{"待发文本", "该 region 的规则包", "近期被驳回样本"}
		notGive = []string{"整篇产品文档"}
	case KindStorefrontCopy:
		mustGive = []string{"店主店铺信息", "其私域画像", "合规边界"}
		notGive = []string{"我们的官方话术（店主要自己的语气）"}
	case KindAnalyzePerformance:
		mustGive = []string{"效果数据", "产品一句话"}
		notGive = []string{"全部产品文档（太长）"}
	}
	return mustGive, notGive
}

// ---- 组装 ----

// AssembleContext 按 (kind, region, product) 组装上下文包（规格 §5「必须给」/「明确不给」），
// 产出 ContextSnapshot（JSON，落库可回放）+ ContextHash（snapshot 的 sha256）。
//
// 返回错误的情形：
//   - kind / region / product 非法枚举；
//   - 双向词表互斥硬要求被违反（cn 命中海外产品话术 / intl 命中国内发票采购口径）——
//     上下文包不产出（服务端硬拦，不靠模型自觉）。
func AssembleContext(in ContextInput) (ContextSnapshot, string, error) {
	if !ValidKind(in.Kind) {
		return ContextSnapshot{}, "", fmt.Errorf("marketing: 非法 kind %q", in.Kind)
	}
	if !ValidRegion(in.Region) {
		return ContextSnapshot{}, "", fmt.Errorf("marketing: 非法 region %q（合法值：intl | cn）", in.Region)
	}
	if !ValidProduct(in.Product) {
		return ContextSnapshot{}, "", fmt.Errorf("marketing: 非法 product %q（合法值：network | shop）", in.Product)
	}

	mustGive, notGive := contextSpec(in.Kind, in.Region)
	brief, err := productBrief(in.Region, in.Product)
	if err != nil {
		return ContextSnapshot{}, "", err
	}

	sections := map[string]string{}
	add := func(key, val string) {
		if strings.TrimSpace(val) != "" {
			sections[key] = val
		}
	}
	addList := func(key string, vals []string) {
		if len(vals) > 0 {
			sections[key] = strings.Join(vals, "\n---\n")
		}
	}

	switch in.Kind {
	case KindJudgeLead:
		add("post", in.PostText)
		addList("author_recent", in.AuthorPosts)
		addList("replies", in.Replies)
		sections["product_brief"] = brief
	case KindCommentSlot:
		// 明确不给：我们的产品信息（连 product_brief 都不放）。
		add("post", in.PostText)
		addList("replies", in.Replies)
		add("op_style", in.OPStyle)
		add("comment_rules", in.CommentRules)
	case KindDraftReply:
		add("post", in.PostText)
		add("persona", in.Persona)
		addList("success_samples", in.SuccessSamples)
		addList("rejected_samples", in.RejectedSamples)
		add("platform_rules", in.PlatformRules)
	case KindCritiqueReply:
		add("draft", in.DraftText)
		add("rule_pack", in.RulePack)
	case KindAnswerOrEscalate:
		add("question", in.Question)
		// RAG Top-5 属 A4 后续卡（不接 Qdrant）：段落恒在、当前为空，回放可见该槽位。
		sections["rag_top5"] = marshalRAGHits(in.RAGTop5)
		add("dialogue", in.Dialogue)
		add("business_terms", in.BusinessTerms)
	case KindComplianceCheck:
		add("draft", in.DraftText)
		add("rule_pack", in.RulePack)
		add("recent_rejected", in.RecentRejected)
	case KindStorefrontCopy:
		add("store_info", in.StoreInfo)
		add("store_profile", in.StoreProfile)
		add("compliance_bounds", in.ComplianceBounds)
	case KindAnalyzePerformance:
		add("performance", in.Performance)
		sections["product_brief"] = brief
	}

	s := ContextSnapshot{
		Kind:     in.Kind,
		Region:   in.Region,
		Product:  in.Product,
		MustGive: mustGive,
		NotGive:  notGive,
		Sections: sections,
	}
	if err := CheckRegionPurity(s); err != nil {
		return ContextSnapshot{}, "", err
	}
	hash, err := HashSnapshot(s)
	if err != nil {
		return ContextSnapshot{}, "", err
	}
	return s, hash, nil
}

// marshalRAGHits 序列化 RAG Top-5（nil → "[]"，保证 snapshot 确定性）。
func marshalRAGHits(hits []RAGHit) string {
	if hits == nil {
		return "[]"
	}
	raw, err := json.Marshal(hits)
	if err != nil {
		return "[]"
	}
	return string(raw)
}

// HashSnapshot 产出 ContextHash（snapshot JSON 的 sha256，hex 编码）。
// json.Marshal 对同一输入是确定性的（结构体字段顺序固定、map key 排序），
// 因此同一输入两次组装 hash 稳定。
func HashSnapshot(s ContextSnapshot) (string, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("marketing: 序列化上下文包快照: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
