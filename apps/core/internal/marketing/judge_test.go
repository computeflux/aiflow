// judge_test.go — MK-14 judge 主流程验收测试（docs/marketing/plan/P2-判断服务地基.md MK-14）。
//
// 验收（mock 模型输出跑通三条路径）：
//  1. 通过：合法 J1 输出 → verdict=pass，落库（snapshot/hash/raw 齐全，可回放）；
//  2. 判废：校验规则命中 → verdict=reject，**同样落库**（规格 §2.1 护栏：
//     intervene=no 也要落库，判断结论不是落库条件）；
//  3. 解析失败重试一次：首次输出非法 JSON → 回灌解析错误重试一次 → 合法则通过；
//     重试仍非法 → 判废（rule1）落库。
//
// LLM 以 LLMClient 接口注入（mockLLM）；落库用 sqlite 内存库（gorm 驱动已在 go.mod）。
package marketing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// ---- 测试夹具 ----

// judgeSource 是 J1 判断的原文（帖子正文），evidence.quote 必须能在其中子串匹配。
const judgeSource = "我们的 TEE 中转节点最近延迟很高，请求经常超时，" +
	"有没有人知道怎么解决？另外 KYC 审核要等一周，太慢了。"

// validJ1Out 是一条合法的 J1（judge_lead）输出（intl）。
const validJ1Out = `{
  "is_real_need": true,
  "need_type": "demand",
  "intervene": "yes",
  "pain": "latency",
  "freshness": "fresh",
  "score": 80,
  "confidence": 0.9,
  "evidence": [{"quote": "延迟很高，请求经常超时", "why": "明确性能诉求"}],
  "reason": "真实需求，可切入"
}`

// validJ1NoOut 是 intervene=no 的合法 J1 输出（规格 §2.1 护栏：判废/不介入也要落库）。
const validJ1NoOut = `{
  "is_real_need": false,
  "need_type": "noise",
  "intervene": "no",
  "pain": "cost",
  "freshness": "stale",
  "score": 10,
  "confidence": 0.7,
  "evidence": [{"quote": "有没有人知道怎么解决", "why": "已被他人解决，再回是刷存在感"}],
  "reason": "3 天前的求助已被解决，不介入"
}`

// fabricatedJ1Out 是 quote 编造的 J1 输出（规则3 命中 → 判废）。
const fabricatedJ1Out = `{
  "is_real_need": true,
  "need_type": "demand",
  "intervene": "yes",
  "pain": "latency",
  "freshness": "fresh",
  "score": 80,
  "confidence": 0.9,
  "evidence": [{"quote": "原文里根本没有这句话", "why": "编造的引用"}],
  "reason": "真实需求"
}`

// unknownEnumJ1Out 是未知枚举的 J1 输出（规则6 命中 → 判废）。
const unknownEnumJ1Out = `{
  "is_real_need": true,
  "need_type": "hype",
  "intervene": "yes",
  "pain": "latency",
  "freshness": "fresh",
  "score": 80,
  "confidence": 0.9,
  "evidence": [{"quote": "延迟很高", "why": "性能诉求"}],
  "reason": "真实需求"
}`

// ---- mock LLM ----

// mockLLM 是 LLMClient 的测试实现：按调用次序返回预设输出（用尽后重复最后一条）。
type mockLLM struct {
	responses []string
	errs      []error // 与 responses 对齐（nil = 该次成功）
	calls     int
	lastUser  string // 最近一次 user prompt（断言回灌用）
}

func (m *mockLLM) ChatCompletion(_ context.Context, _ string, user string) (string, error) {
	m.calls++
	m.lastUser = user
	i := m.calls - 1
	if i >= len(m.responses) {
		i = len(m.responses) - 1
	}
	if m.errs != nil && m.errs[i] != nil {
		return "", m.errs[i]
	}
	return m.responses[i], nil
}

// newTestStore 建 sqlite 内存库 + mk_judgments 表（Store 直连 *gorm.DB，与生产同一条路径）。
// 用 GORM AutoMigrate 建表（ensureDDL 是 MySQL 方言，sqlite 不认内联 KEY）；
// 表结构一致性由 store_judgment_test.go 的双轨测试保证。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&Judgment{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	return NewStore(db)
}

