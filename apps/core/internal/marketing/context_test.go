package marketing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// scanForbidden 在归一化文本（小写、去空白）里找禁词，返回命中的词。
func scanForbidden(t *testing.T, s ContextSnapshot, terms []string) []string {
	t.Helper()
	text := s.ContentText()
	var hits []string
	for _, term := range terms {
		norm := strings.ToLower(strings.ReplaceAll(term, " ", ""))
		if strings.Contains(text, norm) {
			hits = append(hits, term)
		}
	}
	return hits
}

// TestAssembleContext_CN_NoOverseasTerms 硬要求方向一：
// region=cn 的上下文（含产品一句话）不得出现海外产品话术（crypto 词表）。
func TestAssembleContext_CN_NoOverseasTerms(t *testing.T) {
	for _, product := range []string{ProductNetwork, ProductShop} {
		for _, kind := range []string{KindJudgeLead, KindAnalyzePerformance} {
			s, hash, err := AssembleContext(ContextInput{
				Kind:     kind,
				Region:   RegionCN,
				Product:  product,
				PostText: "求推荐一个能开发票的 AI 网关，公司采购用",
			})
			if err != nil {
				t.Fatalf("AssembleContext(cn/%s/%s) = %v", kind, product, err)
			}
			if hash == "" {
				t.Fatalf("cn/%s/%s: ContextHash 为空", kind, product)
			}
			if hits := scanForbidden(t, s, cnForbiddenTerms); len(hits) > 0 {
				t.Errorf("region=cn 上下文命中海外产品话术 %v（product=%s kind=%s）", hits, product, kind)
			}
		}
	}
}

// TestAssembleContext_Intl_NoDomesticTerms 硬要求方向二：
// region=intl 的上下文（含产品一句话）不得出现国内发票/采购口径。
func TestAssembleContext_Intl_NoDomesticTerms(t *testing.T) {
	for _, product := range []string{ProductNetwork, ProductShop} {
		for _, kind := range []string{KindJudgeLead, KindAnalyzePerformance} {
			s, hash, err := AssembleContext(ContextInput{
				Kind:     kind,
				Region:   RegionIntl,
				Product:  product,
				PostText: "Looking for a cheap LLM gateway with wallet login",
			})
			if err != nil {
				t.Fatalf("AssembleContext(intl/%s/%s) = %v", kind, product, err)
			}
			if hash == "" {
				t.Fatalf("intl/%s/%s: ContextHash 为空", kind, product)
			}
			if hits := scanForbidden(t, s, intlForbiddenTerms); len(hits) > 0 {
				t.Errorf("region=intl 上下文命中国内发票/采购口径 %v（product=%s kind=%s）", hits, product, kind)
			}
		}
	}
}

// TestAssembleContext_CN_RejectsCryptoContamination 组装侧硬拦：
// cn 上下文内容里混入 crypto 话术 → 不产出上下文包（报错）。
func TestAssembleContext_CN_RejectsCryptoContamination(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*ContextInput)
	}{
		{"原帖含挖矿", func(in *ContextInput) { in.PostText = "这个节点挖矿年化 300%" }},
		{"原帖含空投", func(in *ContextInput) { in.PostText = "听说要空投代币" }},
		{"回复含usdt", func(in *ContextInput) { in.Replies = []string{"支持 USDT 结算吗"} }},
		{"作者近条含链上", func(in *ContextInput) { in.AuthorPosts = []string{"链上结算，无 KYC"} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := ContextInput{Kind: KindJudgeLead, Region: RegionCN, Product: ProductNetwork}
			c.mut(&in)
			if _, _, err := AssembleContext(in); err == nil {
				t.Fatalf("region=cn 混入海外产品话术（%s）应被硬拦，got nil", c.name)
			}
		})
	}
}

// TestAssembleContext_Intl_RejectsDomesticContamination 组装侧硬拦：
// intl 上下文内容里混入国内发票/采购口径 → 不产出上下文包（报错）。
func TestAssembleContext_Intl_RejectsDomesticContamination(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*ContextInput)
	}{
		{"原帖含发票", func(in *ContextInput) { in.PostText = "你们能开增值税发票吗" }},
		{"回复含对公", func(in *ContextInput) { in.Replies = []string{"走对公转账可以吗"} }},
		{"作者近条含人民币", func(in *ContextInput) { in.AuthorPosts = []string{"人民币支付宝结算"} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := ContextInput{Kind: KindJudgeLead, Region: RegionIntl, Product: ProductNetwork}
			c.mut(&in)
			if _, _, err := AssembleContext(in); err == nil {
				t.Fatalf("region=intl 混入国内发票/采购口径（%s）应被硬拦，got nil", c.name)
			}
		})
	}
}

