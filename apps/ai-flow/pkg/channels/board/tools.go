// Package board — declarative mutation tools (P3-1).
//
// The board chat channel exposes the legacy board mutation tool surface
// (create_card / create_cards / update_card / move_card / delete_card /
// create_list) with the SAME schemas as workflow-api's legacy
// /boards/:id/ai-chat/stream (apps/core/cmd/api/board_ai_tools.go), so the
// frontend keeps rendering confirmation cards and lands changes through
// /ai-execute after human confirmation.
//
// These tools never write: Execute renders a human-readable preview (for the
// model to explain to the user) and records the concrete call on the
// request-scoped *Turn (from context) — the HTTP handler reads the captured
// calls and ships them in the final `result` frame. Deletions are refused
// outright on this surface (the P4 workflow tools remain the only delete
// path, with role gating).
package board

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/computeflux/ai-flow/pkg/tools"
)

// MutationToolNames lists the declarative board mutation tool names.
var MutationToolNames = []string{
	"create_card",
	"create_cards",
	"update_card",
	"move_card",
	"delete_card",
	"create_list",
}

// IsMutationTool reports whether a tool name is a board mutation tool.
func IsMutationTool(name string) bool {
	for _, n := range MutationToolNames {
		if n == name {
			return true
		}
	}
	return false
}

type mutationTool struct {
	name        string
	description string
	params      map[string]any
	preview     func(args map[string]any) string
}

func (t *mutationTool) Name() string               { return t.name }
func (t *mutationTool) Description() string        { return t.description }
func (t *mutationTool) Parameters() map[string]any { return t.params }

func (t *mutationTool) Execute(ctx context.Context, args map[string]any) *tools.ToolResult {
	if t.name == "delete_card" {
		// Deletions are refused on this surface: the P4 workflow write tools
		// (role-gated, approval-backed) remain the only delete path.
		return tools.ErrorResult(t.name + ": 删除操作请通过工作流写工具（审批链）执行，AI 侧栏不支持删卡。")
	}
	preview := t.preview(args)
	if preview == "" {
		return tools.ErrorResult(t.name + ": invalid arguments")
	}
	// Capture the concrete call on the request-scoped turn so the HTTP
	// handler can ship it to the frontend for confirmation. Tool executions
	// always run with the turn context (pipeline sets it), so capture here
	// is race-free with respect to turn finalization.
	if turn := TurnFromContext(ctx); turn != nil {
		turn.AddCall(t.name, args)
	} else if sessionKey := tools.ToolSessionKey(ctx); sessionKey != "" {
		// Tool executions run with the pipeline's execution context; in the
		// gateway the turn travels on the inbound message (not the tool
		// ctx), so fall back to resolving the board turn from the session.
		if ch := DefaultChannel(); ch != nil {
			if bt := ch.BoardTurnForSession(sessionKey); bt != nil {
				bt.AddCall(t.name, args)
			}
		}
	}
	// Silent: the preview goes to the LLM (which must explain it to the
	// user); the HTTP layer ships the call to the frontend for confirmation.
	return tools.SilentResult(fmt.Sprintf(
		"%s 预览（未执行）：%s\n请向用户完整解释以上将要执行的变更，并明确告知：回复确认后由页面「确认执行」落卡。在用户确认前不要重复调用该工具。",
		t.name, preview,
	))
}

