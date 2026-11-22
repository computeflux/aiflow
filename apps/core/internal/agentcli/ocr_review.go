package agentcli

import (
	"encoding/json"
	"strings"
)

// ocr_review.go：ocr reviewer 输出解析（manifest / comments 归一 / 去重 / 严重度映射）。
// 自 ocr.go 拆出（批次 F，纯机械搬迁，行为无变更）。

const (
	commentMessageMax    = 4000 // 单条评论正文上限（字符）
	commentSuggestionMax = 1500 // 附加「建议改法」上限（字符）
)

// ocrLevelMap 严重度归一：ocr 各版本词表不一（fatal/critical/high/medium/low/...），
// 协议只认 error|warning|info → 未识别一律降级 info，保证 driver 侧 schema 不漂移；
// 高严重度归 error，保留 driver「error 级即打回 coder 重修」语义。
var ocrLevelMap = map[string]string{
	"error": "error", "fatal": "error", "critical": "error", "high": "error",
	"blocker": "error", "block": "error", "bug": "error", "severe": "error",
	"major": "error", "p0": "error", "p1": "error",
	"warning": "warning", "warn": "warning", "medium": "warning",
	"moderate": "warning", "minor": "warning", "low": "warning",
	"p2": "warning", "p3": "warning",
	"info": "info", "informational": "info", "note": "info", "nit": "info",
	"style": "info", "suggestion": "info", "cosmetic": "info", "p4": "info",
}

// ocrCommentTextKeys 正文取键顺序。
//
// ⚠️ ocr v1.13 起正文在 content（旧版 message/comment）——取错键 = 评论 message 全空，
// driver 会把「空文本」的返工反馈发给 coder，白烧一轮（2026-10-17 实测事故）。
// 故逐个尝试，并对无法识别的结构做 JSON 兜底，任何情况下不丢信息。
var ocrCommentTextKeys = []string{"content", "message", "comment", "body", "summary", "description", "title"}

// ocrCommentListKeys 评论数组的候选键。
var ocrCommentListKeys = []string{"comments", "issues", "findings", "problems", "violations", "review_comments"}

// ocrRunManifest 识别并解析 ocr v1.13 的 run manifest（--format json 的顶层结构）：
// {"status":"skipped|completed|failed", "comments":[...], "manifest":{"coverage":{"failed":[...]}}}
// 返回 (status, 失败评审单元数, 是否 manifest 结构)。
func ocrRunManifest(raw string) (string, int, bool) {
	var data struct {
		Status   string `json:"status"`
		Manifest *struct {
			Coverage struct {
				Failed []any `json:"failed"`
			} `json:"coverage"`
		} `json:"manifest"`
	}
	if json.Unmarshal([]byte(raw), &data) != nil || data.Status == "" {
		return "", 0, false
	}
	failed := 0
	if data.Manifest != nil {
		failed = len(data.Manifest.Coverage.Failed)
	}
	return data.Status, failed, true
}

// parseOCRComments 把 ocr 输出映射为协议评论（跨版本字段漂移的归一入口）。
func parseOCRComments(raw string) []Comment {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return []Comment{}
	}
	var data any
	if err := json.Unmarshal([]byte(trimmed), &data); err != nil {
		// 非 JSON 输出（如纯文本报告）→ 整体包装为一条 info 评论
		return []Comment{{Level: "info", Message: truncate(trimmed, commentMessageMax)}}
	}

	var items []any
	foundList := false
	switch v := data.(type) {
	case map[string]any:
		for _, key := range ocrCommentListKeys {
			if arr, ok := v[key].([]any); ok {
				items, foundList = arr, true
				break
			}
		}
	case []any:
		items, foundList = v, true
	}

	comments := make([]Comment, 0, len(items))
	for _, item := range items {
		if c, ok := normOCRComment(item); ok {
			comments = append(comments, c)
		}
	}
	// 识别出 comments 数组就是权威结果（哪怕是空的 = 评审完无问题，不再塞兜底评论）；
	// 仅当结构完全认不出时才原样包一条 info，保证不丢信息。
	if !foundList {
		b, _ := jsonCompact(data)
		comments = []Comment{{Level: "info", Message: truncate(b, commentMessageMax)}}
	}
	return dedupComments(comments)
}

// normOCRComment 单条评论 → 协议结构（file/line/level/message）。
func normOCRComment(item any) (Comment, bool) {
	switch v := item.(type) {
	case string:
		return Comment{Level: "info", Message: truncate(strings.TrimSpace(v), commentMessageMax)}, true
	case map[string]any:
		msg := ""
		for _, key := range ocrCommentTextKeys {
			if s := text(v[key]); s != "" {
				msg = s
				break
			}
		}
		if msg == "" {
			msg = truncate(mustJSON(v), commentMessageMax)
		}
		if cat := text(v["category"]); cat != "" {
			msg = "[" + cat + "] " + msg
		}
		if sugg := text(v["suggestion_code"]); sugg != "" {
			msg += "\n\n建议改法：\n```\n" + truncate(sugg, commentSuggestionMax) + "\n```"
		}
		level := firstNonEmpty(text(v["level"]), text(v["severity"]))
		return Comment{
			File:    firstNonEmpty(text(v["file"]), text(v["path"])),
			Line:    firstNonZero(toInt(v["line"]), toInt(v["start_line"])),
			Level:   normOCRLevel(level),
			Message: truncate(msg, commentMessageMax),
		}, true
	}
	return Comment{}, false
}

// normOCRLevel 严重度归一（未识别降级 info）。
func normOCRLevel(level string) string {
	if l, ok := ocrLevelMap[strings.ToLower(strings.TrimSpace(level))]; ok {
		return l
	}
	return "info"
}

// dedupComments 多轮 review 会重复报同一条 → 去重（coder 反馈里重复项纯属噪音）。
func dedupComments(comments []Comment) []Comment {
	seen := map[Comment]bool{}
	out := make([]Comment, 0, len(comments))
	for _, c := range comments {
		if seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}
