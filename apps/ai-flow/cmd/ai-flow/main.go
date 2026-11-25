// ComputeFlux AI Bot (github.com/computeflux/ai-flow) - Ultra-lightweight personal AI agent
//
// CLI entrypoint. Subcommands:
//
//	ai-bot gateway            Start the long-running gateway (channels + agent loop).
//	ai-bot agent -m "..."     Ask the agent a question directly from the CLI.
//	ai-bot onboard            Create default config.json and seed the workspace.
package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	picoclaw "github.com/computeflux/ai-flow"
	"github.com/computeflux/ai-flow/pkg"
	"github.com/computeflux/ai-flow/pkg/agent"
	"github.com/computeflux/ai-flow/pkg/bus"
	"github.com/computeflux/ai-flow/pkg/channels"
	"github.com/computeflux/ai-flow/pkg/channels/board"
	"github.com/computeflux/ai-flow/pkg/modeladmin"

	// [workflow-fork] P1-6: blank-import channel factories so the manager can
	// init them (upstream design: sub-packages self-register via init(), the
	// gateway triggers registration with blank imports; matrix is excluded,
	// it needs cgo libolm and is not part of the workflow deployment).
	_ "github.com/computeflux/ai-flow/pkg/channels/deltachat"
	_ "github.com/computeflux/ai-flow/pkg/channels/dingtalk"
	_ "github.com/computeflux/ai-flow/pkg/channels/discord"
	_ "github.com/computeflux/ai-flow/pkg/channels/feishu"
	_ "github.com/computeflux/ai-flow/pkg/channels/irc"
	_ "github.com/computeflux/ai-flow/pkg/channels/line"
	_ "github.com/computeflux/ai-flow/pkg/channels/maixcam"
	_ "github.com/computeflux/ai-flow/pkg/channels/mqtt"
	_ "github.com/computeflux/ai-flow/pkg/channels/onebot"
	_ "github.com/computeflux/ai-flow/pkg/channels/qq"
	_ "github.com/computeflux/ai-flow/pkg/channels/slack"
	_ "github.com/computeflux/ai-flow/pkg/channels/slack_webhook"
	_ "github.com/computeflux/ai-flow/pkg/channels/teams_webhook"
	_ "github.com/computeflux/ai-flow/pkg/channels/telegram"
	_ "github.com/computeflux/ai-flow/pkg/channels/vk"
	_ "github.com/computeflux/ai-flow/pkg/channels/wecom"
	_ "github.com/computeflux/ai-flow/pkg/channels/whatsapp"
	_ "github.com/computeflux/ai-flow/pkg/channels/whatsapp_native"
	"github.com/computeflux/ai-flow/pkg/config"
	"github.com/computeflux/ai-flow/pkg/cron"
	"github.com/computeflux/ai-flow/pkg/health"
	"github.com/computeflux/ai-flow/pkg/logger"
	"github.com/computeflux/ai-flow/pkg/media"
	"github.com/computeflux/ai-flow/pkg/providers"
	"github.com/computeflux/ai-flow/pkg/tools"
	wfconfig "github.com/computeflux/ai-flow/pkg/workflow"
)

const version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-v", "--version", "version":
			fmt.Printf("%s %s\n", pkg.AppName, config.FormatVersion())
			return
		case "gateway":
			os.Exit(runGateway(os.Args[2:]))
		case "agent":
			os.Exit(runAgent(os.Args[2:]))
		case "onboard":
			os.Exit(runOnboard(os.Args[2:]))
		}
	}

	usage()
	if len(os.Args) > 1 {
		fmt.Fprintf(os.Stderr, "\nunknown command: %s\n", os.Args[1])
		os.Exit(2)
	}
	os.Exit(2)
}

func usage() {
	fmt.Fprintf(os.Stderr, `%s %s - ultra-lightweight personal AI agent

Usage:
  ai-bot <command> [options]

Commands:
  gateway                 Start the gateway (channels + agent loop)
  agent -m "question"     Ask the agent directly from the CLI
  onboard                 Create default config.json and workspace

Options:
  -c, --config <path>     Path to config.json (default: $PICOCLAW_CONFIG or ~/.picoclaw/config.json)
  -h, --help              Show this help
  -v, --version           Show version
`, pkg.AppName, version)
}

