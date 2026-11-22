// validate.go — MK-12 服务端校验器（docs/marketing/implementation-spec.md §6.1，6 条硬规则）。
//
// 设计原则：不信任模型，硬规则全部服务端强制：
//
//	规则1 JSON 非法 → 重试 1 次（把解析错误回灌）→ 仍失败判废
//	规则2 evidence 为空 → 判废
//	规则3 evidence[].quote 去空白后必须能在原文子串匹配 → 找不到即「编的」→ 判废
//	规则4 covered=false 但输出里含 answer → 判废（J10 类判断；J1 不产 covered）
//	规则5 region=cn 命中 crypto 词表 → 判废（不进人工队列，直接丢）
//	规则6 输出含未知枚举值 → 判废
//
// 边界：词表用内置常量（最小集），规则包文件化留给后续卡；
// 不做语义级合规（那是 J13，属 P4）。
package marketing

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// 校验规则编号（判废留痕用，audit / mk_judgments.reason 引用）。
const (
	Rule1JSONInvalid     = "json_invalid"     // 规则1：JSON 非法（重试 1 次后仍失败）
	Rule2EvidenceEmpty   = "evidence_empty"   // 规则2：evidence 为空
	Rule3QuoteFabricated = "quote_fabricated" // 规则3：quote 原文找不到（编的）
	Rule4AnswerLeak      = "answer_leak"      // 规则4：covered=false 却含 answer
	Rule5CNCrypto        = "cn_crypto"        // 规则5：国内稿命中 crypto 词表
	Rule6UnknownEnum     = "unknown_enum"     // 规则6：未知枚举值
)

// ---- 输入 / 输出 ----

// ValidateInput 是校验器输入。
type ValidateInput struct {
	Region string // intl | cn（规则5 用）
	Kind   string // 判断点 kind（决定输出契约 / 枚举集合，规则6 用）
	Raw    string // 模型原始输出（JSON 字符串）
	Source string // 原文（帖子 / issue 正文，规则3 quote 子串匹配用）
}

// ValidateResult 是校验结果。判废 = OK=false + Rule + Reason（不进人工队列）。
type ValidateResult struct {
	OK     bool           // true = 通过全部硬规则
	Rule   string         // 命中的规则编号（OK=true 时为空）
	Reason string         // 一句话判废理由（OK=true 时为空）
	Parsed map[string]any // 解析后的 JSON（OK=true 时有效，供下游落库 / 组装）
}

// RetryFn 是规则1 的重试回调：接收解析错误（回灌进 prompt），返回新的原始输出。
type RetryFn func(feedback string) (string, error)

// ---- 规则1：JSON 解析 ----

// ParseOutput 把模型原始输出解析为 JSON 对象。
func ParseOutput(raw string) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("marketing: 模型输出不是合法 JSON: %w", err)
	}
	return m, nil
}

// ValidateWithRetry 实现规则1：JSON 非法 → 重试 1 次（把解析错误回灌）→ 仍失败判废。
// retry 为 nil 时不重试，解析失败直接判废。
// 解析成功后继续跑规则2–6（Validate）。
func ValidateWithRetry(in ValidateInput, retry RetryFn) ValidateResult {
	parseErr := func() error {
		_, err := ParseOutput(in.Raw)
		return err
	}()
	if parseErr == nil {
		return Validate(in)
	}
	if retry == nil {
		return fail(Rule1JSONInvalid, "JSON 非法且未提供重试")
	}
	feedback := fmt.Sprintf("上一次输出不是合法 JSON，请修正后重新输出。解析错误：%v", parseErr)
	raw2, err := retry(feedback)
	if err != nil {
		return fail(Rule1JSONInvalid, fmt.Sprintf("重试失败: %v", err))
	}
	in.Raw = raw2
	return Validate(in)
}

// ---- 规则2–6 ----

