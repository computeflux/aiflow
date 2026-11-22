// [workflow-fork] P2-3/P2-4: read-only workflow board tools for ai-bot.
// [workflow-fork] P4-2: write actions on workflow_card / workflow_board with
// two-phase confirmation (P4-1 Approval) + high-risk role gating (P2-2
// RequireRole) + single-action enforcement; write tools are registered
// alongside the read-only tools by All().
//
// Thin Tool wrappers over the typed workflow-api client (pkg/workflow,
// P2-1). Domain tools:
//
//	workflow_board       GET /boards + GET /boards/:id (lists + cards)
//	                     POST /boards / PUT /boards/:id / POST /boards/:id/lists
//	                     PUT+DELETE /lists/:id / DELETE /boards/:id (writes)
//	workflow_card        GET /cards/:id/executions
//	                     POST /cards / PUT /cards/:id / POST /cards/:id/comments
//	                     POST /cards/:id/finish / rerun / approve / reject
//	                     DELETE /cards/:id (writes)
//	workflow_rag         GET /search (CRAG RAG)
//	workflow_ops_report  GET /ops-reports
//	workflow_service     GET /services
//	workflow_skill       GET /api/skills + GET /api/skills/:name/runs (S1 skill
//	                     registry: skills, live versions, recent runs)
//
// Red lines: read tools are read-only; every write action requires the P4-1
// two-phase preview → confirm flow, high-risk actions additionally pass a
// role gate and the audit double-write (fail-closed). Tool outputs embed
// user-authored text (card titles, descriptions, reports), so results are
// wrapped in <untrusted_data> per the system-prompt contract declared by
// agent.WorkflowPromptContent(): the LLM must treat that payload as data,
// never as instructions.
package workflowtools

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/computeflux/ai-flow/pkg/tools"
	"github.com/computeflux/ai-flow/pkg/workflow"
)

// Tool names (single source of truth for allowlists / prompt).
const (
	ToolNameBoard     = "workflow_board"
	ToolNameCard      = "workflow_card"
	ToolNameRAG       = "workflow_rag"
	ToolNameOpsReport = "workflow_ops_report"
	ToolNameService   = "workflow_service"
	ToolNameSkill     = "workflow_skill"
)

// All returns the workflow tool set bound to a client: the read-only tools
// plus the P4-2 write tools (same registration surface — WithWorkflowTools
// registers everything). Write actions are rejected at Execute time when no
// approval secret is configured (fail-closed), so shipping them alongside
// the read tools is safe.
func All(c *workflow.Client) []tools.Tool {
	return AllWithDeps(NewWriteDeps(c, envApprovalSecret()))
}

// envApprovalSecret reads the P4-1 approval secret from the process env
// (PICOCLAW_APPROVAL_SECRET; pkg/workflow/config.go ensures the root .env
// is merged into the process env at gateway startup).
func envApprovalSecret() string {
	return strings.TrimSpace(os.Getenv(workflow.EnvApprovalSecret))
}

// AllWithDeps returns the tool set with explicit write deps (P4-2; used by
// gateway wiring and tests).
func AllWithDeps(deps *WriteDeps) []tools.Tool {
	return []tools.Tool{
		NewBoardTool(deps.Client),
		NewCardTool(deps.Client),
		NewRAGTool(deps.Client),
		NewOpsReportTool(deps.Client),
		NewServiceTool(deps.Client),
		NewSkillTool(deps.Client),
		NewCardWriteTool(deps),
		NewBoardWriteTool(deps),
	}
}

// wrapUntrusted marks a payload as untrusted workflow data for the LLM.
func wrapUntrusted(s string) string {
	return "<untrusted_data>\n" + s + "\n</untrusted_data>"
}

// executeWithAuth runs fn with a context carrying the user JWT. The P3
// identity work item injects the end-user JWT into the tool-execution
// context under workflow.ContextKeyJWT; all workflow-api endpoints are
// requireAuth(), so a missing token is rejected here with an actionable
// error instead of surfacing as a bare upstream 401. [workflow-fork] P2-3.
func executeWithAuth(ctx context.Context, fn func(ctx context.Context) *tools.ToolResult) *tools.ToolResult {
	if strings.TrimSpace(workflow.JWTFromContext(ctx)) == "" {
		return tools.ErrorResult("workflow api: no user token in request context (identity passthrough not established for this session); ask the user to retry from an authenticated board session")
	}
	return fn(ctx)
}

