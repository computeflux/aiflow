// Package project 是 S9 项目注册表（docs/plan/S9-统一工作项流水线.md §7.8 项目注册）：
// 把仓库 `apps/` 下的每个目录注册成一个**项目**（1 项目 = 1 目录），作为工作项 / S1 进化链的
// **可执行目标**（work_items.target_name）。此前自动执行目标只认 skills 表（5 个内置技能），
// 指向 apps 目录的任务全部被打回人工；注册后 `apps/<dir>` ↔ 项目名，卡片 labels（如 `app,pycraper`）
// 与错误日志分诊（LLM 判定的 target_name）都能落到可执行目标上。
package project

import (
	"strings"
	"time"
)

// Project 是一个注册项目（1:1 对应 apps/ 下的一个目录）。
type Project struct {
	ID          uint   `gorm:"primaryKey" json:"id"`
	Name        string `gorm:"size:128;uniqueIndex;not null" json:"name"` // = 目录名（工作项 target_name）
	Dir         string `gorm:"size:255;not null" json:"dir"`              // 相对仓库根，如 apps/pycraper
	Runtime     string `gorm:"size:32;not null" json:"runtime"`           // go | python | node
	Image       string `gorm:"size:255" json:"image"`                     // 沙箱测试镜像（须在 SANDBOX_ALLOWED_IMAGES 白名单）
	TestCmd     string `gorm:"size:512" json:"test_cmd"`                  // 空 = 按 runtime 默认（见 DefaultTestCmd）
	Description string `gorm:"size:512" json:"description"`
	Active      bool   `gorm:"not null" json:"active"` // 停用 = 不作为自动执行目标（仍可在看板/工作项中引用）；
	// ⚠ 不能给 gorm tag 加 default:true——GORM 会把 false（零值）省略 → 落库拿到 DB 默认值 1，
	// 导致 board* 等默认停用的项目创建即启用（E2E 实证）。
	RegisteredAt time.Time `json:"registered_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// DefaultTestCmd 按 runtime 返回沙箱默认测试命令（在项目目录挂载根 /work 下执行）。
// 空返回 = 无默认（须在项目上配置 test_cmd）。
//
// python 约定同 skills：worktree 里 syncVenvs 已拷入 .venv + 自包含 python 树，
// uv 0.11+ 的 venv 只含 site-packages（无 stdlib）→ PYTHONHOME 指向树，PYTHONPATH 补 site-packages。
func DefaultTestCmd(runtime string) []string {
	switch runtime {
	case "go":
		return []string{"go", "test", "./..."}
	case "python":
		return []string{"sh", "-c", "PYTHONHOME=/work/.venv/python PYTHONPATH=/work/.venv/lib/python3.13/site-packages .venv/python/bin/python -m pytest -q"}
	case "node":
		return []string{"sh", "-c", "npm test --silent --if-present"}
	}
	return nil
}

// SandboxCmd 返回该项目在沙箱中执行的命令：test_cmd 覆盖优先，否则按 runtime 默认。
func (p *Project) SandboxCmd() []string {
	if cmd := strings.TrimSpace(p.TestCmd); cmd != "" {
		return []string{"sh", "-c", cmd}
	}
	return DefaultTestCmd(p.Runtime)
}
