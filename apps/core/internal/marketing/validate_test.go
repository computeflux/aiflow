package marketing

import (
	"errors"
	"strings"
	"testing"
)

// ---- 测试夹具 ----

// source 是规则3 子串匹配用的「原文」（帖子 / issue 正文）。
const testSource = "我们的 TEE 中转节点最近延迟很高，请求经常超时，" +
	"有没有人知道怎么解决？另外 KYC 审核要等一周，太慢了。"

// validJ1 是一条合法的 J1（judge_lead）输出（intl）。
const validJ1 = `{
  "is_real_need": true,
  "need_type": "demand",
  "intervene": "yes",
  "pain": "latency",
  "freshness": "fresh",
  "score": 8,
  "confidence": 0.9,
  "evidence": [{"quote": "延迟很高，请求经常超时", "why": "明确性能诉求"}],
  "reason": "真实需求，可切入"
}`

// validJ10 是一条合法的 J10（answer_or_escalate）输出（covered=true）。
const validJ10 = `{
  "covered": true,
  "answer": "节点延迟可通过切换 TEE 中转线路解决。",
  "citations": ["docs/faq.md#L10-L40"],
  "escalate": false,
  "escalate_reason": "unknown"
}`

// validJ1CN 是一条合法的 J1 输出（cn，pain 走国内枚举）。
const validJ1CN = `{
  "is_real_need": true,
  "need_type": "question",
  "intervene": "monitor",
  "pain": "stability",
  "freshness": "fresh",
  "score": 6,
  "confidence": 0.8,
  "evidence": [{"quote": "请求经常超时", "why": "稳定性诉求"}],
  "reason": "国内稳定性问题，先观察"
}`

func j1Input(raw string) ValidateInput {
	return ValidateInput{Region: RegionIntl, Kind: KindJudgeLead, Raw: raw, Source: testSource}
}

func j10Input(raw string) ValidateInput {
	return ValidateInput{Region: RegionIntl, Kind: KindAnswerOrEscalate, Raw: raw, Source: testSource}
}

// ---- 规则1：JSON 非法 → 重试 1 次（回灌解析错误）→ 仍失败判废 ----

func TestValidateRule1JSON(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		retry    RetryFn
		wantOK   bool
		wantRule string
	}{
		{
			name:   "正例-合法JSON直接通过",
			raw:    validJ1,
			retry:  nil,
			wantOK: true,
		},
		{
			name:     "反例-非法JSON无重试判废",
			raw:      "这不是 JSON {",
			retry:    nil,
			wantOK:   false,
			wantRule: Rule1JSONInvalid,
		},
		{
			name: "反例-非法JSON重试后仍非法判废",
			raw:  "这不是 JSON {",
			retry: func(feedback string) (string, error) {
				if !strings.Contains(feedback, "解析错误") {
					t.Errorf("回灌内容应含解析错误，got: %q", feedback)
				}
				return "还是不是 JSON {", nil
			},
			wantOK:   false,
			wantRule: Rule1JSONInvalid,
		},
		{
			name: "正例-非法JSON重试一次后合法通过",
			raw:  "这不是 JSON {",
			retry: func(feedback string) (string, error) {
				return validJ1, nil
			},
			wantOK: true,
		},
		{
			name: "反例-重试回调报错判废",
			raw:  "这不是 JSON {",
			retry: func(string) (string, error) {
				return "", errRetry
			},
			wantOK:   false,
			wantRule: Rule1JSONInvalid,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := ValidateWithRetry(j1Input(c.raw), c.retry)
			if res.OK != c.wantOK {
				t.Fatalf("OK = %v, want %v (rule=%s reason=%s)", res.OK, c.wantOK, res.Rule, res.Reason)
			}
			if !c.wantOK && res.Rule != c.wantRule {
				t.Errorf("Rule = %q, want %q", res.Rule, c.wantRule)
			}
			if c.wantOK && res.Parsed == nil {
				t.Error("通过时 Parsed 不应为 nil")
			}
		})
	}
}

var errRetry = errors.New("llm 重试调用失败")

// ---- 规则2：evidence 为空 → 判废 ----

