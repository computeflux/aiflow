package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/workitem"
)

// board_ai_exec.go：/boards/:id/ai-execute 执行器。
//
// 2026-11-20：board AI 对话链路由 ai-flow 网关承接（/chat SSE），旧
// /boards/:id/ai-chat* 已删除。本文件是变更提案的**唯一落卡执行路径**：
// ai-flow board 通道的 mutation tools 只产出预览，最终由前端确认后 POST 到
// 这里执行（create_card / create_cards / update_card / move_card /
// delete_card / create_list）。
//
// 入参解析容错逻辑自旧 board_ai_args.go / board_ai_util.go 搬迁（历史来源说明；
// board_ai 链路已删除，本文件是 /ai-execute 唯一实现，沿用同一套 tool schema）。

// toString 安全取 string（nil / 非 string → ""）。
func toString(v any) string {
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// maxCardHours 是单张看板卡的工时上限（与 internal/product 的 maxTaskHours 同口径）。
const maxCardHours = 3.0

// formatHours 格式化预计工时（1.5 → "1.5"，2 → "2"）。
func formatHours(h float64) string {
	return strconv.FormatFloat(h, 'f', -1, 64)
}

func toFloat64(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

// parseCardsArg 解析 create_cards 的批量入参，容错三种形态：
//
//	cards_json: "[{...}]" / cards: "[{...}]" / cards: [{...}]
//
// 无论哪种，返回的都是 []any（元素为 map[string]any）。解析失败返回 nil。
func parseCardsArg(args map[string]any) []any {
	for _, key := range []string{"cards_json", "cards"} {
		v, ok := args[key]
		if !ok || v == nil {
			continue
		}
		switch t := v.(type) {
		case []any:
			return t
		case string:
			s := strings.TrimSpace(t)
			if s == "" {
				continue
			}
			// 容错：模型偶尔包一层 markdown 代码围栏
			s = strings.TrimPrefix(s, "```json")
			s = strings.TrimPrefix(s, "```")
			s = strings.TrimSuffix(s, "```")
			var arr []any
			if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &arr); err == nil {
				return arr
			}
		}
	}
	return nil
}

// normalizeChecklist 把模型给的各种 checklist 形态统一成看板期望的 `[{"text","done"}]` JSON 字符串。
// 接受：[{"text","done"}] / ["a","b"] / "[\"a\"]"（JSON 字符串）/ "- a\n- b"（纯文本行）。
func normalizeChecklist(v any) string {
	type item struct {
		Text string `json:"text"`
		Done bool   `json:"done"`
	}
	toItems := func(raw []any) []item {
		out := make([]item, 0, len(raw))
		for _, e := range raw {
			switch t := e.(type) {
			case string:
				if s := strings.TrimSpace(t); s != "" {
					out = append(out, item{Text: s})
				}
			case map[string]any:
				text := strings.TrimSpace(toString(t["text"]))
				if text == "" {
					continue
				}
				done, _ := t["done"].(bool)
				out = append(out, item{Text: text, Done: done})
			}
		}
		return out
	}
	marshal := func(items []item) string {
		if len(items) == 0 {
			return ""
		}
		b, err := json.Marshal(items)
		if err != nil {
			return ""
		}
		return string(b)
	}

	switch t := v.(type) {
	case nil:
		return ""
	case []any:
		return marshal(toItems(t))
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return ""
		}
		// ① 已经是 JSON（数组）→ 递归交给上面处理
		if strings.HasPrefix(s, "[") {
			var arr []any
			if err := json.Unmarshal([]byte(s), &arr); err == nil {
				return marshal(toItems(arr))
			}
		}
		// ② 纯文本：按行拆，去掉 markdown 的 - / * / 1. 前缀
		var raw []any
		for _, line := range strings.Split(s, "\n") {
			line = strings.TrimSpace(line)
			line = strings.TrimSpace(strings.TrimLeft(line, "-*•"))
			// 去掉 "1. " / "1) " 这类序号
			if i := strings.IndexAny(line, ".)"); i > 0 && i <= 3 {
				if _, err := strconv.Atoi(strings.TrimSpace(line[:i])); err == nil {
					line = strings.TrimSpace(line[i+1:])
				}
			}
			if line != "" {
				raw = append(raw, line)
			}
		}
		return marshal(toItems(raw))
	}
	return ""
}