// resolveConfigPath returns the config file path from an explicit flag,
// the PICOCLAW_CONFIG env var, or $PICOCLAW_HOME/config.json.
func resolveConfigPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if envCfg := os.Getenv(config.EnvConfig); envCfg != "" {
		return envCfg
	}
	return filepath.Join(config.GetHome(), "config.json")
}

// defaultModelConfig returns the model config to use as the primary provider:
// the first enabled entry of cfg.ModelList.
func defaultModelConfig(cfg *config.Config) (*config.ModelConfig, error) {
	for _, m := range cfg.ModelList {
		if m == nil {
			continue
		}
		if m.Enabled || m.APIKey() != "" {
			return m, nil
		}
	}
	if len(cfg.ModelList) > 0 && cfg.ModelList[0] != nil {
		return cfg.ModelList[0], nil
	}
	return nil, fmt.Errorf("no model configured: add a model_list entry to config.json")
}

// newProviderFromConfig creates the primary LLM provider from the config's
// model_list using providers.CreateProviderFromConfig. When an explicit
// default model name is selected (agents.defaults.model_name, e.g. set by the
// P1-5 workflow merge), that model_list entry wins over the first-enabled
// scan, so workflow-created entries are never silently bypassed.
func newProviderFromConfig(cfg *config.Config) (providers.LLMProvider, string, error) {
	modelCfg, err := defaultModelConfig(cfg)
	if name := strings.TrimSpace(cfg.Agents.Defaults.GetModelName()); name != "" {
		if m, e := cfg.GetModelConfig(name); e == nil {
			modelCfg, err = m, nil
		}
	}
	if err != nil {
		return nil, "", err
	}
	provider, modelID, err := providers.CreateProviderFromConfig(modelCfg)
	if err != nil {
		return nil, "", fmt.Errorf("create provider for model %q: %w", modelCfg.Model, err)
	}
	return provider, modelID, nil
}

// gatewayRuntime owns the long-running gateway components assembled by
// runGateway so shutdown can unwind them in order.
type gatewayRuntime struct {
	mu           sync.Mutex
	cfgFile      string
	cfg          *config.Config
	bus          *bus.MessageBus
	agentLoop    *agent.AgentLoop
	channelMgr   *channels.Manager
	mediaStore   *media.FileMediaStore
	healthServer *health.Server
	cronService  *cron.CronService

	// [workflow-fork] P3-1: board chat channel (internal) + its /chat SSE
	// handler. Started/stopped alongside the channel manager.
	boardChannel *board.Channel
	boardHandler *board.Handler
}

