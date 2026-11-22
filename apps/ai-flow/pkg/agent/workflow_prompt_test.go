// [workflow-fork] P2-4 tests: workflow prompt contributor injection.
// [workflow-fork] P4-2: 写操作红线段落（两阶段确认 / 角色门控）内容断言。

package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/computeflux/ai-flow/pkg/bus"
	"github.com/computeflux/ai-flow/pkg/config"
	wfclient "github.com/computeflux/ai-flow/pkg/workflow"
)

func TestWorkflowPromptContent_Sections(t *testing.T) {
	content := WorkflowPromptContent()

	required := []string{
		// 领域术语表
		"board = 看板",
		"list = 列",
		"card = 任务卡",
		// 工具使用规范（先查后写 + 标题引用）
		"先", "真实 id",
		"标题",
		// P4 红线：两阶段确认（preview → explain → 确认执行）+ 角色门控
		"P4 写操作红线",
		"两阶段",
		"confirm=false",
		"confirm_token",
		"explain",
		"等待用户明确确认",
		"admin",
		"board_approver",
		"单次仅允许 1 个动作",
		// untrusted data 非指令声明
		"<untrusted_data>",
	}
	for _, want := range required {
		if !strings.Contains(content, want) {
			t.Fatalf("WorkflowPromptContent() missing %q", want)
		}
	}
}

func TestContextBuilder_CollectsWorkflowContributor(t *testing.T) {
	t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
	cb := NewContextBuilder(t.TempDir())

	if err := cb.RegisterPromptContributor(workflowPromptContributor{}); err != nil {
		t.Fatalf("RegisterPromptContributor() error = %v", err)
	}

	prompt := cb.BuildSystemPrompt()
	if !strings.Contains(prompt, "# Workflow 看板工具使用规范") {
		t.Fatalf("BuildSystemPrompt() missing workflow section:\n%s", prompt)
	}
	if !strings.Contains(prompt, "board = 看板") {
		t.Fatalf("BuildSystemPrompt() missing terminology table")
	}

	// Structured metadata must carry the workflow source/layer/slot.
	var found bool
	for _, part := range cb.BuildSystemPromptParts() {
		if part.Source.ID == PromptSourceWorkflow {
			found = true
			if part.Layer != PromptLayerCapability || part.Slot != PromptSlotTooling {
				t.Fatalf("workflow part metadata = %s/%s, want capability/tooling", part.Layer, part.Slot)
			}
		}
	}
	if !found {
		t.Fatal("system prompt parts missing workflow contributor part")
	}
}

func TestContextBuilder_WithWorkflowPrompt(t *testing.T) {
	t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
	cb := NewContextBuilder(t.TempDir()).WithWorkflowPrompt()

	if !strings.Contains(cb.BuildSystemPrompt(), "P4 写操作红线") {
		t.Fatal("WithWorkflowPrompt() prompt missing P4 write red-line section")
	}

	// Registration is idempotent: re-registering replaces, never duplicates.
	cb.WithWorkflowPrompt()
	count := 0
	for _, part := range cb.BuildSystemPromptParts() {
		if part.Source.ID == PromptSourceWorkflow {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("workflow prompt parts count = %d, want 1", count)
	}
}

func TestWorkflowContributor_SuppressedByToolUseRule(t *testing.T) {
	c := workflowPromptContributor{}
	parts, err := c.ContributePrompt(context.Background(), PromptBuildRequest{
		SuppressToolUseRule: true,
	})
	if err != nil {
		t.Fatalf("ContributePrompt() error = %v", err)
	}
	if len(parts) != 0 {
		t.Fatalf("ContributePrompt() parts = %d, want 0 when SuppressToolUseRule", len(parts))
	}
}

func TestAgentLoop_WithWorkflowTools_InjectsPromptAndSurvivesReload(t *testing.T) {
	t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())

	cfg := testCfg([]config.AgentConfig{
		{ID: "main", Default: true},
		{ID: "research"},
	})
	al := NewAgentLoop(cfg, bus.NewMessageBus(), &mockRegistryProvider{})
	defer al.Close()

	al.WithWorkflowTools(nil)
	if !al.WorkflowToolsEnabled() {
		t.Fatal("WorkflowToolsEnabled() = false after WithWorkflowTools")
	}

	for _, id := range []string{"main", "research"} {
		instance, ok := al.GetRegistry().GetAgent(id)
		if !ok || instance.ContextBuilder == nil {
			t.Fatalf("agent %q missing", id)
		}
		if !strings.Contains(instance.ContextBuilder.BuildSystemPrompt(), "# Workflow 看板工具使用规范") {
			t.Fatalf("agent %q system prompt missing workflow section", id)
		}
	}

	// Hot reload rebuilds the registry; the activation flag must re-apply the
	// workflow contributor onto the fresh ContextBuilders.
	reloadCfg := testCfg([]config.AgentConfig{
		{ID: "main", Default: true},
		{ID: "research"},
	})
	if err := al.ReloadProviderAndConfig(context.Background(), &mockRegistryProvider{}, reloadCfg); err != nil {
		t.Fatalf("ReloadProviderAndConfig() error = %v", err)
	}
	instance, ok := al.GetRegistry().GetAgent("main")
	if !ok || instance.ContextBuilder == nil {
		t.Fatal("agent main missing after reload")
	}
	if !strings.Contains(instance.ContextBuilder.BuildSystemPrompt(), "board = 看板") {
		t.Fatal("workflow prompt section lost after reload")
	}
}

