// [workflow-fork] P4-2: workflow_card / workflow_board 写工具。
//
// workflow_card  action=create|update|move|comment|finish|rerun（+ approve /
//                decision / delete 高危门控入口）
// workflow_board action=create|update|add_list（+ delete：board/list 高危）
//
// 每个写动作走 runTwoPhase（preview → confirm，P4-1），confirm 通过后调用
// workflow-api 真实写端点（apps/core/cmd/api）。执行结果以 Silent +
// <untrusted_data> 返回；错误信息透传上游状态码语义。

package workflowtools

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/computeflux/ai-flow/pkg/tools"
)

// cardWriteTool is the P4-2 write surface of workflow_card. It shares the
// tool name with the read-only card tool (registered first by All): the
// registry keeps the last registration for a name, so at runtime the
// combined tool serves read (executions) + write actions.
// The registry keeps the LAST registration for a name (Register is
// last-wins), so the write tools returned by AllWithDeps ARE the effective
// workflow_card / workflow_board at runtime. They therefore absorb the P2-3
// read surfaces: card serves action=executions, board serves action=list /
// get — the read-only behaviour stays available after P4-2 registration.
type cardWriteTool struct {
	d    *WriteDeps
	read tools.Tool
}

// NewCardWriteTool builds the workflow_card write tool.
func NewCardWriteTool(d *WriteDeps) tools.Tool {
	return &cardWriteTool{d: d, read: NewCardTool(d.Client)}
}

func (t *cardWriteTool) Name() string { return ToolNameCard }

func (t *cardWriteTool) Description() string {
	return "Board card tool. Read: action=executions (workflow history, requires id; read-only). " +
		"Write (P4 两阶段确认，先 confirm=false 取 preview+confirm_token，向用户解释并等待确认，再 confirm=true 携带 token 执行): " +
		"action=create|update|move|comment|finish|rerun；高危动作需相应角色并强制两阶段: " +
		"approve|decision(审批 approve/reject, 需 board_approver), delete(删除卡/列/看板, 需 admin)。高危单次仅允许 1 个动作。"
}

func (t *cardWriteTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type": "string",
				"enum": append([]string{"executions"}, WriteActions...),
			},
			"id":            map[string]any{"type": "integer", "description": "card id（写入动作必填；先用 workflow_board 查询真实 id）"},
			"title":         map[string]any{"type": "string", "description": "create: 卡片标题"},
			"list_id":       map[string]any{"type": "integer", "description": "create: 目标列 id"},
			"description":   map[string]any{"type": "string", "description": "create/update: 卡片描述"},
			"priority":      map[string]any{"type": "string", "description": "create/update: low|medium|high|urgent"},
			"status":        map[string]any{"type": "string", "description": "update: 卡片状态"},
			"assignee":      map[string]any{"type": "string", "description": "update: 负责人"},
			"labels":        map[string]any{"type": "string", "description": "update: 标签"},
			"target_name":   map[string]any{"type": "string", "description": "update: 目标项目名"},
			"to_list_id":    map[string]any{"type": "integer", "description": "move: 目标列 id"},
			"body":          map[string]any{"type": "string", "description": "comment: 评论内容"},
			"outcome":       map[string]any{"type": "string", "description": "finish: done|reject|review"},
			"reason":        map[string]any{"type": "string", "description": "finish/rerun: 原因说明"},
			"mode":          map[string]any{"type": "string", "description": "rerun: resume|restart"},
			"decision":      map[string]any{"type": "string", "description": "decision: approve|reject"},
			"target":        map[string]any{"type": "string", "description": "delete: card|list|board（默认 card）"},
			"name":          map[string]any{"type": "string", "description": "board add_list/update: 列名/看板名"},
			"position":      map[string]any{"type": "integer", "description": "add_list/update(list): 列位置"},
			"confirm":       map[string]any{"type": "boolean", "description": "false/缺省 = preview（签发确认令牌）；true = 携带 token 执行"},
			"confirm_token": map[string]any{"type": "string", "description": "preview 阶段返回的 confirm_token（confirm=true 必填）"},
		},
		"required": []string{"action"},
	}
}