// Validate 跑规则2–6（规则1 由 ValidateWithRetry 处理）。
// 任一规则命中即判废（OK=false），不进人工队列。
func Validate(in ValidateInput) ValidateResult {
	parsed, err := ParseOutput(in.Raw)
	if err != nil {
		return fail(Rule1JSONInvalid, err.Error())
	}
	if r, done := checkEvidence(in.Kind, parsed, in.Source); done {
		return r
	}
	if r, done := checkCoveredAnswer(parsed); done {
		return r
	}
	if r, done := checkCNCrypto(in.Region, in.Raw); done {
		return r
	}
	if r, done := checkEnums(in.Kind, in.Region, parsed); done {
		return r
	}
	return ValidateResult{OK: true, Parsed: parsed}
}

// evidenceKinds 是输出契约里 evidence 必填的判断点（规格 §2.1：J1 类必填）。
// J10（answer_or_escalate）/ critique 不产 evidence，不受规则2/3 约束；
// 其他 kind 的契约由后续卡补齐后再加入本集合。
var evidenceKinds = map[string]bool{KindJudgeLead: true}

// checkEvidence 规则2（evidence 为空）+ 规则3（quote 去空白子串匹配）。
// 只对 evidence 必填的 kind 生效。
func checkEvidence(kind string, parsed map[string]any, source string) (ValidateResult, bool) {
	if !evidenceKinds[kind] {
		return ValidateResult{}, false
	}
	ev, ok := parsed["evidence"]
	if !ok {
		return fail(Rule2EvidenceEmpty, "evidence 缺失（必填）"), true
	}
	arr, ok := ev.([]any)
	if !ok {
		return fail(Rule2EvidenceEmpty, "evidence 不是数组"), true
	}
	if len(arr) == 0 {
		return fail(Rule2EvidenceEmpty, "evidence 为空"), true
	}
	normalizedSource := stripWS(source)
	for i, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			return fail(Rule3QuoteFabricated, fmt.Sprintf("evidence[%d] 不是对象", i)), true
		}
		quote, _ := obj["quote"].(string)
		if strings.TrimSpace(quote) == "" {
			return fail(Rule3QuoteFabricated, fmt.Sprintf("evidence[%d].quote 为空", i)), true
		}
		if !strings.Contains(normalizedSource, stripWS(quote)) {
			return fail(Rule3QuoteFabricated,
				fmt.Sprintf("evidence[%d].quote 在原文中找不到（疑似编造）: %q", i, quote)), true
		}
	}
	return ValidateResult{}, false
}

// checkCoveredAnswer 规则4：covered=false 但输出里含 answer → 判废。
// 只适用于产 covered 的判断（J10 类 answer_or_escalate）；J1 不产 covered，直接跳过。
func checkCoveredAnswer(parsed map[string]any) (ValidateResult, bool) {
	covered, ok := parsed["covered"]
	if !ok {
		return ValidateResult{}, false
	}
	coveredBool, ok := covered.(bool)
	if !ok {
		return fail(Rule4AnswerLeak, "covered 不是布尔值"), true
	}
	if coveredBool {
		return ValidateResult{}, false
	}
	answer, exists := parsed["answer"]
	if !exists {
		return ValidateResult{}, false
	}
	if s, isStr := answer.(string); isStr {
		if strings.TrimSpace(s) == "" {
			return ValidateResult{}, false // 空 answer 视同未输出
		}
	}
	return fail(Rule4AnswerLeak, "covered=false 但输出含 answer（服务端硬拦，不靠模型自觉）"), true
}

// cnCryptoWords 是国内稿 crypto 词表（内置常量，最小集；规则包文件化留给后续卡）。
// 国内（AI 服务）稿绝不出现币 / 挖矿话术 —— 命中即判废，不交人工。
var cnCryptoWords = []string{
	"挖矿", "矿工", "挖币", "空投", "年化", "币圈", "代币", "链上",
}