// NewMutationTools builds the declarative mutation tool set.
func NewMutationTools() []tools.Tool {
	return []tools.Tool{
		&mutationTool{
			name:        "create_card",
			description: "Create a new card in a list (preview only; lands after the user confirms on the board page).",
			params: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"list_id":        map[string]any{"type": "number", "description": "Target list ID"},
					"title":          map[string]any{"type": "string", "description": "Card title"},
					"description":    map[string]any{"type": "string", "description": "Optional description"},
					"priority":       map[string]any{"type": "string", "enum": []string{"low", "medium", "high", "urgent"}},
					"assignee":       map[string]any{"type": "string"},
					"labels":         map[string]any{"type": "string", "description": "Comma-separated display labels"},
					"target_name":    map[string]any{"type": "string", "description": "Target project/skill name (apps/<dir> or skill)"},
					"due_date":       map[string]any{"type": "string", "description": "ISO date"},
					"checklist":      map[string]any{"type": "string", "description": "REQUIRED. 3-8 concrete steps as a JSON array string, e.g. '[\"a\",\"b\"]' or '[{\"text\":\"a\",\"done\":false}]'. A card without checklist is incomplete."},
					"depends_on":     map[string]any{"type": "string", "description": "Optional prerequisite card IDs, JSON array like '[137]'. ALWAYS set it when this card genuinely cannot start before another card is done."},
					"estimate_hours": map[string]any{"type": "number", "description": "Estimated engineer hours for this card. MUST be <=3; if the work is larger, split it into multiple cards instead."},
				},
				"required": []string{"list_id", "title"},
			},
			preview: previewCreateCard,
		},
		&mutationTool{
			name: "create_cards",
			description: "Create MULTIPLE cards in one call (preview only; lands after the user confirms). Use this instead of calling create_card repeatedly when a document/plan lists many tasks: " +
				"one call carrying all cards. Each item has the same fields as create_card; list_id may be omitted to use the current target list.",
			params: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"list_id": map[string]any{"type": "number", "description": "Default list for cards that omit list_id"},
					"cards_json": map[string]any{
						"type":        "string",
						"description": "JSON array of card objects, e.g. [{\"title\":\"...\",\"description\":\"...\",\"priority\":\"high\",\"target_name\":\"core\",\"estimate_hours\":2,\"checklist\":[\"step1\",\"step2\",\"step3\"]}]. Each object: title(REQUIRED), description, priority(low|medium|high|urgent), assignee, labels, target_name, due_date, estimate_hours(<=3), list_id(optional override), checklist(REQUIRED, 3-8 steps; array or JSON-array string), depends_on(existing card IDs only, array or JSON-array string), depends_on_index(1-based indexes INTO THIS ARRAY for cards created in the same call, e.g. [1,2]).",
					},
				},
				"required": []string{"cards_json"},
			},
			preview: previewCreateCards,
		},
		&mutationTool{
			name:        "update_card",
			description: "Update a card. checklist field accepts JSON array [{text:'',done:false}]. Use this for syncing description items to checklist. (preview only; lands after the user confirms)",
			params: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"card_id":     map[string]any{"type": "number"},
					"title":       map[string]any{"type": "string"},
					"description": map[string]any{"type": "string"},
					"priority": map[string]any{"type": "string",
						"enum": []string{"low", "medium", "high", "urgent"}},
					"assignee":    map[string]any{"type": "string"},
					"labels":      map[string]any{"type": "string"},
					"target_name": map[string]any{"type": "string", "description": "Target project/skill name (apps/<dir> or skill)"},
					"due_date":    map[string]any{"type": "string"},
					"checklist":   map[string]any{"type": "string", "description": "JSON array of {text,done}"},
					"depends_on":  map[string]any{"type": "string", "description": "Prerequisite card IDs, JSON array like '[137]'. Empty string clears the dependency."},
				},
				"required": []string{"card_id"},
			},
			preview: previewUpdateCard,
		},
		&mutationTool{
			name:        "move_card",
			description: "Move a card to a different list (preview only; lands after the user confirms).",
			params: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"card_id":        map[string]any{"type": "number"},
					"target_list_id": map[string]any{"type": "number"},
				},
				"required": []string{"card_id", "target_list_id"},
			},
			preview: func(args map[string]any) string {
				cardID := argNum(args, "card_id")
				target := argNum(args, "target_list_id")
				if cardID == 0 || target == 0 {
					return ""
				}
				return fmt.Sprintf("将卡片 #%d 移动到列 #%d", int(cardID), int(target))
			},
		},
		&mutationTool{
			name:        "delete_card",
			description: "Delete a card (NOT supported on this surface; use the workflow write tools with approval).",
			params: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"card_id": map[string]any{"type": "number"},
					"reason":  map[string]any{"type": "string"},
				},
				"required": []string{"card_id"},
			},
			preview: nil, // Execute refuses deletions before preview.
		},
		&mutationTool{
			name:        "create_list",
			description: "Create a new board column/list (NOT checklist items). Checklist items go into update_card.checklist (preview only; lands after the user confirms).",
			params: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string"},
				},
				"required": []string{"name"},
			},
			preview: func(args map[string]any) string {
				name := argStr(args, "name")
				if name == "" {
					return ""
				}
				return fmt.Sprintf("新建列「%s」", truncate(name, 60))
			},
		},
	}
}

// ---------------------------------------------------------------------------
// previews
// ---------------------------------------------------------------------------

func previewCreateCard(args map[string]any) string {
	listID := argNum(args, "list_id")
	title := argStr(args, "title")
	if listID == 0 || title == "" {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "在列 #%d 新建卡片「%s」", int(listID), truncate(title, 80))
	if desc := argStr(args, "description"); desc != "" {
		fmt.Fprintf(&sb, "；描述=%s", truncate(desc, 120))
	}
	if p := argStr(args, "priority"); p != "" {
		fmt.Fprintf(&sb, "；优先级=%s", p)
	}
	if t := argStr(args, "target_name"); t != "" {
		fmt.Fprintf(&sb, "；目标=%s", t)
	}
	if steps := parseChecklistSteps(args["checklist"]); len(steps) > 0 {
		fmt.Fprintf(&sb, "；检查项 %d 条", len(steps))
	}
	if h := argNum(args, "estimate_hours"); h > 0 {
		fmt.Fprintf(&sb, "；预估 %g 小时", h)
	}
	return sb.String()
}

