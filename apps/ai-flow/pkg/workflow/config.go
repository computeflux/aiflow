// [workflow-fork] P1-5: ai-bot workflow 配置加载（docs/plan，工作项 wi-1794304991-cf50）
//
// 本包把 workflow 侧的运行时配置（根 .env，经 compose env_file 注入进程环境；
// 容器内同时挂载到 /app/.env 供 godotenv 直读）与 ai-bot 自身的 config.json
// （agents.defaults + model_list + gateway.port）合并，生成最终运行时配置。
//
// 优先级（低 → 高）：
//  1. config.json 文件里已有的值（agents.defaults / model_list / gateway）
//  2. workflow 侧 .env 文件里的 AIFLOW_* / WORKFLOW_API_URL
//     （godotenv 语义：不覆盖进程环境中已存在的变量）
//  3. 进程环境里已有的 PICOCLAW_* 变量（由 compose environment / env_file 注入，
//     最终经 config.LoadConfig 内部的 env.Parse 统一应用）
//
// 支持的环境变量（AIFLOW_ 前缀）：
//   AIFLOW_PORT            gateway.port（仅显式设置时覆盖；未设且配置也无端口时默认 8134）
//   AIFLOW_HOST            gateway.host（容器内默认 0.0.0.0）
//   AIFLOW_MODEL_NAME      agents.defaults.model_name + model_list 条目别名
//   AIFLOW_PROVIDER        model_list 条目 provider（如 openai / deepseek）
//   AIFLOW_BASE_URL        model_list 条目 api_base
//   AIFLOW_API_KEY         model_list 条目 api_keys[0]
//   AIFLOW_MAX_TOOL_ITERATIONS   agents.defaults.max_tool_iterations（轮次上限）
//
// [workflow-fork] Telegram 渠道接入（workflow 侧 env，无 config.json 时自动注入）：
//   PICOCLAW_CHANNELS_TELEGRAM_TOKEN          bot token（已设 → 自动启用 telegram 渠道）
//   PICOCLAW_CHANNELS_TELEGRAM_ENABLED        显式启用/禁用（未设时由 token 推断）
//   PICOCLAW_CHANNELS_TELEGRAM_ALLOW_FROM     逗号分隔白名单（"*" = 公开 bot）；
//                                             未设且非公开 → 启动告警（开放给任何人）
//
// WORKFLOW_API_URL 是 workflow API（cmd/api，:8001）地址；本包加载后将其确保
// 存在于进程环境（.env 文件值补齐到 env），供 pkg/workflow 客户端与日志使用。

package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/joho/godotenv"

	"github.com/computeflux/ai-flow/pkg/config"
	"github.com/computeflux/ai-flow/pkg/logger"
)

// DefaultWorkflowAPIURLEnv is the env key carrying the workflow API base URL.
const DefaultWorkflowAPIURLEnv = "WORKFLOW_API_URL"

// Env keys (all AIFLOW_ prefixed, see package doc).
const (
	EnvPort            = "AIFLOW_PORT"
	EnvHost            = "AIFLOW_HOST"
	EnvModelName       = "AIFLOW_MODEL_NAME"
	EnvProvider        = "AIFLOW_PROVIDER"
	EnvBaseURL         = "AIFLOW_BASE_URL"
	EnvAPIKey          = "AIFLOW_API_KEY"
	EnvMaxToolIter     = "AIFLOW_MAX_TOOL_ITERATIONS"
	defaultGatewayPort = 8134
)

// [workflow-fork] Telegram channel env keys (see package doc).
const (
	EnvTelegramToken    = "PICOCLAW_CHANNELS_TELEGRAM_TOKEN"
	EnvTelegramEnabled  = "PICOCLAW_CHANNELS_TELEGRAM_ENABLED"
	EnvTelegramAllowFrm = "PICOCLAW_CHANNELS_TELEGRAM_ALLOW_FROM"
)