func TestValidateRule2EvidenceEmpty(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		kind     string
		wantOK   bool
		wantRule string
	}{
		{"正例-evidence非空通过", validJ1, KindJudgeLead, true, ""},
		{"反例-evidence空数组判废",
			`{"need_type":"demand","intervene":"yes","pain":"latency","freshness":"fresh","score":8,"confidence":0.9,"evidence":[],"reason":"x"}`,
			KindJudgeLead, false, Rule2EvidenceEmpty},
		{"反例-evidence缺失判废",
			`{"need_type":"demand","intervene":"yes","pain":"latency","freshness":"fresh","score":8,"confidence":0.9,"reason":"x"}`,
			KindJudgeLead, false, Rule2EvidenceEmpty},
		{"反例-evidence不是数组判废",
			`{"need_type":"demand","intervene":"yes","pain":"latency","freshness":"fresh","score":8,"confidence":0.9,"evidence":"一段话","reason":"x"}`,
			KindJudgeLead, false, Rule2EvidenceEmpty},
		{"正例-J10不产evidence不受规则2约束",
			`{"covered":true,"answer":"x","citations":["docs/a.md"],"escalate":false,"escalate_reason":"unknown"}`,
			KindAnswerOrEscalate, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := Validate(ValidateInput{Region: RegionIntl, Kind: c.kind, Raw: c.raw, Source: testSource})
			if res.OK != c.wantOK {
				t.Fatalf("OK = %v, want %v (rule=%s reason=%s)", res.OK, c.wantOK, res.Rule, res.Reason)
			}
			if !c.wantOK && res.Rule != c.wantRule {
				t.Errorf("Rule = %q, want %q", res.Rule, c.wantRule)
			}
		})
	}
}

// ---- 规则3：evidence[].quote 去空白子串匹配 → 找不到即编的 → 判废 ----

func TestValidateRule3Quote(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantOK   bool
		wantRule string
	}{
		{"正例-quote原文存在通过", validJ1, true, ""},
		{"正例-quote跨换行去空白后匹配通过",
			`{"need_type":"demand","intervene":"yes","pain":"latency","freshness":"fresh","score":8,"confidence":0.9,
			  "evidence":[{"quote":"延迟很高，\n请求经常超时","why":"跨行引用"}],
			  "reason":"x"}`,
			true, ""},
		{"反例-quote原文不存在判废",
			`{"need_type":"demand","intervene":"yes","pain":"latency","freshness":"fresh","score":8,"confidence":0.9,
			  "evidence":[{"quote":"我们承诺零延迟且绝对安全","why":"编的"}],
			  "reason":"x"}`,
			false, Rule3QuoteFabricated},
		{"反例-quote为空判废",
			`{"need_type":"demand","intervene":"yes","pain":"latency","freshness":"fresh","score":8,"confidence":0.9,
			  "evidence":[{"quote":"   ","why":"空引用"}],
			  "reason":"x"}`,
			false, Rule3QuoteFabricated},
		{"反例-多条quote其中一条编造判废",
			`{"need_type":"demand","intervene":"yes","pain":"latency","freshness":"fresh","score":8,"confidence":0.9,
			  "evidence":[{"quote":"延迟很高","why":"真"},{"quote":"年化收益300%","why":"编"}],
			  "reason":"x"}`,
			false, Rule3QuoteFabricated},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := Validate(j1Input(c.raw))
			if res.OK != c.wantOK {
				t.Fatalf("OK = %v, want %v (rule=%s reason=%s)", res.OK, c.wantOK, res.Rule, res.Reason)
			}
			if !c.wantOK && res.Rule != c.wantRule {
				t.Errorf("Rule = %q, want %q", res.Rule, c.wantRule)
			}
		})
	}
}

// ---- 规则4：covered=false 但输出含 answer → 判废（J10 类；J1 不产 covered）----