func (t *cardWriteTool) Execute(ctx context.Context, args map[string]any) *tools.ToolResult {
	action, _ := argString(args, "action")
	if action == "" {
		return tools.ErrorResult("workflow_card: requires 'action' (executions | create | update | move | comment | finish | rerun | approve | decision | delete)")
	}
	// 高危单动作红线在动作分派之前：多动作 args（actions 数组 / action 逗号
	// 分隔多值）直接拒绝，不进入 preview / 执行。
	if reason := multiActionReason(ToolNameCard, action, args); reason != "" {
		return tools.ErrorResult(reason)
	}
	if action == "executions" {
		// Read path (P2-3): workflow history of a card, delegated to the
		// read-only tool implementation (same behaviour, same contract).
		return t.read.Execute(ctx, args)
	}
	if !isWriteAction(action) {
		return tools.ErrorResult(fmt.Sprintf("workflow_card: unknown action %q (allowed: executions, %s)", action, strings.Join(WriteActions, ", ")))
	}

	preview, done, err := t.d.runTwoPhase(ctx, ToolNameCard, args, func(ctx context.Context, w map[string]any, out *writeOutcome) error {
		return t.executeWrite(ctx, action, w, out)
	})
	if err != nil {
		return clientErrorResult(err)
	}
	if preview != nil {
		return preview
	}
	if done != nil && done.Allowed {
		res := jsonResult(done.Result)
		res.ForLLM = res.ForLLM + "\n\n[executed] 写操作已确认并执行完成。"
		return res
	}
	return tools.ErrorResult("workflow_card: 写操作未执行（内部状态异常）")
}

// executeWrite performs the confirmed real API call for one card action.
func (t *cardWriteTool) executeWrite(ctx context.Context, action string, w map[string]any, out *writeOutcome) error {
	c := t.d.writeClient()
	if c == nil {
		return fmt.Errorf("workflow client not configured")
	}
	switch action {
	case ActionCreate:
		title, _ := argString(w, "title")
		if title == "" {
			return fmt.Errorf("workflow_card create: requires 'title'")
		}
		listID, _ := argInt(w, "list_id")
		body := map[string]any{"title": title, "list_id": listID, "source": "ai-bot"}
		for _, k := range []string{"description", "priority", "assignee", "labels", "target_name"} {
			if v, ok := w[k]; ok {
				body[k] = v
			}
		}
		var resp struct {
			Card map[string]any `json:"card"`
		}
		if err := c.Do(ctx, http.MethodPost, "/cards", nil, body, &resp); err != nil {
			return err
		}
		out.Result = map[string]any{"action": action, "card": resp.Card}
		return nil
	case ActionUpdate:
		id, ok := argInt(w, "id")
		if !ok || id <= 0 {
			return fmt.Errorf("workflow_card update: requires positive 'id'")
		}
		body := map[string]any{}
		for _, k := range cardUpdateFields {
			if v, ok := w[k]; ok {
				body[k] = v
			}
		}
		if len(body) == 0 {
			return fmt.Errorf("workflow_card update: no fields to update")
		}
		var resp struct {
			Card map[string]any `json:"card"`
		}
		if err := c.Do(ctx, http.MethodPut, fmt.Sprintf("/cards/%d", id), nil, body, &resp); err != nil {
			return err
		}
		out.Result = map[string]any{"action": action, "card_id": id, "card": resp.Card}
		return nil
	case ActionMove:
		id, ok := argInt(w, "id")
		if !ok || id <= 0 {
			return fmt.Errorf("workflow_card move: requires positive 'id'")
		}
		toList, ok := argInt(w, "to_list_id")
		if !ok || toList <= 0 {
			return fmt.Errorf("workflow_card move: requires positive 'to_list_id' (discover with workflow_board action=get)")
		}
		var resp struct {
			Card map[string]any `json:"card"`
		}
		if err := c.Do(ctx, http.MethodPut, fmt.Sprintf("/cards/%d", id), nil, map[string]any{"list_id": toList}, &resp); err != nil {
			return err
		}
		out.Result = map[string]any{"action": action, "card_id": id, "to_list_id": toList, "card": resp.Card}
		return nil
	case ActionComment:
		id, ok := argInt(w, "id")
		if !ok || id <= 0 {
			return fmt.Errorf("workflow_card comment: requires positive 'id'")
		}
		body, _ := argString(w, "body")
		if body == "" {
			return fmt.Errorf("workflow_card comment: requires 'body'")
		}
		var resp map[string]any
		if err := c.Do(ctx, http.MethodPost, fmt.Sprintf("/cards/%d/comments", id), nil, map[string]any{"author": "ai-bot", "body": body}, &resp); err != nil {
			return err
		}
		out.Result = map[string]any{"action": action, "card_id": id, "comment": resp}
		return nil
	case ActionFinish:
		id, ok := argInt(w, "id")
		if !ok || id <= 0 {
			return fmt.Errorf("workflow_card finish: requires positive 'id'")
		}
		outcome, _ := argString(w, "outcome")
		if outcome != "done" && outcome != "reject" && outcome != "review" {
			return fmt.Errorf("workflow_card finish: 'outcome' must be done|reject|review")
		}
		reason, _ := argString(w, "reason")
		var resp map[string]any
		if err := c.Do(ctx, http.MethodPost, fmt.Sprintf("/cards/%d/finish", id), nil, map[string]any{"outcome": outcome, "reason": reason}, &resp); err != nil {
			return err
		}
		out.Result = map[string]any{"action": action, "card_id": id, "result": resp}
		return nil
	case ActionRerun:
		id, ok := argInt(w, "id")
		if !ok || id <= 0 {
			return fmt.Errorf("workflow_card rerun: requires positive 'id'")
		}
		mode, _ := argString(w, "mode")
		var resp map[string]any
		if err := c.Do(ctx, http.MethodPost, fmt.Sprintf("/cards/%d/rerun", id), nil, map[string]any{"mode": mode}, &resp); err != nil {
			return err
		}
		out.Result = map[string]any{"action": action, "card_id": id, "result": resp}
		return nil
	case ActionApprove:
		id, ok := argInt(w, "id")
		if !ok || id <= 0 {
			return fmt.Errorf("workflow_card approve: requires positive 'id'")
		}
		var resp map[string]any
		if err := c.Do(ctx, http.MethodPost, fmt.Sprintf("/cards/%d/approve", id), nil, map[string]any{}, &resp); err != nil {
			return err
		}
		out.Result = map[string]any{"action": action, "card_id": id, "result": resp}
		return nil
	case ActionDecision:
		id, ok := argInt(w, "id")
		if !ok || id <= 0 {
			return fmt.Errorf("workflow_card decision: requires positive 'id'")
		}
		decision, _ := argString(w, "decision")
		var path string
		switch decision {
		case "approve":
			path = fmt.Sprintf("/cards/%d/approve", id)
		case "reject":
			path = fmt.Sprintf("/cards/%d/reject", id)
		default:
			return fmt.Errorf("workflow_card decision: 'decision' must be approve|reject")
		}
		var resp map[string]any
		if err := c.Do(ctx, http.MethodPost, path, nil, map[string]any{}, &resp); err != nil {
			return err
		}
		out.Result = map[string]any{"action": action, "card_id": id, "decision": decision, "result": resp}
		return nil
	case ActionDelete:
		// target 已由 runTwoPhase 按工具规范化（card 工具缺省 = card）。
		target, _ := argString(w, "target")
		if target == "" {
			target = defaultDeleteTarget(ToolNameCard)
		}
		id, ok := argInt(w, "id")
		if !ok || id <= 0 {
			return fmt.Errorf("workflow_card delete: requires positive 'id'")
		}
		var path string
		switch target {
		case "card":
			path = fmt.Sprintf("/cards/%d", id)
		case "list":
			path = fmt.Sprintf("/lists/%d", id)
		case "board":
			path = fmt.Sprintf("/boards/%d", id)
		default:
			return fmt.Errorf("workflow_card delete: 'target' must be card|list|board")
		}
		var resp map[string]any
		if err := c.Do(ctx, http.MethodDelete, path, nil, nil, &resp); err != nil {
			return err
		}
		out.Result = map[string]any{"action": action, "target": target, "id": id, "result": resp}
		return nil
	}
	return fmt.Errorf("workflow_card: non-writable action %q", action)
}