// WorkflowConfig is the merged runtime view of workflow-side (.env) and
// ai-bot-side (config.json) configuration.
type WorkflowConfig struct {
	// WorkflowAPIURL is the workflow API base URL (WORKFLOW_API_URL).
	WorkflowAPIURL string
	// GatewayPort is the ai-bot gateway listen port (default 8134).
	GatewayPort int
	// GatewayHost is the ai-bot gateway listen host.
	GatewayHost string
	// ModelName / Provider / BaseURL / APIKey describe the model override
	// coming from AIFLOW_* env vars (empty = no override).
	ModelName string
	Provider  string
	BaseURL   string
	APIKey    string
	// MaxToolIterations is the agent tool-iteration round limit (0 = keep file/default).
	MaxToolIterations int
	// telegramEnabled / telegramAllowFrom mirror the resolved telegram channel
	// state for Summary logging (secrets never included).
	telegramEnabled   bool
	telegramAllowFrom []string
}

// envCandidates returns .env file candidates to load (root .env mounted at
// /app/.env in containers; repo-root relative path for local runs).
func envCandidates(configPath string) []string {
	candidates := []string{".env", "/app/.env"}
	if configPath != "" {
		candidates = append(candidates, filepath.Join(filepath.Dir(configPath), ".env"))
	}
	return candidates
}

// LoadWorkflowConfig reads the workflow-side .env (godotenv; existing process
// env wins) and returns the merged workflow configuration. It applies
// AIFLOW_* values into the ai-bot config (gateway port/host, agent
// defaults model/iterations, model_list entry) and ensures WORKFLOW_API_URL is
// present in the process environment.
func LoadWorkflowConfig(cfg *config.Config, configPath string) (*WorkflowConfig, error) {
	if cfg == nil {
		return nil, fmt.Errorf("workflow config: nil config")
	}

	// godotenv.Load: values already present in the process environment are NOT
	// overridden (compose env_file semantics). Load each existing candidate
	// (cwd .env → /app/.env → config-dir .env) so a key is picked up from
	// whichever canonical location holds it.
	for _, p := range envCandidates(configPath) {
		if _, err := os.Stat(p); err == nil {
			if err := godotenv.Load(p); err != nil {
				return nil, fmt.Errorf("workflow config: load %s: %w", p, err)
			}
		}
	}

	wf := &WorkflowConfig{
		WorkflowAPIURL: strings.TrimSpace(os.Getenv(DefaultWorkflowAPIURLEnv)),
		GatewayHost:    strings.TrimSpace(os.Getenv(EnvHost)),
		ModelName:      strings.TrimSpace(os.Getenv(EnvModelName)),
		Provider:       strings.TrimSpace(os.Getenv(EnvProvider)),
		BaseURL:        strings.TrimSpace(os.Getenv(EnvBaseURL)),
		APIKey:         strings.TrimSpace(os.Getenv(EnvAPIKey)),
	}

	// Gateway port: only an explicit AIFLOW_PORT overrides the
	// baseline (config.json gateway.port, incl. values already applied by
	// LoadConfig's env.Parse for PICOCLAW_GATEWAY_PORT). When unset, the
	// loaded config keeps its port; if that is still empty (a config.json
	// without a gateway section), fall back to the workflow default 8134.
	if v := strings.TrimSpace(os.Getenv(EnvPort)); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil || port <= 0 || port > 65535 {
			return nil, fmt.Errorf("workflow config: invalid %s: %q", EnvPort, v)
		}
		wf.GatewayPort = port
		cfg.Gateway.Port = port
	} else if cfg.Gateway.Port <= 0 {
		wf.GatewayPort = defaultGatewayPort
		cfg.Gateway.Port = defaultGatewayPort
	} else {
		wf.GatewayPort = cfg.Gateway.Port
	}
	if v := strings.TrimSpace(os.Getenv(EnvMaxToolIter)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("workflow config: invalid %s: %q", EnvMaxToolIter, v)
		}
		wf.MaxToolIterations = n
	}

	// Gateway host: explicit AIFLOW_HOST overrides config.json.
	if wf.GatewayHost != "" {
		cfg.Gateway.Host = wf.GatewayHost
	}
	// Model override: patch agents.defaults + ensure a model_list entry exists.
	if wf.ModelName != "" {
		cfg.Agents.Defaults.ModelName = wf.ModelName
		wf.mergeModelList(cfg)
	}
	if wf.MaxToolIterations > 0 {
		cfg.Agents.Defaults.MaxToolIterations = wf.MaxToolIterations
	}

	// [workflow-fork] board AI 侧栏原生逐 token 流式：渠道级开关（与
	// mergeModelList 的模型级开关配套，缺一不可）。board 是内部虚拟渠道，
	// 不走 factory 初始化（由 cmd/ai-flow 直接 RegisterChannel），这里只需
	// 把 settings.streaming.enabled 写进渠道配置供 pipeline 的
	// channelStreamingConfig 读取；预先用原型 Decode 一次，避免
	// InitChannelList 对未注册类型报 unknown type。
	wf.mergeBoardChannel(cfg)

	// [workflow-fork] Telegram channel: inject/enable from workflow env so a
	// deployment needs no config.json (channel enabled is not env-bindable).
	if err := wf.applyTelegramChannel(cfg); err != nil {
		return nil, fmt.Errorf("workflow config: telegram channel: %w", err)
	}

	// Ensure WORKFLOW_API_URL is visible to downstream consumers (env.Parse,
	// pkg/workflow clients) even when it only came from the .env file.
	if wf.WorkflowAPIURL != "" && os.Getenv(DefaultWorkflowAPIURLEnv) == "" {
		_ = os.Setenv(DefaultWorkflowAPIURLEnv, wf.WorkflowAPIURL)
	}

	return wf, nil
}