// normalizeDependsOn 把模型给的依赖统一成 parseDependsOn 能读的字符串（"137,138"）。
func normalizeDependsOn(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			s := strings.TrimSpace(toString(e))
			if s == "" {
				s = formatHours(toFloat64(e)) // 数字卡片 ID
				if s == "0" {
					continue
				}
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, ",")
	case float64:
		return formatHours(t)
	}
	return ""
}

// parseIndexList 解析 depends_on_index：批次内 1-based 序号（不是卡 ID）。
// 容忍：[2,3] / "[2,3]" / "2,3" / "#2 #3" / 单个数字。
func parseIndexList(v any) []int {
	switch t := v.(type) {
	case nil:
		return nil
	case []any:
		out := make([]int, 0, len(t))
		for _, e := range t {
			if n := int(toFloat64(e)); n > 0 {
				out = append(out, n)
			}
		}
		return out
	case float64:
		if n := int(t); n > 0 {
			return []int{n}
		}
		return nil
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return nil
		}
		if strings.HasPrefix(s, "[") {
			var arr []any
			if json.Unmarshal([]byte(s), &arr) == nil {
				return parseIndexList(arr)
			}
		}
		var out []int
		for _, p := range strings.FieldsFunc(s, func(r rune) bool {
			return r == ',' || r == ' ' || r == '、' || r == ';'
		}) {
			p = strings.TrimLeft(strings.TrimSpace(p), "#")
			if n, err := strconv.Atoi(p); err == nil && n > 0 {
				out = append(out, n)
			}
		}
		return out
	}
	return nil
}