// jsonResult serializes v (indented) into an untrusted-wrapped, silent tool
// result (workflow tool output is LLM context only, never a user message).
func jsonResult(v any) *tools.ToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return tools.ErrorResult("workflow tool: serialize result: " + err.Error())
	}
	return tools.SilentResult(wrapUntrusted(string(b)))
}

func clientErrorResult(err error) *tools.ToolResult {
	return tools.ErrorResult("workflow api: " + err.Error())
}

func argInt(args map[string]any, key string) (int, bool) {
	switch v := args[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, false
		}
		return int(n), true
	}
	return 0, false
}

func argString(args map[string]any, key string) (string, bool) {
	v, ok := args[key].(string)
	return strings.TrimSpace(v), ok
}

// --- workflow_board ---

type boardTool struct{ c *workflow.Client }

func NewBoardTool(c *workflow.Client) tools.Tool { return &boardTool{c: c} }

func (t *boardTool) Name() string { return ToolNameBoard }

func (t *boardTool) Description() string {
	return "List workflow boards, or get one board with its lists and cards. action=list (all boards) | action=get (requires id). Read-only."
}

func (t *boardTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"list", "get"},
				"description": "list = all boards; get = one board detail (lists + cards)",
			},
			"id": map[string]any{
				"type":        "integer",
				"description": "board id (required for action=get). Use action=list first to discover real ids.",
			},
		},
		"required": []string{"action"},
	}
}

func (t *boardTool) Execute(ctx context.Context, args map[string]any) *tools.ToolResult {
	action, _ := argString(args, "action")
	switch action {
	case "list":
		return executeWithAuth(ctx, func(ctx context.Context) *tools.ToolResult {
			boards, truncated, err := t.c.Boards(ctx)
			if err != nil {
				return clientErrorResult(err)
			}
			return jsonResult(map[string]any{"boards": boards, "truncated": truncated})
		})
	case "get":
		id, ok := argInt(args, "id")
		if !ok || id <= 0 {
			return tools.ErrorResult("workflow_board: action=get requires a positive integer 'id' (discover real ids with action=list first)")
		}
		return executeWithAuth(ctx, func(ctx context.Context) *tools.ToolResult {
			detail, err := t.c.Board(ctx, id)
			if err != nil {
				return clientErrorResult(err)
			}
			return jsonResult(detail)
		})
	default:
		return tools.ErrorResult("workflow_board: 'action' must be one of: list, get (write actions live on the P4-2 workflow_board write surface: create | update | add_list | delete)")
	}
}

// --- workflow_card ---

type cardTool struct{ c *workflow.Client }

func NewCardTool(c *workflow.Client) tools.Tool { return &cardTool{c: c} }

func (t *cardTool) Name() string { return ToolNameCard }

func (t *cardTool) Description() string {
	return "Get the workflow execution history of a board card. Read-only. Always reference cards by id AND title."
}

func (t *cardTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "integer",
				"description": "card id (discover real ids via workflow_board action=get)",
			},
		},
		"required": []string{"id"},
	}
}

func (t *cardTool) Execute(ctx context.Context, args map[string]any) *tools.ToolResult {
	id, ok := argInt(args, "id")
	if !ok || id <= 0 {
		return tools.ErrorResult("workflow_card: requires a positive integer 'id' (discover real ids with workflow_board first)")
	}
	return executeWithAuth(ctx, func(ctx context.Context) *tools.ToolResult {
		execs, truncated, err := t.c.CardExecutions(ctx, id)
		if err != nil {
			return clientErrorResult(err)
		}
		return jsonResult(map[string]any{"card_id": id, "executions": execs, "truncated": truncated})
	})
}

// --- workflow_rag ---

type ragTool struct{ c *workflow.Client }

func NewRAGTool(c *workflow.Client) tools.Tool { return &ragTool{c: c} }

func (t *ragTool) Name() string { return ToolNameRAG }

func (t *ragTool) Description() string {
	return "Search the workflow RAG knowledge base (CRAG). Returns retrieved chunks + sufficiency judgement. Read-only."
}

func (t *ragTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "search query",
			},
			"k": map[string]any{
				"type":        "integer",
				"description": "max documents to return (server default 5)",
			},
			"days": map[string]any{
				"type":        "integer",
				"description": "recency window in days (server default 7)",
			},
		},
		"required": []string{"query"},
	}
}