// mergeModelList ensures cfg.ModelList contains an entry named
// WorkflowConfig.ModelName; creates one from AIFLOW_PROVIDER /
// BASE_URL / API_KEY when missing, otherwise patches api_base / api_keys when
// they are set on the env. Newly created entries are inserted at the FRONT of
// model_list: primary provider selection (cmd/ai-bot defaultModelConfig)
// picks the first enabled/keyed entry, so a workflow-created model must be
// found before any config.json / DefaultConfig entry.
func (w *WorkflowConfig) mergeModelList(cfg *config.Config) {
	for _, m := range cfg.ModelList {
		if m == nil || m.ModelName != w.ModelName {
			continue
		}
		if w.BaseURL != "" {
			m.APIBase = w.BaseURL
		}
		if w.Provider != "" {
			m.Provider = w.Provider
		}
		if w.APIKey != "" {
			m.APIKeys = config.SecureStrings{config.NewSecureString(w.APIKey)}
		}
		m.Enabled = true
		// [workflow-fork] board AI 侧栏原生逐 token 流式：agent pipeline 的
		// configured-streaming 要求 model_list 条目显式开启 streaming.enabled，
		// 这里随 AIFLOW_* 注入一并打开（配合 mergeBoardChannel 的渠道级开关）。
		m.Streaming = config.ModelStreamingConfig{Enabled: true}
		return
	}

	entry := &config.ModelConfig{
		ModelName: w.ModelName,
		Provider:  w.Provider,
		Model:     w.ModelName,
		APIBase:   w.BaseURL,
		Enabled:   true,
		Streaming: config.ModelStreamingConfig{Enabled: true},
	}
	if w.APIKey != "" {
		entry.APIKeys = config.SecureStrings{config.NewSecureString(w.APIKey)}
	}
	cfg.ModelList = append(config.SecureModelList{entry}, cfg.ModelList...)
}