func runGateway(args []string) int {
	fs := flag.NewFlagSet("gateway", flag.ExitOnError)
	cfgPath := fs.String("c", "", "path to config.json")
	fs.StringVar(cfgPath, "config", "", "path to config.json")
	_ = fs.Parse(args)

	rt := &gatewayRuntime{cfgFile: resolveConfigPath(*cfgPath)}

	// [workflow-fork] P1-5: merge workflow-side .env (WORKFLOW_API_URL /
	// AIFLOW_*) into the ai-bot runtime config before use.
	cfg, err := rt.loadMergedConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gateway: load config: %v\n", err)
		return 1
	}
	rt.cfg = cfg

	provider, modelID, err := newProviderFromConfig(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gateway: %v\n", err)
		return 1
	}
	logger.InfoCF("gateway", "Primary provider initialized", map[string]any{
		"model": modelID,
	})

	// Core components (see pkg/channels/README.md §A.4 Gateway Startup Sequence).
	rt.bus = bus.NewMessageBus()
	rt.agentLoop = agent.NewAgentLoop(cfg, rt.bus, provider)
	defer rt.agentLoop.Close()

	// [workflow-fork] P2-4: inject the workflow system-prompt section
	// (terminology + query-before-write + P2 read-only red lines) into every
	// agent; persists across hot reloads. Client is wired when
	// WORKFLOW_API_URL is configured (P2-1 typed read-only client).
	rt.agentLoop.WithWorkflowTools(newWorkflowClient())

	// [workflow-fork] P3-1: board chat channel (POST /chat SSE for the
	// board frontend AI sidebar). The channel is an internal channel: its
	// relay goroutine consumes bus messages addressed to "board" directly;
	// the manager's dispatcher skips internal channels. The declarative
	// mutation tools + the identity interceptor hook are registered on the
	// agent loop (persisted across hot reloads).
	rt.boardChannel = board.New(rt.bus)
	rt.boardHandler = board.NewHandler(rt.boardChannel, rt.bus, board.Options{})
	rt.agentLoop.WithBoardTools(board.NewMutationTools, board.NewIdentityHook(rt.boardChannel))
	agent.SetStreamChannelReopener(rt.boardChannel)
	agent.SetBoardTurnResolver(func(ctx context.Context) *agent.BoardTurnInfo {
		t := board.TurnFromContext(ctx)
		if t == nil {
			return nil
		}
		userID, jwt := t.TurnInfo()
		return &agent.BoardTurnInfo{UserID: userID, JWT: jwt}
	})

	rt.mediaStore = media.NewFileMediaStoreWithCleanup(mediaCleanerConfig(cfg))
	rt.mediaStore.Start()
	defer rt.mediaStore.Stop()

	chMgr, err := channels.NewManager(cfg, rt.bus, rt.mediaStore,
		channels.WithExtraHTTPHandlers(map[string]http.Handler{
			board.ChatPath: rt.boardHandler,
			// [workflow-fork] S8 模型管理：/models 读写模型目录（GET 脱敏目录，
			// PUT 替换 + 落盘 config.json + 热更全部 agent）。仅内部 :8134，
			// board 经 core-ui nginx /api/ai-bot/models 代理访问。
			"/models": modeladmin.NewHandler(modelStore{rt: rt}),
		}),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gateway: init channels: %v\n", err)
		rt.bus.Close()
		return 1
	}
	rt.channelMgr = chMgr
	rt.agentLoop.SetChannelManager(chMgr)
	chMgr.RegisterChannel(board.ChannelName, rt.boardChannel)
	// Share the same media store with the agent loop so that inbound media://
	// refs, audio transcription, and send_file/load_image tools can resolve
	// media stored by the channels manager.
	rt.agentLoop.SetMediaStore(rt.mediaStore)

	// Shared HTTP server: health/ready/reload endpoints + channel webhooks
	// (pico /pico/ws, Slack/Teams webhooks, ...). StartAll serves it when set.
	addr := net.JoinHostPort(cfg.Gateway.Host, strconv.Itoa(cfg.Gateway.Port))
	rt.healthServer = health.NewServer(cfg.Gateway.Host, cfg.Gateway.Port, "")
	chMgr.SetupHTTPServer(addr, rt.healthServer)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Hot reload: /reload endpoint + /reload chat command.
	rt.healthServer.SetReloadFunc(rt.reload)
	rt.agentLoop.SetReloadFunc(rt.reload)

	// Cron service + tool: scheduled reminders/commands.
	if err := rt.setupCron(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "gateway: init cron: %v\n", err)
		rt.bus.Close()
		return 1
	}
	defer func() {
		if rt.cronService != nil {
			rt.cronService.Stop()
		}
	}()

	if err := chMgr.StartAll(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "gateway: start channels: %v\n", err)
		rt.bus.Close()
		return 1
	}
	rt.healthServer.SetReady(true)
	defer func() {
		rt.healthServer.SetReady(false)
		if err := chMgr.StopAll(context.Background()); err != nil {
			logger.WarnCF("gateway", "Error stopping channels", map[string]any{"error": err.Error()})
		}
	}()

	// Heartbeat: periodic heartbeat prompt driven by agents.defaults.heartbeat.
	stopHeartbeat := rt.startHeartbeat(ctx)
	defer stopHeartbeat()

	logger.InfoCF("gateway", "Gateway started; press Ctrl+C to stop", map[string]any{
		"addr": addr,
	})

	runErr := rt.agentLoop.Run(ctx)

	stop() // restore default signal handling
	rt.bus.Close()

	if runErr != nil && !strings.Contains(runErr.Error(), context.Canceled.Error()) {
		fmt.Fprintf(os.Stderr, "gateway: %v\n", runErr)
		return 1
	}
	logger.InfoCF("gateway", "Gateway stopped", nil)
	return 0
}

// mediaCleanerConfig derives the media TTL cleanup config from gateway config.
func mediaCleanerConfig(cfg *config.Config) media.MediaCleanerConfig {
	return media.MediaCleanerConfig{
		Enabled:  true,
		Interval: time.Hour,
		MaxAge:   24 * time.Hour,
	}
}

