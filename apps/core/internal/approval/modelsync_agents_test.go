package approval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wetee/ai-workflow/internal/db"
)

// TestSyncPiWebModelsPrunesStaleManagedEntries 锁定 enabledModels 清理口径：
// model-managed 命名空间由同步器独占——档案改名/删除后旧条目必须清掉，否则 pi-web
// 每次解析 enabledModels 都会报全局警告 "No models match pattern ..."（2026-11-27）。
// 其它 provider / 通配条目是用户维护的，必须原样保留。
func TestSyncPiWebModelsPrunesStaleManagedEntries(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".pi", "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(dir, "settings.json")
	initial := map[string]any{
		"theme": "dark",
		"enabledModels": []any{
			"*",
			"model-managed/Qwen3.8-27B",      // 旧模型 ID（无匹配）
			"model-managed/Qwen3.8-27B:high", // 旧模型 ID + thinking 后缀（同样要清）
			"model-managed/Qwen3.8",          // 当前模型：保留
			"anthropic/claude-sonnet-5",      // 用户维护：保留
		},
	}
	b, _ := json.Marshal(initial)
	if err := os.WriteFile(settingsPath, b, 0o600); err != nil {
		t.Fatal(err)
	}

	msg, err := syncPiWebModels([]db.ModelProfile{{Name: "Coder", Model: "Qwen3.8"}}, home, "", "test-key")
	if err != nil {
		t.Fatalf("syncPiWebModels: %v", err)
	}
	if !strings.Contains(msg, "清理 2 项") {
		t.Errorf("同步结果未报告清理数：%s", msg)
	}

	updated, err := readJSONFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if updated["theme"] != "dark" {
		t.Errorf("无关设置被改动: %v", updated["theme"])
	}
	raw, _ := updated["enabledModels"].([]any)
	got := make([]string, 0, len(raw))
	for _, r := range raw {
		if s, ok := r.(string); ok {
			got = append(got, s)
		}
	}
	want := []string{"*", "model-managed/Qwen3.8", "anthropic/claude-sonnet-5"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("enabledModels = %v, want %v", got, want)
	}

	// models.json 只保留当前档案（回归保障）
	models, err := readJSONFile(filepath.Join(dir, "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	providers, _ := models["providers"].(map[string]any)
	entry, _ := providers[managedProviderName].(map[string]any)
	rawModels, _ := entry["models"].([]any)
	if len(rawModels) != 1 {
		t.Fatalf("managed provider models = %d, want 1", len(rawModels))
	}
	m, _ := rawModels[0].(map[string]any)
	if m["id"] != "Qwen3.8" {
		t.Errorf("managed model id = %v, want Qwen3.8", m["id"])
	}
}
