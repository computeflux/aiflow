// Package skillcontract 固化「skill 统一调用契约」的 Go 侧定义
// （P1 Skill 框架，docs/plan/transform-plan.md §6.1 / §6.4）。
//
// 为什么要有这个包：skill 的输入输出契约只写在各自的 skill.json 里（Python / Go /
// cli 三种 runtime 各写各的），调用方（Temporal workflow、board 手动调用、agent）
// 于是各拼各的 map[string]any —— 字段名漂一个字母，线上才会以「skill 报失败」的形式
// 暴露。本包把契约的**共性部分**收在 Go 侧一处：
//
//   - InvokeInput：所有 skill 输入都是 JSON object，这里给一个带类型的构造器；
//   - Output：所有 skill 输出必须含布尔 ok（skillgateway.Runner 据此判定 skill_runs
//     状态并决定熔断计数），这里给出解析 + 判定 + 失败原因提取；
//   - LikeInput：like 能力（apps/pyreplay/skill.json）的输入构造器 —— 字段名与取值
//     口径（like_count 缺省 1、上限 20）与 pyreplay 的 skill schema 逐字对齐，
//     Go 侧调用点从此不再手写字面量键名。
//
// 本包刻意零依赖（stdlib only）：workflows / activities / cmd 各层都能引，不会把
// db、temporal 等重依赖拖进契约层。schema 的**校验**仍在 skillgateway（读库里的
// input_schema/output_schema），这里只保证「构造出来的形状符合契约」。
package skillcontract

import "fmt"

// like 能力的输入字段名（与 apps/pyreplay/skill.json 的 input_schema.properties 一致）。
const (
	// FieldPostURL 是推文/帖子 URL（like skill 的必填输入）。
	FieldPostURL = "post_url"
	// FieldLikeCount 是同一推文的点赞次数（用不同账号各点一次）。
	FieldLikeCount = "like_count"
	// FieldOK 是统一输出契约的布尔状态位（skillgateway 依它判定调用结果）。
	FieldOK = "ok"
	// FieldReason 是失败原因（业务失败时由 skill 填写，落 skill_runs.error）。
	FieldReason = "reason"

	// LikeCountDefault 是 like_count 的缺省值（与 skill.json 的默认口径一致）。
	LikeCountDefault = 1
	// LikeCountMax 是 like_count 的上限（与 skill.json 的 maximum 一致）。
	LikeCountMax = 20
)

// InvokeInput 是一次 skill 调用的输入：JSON object（键 = skill.json input_schema 的属性名）。
type InvokeInput map[string]any

// NewInvokeInput 构造一个空输入（零依赖调用方写 map 字面量时的类型锚点）。
func NewInvokeInput() InvokeInput { return InvokeInput{} }

// With 链式写入一个字段，返回自身便于串起来构造输入。
func (in InvokeInput) With(key string, value any) InvokeInput {
	in[key] = value
	return in
}

// Map 返回底层 map（传给 InvokeSkill / skillgateway 时使用）。
func (in InvokeInput) Map() map[string]any { return in }

// Output 是一次 skill 调用的输出：JSON object（含统一契约要求的布尔 ok）。
type Output map[string]any

// OK 返回 skill 自报的成功位；字段缺失或类型不对时返回 false（视为失败）。
func (out Output) OK() bool {
	v, ok := out[FieldOK].(bool)
	return ok && v
}

// Reason 返回 skill 自报的失败原因（无则空串）。
func (out Output) Reason() string {
	if v, ok := out[FieldReason].(string); ok {
		return v
	}
	return ""
}

// Str 读取一个字符串字段（缺失/类型不对 = 空串）。
func (out Output) Str(key string) string {
	if v, ok := out[key].(string); ok {
		return v
	}
	return ""
}

// Int 读取一个整数字段：JSON 数字经 encoding/json 解码可能是 float64 / json.Number /
// int，这里统一收口；读不出整数时返回 fallback。
func (out Output) Int(key string, fallback int) int {
	switch v := out[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case string:
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return fallback
}

// LikeInput 构造 like skill（apps/pyreplay/skill.json）的输入。
//
// likeCount 由调用侧规整：<=0 → 缺省 1，>LikeCountMax → 上限 20（与 pyreplay 侧的
// clamp_like_count 完全同口径，两侧任何一侧改动都会让单测红）。postURL 为空时不填
// like_count 之外的任何默认值：必填校验交给 skillgateway 的 input_schema（400），
// 好让 rejected 记录带 schema 给出的可读错误，而不是被 Go 侧静默改成空跑。
func LikeInput(postURL string, likeCount int) InvokeInput {
	in := NewInvokeInput().With(FieldPostURL, postURL)
	in.With(FieldLikeCount, ClampLikeCount(likeCount))
	return in
}

// ClampLikeCount 把点赞次数规整到 [LikeCountDefault, LikeCountMax]。
func ClampLikeCount(n int) int {
	if n < LikeCountDefault {
		return LikeCountDefault
	}
	if n > LikeCountMax {
		return LikeCountMax
	}
	return n
}
