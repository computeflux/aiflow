package marketing

import (
	"strings"
	"testing"
	"time"
)

// TestValidRegion 表驱动：region 合法 / 非法枚举。
func TestValidRegion(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{RegionIntl, true},
		{RegionCN, true},
		{"", false},
		{"INTL", false},
		{"us", false},
		{"global", false},
	}
	for _, c := range cases {
		if got := ValidRegion(c.in); got != c.want {
			t.Errorf("ValidRegion(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestValidProduct 表驱动：product 合法 / 非法枚举。
func TestValidProduct(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{ProductNetwork, true},
		{ProductShop, true},
		{"", false},
		{"Network", false},
		{"minishop", false},
		{"tee", false},
	}
	for _, c := range cases {
		if got := ValidProduct(c.in); got != c.want {
			t.Errorf("ValidProduct(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestValidKind 表驱动：判断点 kind 合法 / 非法枚举（8 种）。
func TestValidKind(t *testing.T) {
	valid := []string{
		KindJudgeLead, KindCommentSlot, KindDraftReply, KindCritiqueReply,
		KindAnswerOrEscalate, KindComplianceCheck, KindStorefrontCopy,
		KindAnalyzePerformance,
	}
	for _, k := range valid {
		if !ValidKind(k) {
			t.Errorf("ValidKind(%q) = false, want true", k)
		}
	}
	invalid := []string{"", "judge", "lead", "JUDGE_LEAD", "draft", "publish", "comment"}
	for _, k := range invalid {
		if ValidKind(k) {
			t.Errorf("ValidKind(%q) = true, want false", k)
		}
	}
}

// TestValidActionKind 表驱动：mk_actions.kind 合法 / 非法枚举（4 种）。
func TestValidActionKind(t *testing.T) {
	valid := []string{ActionKindPublish, ActionKindComment, ActionKindDM, ActionKindRecruit}
	for _, k := range valid {
		if !ValidActionKind(k) {
			t.Errorf("ValidActionKind(%q) = false, want true", k)
		}
	}
	invalid := []string{"", "reply", "answer", "intercept", "PUBLISH", "post"}
	for _, k := range invalid {
		if ValidActionKind(k) {
			t.Errorf("ValidActionKind(%q) = true, want false", k)
		}
	}
}

// TestJudgmentValidate 表驱动：Judgment 三类非法枚举（region / product / kind）各被拒绝。
func TestJudgmentValidate(t *testing.T) {
	base := func() Judgment {
		return Judgment{
			Region:  RegionIntl,
			Product: ProductNetwork,
			Kind:    KindJudgeLead,
			Source:  "github",
		}
	}
	cases := []struct {
		name    string
		mutate  func(*Judgment)
		wantErr string // 空 = 期望通过
	}{
		{"合法", func(*Judgment) {}, ""},
		{"非法region", func(j *Judgment) { j.Region = "us" }, "region"},
		{"空region", func(j *Judgment) { j.Region = "" }, "region"},
		{"非法product", func(j *Judgment) { j.Product = "minishop" }, "product"},
		{"空product", func(j *Judgment) { j.Product = "" }, "product"},
		{"非法kind", func(j *Judgment) { j.Kind = "lead" }, "kind"},
		{"空kind", func(j *Judgment) { j.Kind = "" }, "kind"},
		{"action kind 不是判断 kind", func(j *Judgment) { j.Kind = ActionKindPublish }, "kind"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			j := base()
			c.mutate(&j)
			err := j.Validate()
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("Validate() error = %v, want containing %q", err, c.wantErr)
			}
		})
	}
}

// TestLeadValidate 表驱动：Lead 非法 region / product 被拒绝。
func TestLeadValidate(t *testing.T) {
	base := func() Lead {
		return Lead{Region: RegionCN, Product: ProductShop, Source: "zhihu"}
	}
	cases := []struct {
		name    string
		mutate  func(*Lead)
		wantErr string
	}{
		{"合法", func(*Lead) {}, ""},
		{"非法region", func(l *Lead) { l.Region = "intl2" }, "region"},
		{"非法product", func(l *Lead) { l.Product = "network2" }, "product"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := base()
			c.mutate(&l)
			err := l.Validate()
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, c.wantErr)
			}
		})
	}
}

// TestActionValidate 表驱动：Action 三类非法枚举（region / product / action kind）。
func TestActionValidate(t *testing.T) {
	base := func() Action {
		return Action{
			Region:  RegionIntl,
			Product: ProductNetwork,
			Kind:    ActionKindComment,
			Agent:   "a2",
		}
	}
	cases := []struct {
		name    string
		mutate  func(*Action)
		wantErr string
	}{
		{"合法", func(*Action) {}, ""},
		{"非法region", func(a *Action) { a.Region = "cn2" }, "region"},
		{"非法product", func(a *Action) { a.Product = "shop2" }, "product"},
		{"非法action kind", func(a *Action) { a.Kind = "reply" }, "kind"},
		{"空action kind", func(a *Action) { a.Kind = "" }, "kind"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := base()
			c.mutate(&a)
			err := a.Validate()
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, c.wantErr)
			}
		})
	}
}