// --- workflow_board write tool ---

type boardWriteTool struct {
	d    *WriteDeps
	read tools.Tool
}

// NewBoardWriteTool builds the workflow_board write tool.
func NewBoardWriteTool(d *WriteDeps) tools.Tool {
	return &boardWriteTool{d: d, read: NewBoardTool(d.Client)}
}

func (t *boardWriteTool) Name() string { return ToolNameBoard }

func (t *boardWriteTool) Description() string {
	return "Board tool. Read: action=list (所有看板) / action=get (看板详情, 需 id; 只读). Write (P4 两阶段确认，先 confirm=false 取 preview+confirm_token，向用户解释并等待确认，再 confirm=true 携带 token 执行): " +
		"action=create (新建看板, name) | update (改名, id+name) | add_list (加列, id+name) | " +
		"delete (高危删除, 需 admin: target=board|list + id)。高危单次仅允许 1 个动作。只读查询请用 action=list/get。"
}

func (t *boardWriteTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type": "string",
				"enum": []string{"list", "get", ActionCreate, ActionUpdate, ActionAddList, ActionDelete},
			},
			"id":            map[string]any{"type": "integer", "description": "board id（update/add_list/delete(board)）或 list id（delete(list)）"},
			"name":          map[string]any{"type": "string", "description": "create/update: 看板名；add_list: 列名"},
			"description":   map[string]any{"type": "string", "description": "create: 看板描述"},
			"position":      map[string]any{"type": "integer", "description": "add_list: 列位置（可空）"},
			"target":        map[string]any{"type": "string", "description": "delete: board|list（默认 board）"},
			"confirm":       map[string]any{"type": "boolean", "description": "false/缺省 = preview；true = 携带 token 执行"},
			"confirm_token": map[string]any{"type": "string", "description": "preview 返回的 confirm_token（confirm=true 必填）"},
		},
		"required": []string{"action"},
	}
}