func judgeReq() JudgeRequest {
	return JudgeRequest{
		Kind:          KindJudgeLead,
		Region:        RegionIntl,
		Product:       ProductNetwork,
		Source:        "reddit",
		SourceRef:     "r/ethdev/12345",
		Model:         "test-model",
		PromptVersion: "v1",
		Context: ContextInput{
			Kind:     KindJudgeLead,
			Region:   RegionIntl,
			Product:  ProductNetwork,
			PostText: judgeSource,
		},
	}
}

// ---- 验收1：通过 ----

func TestJudgePass(t *testing.T) {
	store := newTestStore(t)
	llm := &mockLLM{responses: []string{validJ1Out}}
	req := judgeReq()

	j, err := Judge(context.Background(), store, llm, req)
	if err != nil {
		t.Fatalf("Judge 通过路径报错: %v", err)
	}
	if j.Verdict != VerdictPass {
		t.Fatalf("Verdict = %q, want pass", j.Verdict)
	}
	if j.Reason != "真实需求，可切入" {
		t.Errorf("Reason = %q, want 模型输出的 reason", j.Reason)
	}
	if llm.calls != 1 {
		t.Errorf("LLM 调用次数 = %d, want 1（通过路径不重试）", llm.calls)
	}
	// 可回放硬要求：snapshot + hash + raw 同表。
	if j.ContextHash == "" || j.ContextSnapshot == "" || j.RawOutput == "" {
		t.Errorf("回放字段缺失: hash=%q snapshot=%dB raw=%dB", j.ContextHash, len(j.ContextSnapshot), len(j.RawOutput))
	}
	if !strings.Contains(j.ContextSnapshot, judgeSource) {
		t.Errorf("snapshot 未包含原文（模型看到的上下文）")
	}
	// 落库：能从库里按 id 取回同一条。
	got, err := store.ByID(context.Background(), j.ID)
	if err != nil {
		t.Fatalf("落库后按 id 取回失败: %v", err)
	}
	if got.Verdict != VerdictPass || got.ContextHash != j.ContextHash {
		t.Errorf("库中记录不一致: verdict=%q hash=%q", got.Verdict, got.ContextHash)
	}
}

// ---- 验收2：判废（同样落库，规格 §2.1 护栏）----

func TestJudgeReject(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantRule string
	}{
		{"quote编造-规则3", fabricatedJ1Out, Rule3QuoteFabricated},
		{"未知枚举-规则6", unknownEnumJ1Out, Rule6UnknownEnum},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newTestStore(t)
			llm := &mockLLM{responses: []string{c.raw}}

			j, err := Judge(context.Background(), store, llm, judgeReq())
			if err != nil {
				t.Fatalf("判废路径不应报错（判废是正常结论）: %v", err)
			}
			if j.Verdict != VerdictReject {
				t.Fatalf("Verdict = %q, want reject", j.Verdict)
			}
			if !strings.Contains(j.Reason, c.wantRule) {
				t.Errorf("Reason 未含判废规则编号 %q: %q", c.wantRule, j.Reason)
			}
			if llm.calls != 1 {
				t.Errorf("LLM 调用次数 = %d, want 1（JSON 合法，不触发规则1 重试）", llm.calls)
			}
			// 判废也要落库。
			got, err := store.ByID(context.Background(), j.ID)
			if err != nil {
				t.Fatalf("判废记录未落库: %v", err)
			}
			if got.Verdict != VerdictReject {
				t.Errorf("库中 verdict = %q, want reject", got.Verdict)
			}
		})
	}
}

// TestJudgeInterveneNoStored 规格 §2.1 护栏：intervene=no 同样落库。
func TestJudgeInterveneNoStored(t *testing.T) {
	store := newTestStore(t)
	llm := &mockLLM{responses: []string{validJ1NoOut}}

	j, err := Judge(context.Background(), store, llm, judgeReq())
	if err != nil {
		t.Fatalf("Judge 报错: %v", err)
	}
	if j.Verdict != VerdictPass {
		t.Fatalf("Verdict = %q, want pass（intervene=no 是合法结论，不是判废）", j.Verdict)
	}
	if _, err := store.ByID(context.Background(), j.ID); err != nil {
		t.Fatalf("intervene=no 未落库: %v", err)
	}
}