// setupCron constructs the cron service backed by $PICOCLAW_HOME/cron.json,
// wires its job handler through the cron tool executor, starts it, and
// registers the cron tool on every agent.
func (rt *gatewayRuntime) setupCron(ctx context.Context) error {
	storePath := filepath.Join(config.GetHome(), "cron.json")
	rt.cronService = cron.NewCronService(storePath, func(job *cron.CronJob) (string, error) {
		execCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		return rt.runCronJob(execCtx, job)
	})

	cronTool, err := tools.NewCronTool(
		rt.cronService,
		&cronExecutor{al: rt.agentLoop},
		rt.bus,
		defaultAgentWorkspace(rt.cfg),
		rt.cfg.Agents.Defaults.RestrictToWorkspace,
		time.Duration(rt.cfg.Tools.Cron.ExecTimeoutMinutes)*time.Minute,
		rt.cfg,
	)
	if err != nil {
		return err
	}
	rt.agentLoop.RegisterTool(cronTool)

	return rt.cronService.Start()
}

// runCronJob dispatches a triggered job. When the cron tool was not yet
// registered (e.g. gateway restarted mid-run), fall back to executing it
// through an ephemeral tool bound to the current agent loop.
func (rt *gatewayRuntime) runCronJob(ctx context.Context, job *cron.CronJob) (string, error) {
	cronTool, ok := findCronTool(rt.agentLoop)
	if ok {
		return cronTool.ExecuteJob(ctx, job), nil
	}
	execCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	tool, err := tools.NewCronTool(
		rt.cronService,
		&cronExecutor{al: rt.agentLoop},
		rt.bus,
		defaultAgentWorkspace(rt.cfg),
		rt.cfg.Agents.Defaults.RestrictToWorkspace,
		time.Duration(rt.cfg.Tools.Cron.ExecTimeoutMinutes)*time.Minute,
		rt.cfg,
	)
	if err != nil {
		return "", err
	}
	return tool.ExecuteJob(execCtx, job), nil
}

// findCronTool looks up the registered cron tool on the current registry.
func findCronTool(al *agent.AgentLoop) (*tools.CronTool, bool) {
	var found *tools.CronTool
	al.GetRegistry().ForEachTool("cron", func(t tools.Tool) {
		if ct, ok := t.(*tools.CronTool); ok {
			found = ct
		}
	})
	return found, found != nil
}

// cronExecutor adapts AgentLoop to the tools.JobExecutor interface.
type cronExecutor struct {
	al *agent.AgentLoop
}

func (e *cronExecutor) ProcessDirectWithChannel(
	ctx context.Context, content, sessionKey, channel, chatID string,
) (string, error) {
	return e.al.ProcessDirectWithChannel(ctx, content, sessionKey, channel, chatID)
}

func (e *cronExecutor) PublishResponseIfNeeded(
	ctx context.Context, channel, chatID, sessionKey, response string,
) {
	e.al.PublishResponseIfNeeded(ctx, channel, chatID, sessionKey, response)
}

