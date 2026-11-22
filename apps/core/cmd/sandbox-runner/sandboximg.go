// Package sandboximg 是沙箱镜像白名单匹配（evolve-driver 与 sandbox-runner **两端共用**，口径必须一致）。
//
// 背景：S5 灰度版本容器的镜像 tag 是**版本化**的（如 pyreplay:evolve-card-20-1790984255），
// 而白名单常配成 `pyreplay:latest` → 灰度上线过的技能在沙箱里永远匹配不上（2026-10-08 实测）。
// 为此白名单支持 **tag 通配**：`pyreplay:*` 命中 pyreplay 仓库的任意 tag（含 latest 与版本化 tag）。
package main

import "strings"

// Split 把逗号分隔的白名单字符串拆为条目（去空白/空项）。
func sandboximgSplit(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Allowed 判断 img 是否命中白名单条目：
//   - 精确匹配：`pyreplay:latest` == `pyreplay:latest`；
//   - tag 通配：`pyreplay:*` 命中 `pyreplay:任意 tag`（含版本化 tag）。
func sandboximgAllowed(entries []string, img string) bool {
	img = strings.TrimSpace(img)
	if img == "" {
		return false
	}
	for _, a := range entries {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if a == img {
			return true
		}
		if strings.HasSuffix(a, ":*") && strings.HasPrefix(img, strings.TrimSuffix(a, "*")) {
			return true
		}
	}
	return false
}