func TestValidateRule4CoveredAnswer(t *testing.T) {
	cases := []struct {
		name     string
		in       ValidateInput
		wantOK   bool
		wantRule string
	}{
		{"正例-covered=true含answer通过",
			ValidateInput{Region: RegionIntl, Kind: KindAnswerOrEscalate, Raw: validJ10, Source: testSource},
			true, ""},
		{"正例-covered=false无answer通过",
			ValidateInput{Region: RegionIntl, Kind: KindAnswerOrEscalate,
				Raw:    `{"covered":false,"citations":[],"escalate":true,"escalate_reason":"kb_miss"}`,
				Source: testSource},
			true, ""},
		{"反例-covered=false含answer判废",
			ValidateInput{Region: RegionIntl, Kind: KindAnswerOrEscalate,
				Raw:    `{"covered":false,"answer":"其实可以这样配置……","citations":[],"escalate":false,"escalate_reason":"unknown"}`,
				Source: testSource},
			false, Rule4AnswerLeak},
		{"正例-covered=false空answer视同未输出通过",
			ValidateInput{Region: RegionIntl, Kind: KindAnswerOrEscalate,
				Raw:    `{"covered":false,"answer":"  ","citations":[],"escalate":true,"escalate_reason":"kb_miss"}`,
				Source: testSource},
			true, ""},
		{"正例-J1不产covered不受规则4约束",
			j1Input(validJ1),
			true, ""},
		{"反例-covered不是布尔判废",
			ValidateInput{Region: RegionIntl, Kind: KindAnswerOrEscalate,
				Raw:    `{"covered":"yes","answer":"x","citations":[],"escalate":false,"escalate_reason":"unknown"}`,
				Source: testSource},
			false, Rule4AnswerLeak},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := Validate(c.in)
			if res.OK != c.wantOK {
				t.Fatalf("OK = %v, want %v (rule=%s reason=%s)", res.OK, c.wantOK, res.Rule, res.Reason)
			}
			if !c.wantOK && res.Rule != c.wantRule {
				t.Errorf("Rule = %q, want %q", res.Rule, c.wantRule)
			}
		})
	}
}

// ---- 规则5：region=cn 命中 crypto 词表 → 判废（不进人工队列）----

func TestValidateRule5CNCrypto(t *testing.T) {
	cases := []struct {
		name     string
		in       ValidateInput
		wantOK   bool
		wantRule string
	}{
		{"正例-cn无crypto词通过",
			ValidateInput{Region: RegionCN, Kind: KindJudgeLead, Raw: validJ1CN, Source: testSource},
			true, ""},
		{"反例-cn含挖矿判废",
			ValidateInput{Region: RegionCN, Kind: KindJudgeLead,
				Raw:    strings.Replace(validJ1CN, "国内稳定性问题，先观察", "可以引导去挖矿赚币", 1),
				Source: testSource},
			false, Rule5CNCrypto},
		{"反例-cn含空投判废",
			ValidateInput{Region: RegionCN, Kind: KindJudgeLead,
				Raw:    strings.Replace(validJ1CN, "国内稳定性问题，先观察", "关注我们的空投活动", 1),
				Source: testSource},
			false, Rule5CNCrypto},
		{"反例-cn含年化判废",
			ValidateInput{Region: RegionCN, Kind: KindJudgeLead,
				Raw:    strings.Replace(validJ1CN, "国内稳定性问题，先观察", "年化收益很可观", 1),
				Source: testSource},
			false, Rule5CNCrypto},
		{"正例-intl含crypto词不受限",
			ValidateInput{Region: RegionIntl, Kind: KindJudgeLead,
				Raw:    strings.Replace(validJ1, "真实需求，可切入", "real mining demand", 1),
				Source: testSource},
			true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := Validate(c.in)
			if res.OK != c.wantOK {
				t.Fatalf("OK = %v, want %v (rule=%s reason=%s)", res.OK, c.wantOK, res.Rule, res.Reason)
			}
			if !c.wantOK && res.Rule != c.wantRule {
				t.Errorf("Rule = %q, want %q", res.Rule, c.wantRule)
			}
		})
	}
}

// ---- 规则6：输出含未知枚举值 → 判废 ----