// mergeDependsOn 把已有依赖与新增依赖合并成去重后的 "[id1,id2]"。
func mergeDependsOn(existing string, add []uint) string {
	seen := map[uint]bool{}
	var ids []uint
	for _, id := range workitem.ParseDependsOn(existing) {
		if id > 0 && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, id := range add {
		if id > 0 && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatUint(uint64(id), 10)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// createCardFromAI 把一次 create_card / create_cards 的入参落成一张卡，并返回落库后的卡（含 ID）。
// 返回 ID 是为了 create_cards 的第二轮：同批卡建完后把 depends_on_index（批次内序号）回填成 depends_on。
func createCardFromAI(database *db.DB, listID uint, args map[string]any) (db.BoardCard, error) {
	card := db.BoardCard{
		ListID:      listID,
		Title:       toString(args["title"]),
		Description: toString(args["description"]),
		Priority:    toString(args["priority"]),
		Assignee:    toString(args["assignee"]),
		Labels:      toString(args["labels"]),
		TargetName:  toString(args["target_name"]),
		Checklist:   normalizeChecklist(args["checklist"]),
		DependsOn:   normalizeDependsOn(args["depends_on"]),
		Source:      "ai",
	}
	// 工时标注（≤3h 红线见 ai-flow 侧提示）：写给审核者看，避免一个需求整包塞进单卡。
	if h := toFloat64(args["estimate_hours"]); h > 0 && !strings.Contains(card.Description, "预计工时") {
		note := "预计工时：" + formatHours(h) + " 小时"
		if h > maxCardHours {
			note += "（⚠️ 超过 3 小时上限，请拆分为多张卡）"
		}
		if strings.TrimSpace(card.Description) == "" {
			card.Description = note
		} else {
			card.Description = strings.TrimSpace(card.Description) + "\n\n" + note
		}
	}
	if card.Priority == "" {
		card.Priority = "medium"
	}
	if due, ok := args["due_date"].(string); ok && due != "" {
		if t, err := time.Parse(time.RFC3339, due); err == nil {
			card.DueDate = &t
		}
	}
	return card, database.Create(&card).Error
}

// boardAIExecute 执行前端确认后的变更提案（逐条执行，失败收集进 errors，
// 全成 → {"status":"ok"}，有失败 → {"status":"partial","errors":[...]}）。
func boardAIExecute(database *db.DB) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		boardID, _ := strconv.Atoi(ctx.Param("id"))

		var input struct {
			ToolCalls []struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"tool_calls"`
		}
		if err := ctx.ShouldBindJSON(&input); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		errors := []string{}
		for _, tc := range input.ToolCalls {
			switch tc.Name {
			case "create_card":
				listID := uint(toFloat64(tc.Arguments["list_id"]))
				title := toString(tc.Arguments["title"])
				if listID == 0 || title == "" {
					errors = append(errors, "create_card: list_id and title required")
					continue
				}
				if _, err := createCardFromAI(database, listID, tc.Arguments); err != nil {
					errors = append(errors, "create_card: "+err.Error())
				}

			case "create_cards":
				defaultListID := uint(toFloat64(tc.Arguments["list_id"]))
				items := parseCardsArg(tc.Arguments)
				if len(items) == 0 {
					errors = append(errors, "create_cards: cards_json is empty or not a JSON array")
					continue
				}
				created := make([]db.BoardCard, len(items))
				valid := make([]bool, len(items))
				for i, raw := range items {
					item, ok := raw.(map[string]any)
					if !ok {
						errors = append(errors, fmt.Sprintf("create_cards[%d]: not an object", i))
						continue
					}
					listID := uint(toFloat64(item["list_id"]))
					if listID == 0 {
						listID = defaultListID
					}
					if listID == 0 {
						errors = append(errors, fmt.Sprintf("create_cards[%d]: list_id required", i))
						continue
					}
					if toString(item["title"]) == "" {
						errors = append(errors, fmt.Sprintf("create_cards[%d]: title required", i))
						continue
					}
					card, err := createCardFromAI(database, listID, item)
					if err != nil {
						errors = append(errors, fmt.Sprintf("create_cards[%d]: %v", i, err))
						continue
					}
					created[i] = card
					valid[i] = true
				}

				// 第二轮：回填同批依赖（depends_on_index → 真实卡 ID）。
				for i, raw := range items {
					if !valid[i] || created[i].ID == 0 {
						continue
					}
					item, _ := raw.(map[string]any)
					idxs := parseIndexList(item["depends_on_index"])
					if len(idxs) == 0 {
						continue
					}
					var add []uint
					for _, idx := range idxs {
						if idx == i+1 { // 自依赖忽略
							continue
						}
						if idx >= 1 && idx <= len(created) && valid[idx-1] && created[idx-1].ID != 0 {
							add = append(add, created[idx-1].ID)
						}
					}
					if len(add) == 0 {
						continue
					}
					merged := mergeDependsOn(created[i].DependsOn, add)
					if err := database.Model(&db.BoardCard{}).Where("id = ?", created[i].ID).
						Update("depends_on", merged).Error; err != nil {
						errors = append(errors, fmt.Sprintf("create_cards[%d]: set depends_on: %v", i, err))
					}
				}

			case "update_card":
				cardID := uint(toFloat64(tc.Arguments["card_id"]))
				if cardID == 0 {
					errors = append(errors, "update_card: card_id required")
					continue
				}
				updates := map[string]any{"updated_at": time.Now()}
				for _, f := range []string{"title", "description", "priority", "assignee", "labels", "target_name", "checklist", "depends_on"} {
					if v := toString(tc.Arguments[f]); v != "" {
						updates[f] = v
					}
				}
				if due, ok := tc.Arguments["due_date"].(string); ok && due != "" {
					if t, err := time.Parse(time.RFC3339, due); err == nil {
						updates["due_date"] = t
					}
				}
				database.Model(&db.BoardCard{}).Where("id = ?", cardID).Updates(updates)

			case "move_card":
				cardID := uint(toFloat64(tc.Arguments["card_id"]))
				targetListID := uint(toFloat64(tc.Arguments["target_list_id"]))
				if cardID == 0 || targetListID == 0 {
					errors = append(errors, "move_card: card_id and target_list_id required")
					continue
				}
				database.Model(&db.BoardCard{}).Where("id = ?", cardID).Update("list_id", targetListID)

			case "delete_card":
				cardID := uint(toFloat64(tc.Arguments["card_id"]))
				if cardID == 0 {
					errors = append(errors, "delete_card: card_id required")
					continue
				}
				database.Where("card_id = ?", cardID).Delete(&db.BoardComment{})
				database.Delete(&db.BoardCard{}, cardID)

			case "create_list":
				name := toString(tc.Arguments["name"])
				if name == "" {
					errors = append(errors, "create_list: name required")
					continue
				}
				var maxPos int
				database.Model(&db.BoardList{}).Where("board_id = ?", boardID).
					Select("COALESCE(MAX(position), -1)").Scan(&maxPos)
				database.Create(&db.BoardList{
					BoardID:  uint(boardID),
					Name:     name,
					Position: maxPos + 1,
				})

			default:
				errors = append(errors, fmt.Sprintf("unknown tool: %s", tc.Name))
			}
		}

		if len(errors) > 0 {
			ctx.JSON(http.StatusOK, gin.H{"status": "partial", "errors": errors})
		} else {
			ctx.JSON(http.StatusOK, gin.H{"status": "ok"})
		}
	}
}