func TestWorkflowPromptPart(t *testing.T) {
	part := WorkflowPromptPart()
	if part.ID != "capability.workflow" ||
		part.Layer != PromptLayerCapability ||
		part.Slot != PromptSlotTooling ||
		part.Source.ID != PromptSourceWorkflow {
		t.Fatalf("WorkflowPromptPart() metadata = %#v", part)
	}
	if part.Content != WorkflowPromptContent() {
		t.Fatal("WorkflowPromptPart() content mismatch")
	}

	// The part must pass placement validation of a fresh registry.
	registry := NewPromptRegistry()
	if err := registry.RegisterSource(workflowPromptContributor{}.PromptSource()); err != nil {
		t.Fatalf("RegisterSource() error = %v", err)
	}
	if err := registry.ValidatePart(part); err != nil {
		t.Fatalf("ValidatePart() error = %v", err)
	}
}

func TestAgentLoop_WithWorkflowTools_RegistersReadOnlyTools(t *testing.T) {
	t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())

	cfg := testCfg([]config.AgentConfig{{ID: "main", Default: true}})
	al := NewAgentLoop(cfg, bus.NewMessageBus(), &mockRegistryProvider{})
	defer al.Close()

	// No client → prompt-only activation, no tools.
	al.WithWorkflowTools(nil)
	instance, ok := al.GetRegistry().GetAgent("main")
	if !ok || instance == nil {
		t.Fatal("agent main missing")
	}
	if _, ok := instance.Tools.Get("workflow_board"); ok {
		t.Fatal("workflow tools must not register without a client")
	}

	// With client → the workflow tools land on every agent (read surface +
	// P4-2 write surfaces share workflow_board / workflow_card names).
	al.WithWorkflowTools(wfclient.NewClient("http://127.0.0.1:1"))
	for _, name := range []string{
		"workflow_board", "workflow_card", "workflow_rag",
		"workflow_ops_report", "workflow_service", "workflow_skill",
	} {
		if _, ok := instance.Tools.Get(name); !ok {
			t.Fatalf("tool %q not registered after WithWorkflowTools(client)", name)
		}
	}

	// P4-2: the registered card/board tools expose write actions in their
	// schema (the last registration for a name wins).
	cardTool, ok := instance.Tools.Get("workflow_card")
	if !ok {
		t.Fatal("workflow_card not registered")
	}
	if !strings.Contains(cardTool.Description(), "两阶段") {
		t.Fatal("workflow_card description missing the two-phase write contract")
	}

	// Registration is idempotent across repeated activation.
	al.WithWorkflowTools(wfclient.NewClient("http://127.0.0.1:1"))
	if _, ok := instance.Tools.Get("workflow_board"); !ok {
		t.Fatal("workflow_board lost after re-activation")
	}
}
