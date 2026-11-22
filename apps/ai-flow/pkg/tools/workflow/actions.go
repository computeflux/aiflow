// [workflow-fork] P4-2: workflow 写工具的高危动作目录 + 角色门控（P2-2
// RequireRole）+ 高危单次单动作红线。
//
// 写面（workflow-api, apps/core/cmd/api）分两张工具：
//
//	workflow_card : action=create|update|move|comment|finish|rerun（+ 高危
//	                approve / decision / delete）+ 只读 executions（P2-3 兼容）
//	workflow_board: action=list|get（只读，P2-3）+ create|update|add_list
//	                （+ 高危 delete：看板 / 列按 target 区分）
//
// 高危清单（强制两阶段确认 + 角色门控，与 apps/core 服务端 requireRole 对齐）：
//   - delete（卡 / 列 / 看板）           → admin
//   - approve / decision（审批决策）      → board_approver
//   - service / skill traffic / publish / terminate → admin（尚未开放为工具
//     动作；目录契约在测试中钉死，接入时必须映射 admin）
//
// 高危单次仅允许 1 个动作：写调用一旦携带多个动作语义（actions 数组多元素、
// action 逗号分隔多值、或 create 携带批量 cards 结构）直接拒绝，不进入 preview。

package workflowtools

import (
	"fmt"
	"strings"
)

// Role names required by the high-risk catalog (mirror of the server-side
// requireRole gates in apps/core/cmd/api/main.go).
const (
	RoleAdmin         = "admin"
	RoleBoardApprover = "board_approver"
)

// Write action names. Read actions ("executions" on workflow_card,
// "list"/"get" on workflow_board) live in the read path; everything below
// is a WRITE and flows through the two-phase preview → confirm pipeline
// (P4-1).
const (
	ActionCreate   = "create"
	ActionUpdate   = "update"
	ActionMove     = "move"
	ActionComment  = "comment"
	ActionFinish   = "finish"
	ActionRerun    = "rerun"
	ActionApprove  = "approve"
	ActionDecision = "decision"
	ActionDelete   = "delete"
	ActionAddList  = "add_list"
)

// WriteActions is the full write-action catalog (single source of truth for
// tool schemas, previews and tests).
var WriteActions = []string{
	ActionCreate, ActionUpdate, ActionMove, ActionComment, ActionFinish,
	ActionRerun, ActionApprove, ActionDecision, ActionDelete, ActionAddList,
}

func isWriteAction(action string) bool {
	for _, a := range WriteActions {
		if a == action {
			return true
		}
	}
	return false
}

// highRiskActions are the operations that MUST run the full two-phase
// confirmation (preview → human confirm → audit double-write, fail-closed
// on audit failure) and pass a role gate. Normal writes still require
// preview → confirm (P4 red line), their audit double-write is
// best-effort (highRisk=false in P4-1 Decision semantics).
var highRiskActions = map[string]bool{
	ActionDelete:   true, // 卡 / 列 / 看板删除 → admin
	ActionApprove:  true, // 审批放行 → board_approver
	ActionDecision: true, // 审批决策（approve|reject 参数化）→ board_approver
}

// IsHighRisk reports whether a write action is high-risk.
func IsHighRisk(action string) bool { return highRiskActions[action] }

// RequiredRole returns the role a write action demands, and whether the
// action is high-risk at all. The mapping mirrors the server side:
//   - delete (card/list/board)  → "admin"
//   - approve / decision        → "board_approver"
func RequiredRole(action string) (string, bool) {
	switch action {
	case ActionDelete:
		return RoleAdmin, true
	case ActionApprove, ActionDecision:
		return RoleBoardApprover, true
	default:
		return "", false
	}
}

// multiActionReason inspects a write invocation for multi-action semantics.
// Returns "" when the invocation carries exactly one action. The check is
// deliberately structural: an "actions" array with more than one element,
// an "action" value that lists several actions comma separated, or a batch
// payload ("cards" list on create) are all multi-action.
func multiActionReason(toolName, action string, args map[string]any) string {
	if raw, ok := args["actions"]; ok {
		if list, ok := asStringList(raw); ok && len(list) > 1 {
			return fmt.Sprintf("workflow %s: 高危写操作单次仅允许 1 个动作（收到 %d 个：%s），请逐个发起「preview → 确认 → 执行」",
				toolName, len(list), strings.Join(list, ", "))
		}
	}
	if strings.Contains(action, ",") {
		return fmt.Sprintf("workflow %s: 高危写操作单次仅允许 1 个动作（action=%q 包含多个动作），请逐个发起「preview → 确认 → 执行」",
			toolName, action)
	}
	if action == ActionCreate {
		if raw, ok := args["cards"]; ok {
			if list, ok := asStringList(raw); ok && len(list) > 1 {
				return fmt.Sprintf("workflow %s: 高危写操作单次仅允许 1 个动作（create 携带 %d 张批量卡片），请逐个发起「preview → 确认 → 执行」",
					toolName, len(list))
			}
		}
	}
	return ""
}