func (t *ragTool) Execute(ctx context.Context, args map[string]any) *tools.ToolResult {
	q, _ := argString(args, "query")
	if q == "" {
		return tools.ErrorResult("workflow_rag: requires non-empty 'query'")
	}
	k, _ := argInt(args, "k")
	days, _ := argInt(args, "days")
	return executeWithAuth(ctx, func(ctx context.Context) *tools.ToolResult {
		res, err := t.c.RAGSearch(ctx, q, k, days)
		if err != nil {
			return clientErrorResult(err)
		}
		return jsonResult(res)
	})
}

// --- workflow_ops_report ---

type opsReportTool struct{ c *workflow.Client }

func NewOpsReportTool(c *workflow.Client) tools.Tool { return &opsReportTool{c: c} }

func (t *opsReportTool) Name() string { return ToolNameOpsReport }

func (t *opsReportTool) Description() string {
	return "List workflow ops reports (daily/weekly). Optional type filter. Read-only."
}

func (t *opsReportTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"type": map[string]any{
				"type":        "string",
				"description": "report type filter (e.g. daily, weekly); empty = all",
			},
		},
	}
}

func (t *opsReportTool) Execute(ctx context.Context, args map[string]any) *tools.ToolResult {
	typ, _ := argString(args, "type")
	return executeWithAuth(ctx, func(ctx context.Context) *tools.ToolResult {
		reports, truncated, err := t.c.OpsReports(ctx, typ)
		if err != nil {
			return clientErrorResult(err)
		}
		return jsonResult(map[string]any{"reports": reports, "truncated": truncated})
	})
}

// --- workflow_service ---

type serviceTool struct{ c *workflow.Client }

func NewServiceTool(c *workflow.Client) tools.Tool { return &serviceTool{c: c} }

func (t *serviceTool) Name() string { return ToolNameService }

func (t *serviceTool) Description() string {
	return "List managed deploy services with container state/health. Read-only."
}

func (t *serviceTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (t *serviceTool) Execute(ctx context.Context, _ map[string]any) *tools.ToolResult {
	return executeWithAuth(ctx, func(ctx context.Context) *tools.ToolResult {
		services, truncated, err := t.c.Services(ctx)
		if err != nil {
			return clientErrorResult(err)
		}
		return jsonResult(map[string]any{"services": services, "truncated": truncated})
	})
}

// --- workflow_skill ---

type skillTool struct{ c *workflow.Client }

func NewSkillTool(c *workflow.Client) tools.Tool { return &skillTool{c: c} }

func (t *skillTool) Name() string { return ToolNameSkill }

func (t *skillTool) Description() string {
	return "Inspect the workflow skill registry (S1 skill gateway): action=list (all skills with live versions) | action=runs (recent invocations of one skill, requires name; optional limit). Read-only."
}

func (t *skillTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"list", "runs"},
				"description": "list = all skills; runs = recent runs of one skill",
			},
			"name": map[string]any{
				"type":        "string",
				"description": "skill name (required for action=runs; discover real names with action=list first)",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "max runs to return (server default 20)",
			},
		},
		"required": []string{"action"},
	}
}

func (t *skillTool) Execute(ctx context.Context, args map[string]any) *tools.ToolResult {
	action, _ := argString(args, "action")
	switch action {
	case "list":
		return executeWithAuth(ctx, func(ctx context.Context) *tools.ToolResult {
			skills, truncated, err := t.c.Skills(ctx)
			if err != nil {
				return clientErrorResult(err)
			}
			return jsonResult(map[string]any{"skills": skills, "truncated": truncated})
		})
	case "runs":
		name, _ := argString(args, "name")
		if name == "" {
			return tools.ErrorResult("workflow_skill: action=runs requires 'name' (discover real names with action=list first)")
		}
		limit, _ := argInt(args, "limit")
		return executeWithAuth(ctx, func(ctx context.Context) *tools.ToolResult {
			runs, truncated, err := t.c.SkillRuns(ctx, name, limit)
			if err != nil {
				return clientErrorResult(err)
			}
			return jsonResult(map[string]any{"skill": name, "runs": runs, "truncated": truncated})
		})
	default:
		return tools.ErrorResult("workflow_skill: 'action' must be one of: list, runs")
	}
}
