// prompt_test.go — MK-15 J1 judge_lead prompt 验收测试。
//
// 验收（docs/marketing/plan/P2-判断服务地基.md MK-15）：
//  1. prompt 里的枚举集合 == 代码常量集合（need_type / intervene / freshness / pain，按 region 分支）；
//  2. prompt 明确要求 evidence 必填；
//  3. prompt 明确要求 intervene=no 也要给 reason。
//
// 边界：只测 J1（judge_lead），J2–J18 的 prompt 属后续卡。
package marketing

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// enumLineRe 匹配 prompt 里的枚举行：`<field>: v1 | v2 | ...`（输出契约 JSON 注释行不含冒号，不会误匹配）。
var enumLineRe = regexp.MustCompile(`(?m)^([a-z_]+):\s*([a-z_]+(?:\s*\|\s*[a-z_]+)*)\s*$`)

// extractEnum 从 prompt 正文提取某字段的枚举值集合（未找到返回 nil）。
func extractEnum(prompt, field string) []string {
	for _, m := range enumLineRe.FindAllStringSubmatch(prompt, -1) {
		if m[1] == field {
			vals := strings.Split(m[2], "|")
			for i := range vals {
				vals[i] = strings.TrimSpace(vals[i])
			}
			return vals
		}
	}
	return nil
}

// sameSet 比较两个字符串集合是否相等（忽略顺序与重复）。
func sameSet(a, b []string) bool {
	na, nb := len(a), len(b)
	if na == 0 || nb == 0 || na != nb {
		return false
	}
	sa := append([]string(nil), a...)
	sb := append([]string(nil), b...)
	sort.Strings(sa)
	sort.Strings(sb)
	for i := range sa {
		if sa[i] != sb[i] {
			return false
		}
	}
	return true
}

// TestJudgeLeadPromptEnums 验收1：prompt 枚举集合 == 代码常量集合（按 region 分支）。
func TestJudgeLeadPromptEnums(t *testing.T) {
	cases := []struct {
		region string
		// want 直接取自 validate.go 的 kindEnums / painValues（代码常量集合）。
		needType  []string
		intervene []string
		freshness []string
		pain      []string
	}{
		{
			region:    RegionIntl,
			needType:  []string{"demand", "supply", "complaint", "question", "noise"},
			intervene: []string{"yes", "no", "monitor"},
			freshness: []string{"fresh", "stale"},
			pain:      painValues(RegionIntl),
		},
		{
			region:    RegionCN,
			needType:  []string{"demand", "complaint", "question", "noise"}, // 国内无 supply
			intervene: []string{"yes", "no", "monitor"},
			freshness: []string{"fresh", "stale"},
			pain:      painValues(RegionCN),
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.region, func(t *testing.T) {
			prompt, err := LoadJudgeLeadPrompt(c.region)
			if err != nil {
				t.Fatalf("LoadJudgeLeadPrompt(%q) 失败: %v", c.region, err)
			}
			for field, want := range map[string][]string{
				"need_type": c.needType,
				"intervene": c.intervene,
				"freshness": c.freshness,
				"pain":      c.pain,
			} {
				got := extractEnum(prompt, field)
				if !sameSet(got, want) {
					t.Errorf("prompt 字段 %s 枚举集合与代码常量不一致:\n prompt = %v\n 常量   = %v", field, got, want)
				}
			}
		})
	}
}

// TestJudgeLeadPromptContract 验收2/3：输出契约字段齐全 + evidence 必填 + intervene=no 也要 reason。
func TestJudgeLeadPromptContract(t *testing.T) {
	for _, region := range []string{RegionIntl, RegionCN} {
		region := region
		t.Run(region, func(t *testing.T) {
			prompt, err := LoadJudgeLeadPrompt(region)
			if err != nil {
				t.Fatalf("LoadJudgeLeadPrompt(%q) 失败: %v", region, err)
			}
			// 输出契约九字段（规格 §2.1）必须全部出现在 prompt 里。
			for _, field := range []string{
				"is_real_need", "need_type", "intervene", "pain",
				"freshness", "score", "confidence", "evidence", "reason",
			} {
				if !strings.Contains(prompt, field) {
					t.Errorf("prompt 缺少输出契约字段 %q", field)
				}
			}
			// evidence 必填：prompt 必须显式写明。
			if !strings.Contains(prompt, "evidence 必填") {
				t.Errorf("prompt 未明确要求 evidence 必填")
			}
			// intervene=no 也要给 reason：prompt 必须显式写明。
			if !strings.Contains(prompt, "intervene=no 也要给 reason") {
				t.Errorf("prompt 未明确要求 intervene=no 也要给 reason")
			}
		})
	}
}

// TestLoadJudgeLeadPromptRegion 两个 region 各加载到各自文件，非法 region 报错。
func TestLoadJudgeLeadPromptRegion(t *testing.T) {
	intl, err := LoadJudgeLeadPrompt(RegionIntl)
	if err != nil {
		t.Fatalf("intl 加载失败: %v", err)
	}
	cn, err := LoadJudgeLeadPrompt(RegionCN)
	if err != nil {
		t.Fatalf("cn 加载失败: %v", err)
	}
	if intl == cn {
		t.Errorf("intl 与 cn 加载到了同一份 prompt")
	}
	if !strings.Contains(cn, "国内无 supply") {
		t.Errorf("cn prompt 未体现「国内无 supply」")
	}
	if _, err := LoadJudgeLeadPrompt("us"); err == nil {
		t.Errorf("非法 region 未报错")
	}
}