// ---- 验收3：解析失败重试一次 ----

func TestJudgeParseRetryOnce(t *testing.T) {
	store := newTestStore(t)
	llm := &mockLLM{responses: []string{"这不是 JSON {", validJ1Out}}
	req := judgeReq()

	j, err := Judge(context.Background(), store, llm, req)
	if err != nil {
		t.Fatalf("Judge 报错: %v", err)
	}
	if j.Verdict != VerdictPass {
		t.Fatalf("Verdict = %q, want pass（重试后合法）", j.Verdict)
	}
	if llm.calls != 2 {
		t.Fatalf("LLM 调用次数 = %d, want 2（首次失败 + 重试一次）", llm.calls)
	}
	// 回灌：第二次调用的 user prompt 必须带解析错误反馈。
	if !strings.Contains(llm.lastUser, "解析错误") {
		t.Errorf("重试未回灌解析错误: %q", llm.lastUser)
	}
	// 落库 RawOutput 是重试后的原始输出。
	if j.RawOutput != validJ1Out {
		t.Errorf("RawOutput 不是重试后的输出")
	}
	if _, err := store.ByID(context.Background(), j.ID); err != nil {
		t.Fatalf("落库失败: %v", err)
	}
}

func TestJudgeParseRetryExhausted(t *testing.T) {
	store := newTestStore(t)
	llm := &mockLLM{responses: []string{"这不是 JSON {", "还是不是 JSON {"}}

	j, err := Judge(context.Background(), store, llm, judgeReq())
	if err != nil {
		t.Fatalf("重试耗尽是判废（正常结论），不应报错: %v", err)
	}
	if j.Verdict != VerdictReject {
		t.Fatalf("Verdict = %q, want reject", j.Verdict)
	}
	if !strings.Contains(j.Reason, Rule1JSONInvalid) {
		t.Errorf("Reason 未含 %q: %q", Rule1JSONInvalid, j.Reason)
	}
	if llm.calls != 2 {
		t.Errorf("LLM 调用次数 = %d, want 2（只重试 1 次）", llm.calls)
	}
	// 判废也要落库。
	if _, err := store.ByID(context.Background(), j.ID); err != nil {
		t.Fatalf("判废记录未落库: %v", err)
	}
}

// ---- 硬失败路径（不落库、返回 error）----

func TestJudgeHardFailures(t *testing.T) {
	t.Run("LLM调用失败", func(t *testing.T) {
		store := newTestStore(t)
		llm := &mockLLM{responses: []string{""}, errs: []error{errors.New("upstream 500")}}

		_, err := Judge(context.Background(), store, llm, judgeReq())
		if err == nil {
			t.Fatal("LLM 调用失败应返回 error")
		}
		n, _ := store.Count(context.Background(), JudgmentFilter{})
		if n != 0 {
			t.Errorf("硬失败不应落库，got %d 条", n)
		}
	})

	t.Run("上下文组装失败-非法region", func(t *testing.T) {
		store := newTestStore(t)
		llm := &mockLLM{responses: []string{validJ1Out}}
		req := judgeReq()
		req.Region = "us"
		req.Context.Region = "us"

		_, err := Judge(context.Background(), store, llm, req)
		if err == nil {
			t.Fatal("非法 region 应返回 error")
		}
		if llm.calls != 0 {
			t.Errorf("上下文组装失败不应调 LLM，got %d 次", llm.calls)
		}
	})

	t.Run("上下文串味-硬拦", func(t *testing.T) {
		store := newTestStore(t)
		llm := &mockLLM{responses: []string{validJ1Out}}
		req := judgeReq()
		req.Region = RegionCN
		req.Context.Region = RegionCN
		req.Context.PostText = "想找个能挖矿的节点，空投多不多？"

		_, err := Judge(context.Background(), store, llm, req)
		if err == nil {
			t.Fatal("cn 上下文命中 crypto 词表应硬拦（不产出上下文包）")
		}
		if llm.calls != 0 {
			t.Errorf("串味硬拦不应调 LLM，got %d 次", llm.calls)
		}
	})

	t.Run("kind未实现", func(t *testing.T) {
		store := newTestStore(t)
		llm := &mockLLM{responses: []string{validJ1Out}}
		req := judgeReq()
		req.Kind = KindDraftReply
		req.Context.Kind = KindDraftReply

		_, err := Judge(context.Background(), store, llm, req)
		if err == nil {
			t.Fatal("未实现 kind 的 prompt 应返回 error")
		}
	})
}