// startHeartbeat launches the heartbeat ticker driven by the top-level
// heartbeat config (enabled + interval minutes, min 5). Returns a stop
// function.
func (rt *gatewayRuntime) startHeartbeat(ctx context.Context) func() {
	hb := rt.cfg.Heartbeat
	if !hb.Enabled {
		return func() {}
	}
	intervalMin := hb.Interval
	if intervalMin < 5 {
		intervalMin = 5
	}
	interval := time.Duration(intervalMin) * time.Minute

	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				hbCfg := rt.heartbeatConfig()
				if hbCfg == nil || !hbCfg.Enabled {
					continue
				}
				channel, chatID := rt.lastChannelChat()
				_, err := rt.agentLoop.ProcessHeartbeat(ctx, heartbeatPrompt, channel, chatID)
				if err != nil && ctx.Err() == nil {
					logger.WarnCF("gateway", "Heartbeat run failed", map[string]any{
						"error": err.Error(),
					})
				}
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

// heartbeatPrompt is the instruction injected on each heartbeat turn.
const heartbeatPrompt = "Process HEARTBEAT.md in your workspace: if it defines tasks, run them; otherwise reply HEARTBEAT_OK."

func (rt *gatewayRuntime) heartbeatConfig() *config.HeartbeatConfig {
	cfg := rt.agentLoop.GetConfig()
	if cfg == nil {
		return nil
	}
	return &cfg.Heartbeat
}

// lastChannelChat resolves the last seen channel/chat pair recorded by the
// agent loop state, used to route heartbeat responses.
func (rt *gatewayRuntime) lastChannelChat() (string, string) {
	return rt.agentLoop.LastChannelChat()
}

// newWorkflowClient builds the typed workflow-api read-only client (P2-1)
// from the merged WORKFLOW_API_URL env (LoadWorkflowConfig ensures it is
// present in the process env after merging the root .env); returns nil when
// unset. [workflow-fork] P2-4.
func newWorkflowClient() *wfconfig.Client {
	url := strings.TrimSpace(os.Getenv(wfconfig.DefaultWorkflowAPIURLEnv))
	if url == "" {
		return nil
	}
	return wfconfig.NewClient(url)
}

// loadMergedConfig loads config.json and merges the workflow-side runtime
// configuration (root .env: WORKFLOW_API_URL / AIFLOW_*). Shared by
// gateway startup and hot reload so /reload never loses the workflow
// overrides. [workflow-fork] P1-5.
func (rt *gatewayRuntime) loadMergedConfig() (*config.Config, error) {
	cfg, err := config.LoadConfig(rt.cfgFile)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	wfCfg, err := wfconfig.LoadWorkflowConfig(cfg, rt.cfgFile)
	if err != nil {
		return nil, fmt.Errorf("workflow config: %w", err)
	}
	logger.InfoCF("gateway", "Workflow config loaded", wfCfg.Summary())
	return cfg, nil
}

// reload re-reads config.json and hot-reloads provider/agent registry and
// channels. Wired to both the /reload HTTP endpoint and the /reload chat
// command (gateway.hot_reload).
func (rt *gatewayRuntime) reload() error {
	cfg, err := rt.loadMergedConfig()
	if err != nil {
		return fmt.Errorf("reload: %w", err)
	}

	provider, _, err := newProviderFromConfig(cfg)
	if err != nil {
		return fmt.Errorf("reload: provider: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := rt.agentLoop.ReloadProviderAndConfig(ctx, provider, cfg); err != nil {
		return fmt.Errorf("reload: agent: %w", err)
	}

	rt.mu.Lock()
	rt.cfg = cfg
	rt.mu.Unlock()

	if err := rt.channelMgr.Reload(ctx, cfg); err != nil {
		return fmt.Errorf("reload: channels: %w", err)
	}

	logger.InfoCF("gateway", "Config reloaded", nil)
	return nil
}

// modelStore implements modeladmin.Store on top of the gateway runtime
// ([workflow-fork] S8 模型管理)：GET /models 读当前 model_list + 活动模型；
// PUT /models 替换目录（保留同名条目的既有 api_keys）后落盘 config.json，
// 并走与 /reload 相同的热更链（provider + 全部 agent 注册表 + channels）。
type modelStore struct{ rt *gatewayRuntime }

func (s modelStore) Catalog() ([]config.ModelConfig, string, error) {
	s.rt.mu.Lock()
	defer s.rt.mu.Unlock()
	if s.rt.cfg == nil {
		return nil, "", fmt.Errorf("config not loaded")
	}
	out := make([]config.ModelConfig, 0, len(s.rt.cfg.ModelList))
	for _, m := range s.rt.cfg.ModelList {
		if m == nil {
			continue
		}
		v := *m
		v.APIKeys = nil // 响应绝不携带密钥（has_api_key 由调用方序列化前取）
		out = append(out, v)
	}
	return out, strings.TrimSpace(s.rt.cfg.Agents.Defaults.GetModelName()), nil
}

func (s modelStore) Apply(models []config.ModelConfig, active string) (string, error) {
	s.rt.mu.Lock()
	cur := s.rt.cfg
	s.rt.mu.Unlock()
	if cur == nil {
		return "", fmt.Errorf("config not loaded")
	}

	// 同名条目的 api_keys 从现配置继承（UI 不回显、不回传密钥）。
	oldKeys := map[string]config.SecureStrings{}
	for _, m := range cur.ModelList {
		if m != nil {
			oldKeys[m.ModelName] = m.APIKeys
		}
	}

	newCfg := *cur
	list := make(config.SecureModelList, 0, len(models))
	for i := range models {
		m := models[i]
		if strings.TrimSpace(m.ModelName) == "" {
			return "", fmt.Errorf("models[%d]: model_name is required", i)
		}
		if keys, ok := oldKeys[m.ModelName]; ok {
			m.APIKeys = keys
		}
		if m.Model == "" {
			m.Model = m.ModelName
		}
		list = append(list, &m)
	}
	newCfg.ModelList = list
	if strings.TrimSpace(active) != "" {
		newCfg.Agents.Defaults.ModelName = strings.TrimSpace(active)
	}

	if err := config.SaveConfig(s.rt.cfgFile, &newCfg); err != nil {
		return "", fmt.Errorf("save config: %w", err)
	}

	// 热更：与 /reload 同一条链（重读合并后的新 config → provider → 全部 agent）。
	if err := s.rt.reload(); err != nil {
		return "", fmt.Errorf("config saved, reload failed: %w", err)
	}

	s.rt.mu.Lock()
	nowActive := ""
	if s.rt.cfg != nil {
		nowActive = strings.TrimSpace(s.rt.cfg.Agents.Defaults.GetModelName())
	}
	s.rt.mu.Unlock()
	return nowActive, nil
}

// defaultAgentWorkspace resolves the default agent workspace from config.
func defaultAgentWorkspace(cfg *config.Config) string {
	if ws := strings.TrimSpace(cfg.Agents.Defaults.Workspace); ws != "" {
		return ws
	}
	return filepath.Join(config.GetHome(), pkg.WorkspaceName)
}

func runAgent(args []string) int {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	message := fs.String("m", "", "message to send to the agent")
	cfgPath := fs.String("c", "", "path to config.json")
	fs.StringVar(cfgPath, "config", "", "path to config.json")
	_ = fs.Parse(args)

	if strings.TrimSpace(*message) == "" {
		fmt.Fprintln(os.Stderr, `agent: -m "question" is required (example: ai-bot agent -m "hi")`)
		return 2
	}

	cfg, err := config.LoadConfig(resolveConfigPath(*cfgPath))
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent: load config: %v\n", err)
		return 1
	}

	provider, _, err := newProviderFromConfig(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent: %v\n", err)
		return 1
	}

	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	mediaStore := media.NewFileMediaStore()

	al := agent.NewAgentLoop(cfg, msgBus, provider)
	// [workflow-fork] P2-4: same workflow prompt injection as the gateway.
	al.WithWorkflowTools(newWorkflowClient())
	// Share the media store so send_file/load_image tools and media:// refs
	// resolve in direct CLI sessions.
	al.SetMediaStore(mediaStore)
	defer al.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	response, err := al.ProcessDirectWithChannel(ctx, *message, "cli-direct", "cli", "direct")
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent: %v\n", err)
		return 1
	}
	fmt.Println(strings.TrimRight(response, "\n"))
	return 0
}

