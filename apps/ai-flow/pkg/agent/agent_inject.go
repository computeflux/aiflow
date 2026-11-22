// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"os"

	"github.com/computeflux/ai-flow/pkg/audio/asr"
	"github.com/computeflux/ai-flow/pkg/channels"
	"github.com/computeflux/ai-flow/pkg/config"
	"github.com/computeflux/ai-flow/pkg/media"
	"github.com/computeflux/ai-flow/pkg/tools"
	workflowtools "github.com/computeflux/ai-flow/pkg/tools/workflow"
	"github.com/computeflux/ai-flow/pkg/workflow"
)

func (al *AgentLoop) RegisterTool(tool tools.Tool) {
	registry := al.GetRegistry()
	for _, agentID := range registry.ListAgentIDs() {
		if agent, ok := registry.GetAgent(agentID); ok {
			agent.Tools.Register(tool)
		}
	}
}

// RegisterToolForAgent registers a tool on a single agent instance by id.
// [workflow-fork] P3-1: the board chat channel registers its declarative
// mutation tools on the default agent only, so the global chat surface stays
// untouched for non-board agents.
func (al *AgentLoop) RegisterToolForAgent(agentID string, tool tools.Tool) bool {
	instance, ok := al.GetRegistry().GetAgent(agentID)
	if !ok || instance.Tools == nil {
		return false
	}
	instance.Tools.Register(tool)
	return true
}

// WithWorkflowTools enables the workflow capability surface on every agent of
// the loop: it registers the workflow system-prompt contributor (terminology,
// query-before-write rules, P4 write red lines, <untrusted_data>
// declaration) into each agent's ContextBuilder and persists the activation
// across hot reloads (ReloadProviderAndConfig rebuilds registries from
// scratch, so the flag drives re-registration on the new registry).
//
// The client argument is the typed workflow-api read-only client (P2-1);
// P2-3 registers the matching read-only tools and P4-2 registers the write
// tools (two-phase confirmation). Passing nil only injects the prompt
// section. [workflow-fork] P2-4 / P4-2.
func (al *AgentLoop) WithWorkflowTools(c *workflow.Client) *AgentLoop {
	al.mu.Lock()
	al.workflowTools = true
	if c != nil {
		al.workflowClient = c
	}
	client := al.workflowClient
	al.mu.Unlock()

	// P2-3/P4-2 seam: when the typed client is available, register the
	// workflow tools (board/card/rag/ops-report/service read tools +
	// card/board write actions) onto every agent.
	if client != nil {
		al.registerWorkflowTools(client)
	}
	al.applyWorkflowPrompt()
	return al
}

// registerWorkflowTools registers the workflow tool set (workflow_board /
// workflow_card / workflow_rag / workflow_ops_report / workflow_service
// read tools + P4-2 card/board write-action surfaces, pkg/tools/workflow)
// onto every agent of the current registry. The write deps are built from
// the client + PICOCLAW_APPROVAL_SECRET (fail-closed when unset). Shared
// across all agents: one Approval broker = one token/replay state.
// [workflow-fork] P2-3/P2-4/P4-2 seam.
func (al *AgentLoop) registerWorkflowTools(c *workflow.Client) {
	if c == nil {
		return
	}
	deps := workflowtools.NewWriteDeps(c, os.Getenv(workflow.EnvApprovalSecret))
	registry := al.GetRegistry()
	for _, agentID := range registry.ListAgentIDs() {
		instance, ok := registry.GetAgent(agentID)
		if !ok || instance.Tools == nil {
			continue
		}
		for _, t := range workflowtools.AllWithDeps(deps) {
			instance.Tools.Register(t)
		}
	}
}

// WithBoardTools enables the board chat mutation tools on every agent of the
// loop ([workflow-fork] P3-1). The tools source is a provider func supplied
// by the board channel package (pkg/agent cannot import pkg/channels/board:
// import cycle). Activation persists across ReloadProviderAndConfig so the
// tools are re-registered onto freshly built registries.
func (al *AgentLoop) WithBoardTools(source func() []tools.Tool, identityHook any) *AgentLoop {
	al.mu.Lock()
	al.boardTools = source != nil
	if source != nil {
		al.boardToolsSource = source
	}
	al.boardIdentityHook = identityHook
	al.mu.Unlock()

	al.applyBoardTools()
	if identityHook != nil {
		_ = al.MountHook(NamedHook("board_identity", identityHook))
	}
	return al
}

