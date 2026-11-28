package agentcli

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// coder 会话的工具预设名：直接交给 pi-web 解析（唯一真源
// apps/pi-web/lib/tool-presets.ts），adapter 不再自己维护工具数组。
//
// 为什么默认 full：看板调度（work-dispatch → evolve_tasks → evolve-driver →
// pi adapter）是无人值守链路，缺 grep/find/ls 等内置工具时 coder 只能用 bash
// 硬凑，实测出现 1000+ 次工具调用、几乎零写文件的只读空转。
//
// 可用 PI_TOOL_PRESET 覆盖（none / read-only / default / full），非法值回落 full。
var piToolPresetNames = map[string]bool{
	"none": true, "read-only": true, "default": true, "full": true,
}

const piDefaultToolPreset = "full"

// LoadCoderToolPreset 返回本次 coder 会话使用的工具预设名（永远非空）。
func LoadCoderToolPreset(logw io.Writer) string {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("PI_TOOL_PRESET")))
	if raw == "" {
		return piDefaultToolPreset
	}
	if !piToolPresetNames[raw] {
		fmt.Fprintf(logw, "tool-preset: PI_TOOL_PRESET=%q 非法（可选 none/read-only/default/full），回落 %s\n",
			raw, piDefaultToolPreset)
		return piDefaultToolPreset
	}
	return raw
}
