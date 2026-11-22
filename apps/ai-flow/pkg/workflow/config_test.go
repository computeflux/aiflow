// PicoClaw - Ultra-lightweight personal AI agent
// License: MIT
//
// [workflow-fork] P1-5 unit tests for workflow config merging.

package workflow

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/computeflux/ai-flow/pkg/config"
)

func TestLoadWorkflowConfigPortPriority(t *testing.T) {
	// Isolate from any ambient .env / env in the test environment.
	t.Setenv(EnvPort, "")
	os.Unsetenv(EnvPort)
	t.Setenv(EnvHost, "")
	t.Setenv(EnvModelName, "")
	t.Setenv(EnvProvider, "")
	t.Setenv(EnvBaseURL, "")
	t.Setenv(EnvAPIKey, "")
	t.Setenv(EnvMaxToolIter, "")
	t.Setenv(DefaultWorkflowAPIURLEnv, "")

	// Run in a temp dir so no .env candidates (".env") are picked up.
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	t.Run("config.json port kept when env unset", func(t *testing.T) {
		cfg := config.DefaultConfig() // gateway.port = 18790
		base := cfg.Gateway.Port
		if base == 0 {
			t.Fatal("default config has no gateway port")
		}
		wf, err := LoadWorkflowConfig(cfg, "")
		if err != nil {
			t.Fatalf("LoadWorkflowConfig: %v", err)
		}
		if cfg.Gateway.Port != base {
			t.Fatalf("config.json port dropped: got %d, want %d", cfg.Gateway.Port, base)
		}
		if wf.GatewayPort != base {
			t.Fatalf("summary port %d != config port %d", wf.GatewayPort, base)
		}
	})

	t.Run("PICOCLAW_GATEWAY_PORT applied by LoadConfig env.Parse is kept", func(t *testing.T) {
		// Simulates the value LoadConfig's env.Parse would have applied.
		cfg := config.DefaultConfig()
		cfg.Gateway.Port = 19999
		wf, err := LoadWorkflowConfig(cfg, "")
		if err != nil {
			t.Fatalf("LoadWorkflowConfig: %v", err)
		}
		if cfg.Gateway.Port != 19999 || wf.GatewayPort != 19999 {
			t.Fatalf("PICOCLAW_GATEWAY_PORT overridden: cfg=%d wf=%d", cfg.Gateway.Port, wf.GatewayPort)
		}
	})

	t.Run("explicit AIFLOW_PORT overrides config.json", func(t *testing.T) {
		t.Setenv(EnvPort, "8134")
		cfg := config.DefaultConfig()
		wf, err := LoadWorkflowConfig(cfg, "")
		if err != nil {
			t.Fatalf("LoadWorkflowConfig: %v", err)
		}
		if cfg.Gateway.Port != 8134 || wf.GatewayPort != 8134 {
			t.Fatalf("explicit port not applied: cfg=%d wf=%d", cfg.Gateway.Port, wf.GatewayPort)
		}
	})

	t.Run("empty gateway falls back to default 8134", func(t *testing.T) {
		cfg := &config.Config{} // no gateway section
		wf, err := LoadWorkflowConfig(cfg, "")
		if err != nil {
			t.Fatalf("LoadWorkflowConfig: %v", err)
		}
		if cfg.Gateway.Port != 8134 || wf.GatewayPort != 8134 {
			t.Fatalf("default port fallback failed: cfg=%d wf=%d", cfg.Gateway.Port, wf.GatewayPort)
		}
	})

	t.Run("invalid AIFLOW_PORT rejected", func(t *testing.T) {
		t.Setenv(EnvPort, "not-a-port")
		cfg := config.DefaultConfig()
		if _, err := LoadWorkflowConfig(cfg, ""); err == nil {
			t.Fatal("expected error for invalid port")
		}
	})
}