// applyBoardTools registers the board mutation tools on the current registry.
func (al *AgentLoop) applyBoardTools() {
	al.mu.RLock()
	source := al.boardToolsSource
	al.mu.RUnlock()
	if source == nil {
		return
	}
	for _, t := range source() {
		al.RegisterTool(t)
	}
}

// BoardToolsEnabled reports whether WithBoardTools was applied.
func (al *AgentLoop) BoardToolsEnabled() bool {
	al.mu.RLock()
	defer al.mu.RUnlock()
	return al.boardTools
}

// WorkflowToolsEnabled reports whether WithWorkflowTools was applied.
func (al *AgentLoop) WorkflowToolsEnabled() bool {
	al.mu.RLock()
	defer al.mu.RUnlock()
	return al.workflowTools
}

// applyWorkflowPrompt registers the workflow prompt contributor on every
// agent ContextBuilder of the current registry. Safe to call repeatedly:
// RegisterPromptContributor replaces any existing contributor with the same
// source id and invalidates each builder's system-prompt cache.
func (al *AgentLoop) applyWorkflowPrompt() {
	registry := al.GetRegistry()
	for _, agentID := range registry.ListAgentIDs() {
		if instance, ok := registry.GetAgent(agentID); ok && instance.ContextBuilder != nil {
			instance.ContextBuilder.WithWorkflowPrompt()
		}
	}
}

// SetChannelManager sets the channel manager for the agent loop.
func (al *AgentLoop) SetChannelManager(cm *channels.Manager) {
	al.channelManager = cm
}

func (al *AgentLoop) GetRegistry() *AgentRegistry {
	al.mu.RLock()
	defer al.mu.RUnlock()
	return al.registry
}

func (al *AgentLoop) GetConfig() *config.Config {
	al.mu.RLock()
	defer al.mu.RUnlock()
	return al.cfg
}

func (al *AgentLoop) SetMediaStore(s media.MediaStore) {
	al.mediaStore = s

	// Propagate store to all registered tools that can emit media.
	registry := al.GetRegistry()
	for _, agentID := range registry.ListAgentIDs() {
		if agent, ok := registry.GetAgent(agentID); ok {
			agent.Tools.SetMediaStore(s)
		}
	}
	registry.ForEachTool("send_tts", func(t tools.Tool) {
		if st, ok := t.(*tools.SendTTSTool); ok {
			st.SetMediaStore(s)
		}
	})
}

func (al *AgentLoop) SetTranscriber(t asr.Transcriber) {
	al.transcriber = t
}

func (al *AgentLoop) SetReloadFunc(fn func() error) {
	al.reloadFunc = fn
}

func (al *AgentLoop) RecordLastChannel(channel string) error {
	if al.state == nil {
		return nil
	}
	return al.state.SetLastChannel(channel)
}

// LastChannelChat returns the last channel/chat ID pair recorded by the
// state manager (empty when no state is available).
func (al *AgentLoop) LastChannelChat() (string, string) {
	al.mu.RLock()
	sm := al.state
	al.mu.RUnlock()
	if sm == nil {
		return "", ""
	}
	return sm.GetLastChannel(), sm.GetLastChatID()
}
func (al *AgentLoop) RecordLastChatID(chatID string) error {
	if al.state == nil {
		return nil
	}
	return al.state.SetLastChatID(chatID)
}

func (al *AgentLoop) GetStartupInfo() map[string]any {
	info := make(map[string]any)

	registry := al.GetRegistry()
	agent := registry.GetDefaultAgent()
	if agent == nil {
		return info
	}

	// Tools info
	toolsList := agent.Tools.List()
	info["tools"] = map[string]any{
		"count": len(toolsList),
		"names": toolsList,
	}

	// Skills info
	info["skills"] = agent.ContextBuilder.GetSkillsInfo()

	// Agents info
	info["agents"] = map[string]any{
		"count": len(registry.ListAgentIDs()),
		"ids":   registry.ListAgentIDs(),
	}

	return info
}
