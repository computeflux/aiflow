// S8.1 模型目录同步到 agent 框架：model_profiles 里 sync_agents=1 的档案在
// apply（切 active）时推送到三个 agent 运行时，让 coder/reviewer/analyst 等
// agent 能在框架内看到并选用统一维护的模型列表：
//
//   - ai-flow（PicoClaw gateway，:8134）：PUT /models 合并进 model_list（同名继承
//     既有 api_keys，active 命中时热切 defaults）——复用 pushAIBot；
//   - pi-web（coder/analyst 宿主运行时，:30141）：pi-web 以 $HOME/.pi/agent/
//     models.json 为自定义 provider 目录（进化链 adapter 传 provider/modelId 必须
//     命中该目录）。driver 与 pi-web 同宿主（CODER_HOST_HOME=/host 挂载宿主 HOME），
//     故直接原子写该文件：合并 providers["model-managed"]，并按 consumers 标记
//     enabledModels（pi-web 模型面板可见集，pi 启动时读取）；
//   - ocr（reviewer 宿主 CLI）：~/.opencodereview/config.json 的 custom_providers
//     同理，provider "model-managed" 下挂全部档案 models。
//
// 红线：写入的是宿主上对应进程本就持有的文件（HOME 挂载），密钥仅落在这些
// 进程自己的配置里；board 侧任何 API 响应绝不携带明文 key。
package approval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/wetee/ai-workflow/internal/db"
)

// SyncAgentsCatalog 把 agent 模型目录同步到各 agent 框架：
//   - pi-web：直写宿主 ~/.pi/agent/{models,settings}.json（需可写挂载）；写失败
//     （api 容器只读挂载）且给了 webURL/webPass → 降级走 pi-web PUT /api/models-config
//     HTTP 推送（仅 models.json，即时生效；enabledModels scope 由 evolve-driver 的
//     周期同步补齐）；
//   - ocr：宿主文件直写（无 HTTP 接口）。
func SyncAgentsCatalog(profiles []db.ModelProfile, hostHome, activeName, fallbackKey string) map[string]string {
	res := map[string]string{}
	msg, err := syncPiWebModels(profiles, hostHome, activeName, fallbackKey)
	if err != nil && piWebPushURL() != "" {
		if msg2, err2 := pushPiWebModelsConfig(profiles, fallbackKey); err2 != nil {
			res["pi-web"] = "失败：" + err.Error() + "；HTTP 推送也失败：" + err2.Error()
		} else {
			res["pi-web"] = "文件写入不可用（" + err.Error() + "），已改走 HTTP 推送：" + msg2
		}
	} else if err != nil {
		res["pi-web"] = "失败：" + err.Error()
	} else {
		res["pi-web"] = msg
	}
	if msg, err := syncOCRModels(profiles, hostHome, activeName, fallbackKey); err != nil {
		res["ocr"] = "失败：" + err.Error()
	} else {
		res["ocr"] = msg
	}
	return res
}

// piWebPushURL 取 pi-web HTTP 推送地址（PI_WEB_URL，未配 = 不降级）。
func piWebPushURL() string { return strings.TrimRight(strings.TrimSpace(os.Getenv("PI_WEB_URL")), "/") }

// pushPiModelsConfig 经 pi-web PUT /api/models-config 写 models.json（服务端
// writeModelsConfig 会立即 invalidate 其内存缓存，新档案在模型面板即时可见）。
// pi-web 带 Basic Auth（用户名 pi，密码 PI_WEB_PASSWORD）。注意：PUT 是整文件
// 替换语义，故先 GET 合并 providers["model-managed"] 再 PUT。
func pushPiWebModelsConfig(profiles []db.ModelProfile, fallbackKey string) (string, error) {
	base := piWebPushURL()
	if base == "" {
		return "", fmt.Errorf("PI_WEB_URL 未配置")
	}
	req, err := http.NewRequest(http.MethodGet, base+"/api/models-config", nil)
	if err != nil {
		return "", err
	}
	applyPiAuth(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET /api/models-config: %w", err)
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	cfg := map[string]any{}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &cfg) // 空/损坏 → 以空 providers 重建
	}
	providers, _ := cfg["providers"].(map[string]any)
	if providers == nil {
		providers = map[string]any{}
	}
	entry := providerEntry{API: "openai-completions"}
	var names []string
	for _, p := range profiles {
		key := p.APIKey
		if key == "" {
			key = fallbackKey
		}
		entry.Models = append(entry.Models, profileModel(p, key))
		names = append(names, p.Model)
		if entry.BaseURL == "" && p.BaseURL != "" {
			entry.BaseURL = p.BaseURL
			entry.APIKey = key
		}
	}
	providers[managedProviderName] = entry
	cfg["providers"] = providers
	b, _ := json.Marshal(cfg)
	req, err = http.NewRequest(http.MethodPut, base+"/api/models-config", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	applyPiAuth(req)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("PUT /api/models-config: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("PUT /api/models-config: %d %s", resp.StatusCode, truncate(string(body), 120))
	}
	return fmt.Sprintf("HTTP 推送 models.json 成功（%s；enabledModels 由 evolve-driver 周期补齐）", strings.Join(names, ",")), nil
}