func TestLoadWorkflowConfigMergesModelAndEnvFile(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte(
		"WORKFLOW_API_URL=http://workflow-api:8001\n"+
			"AIFLOW_MODEL_NAME=wfx\n"+
			"AIFLOW_PROVIDER=openai\n"+
			"AIFLOW_BASE_URL=http://example.invalid/v1\n"+
			"AIFLOW_API_KEY=sk-test\n"+
			"AIFLOW_MAX_TOOL_ITERATIONS=7\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}

	// Isolate: run in a dir without .env; load via the config-path sibling
	// .env candidate. Unset (not Setenv "") so godotenv can populate them.
	for _, k := range []string{
		DefaultWorkflowAPIURLEnv, EnvModelName, EnvProvider,
		EnvBaseURL, EnvAPIKey, EnvMaxToolIter, EnvPort, EnvHost,
	} {
		os.Unsetenv(k)
		t.Setenv(k, "") // restore value on cleanup
		os.Unsetenv(k)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	cfg := config.DefaultConfig()
	first := cfg.ModelList[0].ModelName
	wf, err := LoadWorkflowConfig(cfg, filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("LoadWorkflowConfig: %v", err)
	}
	if wf.WorkflowAPIURL != "http://workflow-api:8001" {
		t.Fatalf("WORKFLOW_API_URL = %q", wf.WorkflowAPIURL)
	}
	if os.Getenv(DefaultWorkflowAPIURLEnv) != "http://workflow-api:8001" {
		t.Fatal("WORKFLOW_API_URL not propagated to process env")
	}
	if cfg.Agents.Defaults.GetModelName() != "wfx" {
		t.Fatalf("defaults model = %q", cfg.Agents.Defaults.GetModelName())
	}
	// Workflow-created entry must be first so provider selection hits it.
	if cfg.ModelList[0].ModelName != "wfx" {
		t.Fatalf("workflow model not at front: got %q (was %q)", cfg.ModelList[0].ModelName, first)
	}
	m, err := cfg.GetModelConfig("wfx")
	if err != nil || m.APIKey() != "sk-test" {
		t.Fatalf("GetModelConfig(wfx): %v key=%q", err, m.APIKey())
	}
	if cfg.Agents.Defaults.MaxToolIterations != 7 {
		t.Fatalf("max_tool_iterations = %d", cfg.Agents.Defaults.MaxToolIterations)
	}
}

// [workflow-fork] Telegram channel env injection tests.

func TestApplyTelegramChannel(t *testing.T) {
	t.Run("token env enables and patches settings", func(t *testing.T) {
		os.Unsetenv(EnvTelegramEnabled)
		os.Unsetenv(EnvTelegramAllowFrm)
		t.Setenv(EnvTelegramToken, "123:abc")
		cfg := config.DefaultConfig()
		wf := &WorkflowConfig{}
		if err := wf.applyTelegramChannel(cfg); err != nil {
			t.Fatalf("apply: %v", err)
		}
		bc := cfg.Channels[config.ChannelTelegram]
		if bc == nil || !bc.Enabled {
			t.Fatalf("telegram channel not enabled")
		}
		decoded, err := bc.GetDecoded()
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		tg, ok := decoded.(*config.TelegramSettings)
		if !ok || tg.Token.String() != "123:abc" {
			t.Fatalf("token not applied: %+v", decoded)
		}
	})

	t.Run("explicit enabled=false wins over token", func(t *testing.T) {
		os.Unsetenv(EnvTelegramAllowFrm)
		t.Setenv(EnvTelegramToken, "123:abc")
		t.Setenv(EnvTelegramEnabled, "false")
		cfg := config.DefaultConfig()
		wf := &WorkflowConfig{}
		if err := wf.applyTelegramChannel(cfg); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if bc := cfg.Channels[config.ChannelTelegram]; bc == nil || bc.Enabled {
			t.Fatalf("telegram channel should be disabled")
		}
	})

	t.Run("allow_from csv", func(t *testing.T) {
		os.Unsetenv(EnvTelegramToken)
		t.Setenv(EnvTelegramEnabled, "true")
		t.Setenv(EnvTelegramAllowFrm, "111, 222 ,")
		cfg := config.DefaultConfig()
		wf := &WorkflowConfig{}
		if err := wf.applyTelegramChannel(cfg); err != nil {
			t.Fatalf("apply: %v", err)
		}
		bc := cfg.Channels[config.ChannelTelegram]
		if len(bc.AllowFrom) != 2 || bc.AllowFrom[0] != "111" || bc.AllowFrom[1] != "222" {
			t.Fatalf("allow_from = %v", bc.AllowFrom)
		}
	})

	t.Run("invalid enabled value errors", func(t *testing.T) {
		os.Unsetenv(EnvTelegramToken)
		os.Unsetenv(EnvTelegramAllowFrm)
		t.Setenv(EnvTelegramEnabled, "maybe")
		cfg := config.DefaultConfig()
		wf := &WorkflowConfig{}
		if err := wf.applyTelegramChannel(cfg); err == nil {
			t.Fatalf("expected error for invalid enabled value")
		}
	})
}