// ---- MK-16 回放：JudgeFromSnapshot（按 snapshot 直接重跑）----

// TestJudgeFromSnapshotReusesContext 验收：重跑必须复用原 snapshot 作为 user prompt
// （「当时模型看到了什么」），而不是反序列化回 ContextInput 丢原料。
func TestJudgeFromSnapshotReusesContext(t *testing.T) {
	store := newTestStore(t)
	// 先跑一次 Judge 落库，拿到真实 snapshot。
	first := &mockLLM{responses: []string{validJ1Out}}
	orig, err := Judge(context.Background(), store, first, judgeReq())
	if err != nil {
		t.Fatalf("首次 Judge 报错: %v", err)
	}
	if !strings.Contains(orig.ContextSnapshot, judgeSource) {
		t.Fatalf("原 snapshot 未含原文: %q", orig.ContextSnapshot)
	}

	// 重跑：mock 记录收到的 user prompt。
	replay := &mockLLM{responses: []string{validJ1Out}}
	j, err := JudgeFromSnapshot(context.Background(), store, replay, orig, "replay-model")
	if err != nil {
		t.Fatalf("JudgeFromSnapshot 报错: %v", err)
	}
	// 关键断言：重跑的 user prompt 就是原 snapshot（含原文），不是空白上下文。
	if !strings.Contains(replay.lastUser, judgeSource) {
		t.Errorf("重跑 user prompt 未复用原 snapshot 原文（回放能力失效）: %q", replay.lastUser)
	}
	if replay.lastUser != orig.ContextSnapshot {
		t.Errorf("重跑 user prompt 应等于原 snapshot，实际: %q", replay.lastUser)
	}
	if j.Verdict != VerdictPass {
		t.Errorf("重跑 Verdict = %q, want pass", j.Verdict)
	}
	// 新记录落库，id 不同于原记录。
	if j.ID == orig.ID {
		t.Errorf("重跑应落新记录，id 不应等于原记录 %d", orig.ID)
	}
	if _, err := store.ByID(context.Background(), j.ID); err != nil {
		t.Fatalf("重跑记录未落库: %v", err)
	}
}

// TestJudgeFromSnapshotPromptVersion 验收：重跑 prompt_version 用**当前**版本（非原记录旧版本），
// model 用实际模型（非原记录 Model）。换 prompt 后金标回归按 prompt_version 分组才正确。
func TestJudgeFromSnapshotPromptVersion(t *testing.T) {
	store := newTestStore(t)
	first := &mockLLM{responses: []string{validJ1Out}}
	orig, err := Judge(context.Background(), store, first, judgeReq())
	if err != nil {
		t.Fatalf("首次 Judge 报错: %v", err)
	}
	// 原记录 prompt_version 也应是当前版本（Judge 内部按实际加载 prompt 填）。
	if orig.PromptVersion != JudgeLeadPromptVersion {
		t.Errorf("原记录 PromptVersion = %q, want %q（应按实际加载 prompt 填）", orig.PromptVersion, JudgeLeadPromptVersion)
	}

	replay := &mockLLM{responses: []string{validJ1Out}}
	j, err := JudgeFromSnapshot(context.Background(), store, replay, orig, "actual-model")
	if err != nil {
		t.Fatalf("JudgeFromSnapshot 报错: %v", err)
	}
	if j.PromptVersion != JudgeLeadPromptVersion {
		t.Errorf("重跑 PromptVersion = %q, want 当前版本 %q（非原记录旧版本）", j.PromptVersion, JudgeLeadPromptVersion)
	}
	if j.Model != "actual-model" {
		t.Errorf("重跑 Model = %q, want 实际模型 %q（非原记录 Model）", j.Model, "actual-model")
	}
}