// runOnboard creates the default config.json (when absent) and seeds the
// workspace from the embedded onboarding template. Used by the docker
// entrypoint first-run flow.
func runOnboard(args []string) int {
	fs := flag.NewFlagSet("onboard", flag.ExitOnError)
	cfgPath := fs.String("c", "", "path to config.json")
	fs.StringVar(cfgPath, "config", "", "path to config.json")
	_ = fs.Parse(args)

	cfgFile := resolveConfigPath(*cfgPath)
	home := filepath.Dir(cfgFile)

	if err := os.MkdirAll(home, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "onboard: create home %s: %v\n", home, err)
		return 1
	}

	if _, err := os.Stat(cfgFile); err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "onboard: stat config: %v\n", err)
			return 1
		}
		cfg := config.DefaultConfig()
		if err := config.SaveConfig(cfgFile, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "onboard: write default config: %v\n", err)
			return 1
		}
		fmt.Printf("Created default config: %s\n", cfgFile)
	} else {
		fmt.Printf("Config already exists: %s\n", cfgFile)
	}

	// Seed the default workspace from the embedded template (non-destructive).
	workspace := filepath.Join(home, pkg.WorkspaceName)
	if err := seedWorkspace(workspace); err != nil {
		fmt.Fprintf(os.Stderr, "onboard: seed workspace: %v\n", err)
		return 1
	}
	fmt.Printf("Workspace ready: %s\n", workspace)
	return 0
}

// seedWorkspace copies the embedded onboarding workspace template into dest
// without overwriting existing files.
func seedWorkspace(dest string) error {
	return fs.WalkDir(picoclaw.OnboardWorkspace, "workspace", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path, "workspace")
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			return nil
		}
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if _, err := os.Stat(target); err == nil {
			return nil // keep existing files untouched
		}
		data, err := picoclaw.OnboardWorkspace.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}
