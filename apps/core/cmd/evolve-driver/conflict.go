package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wetee/ai-workflow/internal/llm"
)

// 合并冲突自动解决（2026-10-31）
//
// 场景：evolve 分支落后于 base（main 前进了），mergeOneRepo 会把 base 合入 evolve 分支。
// 两边改了同一段代码 → 冲突。旧行为是 `git merge --abort` 后报错，任务无限重试（每分钟一次）。
//
// 现在：对**文本**冲突逐个文件让 LLM 做三方合并（保留双方意图），校验产物无冲突标记后
// 提交该次合并，让流程继续。这是「分支吸收 base」方向，主线只在最终 CAS 快进时才推进，
// 所以即使解决得不好，也不会写到 main 之外的地方，且全程有 audit 留痕。
//
// 红线：
//   - 冲突文件数超过 ConflictMaxFiles（默认 5）→ 直接交人工；
//   - 二进制 / 超大文件 → 交人工；
//   - 模型输出残留冲突标记或为空 → 交人工；
//   - 任何异常都回退到「abort + 报错」，绝不静默取舍。

const (
	conflictMaxFileBytes = 200 << 10
)

const conflictSystemPrompt = "你是 git 合并冲突解决专家。输入是一个含冲突标记（<<<<<<< / ======= / >>>>>>>）的文件。\n" +
	"任务：输出**解决冲突后的完整文件内容**。\n" +
	"规则：\n" +
	"1) 保留双方意图：branch 侧（HEAD/ours）与 base 侧（theirs）的独有逻辑都要保留，能合并就合并；\n" +
	"2) 同一位置语义冲突时，选择与本文件其余部分风格/结构一致、且不破坏现有调用方的写法；\n" +
	"3) 不要删除任何一方独有的功能，不要顺手重构无关代码；\n" +
	"4) 绝对不要把冲突标记留在输出里；\n" +
	"5) 只输出文件内容本身，不要 markdown 代码围栏、不要解释。\n" +
	"如果需要判断上下文，可以阅读仓库里的其他文件（不要修改其他文件）。\n" +
	"允许调用 read_file / list_dir / search_code 只读工具。"

// newConflictLLM 构造冲突解决用的 LLM 客户端（未配置 → nil，直接走人工）。
func newConflictLLM(cfg *Config) *llm.Client {
	if strings.TrimSpace(cfg.ConflictLLMBaseURL) == "" || strings.TrimSpace(cfg.ConflictLLMModel) == "" {
		return nil
	}
	return llm.NewClient(cfg.ConflictLLMAPIKey, cfg.ConflictLLMBaseURL, "", cfg.ConflictLLMModel)
}

// hasConflictMarkers 判断内容里是否还有未解决的冲突标记（行首 7 个 <> 或 ======= 分隔）。
func hasConflictMarkers(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimRight(line, "\r")
		if strings.HasPrefix(t, "<<<<<<<") || strings.HasPrefix(t, ">>>>>>>") {
			return true
		}
		// "=======" 单独一行（7 个等号）是分隔符；代码里的 ==== 比较不受影响
		if strings.TrimSpace(t) == "=======" {
			return true
		}
	}
	return false
}

// isBinaryContent 前 8KB 含 NUL 视为二进制。
func isBinaryContent(data []byte) bool {
	n := len(data)
	if n > 8192 {
		n = 8192
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}

// resolveConflictFile 让 LLM 合并单个文件；返回解决后的完整内容。
func resolveConflictFile(ctx context.Context, client *llm.Client, relPath, content string) (string, error) {
	user := fmt.Sprintf("文件路径：%s\n\n===== 含冲突标记的内容开始 =====\n%s\n===== 含冲突标记的内容结束 =====", relPath, content)
	raw, err := client.ChatCompletion(ctx, conflictSystemPrompt, user)
	if err != nil {
		return "", err
	}
	out := stripCodeFence(raw)
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("模型返回空内容")
	}
	if hasConflictMarkers(out) {
		return "", fmt.Errorf("模型输出仍含冲突标记")
	}
	return out, nil
}

// stripCodeFence 去掉模型可能加的 ```lang ... ``` 围栏。
func stripCodeFence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return s
	}
	if i := strings.Index(t, "\n"); i >= 0 {
		t = t[i+1:]
	}
	if j := strings.LastIndex(t, "```"); j >= 0 {
		t = t[:j]
	}
	return t
}

// conflictTimeout 单个文件的解决超时（默认 15 分钟）。
// 3 分钟对 60KB 级文件不够：本地 27B 模型要逐 token 重写整个文件。
func (d *driver) conflictTimeout() time.Duration {
	sec := d.cfg.ConflictTimeoutSec
	if sec <= 0 {
		sec = 900
	}
	return time.Duration(sec) * time.Second
}

// autoResolveMergeConflicts 在 workdir 当前合并状态下自动解决冲突文件。
// 成功 → 返回已解决的文件列表（调用方负责 git add + commit）；失败 → 返回错误（调用方 abort）。
func (d *driver) autoResolveMergeConflicts(ctx context.Context, workdir string, files []string) ([]string, error) {
	if !d.cfg.AutoResolveConflicts {
		return nil, fmt.Errorf("自动解决已关闭（EVOLVE_AUTO_RESOLVE_CONFLICTS=false）")
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("没有冲突文件")
	}
	if len(files) > d.cfg.ConflictMaxFiles {
		return nil, fmt.Errorf("冲突文件 %d 个，超过自动解决上限 %d 个", len(files), d.cfg.ConflictMaxFiles)
	}
	client := newConflictLLM(d.cfg)
	if client == nil {
		return nil, fmt.Errorf("未配置 LLM（OPENAI_BASE_URL / LLM_MODEL），无法自动解决")
	}

	ctxL, cancel := context.WithTimeout(ctx, d.conflictTimeout())
	defer cancel()

	resolved := make([]string, 0, len(files))
	for _, rel := range files {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			continue
		}
		abs := filepath.Join(workdir, rel)
		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("读取 %s: %v", rel, err)
		}
		if len(data) > conflictMaxFileBytes {
			return nil, fmt.Errorf("%s 过大（%d 字节 > %d），交人工", rel, len(data), conflictMaxFileBytes)
		}
		if isBinaryContent(data) {
			return nil, fmt.Errorf("%s 是二进制文件，交人工", rel)
		}
		if !hasConflictMarkers(string(data)) {
			continue // 已被 git 标记为冲突但内容无标记（如 delete/modify）→ 交人工更稳
		}
		out, rerr := resolveConflictFile(ctxL, client, rel, string(data))
		if rerr != nil {
			return nil, fmt.Errorf("解决 %s: %v", rel, rerr)
		}
		// 保留原文件权限位
		mode := os.FileMode(0o644)
		if st, serr := os.Stat(abs); serr == nil {
			mode = st.Mode()
		}
		if err := os.WriteFile(abs, []byte(out), mode); err != nil {
			return nil, fmt.Errorf("写回 %s: %v", rel, err)
		}
		resolved = append(resolved, rel)
	}
	if len(resolved) == 0 {
		return nil, fmt.Errorf("没有可自动解决的冲突文件（可能都是 delete/modify 类冲突）")
	}
	return resolved, nil
}

// conflictAuditDetail 序列化解决方案摘要（写 audit_logs 用）。
func conflictAuditDetail(base string, files, resolved []string) string {
	b, _ := json.Marshal(map[string]any{
		"base": base, "conflict_files": files, "resolved_files": resolved,
	})
	return string(b)
}