// TestJudgeFromSnapshotRejectStored 验收：重跑判废（规则命中）同样落库。
func TestJudgeFromSnapshotRejectStored(t *testing.T) {
	store := newTestStore(t)
	first := &mockLLM{responses: []string{validJ1Out}}
	orig, err := Judge(context.Background(), store, first, judgeReq())
	if err != nil {
		t.Fatalf("首次 Judge 报错: %v", err)
	}
	// 重跑时模型输出 quote 编造 → 规则3 判废。
	replay := &mockLLM{responses: []string{fabricatedJ1Out}}
	j, err := JudgeFromSnapshot(context.Background(), store, replay, orig, "replay-model")
	if err != nil {
		t.Fatalf("判废是正常结论，不应报错: %v", err)
	}
	if j.Verdict != VerdictReject {
		t.Errorf("重跑 Verdict = %q, want reject", j.Verdict)
	}
	if !strings.Contains(j.Reason, Rule3QuoteFabricated) {
		t.Errorf("重跑 Reason 未含 %q: %q", Rule3QuoteFabricated, j.Reason)
	}
	if _, err := store.ByID(context.Background(), j.ID); err != nil {
		t.Fatalf("重跑判废记录未落库: %v", err)
	}
}

// TestJudgeFromSnapshotHardFailures 验收：snapshot 非法 / kind 未实现 → 硬失败（不落库）。
func TestJudgeFromSnapshotHardFailures(t *testing.T) {
	t.Run("snapshot非法JSON", func(t *testing.T) {
		store := newTestStore(t)
		llm := &mockLLM{responses: []string{validJ1Out}}
		orig := Judgment{Kind: KindJudgeLead, Region: RegionIntl, ContextSnapshot: "这不是 JSON {"}
		_, err := JudgeFromSnapshot(context.Background(), store, llm, orig, "m")
		if err == nil {
			t.Fatal("snapshot 非法应返回 error")
		}
		if llm.calls != 0 {
			t.Errorf("snapshot 解析失败不应调 LLM，got %d 次", llm.calls)
		}
	})
	t.Run("kind未实现", func(t *testing.T) {
		store := newTestStore(t)
		llm := &mockLLM{responses: []string{validJ1Out}}
		snap, _ := json.Marshal(ContextSnapshot{Kind: KindDraftReply, Region: RegionIntl})
		orig := Judgment{Kind: KindDraftReply, Region: RegionIntl, ContextSnapshot: string(snap)}
		_, err := JudgeFromSnapshot(context.Background(), store, llm, orig, "m")
		if err == nil {
			t.Fatal("未实现 kind 的 prompt 应返回 error")
		}
	})
}

// TestSnapshotSourceText 验收：SourceText 从 snapshot sections 还原原文（规则3 quote 匹配用）。
func TestSnapshotSourceText(t *testing.T) {
	in := ContextInput{Kind: KindJudgeLead, Region: RegionIntl, Product: ProductNetwork, PostText: judgeSource}
	snap, _, err := AssembleContext(in)
	if err != nil {
		t.Fatalf("AssembleContext 报错: %v", err)
	}
	if got := snap.SourceText(); got != judgeSource {
		t.Errorf("SourceText = %q, want 原帖全文 %q", got, judgeSource)
	}

	// 答疑 kind：取 question section。
	in2 := ContextInput{Kind: KindAnswerOrEscalate, Region: RegionIntl, Product: ProductNetwork, Question: "How do I upgrade my node?"}
	snap2, _, err := AssembleContext(in2)
	if err != nil {
		t.Fatalf("AssembleContext 报错: %v", err)
	}
	if got := snap2.SourceText(); got != "How do I upgrade my node?" {
		t.Errorf("SourceText(答疑) = %q, want question", got)
	}
}