// TestAssembleContext_HashStable 验收：同一输入两次组装 hash 稳定。
func TestAssembleContext_HashStable(t *testing.T) {
	in := ContextInput{
		Kind:        KindJudgeLead,
		Region:      RegionCN,
		Product:     ProductNetwork,
		PostText:    "求推荐一个能开发票的 AI 网关，公司采购用",
		AuthorPosts: []string{"我们提供 TEE 隐私计算服务"},
		Replies:     []string{"已回复：可以开票"},
	}
	s1, h1, err := AssembleContext(in)
	if err != nil {
		t.Fatalf("第一次组装: %v", err)
	}
	s2, h2, err := AssembleContext(in)
	if err != nil {
		t.Fatalf("第二次组装: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("同一输入两次组装 hash 不稳定: %s != %s", h1, h2)
	}
	if len(h1) != 64 {
		t.Errorf("ContextHash 长度 = %d, want 64（sha256 hex）", len(h1))
	}
	// 两次 snapshot 内容一致。
	json1, _ := json.Marshal(s1)
	json2, _ := json.Marshal(s2)
	if string(json1) != string(json2) {
		t.Error("同一输入两次组装 snapshot 不一致")
	}
}

// TestAssembleContext_HashIsSnapshotSHA256 验收：ContextHash = snapshot JSON 的 sha256。
func TestAssembleContext_HashIsSnapshotSHA256(t *testing.T) {
	s, hash, err := AssembleContext(ContextInput{
		Kind: KindJudgeLead, Region: RegionIntl, Product: ProductNetwork,
		PostText: "anyone using a crypto-native compute network?",
	})
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	sum := sha256.Sum256(raw)
	want := hex.EncodeToString(sum[:])
	if hash != want {
		t.Fatalf("ContextHash = %s, want sha256(snapshot) = %s", hash, want)
	}
}

// TestAssembleContext_HashChangesWithInput 不同输入 → 不同 hash（hash 有区分度）。
func TestAssembleContext_HashChangesWithInput(t *testing.T) {
	base := ContextInput{Kind: KindJudgeLead, Region: RegionCN, Product: ProductNetwork, PostText: "求推荐 AI 网关"}
	_, h1, err := AssembleContext(base)
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}
	base.PostText = "求推荐 AI 网关（预算 1 万）"
	_, h2, err := AssembleContext(base)
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}
	if h1 == h2 {
		t.Fatal("不同输入产出了相同 hash")
	}
}

// TestAssembleContext_SnapshotJSON 验收：ContextSnapshot 是合法 JSON（落库可回放），
// 且能反序列化回同构结构。
func TestAssembleContext_SnapshotJSON(t *testing.T) {
	s, _, err := AssembleContext(ContextInput{
		Kind: KindJudgeLead, Region: RegionCN, Product: ProductNetwork,
		PostText: "求推荐 AI 网关",
	})
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("snapshot 序列化失败: %v", err)
	}
	var back ContextSnapshot
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("snapshot 反序列化失败: %v", err)
	}
	if back.Kind != s.Kind || back.Region != s.Region || back.Product != s.Product {
		t.Errorf("snapshot 往返后元数据不一致: %+v != %+v", back, s)
	}
	if back.Sections["post"] != "求推荐 AI 网关" {
		t.Errorf("snapshot 往返后 sections 丢失: %+v", back.Sections)
	}
}

// TestAssembleContext_MustGiveNotGive 规格 §5：每个 kind 都有「必须给」与「明确不给」，
// 且 cn / intl 的 judge_lead 依据不同（国内给国内产品一句话、禁 crypto；
// 海外给海外产品一句话、不给国内产品信息）。
func TestAssembleContext_MustGiveNotGive(t *testing.T) {
	kinds := []string{
		KindJudgeLead, KindCommentSlot, KindDraftReply, KindCritiqueReply,
		KindAnswerOrEscalate, KindComplianceCheck, KindStorefrontCopy, KindAnalyzePerformance,
	}
	for _, kind := range kinds {
		for _, region := range []string{RegionCN, RegionIntl} {
			s, _, err := AssembleContext(ContextInput{Kind: kind, Region: region, Product: ProductNetwork})
			if err != nil {
				t.Fatalf("AssembleContext(%s/%s): %v", kind, region, err)
			}
			if len(s.MustGive) == 0 {
				t.Errorf("kind=%s region=%s: MustGive 为空（规格 §5 必须给）", kind, region)
			}
			if len(s.NotGive) == 0 {
				t.Errorf("kind=%s region=%s: NotGive 为空（规格 §5 明确不给）", kind, region)
			}
		}
	}

	cn, _, err := AssembleContext(ContextInput{Kind: KindJudgeLead, Region: RegionCN, Product: ProductNetwork})
	if err != nil {
		t.Fatalf("AssembleContext(cn): %v", err)
	}
	intl, _, err := AssembleContext(ContextInput{Kind: KindJudgeLead, Region: RegionIntl, Product: ProductNetwork})
	if err != nil {
		t.Fatalf("AssembleContext(intl): %v", err)
	}
	if !strings.Contains(strings.Join(cn.MustGive, " "), "国内产品一句话") {
		t.Errorf("cn judge_lead MustGive 应含国内产品一句话: %v", cn.MustGive)
	}
	if !strings.Contains(strings.Join(cn.NotGive, " "), "crypto") {
		t.Errorf("cn judge_lead NotGive 应明确禁 crypto: %v", cn.NotGive)
	}
	if !strings.Contains(strings.Join(intl.MustGive, " "), "海外产品一句话") {
		t.Errorf("intl judge_lead MustGive 应含海外产品一句话: %v", intl.MustGive)
	}
	if !strings.Contains(strings.Join(intl.NotGive, " "), "国内产品信息") {
		t.Errorf("intl judge_lead NotGive 应明确不给国内产品信息: %v", intl.NotGive)
	}
}