func applyPiAuth(req *http.Request) {
	pass := os.Getenv("PI_WEB_PASSWORD")
	if pass == "" {
		pass = os.Getenv("CODER_WEB_PASSWORD")
	}
	if pass != "" {
		req.SetBasicAuth("pi", pass)
	}
}

// providerEntry / ocrProviderEntry 是两个框架的 provider 结构（宽松解析，未知字段保留）。
type providerEntry struct {
	API     string `json:"api"`
	BaseURL string `json:"baseUrl"`
	APIKey  string `json:"apiKey"`
	Models  []any  `json:"models"`
}

type ocrProviderEntry struct {
	APIKey   string `json:"api_key"`
	URL      string `json:"url"`
	Protocol string `json:"protocol"`
	Model    string `json:"model"`
	Models   []any  `json:"models"`
}

const managedProviderName = "model-managed"

func profileModel(p db.ModelProfile, apiKey string) map[string]any {
	return map[string]any{
		"id":            p.Model,
		"apiKey":        apiKey,
		"name":          p.Name,
		"reasoning":     true,
		"contextWindow": 262144,
		"maxTokens":     32000,
		"compat":        map[string]any{"supportsDeveloperRole": false},
	}
}


// readJSONFile 读 JSON 对象文件；不存在/损坏返回空 map（损坏时保留原字节备份）。
func readJSONFile(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if len(strings.TrimSpace(string(data))) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return out, nil
}

// writeJSONFileAtomic 0600 原子写（key 敏感）。
func writeJSONFileAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// syncPiWebModels 合并写 $HOME/.pi/agent/models.json 的 providers["model-managed"]，
// 并把档案名写入 settings.json 的 enabledModels（pi 启动时读取，重启后生效；
// pi-web 的模型面板 API 每次实时读 models.json，新档案即时可见）。
func syncPiWebModels(ps []db.ModelProfile, hostHome, activeName, fallbackKey string) (string, error) {
	if strings.TrimSpace(hostHome) == "" {
		return "", fmt.Errorf("CODER_HOST_HOME 未配置")
	}
	if len(ps) == 0 {
		return "跳过（无 sync_agents 档案）", nil
	}
	modelsPath := filepath.Join(hostHome, ".pi", "agent", "models.json")
	cfg, err := readJSONFile(modelsPath)
	if err != nil {
		return "", err
	}
	providers, _ := cfg["providers"].(map[string]any)
	if providers == nil {
		providers = map[string]any{}
	}
	entry := providerEntry{API: "openai-completions"}
	for _, p := range ps {
		key := p.APIKey
		if key == "" {
			key = fallbackKey // 档案未填 key → CODER_API_KEY 兜底（同 .env 的本地端点 key）
		}
		entry.Models = append(entry.Models, profileModel(p, key))
		if entry.BaseURL == "" && p.BaseURL != "" { // 兼容旧版 pi（provider 级 baseUrl）
			entry.BaseURL = p.BaseURL
			entry.APIKey = key
		}
	}
	providers[managedProviderName] = entry
	cfg["providers"] = providers
	if err := writeJSONFileAtomic(modelsPath, cfg); err != nil {
		return "", fmt.Errorf("写 %s: %w", modelsPath, err)
	}

	// enabledModels：把「model-managed/<model>:*」并入 settings.json（保留既有项，去重）。
	// 若既有 scope 未包含全量通配（"*"），补入 "*"：新档案进 scope 的同时不把
	// 既有全量目录缩掉（pi 的 enabledModels 是白名单 scope，未列入 = 会话内不可选）。
	settingsPath := filepath.Join(hostHome, ".pi", "agent", "settings.json")
	settings, err := readJSONFile(settingsPath)
	if err != nil {
		return fmt.Sprintf("models.json 已更新（%d 个），settings 读取失败：%v", len(ps), err), nil
	}
	raw, _ := settings["enabledModels"].([]any)
	seen := map[string]bool{}
	hasAll := false
	merged := make([]any, 0, len(raw)+len(ps)+1)
	for _, r := range raw {
		if s, ok := r.(string); ok {
			key := strings.TrimSuffix(s, ":*")
			if key == "*" {
				hasAll = true
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, r)
			continue
		}
		merged = append(merged, r) // 未知结构原样保留
	}
	if !hasAll && len(merged) > 0 {
		merged = append([]any{"*"}, merged...)
	}
	added := 0
	for _, p := range ps {
		key := managedProviderName + "/" + p.Model
		if seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, key+":*")
		added++
	}
	if added > 0 || activeName != "" {
		settings["enabledModels"] = merged
		if err := writeJSONFileAtomic(settingsPath, settings); err != nil {
			return fmt.Sprintf("models.json 已更新（%d 个），settings 写入失败：%v", len(ps), err), nil
		}
	}
	return fmt.Sprintf("models.json 已更新（%d 个档案），settings.enabledModels 新增 %d 项（pi-web 重启后模型面板生效）", len(ps), added), nil
}