// TestKindConstantsDistinct 防回归：8 个判断 kind 与 4 个 action kind 互不重复、
// 且两组之间无交集（comment 是 action kind，不是判断 kind）。
func TestKindConstantsDistinct(t *testing.T) {
	judgmentKinds := []string{
		KindJudgeLead, KindCommentSlot, KindDraftReply, KindCritiqueReply,
		KindAnswerOrEscalate, KindComplianceCheck, KindStorefrontCopy,
		KindAnalyzePerformance,
	}
	actionKinds := []string{ActionKindPublish, ActionKindComment, ActionKindDM, ActionKindRecruit}

	seen := map[string]string{}
	for _, k := range judgmentKinds {
		if _, dup := seen[k]; dup {
			t.Errorf("判断 kind 重复: %q", k)
		}
		seen[k] = "judgment"
	}
	for _, k := range actionKinds {
		if prev, exists := seen[k]; exists {
			t.Errorf("action kind %q 与判断 kind 冲突（%s）", k, prev)
		}
		seen[k] = "action"
	}
	if len(judgmentKinds) != 8 {
		t.Errorf("判断 kind 数量 = %d, want 8", len(judgmentKinds))
	}
	if len(actionKinds) != 4 {
		t.Errorf("action kind 数量 = %d, want 4", len(actionKinds))
	}
}

// TestVerdictConstants 防回归：verdict 三值非空且互异。
func TestVerdictConstants(t *testing.T) {
	verdicts := []string{VerdictPass, VerdictRevise, VerdictReject}
	seen := map[string]bool{}
	for _, v := range verdicts {
		if v == "" || seen[v] {
			t.Errorf("verdict 常量非法或重复: %q", v)
		}
		seen[v] = true
	}
}

// TestJudgmentFields 防回归：Judgment 结构体字段齐全（回放硬要求：
// ContextHash + ContextSnapshot + RawOutput 同表，规格 §5 / MK-11）。
func TestJudgmentFields(t *testing.T) {
	j := Judgment{
		Region:          RegionCN,
		Product:         ProductShop,
		Kind:            KindStorefrontCopy,
		Source:          "juejin",
		SourceRef:       "post-123",
		Model:           "test-model",
		PromptVersion:   "v1",
		ContextHash:     "sha256:abc",
		ContextSnapshot: `{"q":"x"}`,
		RawOutput:       `{"verdict":"pass"}`,
		Verdict:         VerdictPass,
		Reason:          "ok",
		CreatedAt:       time.Now(),
	}
	if err := j.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	if j.ContextHash == "" || j.ContextSnapshot == "" || j.RawOutput == "" {
		t.Error("回放三件套（ContextHash/ContextSnapshot/RawOutput）不得为空")
	}
}
