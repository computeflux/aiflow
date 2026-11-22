package agentcli

import "strings"

// 提示词组装（与旧 adapter.sh 逐字一致，避免行为漂移）。
//
// 按任务类型（kind）切换策略：
//   - bug：快速定位（最多读 3 个文件）+ 最小改动 + 改完即停
//     （reasoning model 易把轮次耗在探索上，四两拨千斤的是「立即修改」）
//   - requirement（产品功能）：充分理解现有代码与约定后再实现，允许读更多文件、新增文件、
//     遵循项目规范；不能套用 bug 的「最多 3 个文件 / 只改相关文件」限制。
//   - kind 为空（旧任务）按 bug 处理。
const (
	requirementHeader = `你是一名资深工程师，正在为一个项目实现一个产品功能。请严格按以下流程执行：

## 任务
`

	requirementFilesHeader = `
## 参考文件（实现时重点参考，可在此基础上按需新增/修改其他文件，且必须位于 apps/ 下）
`

	requirementRules = `
## 执行规则（必须遵守）
1. **先落骨架再完善**：只读探索最多 10 轮工具调用，然后必须先用 write/edit 落一个最小可运行骨架（新建或修改文件），再在骨架上迭代完善。禁止长时间只读不写。
2. **充分理解**：读懂相关模块的现有实现、接口约定与项目规范（可读取多个文件，必要时检索代码库），确保新功能与现有架构一致。
3. **完整实现**：必须用编辑工具（write/edit）实际写出代码，包括新增文件、必要的接口/路由/配置改动。不要只输出分析或方案。
4. **遵循规范**：遵守目标项目的技术栈与代码风格（命名、目录结构、依赖管理、错误处理），不引入项目未使用的依赖。
5. **必须写单元测试**：为本次新增/修改的核心逻辑编写单元测试（Go 用 *_test.go，Python 用 test_*.py，遵循目标项目既有测试框架与目录约定），覆盖任务验收标准中的行为点；写完必须运行这些测试（go test / pytest 指定包）并确认全部通过。
6. **可验证**：实现要能编译/通过项目自身的 lint 与测试；如任务含验收标准，逐条满足。
7. **改完即停**：功能实现完成后立即停止，不要做无关重构。

⚠️ 如果你只读文件而没有修改/新增任何文件，本次任务将判定为失败。必须产出代码变更。`

	bugHeader = `你是一名资深工程师，正在修复一个生产 bug。请严格按以下流程执行：

## 任务
`

	bugFilesHeader = `
## 目标文件（只允许修改这些文件，且必须位于 apps/ 下）
`

	bugRules = `
## 执行规则（必须遵守）
1. **快速定位**：最多读取 3 个文件来理解问题，不要遍历整个代码库。
2. **立即修改**：定位到问题后，必须用编辑工具（write/edit）实际修改代码。不要只输出分析或建议。
3. **最小改动**：只改与 bug 直接相关的代码，不要重构、不要加注释、不要改无关文件。
4. **补回归测试**：在对应测试文件中为这个 bug 补充一个能复现它的单元测试（先证明修复前该测试会失败/覆盖该缺陷路径），写完必须运行该测试并确认通过。
5. **改完即停**：所有目标文件修改 + 回归测试通过后立即停止，不要继续探索或验证。

⚠️ 如果你只读文件而没有修改任何文件，本次任务将判定为失败。必须产出代码变更。`

	// continuePromptFallback feedback 为空时的兜底（理论上不会发生：runTask 第 2 轮起必带反馈）。
	continuePromptFallback = "继续执行上一轮未完成任务。请继续用编辑工具修改代码，不要只输出分析。"
)

// WorkdirGuard 每轮（新建与续接都）注入的工作目录锚定提示：
// 实测教训（2026-11-10 WI-1794313637）：续接会话只发反馈，模型沿用旧对话上下文里的路径，
// edit/write 落到宿主仓库 / 它认为的「项目目录」→ driver 在 worktree 侧永远采不到 diff，
// 连烧满重试轮。每轮显式重申唯一合法工作区 + 禁止 git 提交，才能把写盘锚死在 worktree。
func WorkdirGuard(workdir string) string {
	return "\n\n⚠️ 工作目录（本轮强制）：所有 read/write/edit 必须使用绝对路径 " + workdir +
		"/ 下的文件；该目录之外（包括仓库主检出、宿主 home、名字相同的其它路径）一律禁止读写。\n" +
		"禁止用 bash 执行 git add / git commit / git checkout 等任何改动仓库状态的命令（提交由流水线负责）；修改完文件直接结束即可。"
}

// BuildFixPrompt 组装「新建会话」的完整提示词（任务描述 + 参考文件 + 执行规则）。
func BuildFixPrompt(kind, task string, files []string) string {
	var sb strings.Builder
	if kind == "requirement" {
		sb.WriteString(requirementHeader)
		sb.WriteString(task)
		sb.WriteString("\n")
		if len(files) > 0 {
			sb.WriteString(requirementFilesHeader)
			sb.WriteString(strings.Join(files, "\n"))
			sb.WriteString("\n")
		}
		sb.WriteString(requirementRules)
		return sb.String()
	}
	sb.WriteString(bugHeader)
	sb.WriteString(task)
	sb.WriteString("\n")
	if len(files) > 0 {
		sb.WriteString(bugFilesHeader)
		sb.WriteString(strings.Join(files, "\n"))
		sb.WriteString("\n")
	}
	sb.WriteString(bugRules)
	return sb.String()
}

// BuildContinuePrompt 组装「续接会话」的提示词：只带上一轮反馈。
// pi 的 session 已保留任务描述与执行规则，重复发送纯属浪费（且 reasoning model 会重新解析）。
func BuildContinuePrompt(feedback string) string {
	if fb := strings.TrimSpace(feedback); fb != "" {
		return fb
	}
	return continuePromptFallback
}