// syncOCRModels 合并写 ~/.opencodereview/config.json 的 custom_providers["model-managed"]。
// 每档案独立 endpoint → 拆成 per-profile 子 provider（model-managed/<档案名>，字段结构
// 与 ocrProviderEntry 一致）；父 provider "model-managed" 只聚合 models 列表，默认 model
// 指向 active 档案的子 provider（单 provider 单 url 无法容纳多 base_url 的档案集）。
func syncOCRModels(ps []db.ModelProfile, hostHome, activeName, fallbackKey string) (string, error) {
	if strings.TrimSpace(hostHome) == "" {
		return "", fmt.Errorf("CODER_HOST_HOME 未配置")
	}
	if len(ps) == 0 {
		return "跳过（无 sync_agents 档案）", nil
	}
	path := filepath.Join(hostHome, ".opencodereview", "config.json")
	cfg, err := readJSONFile(path)
	if err != nil {
		return "", err
	}
	cps, _ := cfg["custom_providers"].(map[string]any)
	if cps == nil {
		cps = map[string]any{}
	}
	// 清理旧版残留：早先实现把全部档案塞进单个父 provider 的 models（多 endpoint 时取不到 key）。
	for k := range cps {
		if k == managedProviderName || strings.HasPrefix(k, managedProviderName+"/") {
			delete(cps, k)
		}
	}
	entry := ocrProviderEntry{Protocol: "openai"}
	var names []string
	for _, p := range ps {
		entry.Models = append(entry.Models, managedProviderName+"/"+p.Name)
		names = append(names, p.Name)
		key := p.APIKey
		if key == "" {
			key = fallbackKey
		}
		cps[managedProviderName+"/"+p.Name] = ocrProviderEntry{
			APIKey: key, URL: p.BaseURL, Protocol: "openai", Model: p.Model,
		}
	}
	if activeName != "" {
		for _, p := range ps { // active 档案名命中 → 默认 model 指向其子 provider
			if p.Name == activeName {
				entry.Model = managedProviderName + "/" + p.Name
			}
		}
	}
	if entry.Model == "" && len(ps) > 0 {
		entry.Model = managedProviderName + "/" + ps[0].Name
	}
	cps[managedProviderName] = entry
	cfg["custom_providers"] = cps
	if err := writeJSONFileAtomic(path, cfg); err != nil {
		return "", err
	}
	return fmt.Sprintf("custom_providers[%s] 已更新（%s）", managedProviderName, strings.Join(names, ",")), nil
}