// asStringList coerces []string / []any into a string slice (non-string
// elements count as items). The second return is false for non-list values.
func asStringList(v any) ([]string, bool) {
	switch list := v.(type) {
	case []string:
		out := make([]string, 0, len(list))
		for _, s := range list {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out, true
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			s, ok := item.(string)
			if !ok {
				out = append(out, "<item>")
				continue
			}
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out, true
	}
	return nil, false
}

// PreviewAction renders the human-readable one-line action description used
// in the preview payload (what the human is asked to confirm). It must be
// deterministic for identical args: it is embedded into the preview JSON,
// while the token binds only (user, tool, params_hash, expire).
func PreviewAction(toolName, action string, args map[string]any) string {
	// Board-tool actions render with board semantics (the same tool name is
	// shared with card actions; preview text must match what executes).
	if toolName == ToolNameBoard {
		switch action {
		case ActionCreate:
			return fmt.Sprintf("创建看板「%s」", argDefault(args, "name", "?"))
		case ActionUpdate:
			return fmt.Sprintf("更新看板 #%v（字段：%s）", argDisplay(args, "id"), fieldList(args, boardUpdateFields))
		case ActionAddList:
			return fmt.Sprintf("在看板 #%v 添加列「%s」", argDisplay(args, "id"), argDefault(args, "name", "?"))
		case ActionDelete:
			return fmt.Sprintf("删除 %s #%v ⚠️高危", deleteTargetLabel(args), argDisplay(args, "id"))
		default:
			return fmt.Sprintf("%s action=%s", toolName, action)
		}
	}
	switch action {
	case ActionCreate:
		return fmt.Sprintf("创建任务卡「%s」（列 #%v）", argDefault(args, "title", "?"), argDisplay(args, "list_id"))
	case ActionUpdate:
		return fmt.Sprintf("更新任务卡 #%v（字段：%s）", argDisplay(args, "id"), fieldList(args, cardUpdateFields))
	case ActionMove:
		return fmt.Sprintf("移动任务卡 #%v 到列 #%v", argDisplay(args, "id"), argDisplay(args, "to_list_id"))
	case ActionComment:
		return fmt.Sprintf("给任务卡 #%v 添加评论「%s」", argDisplay(args, "id"), argDefault(args, "body", "?"))
	case ActionFinish:
		return fmt.Sprintf("人工结束任务卡 #%v（outcome=%s）", argDisplay(args, "id"), argDefault(args, "outcome", "done"))
	case ActionRerun:
		return fmt.Sprintf("重跑任务卡 #%v（mode=%s）", argDisplay(args, "id"), argDefault(args, "mode", "restart"))
	case ActionApprove:
		return fmt.Sprintf("审批放行任务卡 #%v ⚠️高危", argDisplay(args, "id"))
	case ActionDecision:
		return fmt.Sprintf("审批决策任务卡 #%v（decision=%s）⚠️高危", argDisplay(args, "id"), argDefault(args, "decision", "?"))
	case ActionDelete:
		return fmt.Sprintf("删除 %s #%v ⚠️高危", deleteTargetLabel(args), argDisplay(args, "id"))
	default:
		return fmt.Sprintf("%s action=%s", toolName, action)
	}
}

// deleteTargetLabel normalizes the delete target (card | list | board).
// The empty case renders as card: callers normalize an omitted target via
// defaultDeleteTarget(toolName) BEFORE preview rendering and execution
// (write.go runTwoPhase), so preview text and executed operation agree.
func deleteTargetLabel(args map[string]any) string {
	target, _ := argString(args, "target")
	switch target {
	case "list":
		return "列(list)"
	case "board":
		return "看板(board)"
	default:
		return "任务卡(card)"
	}
}

// Per-action writable field sets (preview display + server payload filter).
var (
	cardUpdateFields  = []string{"title", "description", "priority", "status", "assignee", "labels", "target_name", "checklist", "depends_on", "due_date", "cover_color"}
	boardUpdateFields = []string{"name"}
	listUpdateFields  = []string{"name", "position"}
)

// fieldList renders the subset of args (restricted to allowed fields) as
// "k=v, k=v" for previews.
func fieldList(args map[string]any, allowed []string) string {
	parts := make([]string, 0, len(allowed))
	for _, k := range allowed {
		if v, ok := args[k]; ok {
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
	}
	if len(parts) == 0 {
		return "（无字段）"
	}
	return strings.Join(parts, ", ")
}

func argDisplay(args map[string]any, key string) any {
	if v, ok := args[key]; ok {
		return v
	}
	return "?"
}

func argDefault(args map[string]any, key, def string) string {
	if s, ok := argString(args, key); ok && s != "" {
		return s
	}
	return def
}
