package agentcli

import (
	"strings"
	"testing"
)

// TestBuildFixPromptRequirement requirement 走「充分理解 + 完整实现」，且带参考文件段。
func TestBuildFixPromptRequirement(t *testing.T) {
	got := BuildFixPrompt("requirement", "实现消息中心", []string{"apps/actives/messages.go"})
	for _, want := range []string{
		"正在为一个项目实现一个产品功能",
		"## 任务\n实现消息中心\n",
		"## 参考文件（实现时重点参考",
		"apps/actives/messages.go",
		"## 执行规则（必须遵守）",
		"**充分理解**",
		"必须产出代码变更。",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("requirement 提示词缺少 %q\n---\n%s", want, got)
		}
	}
	if strings.Contains(got, "只允许修改这些文件") {
		t.Errorf("requirement 不应带 bug 的「只允许修改」限制:\n%s", got)
	}
}

// TestBuildFixPromptBug bug（及 kind 为空）走「快速定位 + 最小改动」。
func TestBuildFixPromptBug(t *testing.T) {
	for _, kind := range []string{"bug", ""} {
		got := BuildFixPrompt(kind, "修空指针", []string{"apps/actives/main.go"})
		for _, want := range []string{
			"正在修复一个生产 bug",
			"## 目标文件（只允许修改这些文件",
			"apps/actives/main.go",
			"**快速定位**：最多读取 3 个文件",
			"**最小改动**",
			"必须产出代码变更。",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("kind=%q 提示词缺少 %q\n---\n%s", kind, want, got)
			}
		}
	}
}

// TestBuildFixPromptNoFiles 无参考文件时不应出现空的文件段头。
func TestBuildFixPromptNoFiles(t *testing.T) {
	got := BuildFixPrompt("bug", "修 bug", nil)
	if strings.Contains(got, "## 目标文件") {
		t.Errorf("无文件时不应输出文件段:\n%s", got)
	}
	if !strings.HasSuffix(got, "必须产出代码变更。") {
		t.Errorf("提示词应以警示句结尾:\n%s", got)
	}
}

// TestBuildContinuePrompt 续接只带上一轮反馈（去掉 leading 空白），空反馈有兜底。
func TestBuildContinuePrompt(t *testing.T) {
	if got := BuildContinuePrompt("\n\n上一轮 reviewer 的 error 级评论：\n- a.go:1 问题"); got != "上一轮 reviewer 的 error 级评论：\n- a.go:1 问题" {
		t.Errorf("反馈应去掉 leading 空白，得到 %q", got)
	}
	if got := BuildContinuePrompt("   "); got != continuePromptFallback {
		t.Errorf("空反馈应走兜底提示，得到 %q", got)
	}
}