func TestValidateRule6UnknownEnum(t *testing.T) {
	cases := []struct {
		name     string
		in       ValidateInput
		wantOK   bool
		wantRule string
	}{
		{"正例-全部枚举合法通过", j1Input(validJ1), true, ""},
		{"反例-need_type未知值判废",
			j1Input(strings.Replace(validJ1, `"need_type": "demand"`, `"need_type": "hype"`, 1)),
			false, Rule6UnknownEnum},
		{"反例-intervene未知值判废",
			j1Input(strings.Replace(validJ1, `"intervene": "yes"`, `"intervene": "maybe"`, 1)),
			false, Rule6UnknownEnum},
		{"反例-freshness未知值判废",
			j1Input(strings.Replace(validJ1, `"freshness": "fresh"`, `"freshness": "old"`, 1)),
			false, Rule6UnknownEnum},
		{"反例-pain未知值判废",
			j1Input(strings.Replace(validJ1, `"pain": "latency"`, `"pain": "vibes"`, 1)),
			false, Rule6UnknownEnum},
		{"反例-cn出现supply判废（国内无supply）",
			ValidateInput{Region: RegionCN, Kind: KindJudgeLead,
				Raw:    strings.Replace(validJ1CN, `"need_type": "question"`, `"need_type": "supply"`, 1),
				Source: testSource},
			false, Rule6UnknownEnum},
		{"正例-cn国内pain枚举通过",
			ValidateInput{Region: RegionCN, Kind: KindJudgeLead,
				Raw:    strings.Replace(validJ1CN, `"pain": "stability"`, `"pain": "invoice"`, 1),
				Source: testSource},
			true, ""},
		{"正例-intl海外pain枚举（mev_sandwich）通过",
			ValidateInput{Region: RegionIntl, Kind: KindJudgeLead,
				Raw:    strings.Replace(validJ1, `"pain": "latency"`, `"pain": "mev_sandwich"`, 1),
				Source: testSource},
			true, ""},
		{"反例-J10 escalate_reason未知值判废",
			j10Input(strings.Replace(validJ10, `"escalate_reason": "unknown"`, `"escalate_reason": "other"`, 1)),
			false, Rule6UnknownEnum},
		{"反例-critique verdict未知值判废",
			ValidateInput{Region: RegionIntl, Kind: KindCritiqueReply,
				Raw:    `{"candidates":[],"self_critique":{"spam_risk":1,"hard_sell":false,"overclaim":false,"region_fit":true,"platform_rule_ok":true,"verdict":"meh","why":"x"}}`,
				Source: testSource},
			false, Rule6UnknownEnum},
		{"正例-critique verdict合法通过",
			ValidateInput{Region: RegionIntl, Kind: KindCritiqueReply,
				Raw:    `{"candidates":[],"self_critique":{"spam_risk":1,"hard_sell":false,"overclaim":false,"region_fit":true,"platform_rule_ok":true,"verdict":"pass","why":"x"}}`,
				Source: testSource},
			true, ""},
		{"正例-枚举字段缺失不算未知值",
			j1Input(`{"need_type":"demand","intervene":"yes","pain":"latency","score":8,"confidence":0.9,
				"evidence":[{"quote":"延迟很高","why":"x"}],"reason":"x"}`),
			true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := Validate(c.in)
			if res.OK != c.wantOK {
				t.Fatalf("OK = %v, want %v (rule=%s reason=%s)", res.OK, c.wantOK, res.Rule, res.Reason)
			}
			if !c.wantOK && res.Rule != c.wantRule {
				t.Errorf("Rule = %q, want %q", res.Rule, c.wantRule)
			}
		})
	}
}

// ---- 综合：正例全通过 + 判废结果结构完整 ----

func TestValidateAllPass(t *testing.T) {
	for name, in := range map[string]ValidateInput{
		"J1-intl": j1Input(validJ1),
		"J1-cn":   ValidateInput{Region: RegionCN, Kind: KindJudgeLead, Raw: validJ1CN, Source: testSource},
		"J10":     j10Input(validJ10),
	} {
		t.Run(name, func(t *testing.T) {
			res := Validate(in)
			if !res.OK {
				t.Fatalf("OK = false, want true (rule=%s reason=%s)", res.Rule, res.Reason)
			}
			if res.Rule != "" || res.Reason != "" {
				t.Errorf("通过时 Rule/Reason 应为空，got %q / %q", res.Rule, res.Reason)
			}
			if res.Parsed == nil {
				t.Error("通过时 Parsed 不应为 nil")
			}
		})
	}
}

func TestValidateFailShape(t *testing.T) {
	// 判废结果必须带规则编号 + 理由（留痕用）。
	res := Validate(j1Input(`{"evidence":[],"reason":"x"}`))
	if res.OK {
		t.Fatal("want 判废")
	}
	if res.Rule == "" || res.Reason == "" {
		t.Errorf("判废必须带 Rule + Reason，got %q / %q", res.Rule, res.Reason)
	}
	if res.Parsed != nil {
		t.Error("判废时 Parsed 应为 nil")
	}
}

// TestCNCryptoWords 防回归：词表最小集非空、无空词、无重复。
func TestCNCryptoWords(t *testing.T) {
	if len(cnCryptoWords) == 0 {
		t.Fatal("cn crypto 词表不得为空")
	}
	seen := map[string]bool{}
	for _, w := range cnCryptoWords {
		if w == "" {
			t.Errorf("词表含空词")
		}
		if seen[w] {
			t.Errorf("词表重复: %q", w)
		}
		seen[w] = true
	}
}
