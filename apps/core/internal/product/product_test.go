package product

import (
	"strings"
	"testing"
)

func TestCleanContent(t *testing.T) {
	in := "# 标题\n[链接](http://x.com) 文本\n**加粗**\n代码```go```\n   多余   空格"
	out := CleanContent(in)
	if strings.Contains(out, "http://x.com") {
		t.Fatalf("link not stripped: %q", out)
	}
	if strings.Contains(out, "加粗") == false || strings.Contains(out, "**") {
		t.Fatalf("bold not stripped: %q", out)
	}
	if strings.Contains(out, "```") {
		t.Fatalf("code fence not stripped: %q", out)
	}
}

func TestChunkContent_Overlap(t *testing.T) {
	// 长文本应切成多块，且块数 > 1
	var sb strings.Builder
	for i := 0; i < 50; i++ {
		sb.WriteString("这是第")
		sb.WriteString(strings.Repeat("段", i+1))
		sb.WriteString("的内容，用于测试分块。")
	}
	chunks := ChunkContent(sb.String(), 512, 50)
	if len(chunks) < 2 {
		t.Fatalf("expected >1 chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if strings.TrimSpace(c) == "" {
			t.Fatalf("empty chunk at %d", i)
		}
	}
}

func TestChunkContent_Short(t *testing.T) {
	chunks := ChunkContent("短文本", 512, 50)
	if len(chunks) != 1 {
		t.Fatalf("short text should be 1 chunk, got %d", len(chunks))
	}
}

func TestValidateChangeRequest(t *testing.T) {
	full := "## 改什么\n...## 为什么\n...## 预期效果\n...## 风险\n...## 涉及模块\n..."
	if err := validateChangeRequest("change_request", full); err != nil {
		t.Fatalf("full doc should pass: %v", err)
	}
	if err := validateChangeRequest("change_request", "只有改什么"); err == nil {
		t.Fatalf("missing elements should fail")
	}
	// 非 change_request 直接通过
	if err := validateChangeRequest("product_doc", "任意内容"); err != nil {
		t.Fatalf("non-change_request should pass: %v", err)
	}
}

func TestRoutingStatus(t *testing.T) {
	// 2026-10-29：需求拆解建卡**不再自动入队** → 卡片状态统一 "todo"（候选），
	// 先落在 Backlog，由人 hand-off 后再由卡片同步推进到 ready_for_evolve / in_progress …。
	// （未注册项目/空目标的拦截仍在调度时由 IsRunnableTarget / shouldBypassRejected 负责）
	for _, target := range []string{"pycraper", "core", "core-ui", "pycraper,core", ""} {
		if got := routingStatus(target); got != "todo" {
			t.Fatalf("routingStatus(%q)=%q，期望 todo（候选，不自动入队）", target, got)
		}
	}
}

func TestMapPriority(t *testing.T) {
	cases := map[string]string{"P0": "urgent", "P1": "high", "P2": "medium", "P3": "low", "": "medium"}
	for in, want := range cases {
		if got := mapPriority(in); got != want {
			t.Fatalf("mapPriority(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConfidentialityLevelsFor(t *testing.T) {
	if got := ConfidentialityLevelsFor(nil); len(got) != 1 || got[0] != "public" {
		t.Fatalf("no role should see only public: %v", got)
	}
	if got := ConfidentialityLevelsFor([]string{"admin"}); len(got) != 2 {
		t.Fatalf("admin should see public+internal: %v", got)
	}
	if got := ConfidentialityLevelsFor([]string{"secret"}); len(got) != 3 {
		t.Fatalf("secret role should see all three: %v", got)
	}
}

func TestNormalizeConfidentiality(t *testing.T) {
	if NormalizeConfidentiality("") != "internal" {
		t.Fatalf("default should be internal")
	}
	if NormalizeConfidentiality("Secret") != "secret" {
		t.Fatalf("Secret should normalize to secret")
	}
	if NormalizeConfidentiality("weird") != "internal" {
		t.Fatalf("invalid should default to internal")
	}
}

// TestValidateGranularity 锁定「单卡 ≤3 小时」的小颗粒红线：
// 缺 estimate_hours 或超过 3 小时都必须打回（否则一个需求整包进单卡，工作流难以完成/审核）。
func TestValidateGranularity(t *testing.T) {
	ok := &DecomposeResult{Tasks: []Task{{Title: "a", EstimateHours: 1.5}, {Title: "b", EstimateHours: 3}}}
	if err := validateGranularity(ok); err != nil {
		t.Fatalf("合规任务应通过: %v", err)
	}

	missing := &DecomposeResult{Tasks: []Task{{Title: "a"}}}
	if err := validateGranularity(missing); err == nil || !strings.Contains(err.Error(), "estimate_hours") {
		t.Fatalf("缺工时应打回: %v", err)
	}

	tooBig := &DecomposeResult{Tasks: []Task{{Title: "a", EstimateHours: 1}, {Title: "big", EstimateHours: 6}}}
	err := validateGranularity(tooBig)
	if err == nil || !strings.Contains(err.Error(), "big") || !strings.Contains(err.Error(), ">3") {
		t.Fatalf("超 3 小时应打回并指出任务: %v", err)
	}

	if err := validateGranularity(&DecomposeResult{}); err == nil {
		t.Fatal("空任务列表应打回")
	}
}