func (t *boardWriteTool) Execute(ctx context.Context, args map[string]any) *tools.ToolResult {
	action, _ := argString(args, "action")
	if reason := multiActionReason(ToolNameBoard, action, args); reason != "" {
		return tools.ErrorResult(reason)
	}
	switch action {
	case ActionCreate, ActionUpdate, ActionAddList, ActionDelete:
	case "list", "get":
		// Read path (P2-3): board list/detail, delegated to the read-only
		// tool implementation (the write registration replaces it in the
		// registry, so the read surface must live here too).
		return t.read.Execute(ctx, args)
	default:
		return tools.ErrorResult("workflow_board: action must be one of: list | get | create | update | add_list | delete")
	}

	preview, done, err := t.d.runTwoPhase(ctx, ToolNameBoard, args, func(ctx context.Context, w map[string]any, out *writeOutcome) error {
		return t.executeWrite(ctx, action, w, out)
	})
	if err != nil {
		return clientErrorResult(err)
	}
	if preview != nil {
		return preview
	}
	if done != nil && done.Allowed {
		res := jsonResult(done.Result)
		res.ForLLM = res.ForLLM + "\n\n[executed] 写操作已确认并执行完成。"
		return res
	}
	return tools.ErrorResult("workflow_board: 写操作未执行（内部状态异常）")
}

func (t *boardWriteTool) executeWrite(ctx context.Context, action string, w map[string]any, out *writeOutcome) error {
	c := t.d.writeClient()
	if c == nil {
		return fmt.Errorf("workflow client not configured")
	}
	switch action {
	case ActionCreate:
		name, _ := argString(w, "name")
		if name == "" {
			return fmt.Errorf("workflow_board create: requires 'name'")
		}
		_ = name
		body := map[string]any{"name": name}
		if desc, ok := argString(w, "description"); ok && desc != "" {
			body["description"] = desc
		}
		var resp struct {
			Board map[string]any `json:"board"`
		}
		if err := c.Do(ctx, http.MethodPost, "/boards", nil, body, &resp); err != nil {
			return err
		}
		out.Result = map[string]any{"action": action, "board": resp.Board}
		return nil
	case ActionUpdate:
		id, ok := argInt(w, "id")
		if !ok || id <= 0 {
			return fmt.Errorf("workflow_board update: requires positive 'id'")
		}
		name, _ := argString(w, "name")
		if name == "" {
			return fmt.Errorf("workflow_board update: requires 'name'")
		}
		var resp map[string]any
		if err := c.Do(ctx, http.MethodPut, fmt.Sprintf("/boards/%d", id), nil, map[string]any{"name": name}, &resp); err != nil {
			return err
		}
		out.Result = map[string]any{"action": action, "board_id": id, "result": resp}
		return nil
	case ActionAddList:
		id, ok := argInt(w, "id")
		if !ok || id <= 0 {
			return fmt.Errorf("workflow_board add_list: requires positive board 'id'")
		}
		name, _ := argString(w, "name")
		if name == "" {
			return fmt.Errorf("workflow_board add_list: requires 'name'")
		}
		body := map[string]any{"name": name}
		if pos, ok := argInt(w, "position"); ok {
			body["position"] = pos
		}
		var resp map[string]any
		if err := c.Do(ctx, http.MethodPost, fmt.Sprintf("/boards/%d/lists", id), nil, body, &resp); err != nil {
			return err
		}
		out.Result = map[string]any{"action": action, "board_id": id, "list": resp}
		return nil
	case ActionDelete:
		// target 已由 runTwoPhase 按工具规范化（board 工具缺省 = board），
		// preview 文本与执行目标同源，不会分叉。
		target, _ := argString(w, "target")
		if target == "" {
			target = defaultDeleteTarget(ToolNameBoard)
		}
		id, ok := argInt(w, "id")
		if !ok || id <= 0 {
			return fmt.Errorf("workflow_board delete: requires positive 'id'")
		}
		var path string
		switch target {
		case "board":
			path = fmt.Sprintf("/boards/%d", id)
		case "list":
			path = fmt.Sprintf("/lists/%d", id)
		default:
			return fmt.Errorf("workflow_board delete: 'target' must be board|list")
		}
		var resp map[string]any
		if err := c.Do(ctx, http.MethodDelete, path, nil, nil, &resp); err != nil {
			return err
		}
		out.Result = map[string]any{"action": action, "target": target, "id": id, "result": resp}
		return nil
	}
	return fmt.Errorf("workflow_board: non-writable action %q", action)
}