// checkCNCrypto 规则5：region=cn 命中 crypto 词表 → 判废（不进人工队列）。
// 检查整段原始输出（含 answer / reason / 草稿正文），intl 不受限。
func checkCNCrypto(region, raw string) (ValidateResult, bool) {
	if region != RegionCN {
		return ValidateResult{}, false
	}
	for _, w := range cnCryptoWords {
		if strings.Contains(raw, w) {
			return fail(Rule5CNCrypto,
				fmt.Sprintf("region=cn 输出命中 crypto 词表 %q（直接判废，不进人工队列）", w)), true
		}
	}
	return ValidateResult{}, false
}

// ---- 规则6：枚举校验 ----

// enumSpec 描述一个枚举字段：path（点分路径）+ 合法值集合（可按 region 分支）。
type enumSpec struct {
	path   string
	values func(region string) []string
}

// kindEnums 是各判断点输出契约里的枚举字段（规格 §2.1 / §2.2 / §2.3）。
// 未列出的 kind 不做枚举检查（契约由后续卡补齐）。
var kindEnums = map[string][]enumSpec{
	KindJudgeLead: {
		{
			path: "need_type",
			values: func(region string) []string {
				if region == RegionCN {
					return []string{"demand", "complaint", "question", "noise"} // 国内无 supply
				}
				return []string{"demand", "supply", "complaint", "question", "noise"}
			},
		},
		{"intervene", func(string) []string { return []string{"yes", "no", "monitor"} }},
		{"freshness", func(string) []string { return []string{"fresh", "stale"} }},
		{"pain", painValues},
	},
	KindAnswerOrEscalate: {
		{"escalate_reason", func(string) []string {
			return []string{"kb_miss", "bug", "billing", "legal", "procurement", "unknown"}
		}},
	},
	KindCritiqueReply: {
		{"self_critique.verdict", func(string) []string {
			return []string{VerdictPass, VerdictRevise, VerdictReject}
		}},
	},
}

// painValues 是 J1 pain 枚举（按 region 分支，规格 §2.1；Shop 店主招募一套两 region 通用）。
func painValues(region string) []string {
	shop := []string{"has_capacity_no_channel", "has_customers", "wants_income", "wants_control", "billing_hassle"}
	if region == RegionCN {
		return append([]string{
			"cost", "stability", "data_residency", "invoice",
			"procurement", "domestic_access", "no_support", "latency",
		}, shop...)
	}
	return append([]string{
		// 海外 demand
		"rate_limit", "kyc", "credit_card", "cost", "privacy",
		"multi_chain", "mev_sandwich", "latency",
		// 海外 supply
		"payout_delay", "points_worthless", "gas_cost", "hardware_idle",
	}, shop...)
}

// checkEnums 规则6：输出含未知枚举值 → 判废。
// 字段缺失不算「未知枚举值」（必填性由 prompt 契约 + 规则2 兜底）。
func checkEnums(kind, region string, parsed map[string]any) (ValidateResult, bool) {
	specs, ok := kindEnums[kind]
	if !ok {
		return ValidateResult{}, false
	}
	for _, spec := range specs {
		v, exists := lookupPath(parsed, spec.path)
		if !exists {
			continue
		}
		s, isStr := v.(string)
		if !isStr {
			return fail(Rule6UnknownEnum,
				fmt.Sprintf("枚举字段 %s 不是字符串: %v", spec.path, v)), true
		}
		if !containsStr(spec.values(region), s) {
			return fail(Rule6UnknownEnum,
				fmt.Sprintf("枚举字段 %s 出现未知值 %q", spec.path, s)), true
		}
	}
	return ValidateResult{}, false
}

// ---- 工具函数 ----

func fail(rule, reason string) ValidateResult {
	return ValidateResult{OK: false, Rule: rule, Reason: reason}
}

// stripWS 去掉全部空白字符（空格 / 换行 / tab），用于 quote 子串匹配。
func stripWS(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// lookupPath 按点分路径取嵌套字段（如 self_critique.verdict）。
func lookupPath(m map[string]any, path string) (any, bool) {
	var cur any = m
	for _, p := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