// TestAssembleContext_CommentSlot_NoProductInfo 规格 §5 J18：
// 判「场地」不给我们的产品信息（连 product_brief 都不放）。
func TestAssembleContext_CommentSlot_NoProductInfo(t *testing.T) {
	s, _, err := AssembleContext(ContextInput{
		Kind: KindCommentSlot, Region: RegionCN, Product: ProductNetwork,
		PostText: "热帖", Replies: []string{"回复1"}, OPStyle: "楼主风格", CommentRules: "平台规则",
	})
	if err != nil {
		t.Fatalf("AssembleContext(comment_slot): %v", err)
	}
	if _, ok := s.Sections["product_brief"]; ok {
		t.Error("comment_slot 上下文不得包含产品信息（product_brief）")
	}
	for _, key := range []string{"post", "replies", "op_style", "comment_rules"} {
		if s.Sections[key] == "" {
			t.Errorf("comment_slot 缺少必须给段落 %q", key)
		}
	}
}

// TestAssembleContext_AnswerRAGSlot 边界：J10 的 RAG Top-5 属 A4 后续卡（不接 Qdrant），
// 本卡只留槽位：段落恒在、当前为空数组。
func TestAssembleContext_AnswerRAGSlot(t *testing.T) {
	s, _, err := AssembleContext(ContextInput{
		Kind: KindAnswerOrEscalate, Region: RegionCN, Product: ProductNetwork,
		Question: "怎么开发票？",
	})
	if err != nil {
		t.Fatalf("AssembleContext(answer_or_escalate): %v", err)
	}
	if s.Sections["rag_top5"] != "[]" {
		t.Errorf("RAG Top-5 槽位应为空数组，got %q", s.Sections["rag_top5"])
	}
	if s.Sections["question"] == "" {
		t.Error("answer_or_escalate 缺少问题段落")
	}
}

// TestAssembleContext_InvalidEnum 非法 kind / region / product → 报错，不产出上下文包。
func TestAssembleContext_InvalidEnum(t *testing.T) {
	cases := []struct {
		name string
		in   ContextInput
	}{
		{"非法kind", ContextInput{Kind: "lead", Region: RegionCN, Product: ProductNetwork}},
		{"空kind", ContextInput{Kind: "", Region: RegionCN, Product: ProductNetwork}},
		{"非法region", ContextInput{Kind: KindJudgeLead, Region: "us", Product: ProductNetwork}},
		{"空region", ContextInput{Kind: KindJudgeLead, Region: "", Product: ProductNetwork}},
		{"非法product", ContextInput{Kind: KindJudgeLead, Region: RegionCN, Product: "minishop"}},
		{"空product", ContextInput{Kind: KindJudgeLead, Region: RegionCN, Product: ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := AssembleContext(c.in); err == nil {
				t.Fatalf("%s: 应报错，got nil", c.name)
			}
		})
	}
}

// TestProductBrief_Purity 产品一句话自身必须干净：
// cn 版不含海外产品话术；intl 版不含国内发票/采购口径。
func TestProductBrief_Purity(t *testing.T) {
	for _, product := range []string{ProductNetwork, ProductShop} {
		cn, err := productBrief(RegionCN, product)
		if err != nil {
			t.Fatalf("productBrief(cn, %s): %v", product, err)
		}
		cnNorm := strings.ToLower(strings.ReplaceAll(cn, " ", ""))
		for _, term := range cnForbiddenTerms {
			if strings.Contains(cnNorm, strings.ToLower(strings.ReplaceAll(term, " ", ""))) {
				t.Errorf("cn 产品一句话（%s）命中海外产品话术 %q: %s", product, term, cn)
			}
		}

		intl, err := productBrief(RegionIntl, product)
		if err != nil {
			t.Fatalf("productBrief(intl, %s): %v", product, err)
		}
		intlNorm := strings.ToLower(strings.ReplaceAll(intl, " ", ""))
		for _, term := range intlForbiddenTerms {
			if strings.Contains(intlNorm, strings.ToLower(strings.ReplaceAll(term, " ", ""))) {
				t.Errorf("intl 产品一句话（%s）命中国内发票/采购口径 %q: %s", product, term, intl)
			}
		}
	}
}

// TestCheckRegionPurity_InvalidRegion 纯度检查拒绝非法 region。
func TestCheckRegionPurity_InvalidRegion(t *testing.T) {
	if err := CheckRegionPurity(ContextSnapshot{Region: "us"}); err == nil {
		t.Fatal("非法 region 应报错")
	}
}