// mergeBoardChannel ensures cfg.Channels has a "board" entry with
// settings.streaming.enabled = true so the agent pipeline's configured
// streaming path is eligible for the board AI sidebar (see
// pkg/agent/pipeline_streaming.go configuredStreamingEligible). Settings use
// the registered BoardSettings prototype (pkg/config), matching how the
// built-in channels decode their settings blocks.
func (w *WorkflowConfig) mergeBoardChannel(cfg *config.Config) {
	if cfg.Channels == nil {
		cfg.Channels = config.ChannelsConfig{}
	}
	bc := cfg.Channels["board"]
	if bc == nil {
		bc = &config.Channel{}
		bc.SetName("board")
		bc.Type = "board"
		cfg.Channels["board"] = bc
	}
	settings := &config.BoardSettings{Streaming: config.StreamingConfig{Enabled: true}}
	if err := bc.Decode(settings); err != nil {
		logger.WarnCF("workflow-config", "board channel streaming inject failed", map[string]any{
			"error": err.Error(),
		})
		return
	}
}

// applyTelegramChannel merges workflow-side telegram channel env into cfg.
// The token env is applied onto the decoded TelegramSettings (InitChannelList
// decodes settings from config.json and applies its own env tags; this runs
// after LoadConfig, so we patch the typed struct directly). Enable semantics:
// PICOCLAW_CHANNELS_TELEGRAM_ENABLED explicit true/false wins; unset + token
// present => enabled. allow_from is set on the Channel (comma-separated);
// when it stays empty and not explicitly "*", the channel remains open to any
// sender (upstream secure-by-default audit warning applies).
func (w *WorkflowConfig) applyTelegramChannel(cfg *config.Config) error {
	if cfg.Channels == nil {
		cfg.Channels = config.ChannelsConfig{}
	}
	bc := cfg.Channels[config.ChannelTelegram]
	if bc == nil {
		bc = &config.Channel{Type: config.ChannelTelegram}
		bc.SetName(config.ChannelTelegram)
		cfg.Channels[config.ChannelTelegram] = bc
	}

	token := strings.TrimSpace(os.Getenv(EnvTelegramToken))
	enabledRaw := strings.TrimSpace(os.Getenv(EnvTelegramEnabled))

	if token != "" {
		decoded, err := bc.GetDecoded()
		if err != nil {
			return fmt.Errorf("decode settings: %w", err)
		}
		tg, ok := decoded.(*config.TelegramSettings)
		if !ok {
			return fmt.Errorf("unexpected settings type %T", decoded)
		}
		tg.Token = *config.NewSecureString(token)
	}

	switch strings.ToLower(enabledRaw) {
	case "":
		if token != "" {
			bc.Enabled = true
			w.telegramEnabled = true
		}
	case "true", "1", "yes":
		bc.Enabled = true
		w.telegramEnabled = true
	case "false", "0", "no":
		bc.Enabled = false
		w.telegramEnabled = false
	default:
		return fmt.Errorf("invalid %s: %q", EnvTelegramEnabled, enabledRaw)
	}

	if bc.Enabled {
		if v := strings.TrimSpace(os.Getenv(EnvTelegramAllowFrm)); v != "" {
			w.telegramAllowFrom = splitCSV(v)
			bc.AllowFrom = w.telegramAllowFrom
		}
	}
	return nil
}

// splitCSV splits a comma-separated list, trimming spaces and dropping empties.
func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Summary returns a loggable (secret-free) summary of the merged workflow
// configuration. The API key is masked; only its presence is reported.
func (w *WorkflowConfig) Summary() map[string]any {
	return map[string]any{
		"workflow_api_url":    w.WorkflowAPIURL,
		"gateway_host":        w.GatewayHost,
		"gateway_port":        w.GatewayPort,
		"model_name":          w.ModelName,
		"provider":            w.Provider,
		"base_url":            w.BaseURL,
		"api_key_set":         w.APIKey != "",
		"max_tool_iterations": w.MaxToolIterations,
		"telegram_enabled":    w.telegramEnabled,
		"telegram_allow_from": strings.Join(w.telegramAllowFrom, ","),
	}
}
