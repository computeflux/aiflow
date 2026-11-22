// prompt.go — MK-15 J1 judge_lead prompt 加载（生产入口，MK-14 主流程调用）。
//
// prompt 正文在 prompts/judge_lead.md（intl）/ prompts/judge_lead.cn.md（cn），
// 由 embed 打进二进制。本文件只提供「按 region 加载 prompt 正文」的生产入口
// LoadJudgeLeadPrompt；prompt 正文的验收测试在 prompt_test.go。
//
// 边界：只实现 J1（judge_lead）；J2–J18 的 prompt 属后续卡。
package marketing

import (
	"embed"
	"fmt"
)

//go:embed prompts/judge_lead.md prompts/judge_lead.cn.md
var promptFS embed.FS

// LoadJudgeLeadPrompt 按 region 加载 J1 prompt 正文（MK-14 主流程调用）。
// intl → judge_lead.md；cn → judge_lead.cn.md。
func LoadJudgeLeadPrompt(region string) (string, error) {
	var name string
	switch region {
	case RegionIntl:
		name = "prompts/judge_lead.md"
	case RegionCN:
		name = "prompts/judge_lead.cn.md"
	default:
		return "", fmt.Errorf("marketing: 非法 region %q（合法值：intl | cn）", region)
	}
	b, err := promptFS.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("marketing: 读取 J1 prompt 失败: %w", err)
	}
	return string(b), nil
}