func previewCreateCards(args map[string]any) string {
	cards := parseCardsJSON(args["cards_json"])
	if len(cards) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "批量新建 %d 张卡片：", len(cards))
	shown := cards
	if len(shown) > 5 {
		shown = shown[:5]
	}
	for i, card := range shown {
		title := argStr(card, "title")
		if title == "" {
			continue
		}
		if i > 0 {
			sb.WriteString("；")
		}
		fmt.Fprintf(&sb, "「%s」", truncate(title, 40))
		if steps := parseChecklistSteps(card["checklist"]); len(steps) > 0 {
			fmt.Fprintf(&sb, "(检查项 %d 条)", len(steps))
		}
	}
	if len(cards) > 5 {
		fmt.Fprintf(&sb, "；…共 %d 张", len(cards))
	}
	return sb.String()
}

func previewUpdateCard(args map[string]any) string {
	cardID := argNum(args, "card_id")
	if cardID == 0 {
		return ""
	}
	var parts []string
	if title := argStr(args, "title"); title != "" {
		parts = append(parts, fmt.Sprintf("标题=「%s」", truncate(title, 60)))
	}
	if desc := argStr(args, "description"); desc != "" {
		parts = append(parts, fmt.Sprintf("描述=%s", truncate(desc, 100)))
	}
	if p := argStr(args, "priority"); p != "" {
		parts = append(parts, fmt.Sprintf("优先级=%s", p))
	}
	if a := argStr(args, "assignee"); a != "" {
		parts = append(parts, fmt.Sprintf("负责人=%s", a))
	}
	if l := argStr(args, "labels"); l != "" {
		parts = append(parts, fmt.Sprintf("标签=%s", l))
	}
	if t := argStr(args, "target_name"); t != "" {
		parts = append(parts, fmt.Sprintf("目标=%s", t))
	}
	if d := argStr(args, "due_date"); d != "" {
		parts = append(parts, fmt.Sprintf("截止=%s", d))
	}
	if steps := parseChecklistSteps(args["checklist"]); len(steps) > 0 {
		parts = append(parts, fmt.Sprintf("检查项 %d 条", len(steps)))
	}
	if dep, ok := args["depends_on"]; ok {
		if s, isStr := dep.(string); isStr {
			if s == "" {
				parts = append(parts, "清空依赖")
			} else {
				parts = append(parts, fmt.Sprintf("依赖=%s", truncate(s, 40)))
			}
		} else {
			parts = append(parts, "依赖=已指定")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("更新卡片 #%d：%s", int(cardID), strings.Join(parts, "；"))
}

// ---------------------------------------------------------------------------
// arg helpers (mirror pkg/tools/workflow semantics without importing it)
// ---------------------------------------------------------------------------

// argNum extracts a numeric argument (JSON numbers decode to float64).
func argNum(args map[string]any, key string) float64 {
	switch v := args[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		n, err := v.Float64()
		if err != nil {
			return 0
		}
		return n
	case string:
		var n float64
		if _, err := fmt.Sscanf(v, "%g", &n); err == nil {
			return n
		}
	}
	return 0
}

// argStr extracts a string argument (empty when absent).
func argStr(args map[string]any, key string) string {
	switch v := args[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strings.TrimSpace(fmt.Sprintf("%g", v))
	case json.Number:
		return v.String()
	case nil:
		return ""
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// parseChecklistSteps normalizes a checklist argument (JSON-array string of
// strings or {text,done} objects, or an already-decoded array) into step
// texts. Invalid input yields nil.
func parseChecklistSteps(v any) []string {
	var raw []any
	switch typed := v.(type) {
	case string:
		s := strings.TrimSpace(typed)
		if s == "" {
			return nil
		}
		if err := json.Unmarshal([]byte(s), &raw); err != nil {
			return nil
		}
	case []any:
		raw = typed
	case []string:
		return append([]string(nil), typed...)
	default:
		return nil
	}
	steps := make([]string, 0, len(raw))
	for _, item := range raw {
		switch step := item.(type) {
		case string:
			if t := strings.TrimSpace(step); t != "" {
				steps = append(steps, t)
			}
		case map[string]any:
			if t := argStr(step, "text"); t != "" {
				steps = append(steps, t)
			}
		}
	}
	return steps
}

// parseCardsJSON normalizes a create_cards cards_json argument into card arg
// maps. Invalid input yields nil.
func parseCardsJSON(v any) []map[string]any {
	var raw []any
	switch typed := v.(type) {
	case string:
		s := strings.TrimSpace(typed)
		if s == "" {
			return nil
		}
		if err := json.Unmarshal([]byte(s), &raw); err != nil {
			return nil
		}
	case []any:
		raw = typed
	default:
		return nil
	}
	cards := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if card, ok := item.(map[string]any); ok {
			cards = append(cards, card)
		}
	}
	return cards
}

// truncate shortens s to at most n runes (with ellipsis).
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}
