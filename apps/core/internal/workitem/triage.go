package workitem

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/llm"
	"github.com/wetee/ai-workflow/internal/project"
)

// TriageConfig 是一轮错误日志分诊的参数（§7.4）。
type TriageConfig struct {
	Window   time.Duration // 扫描窗口（默认 24h）
	MinCount int           // 低频非致命簇不建工作项（默认 1，避免噪音）
	Weights  ScoreWeights  // 打分权重
}

// DefaultTriageConfig 返回默认分诊配置。
func DefaultTriageConfig() TriageConfig {
	return TriageConfig{Window: 24 * time.Hour, MinCount: 1, Weights: DefaultWeights()}
}

// TriageResult 是一轮分诊的统计（Created = 新建 Bug 卡数）。
type TriageResult struct {
	Scanned   int
	Clusters  int
	Created   int
	Merged    int
	Dismissed int
	Errors    []string
}

// TriageDecision 是 AI 分诊对单个错误簇的判定（§7.4，JSON Schema 校验，漂移即失败）。
type TriageDecision struct {
	IsDefect   bool   `json:"is_defect"`
	Severity   string `json:"severity"`    // P0 | P1 | P2 | P3
	TargetName string `json:"target_name"` // 目标项目名（可空；只按项目识别，S9.8 §7.10）
	RootCause  string `json:"root_cause_hypothesis"`
	Summary    string `json:"summary"`
}

// errorCluster 是一个错误簇（system + source + 归一化 message）。
// IDs 是本轮扫描到的该簇 error_logs 行 ID（用于精确回填 work_item_no / 标记 resolved）。
type errorCluster struct {
	key     string
	System  string
	Source  string
	NormMsg string
	Sample  db.ErrorLog
	Count   int
	IDs     []uint
}

// TriageErrorLogs 执行一轮错误日志分诊（§7.4 重设：Bug 队列 = 工作看板里的 Bug 看板）：
// 扫描未归并的 error_logs → 按 (system, source, 归一化 message) 聚簇 →
// 去重（同簇已有未完结 Bug 卡 → 累加 frequency，不新建）→
// AI 分诊（LLM 不可用 → 规则降级分诊）→
// 建 Bug 看板卡（Backlog 列，status=todo，等人工拖到 In Progress = 交给工作流；
// hand-off 时复用看板机制建 kind=bug 工作项入统一队列）。
// 非缺陷簇标记 resolved（人工可复核），不建卡。
func TriageErrorLogs(ctx context.Context, store *Store, llmClient *llm.Client, cfg TriageConfig) (*TriageResult, error) {
	res := &TriageResult{}
	now := time.Now()
	start := now.Add(-cfg.Window)

	// Bug 看板幂等确保存在（分诊产卡的落点）
	boardID, backlogID, err := ensureBugBoard(ctx, store)
	if err != nil {
		return nil, fmt.Errorf("ensure bug board: %w", err)
	}

	var rows []db.ErrorLog
	// work_item_no 列可空且默认 NULL（各服务写入时未赋值）→ 必须同时判 NULL 与 ''，
	// 否则 MySQL 的 `NULL = ''` 为假，扫描恒为 0（修复前 triage 永远空转）。
	if err := store.database.WithContext(ctx).
		Where("resolved = ? AND (work_item_no IS NULL OR work_item_no = '') AND created_at >= ?", false, start).
		Order("created_at DESC").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query error_logs: %w", err)
	}
	res.Scanned = len(rows)

	clusters := map[string]*errorCluster{}
	for _, r := range rows {
		key := clusterKey(r.System, r.Source, r.Message)
		c := clusters[key]
		if c == nil {
			c = &errorCluster{key: key, System: r.System, Source: r.Source,
				NormMsg: normalizeMessage(r.Message), Sample: r}
			clusters[key] = c
		}
		c.Count++
		c.IDs = append(c.IDs, r.ID)
	}
	res.Clusters = len(clusters)

	for _, c := range clusters {
		// 低频非致命簇不建卡（避免噪音）
		if c.Count < cfg.MinCount && c.Sample.Level != "fatal" && c.Sample.Level != "error" {
			continue
		}
		// 去重归并：同簇已有未完结 Bug 卡 → 累加 frequency，不新建
		if merged, err := mergeBugCard(ctx, store, boardID, c); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("merge %s: %v", c.key, err))
			continue
		} else if merged {
			res.Merged++
			continue
		}
		// AI 分诊（降级：LLM 不可用 → 规则分诊；降级标注只在工作项层有意义，卡层不记录）
		decision, _, err := triageDecision(ctx, llmClient, c)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("triage %s: %v", c.key, err))
			continue
		}
		// 非缺陷（预期行为 / 第三方故障 / 噪音）→ 标记该簇 resolved，不建卡
		if !decision.IsDefect {
			dismissCluster(ctx, store, c)
			res.Dismissed++
			continue
		}
		// 建 Bug 看板卡（Backlog 列尾，status=todo；交给工作流由看板 hand-off 机制驱动）
		card := buildBugCard(backlogID, c, decision)
		card.Position = nextCardPosition(ctx, store, backlogID)
		if err := store.database.WithContext(ctx).Create(card).Error; err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("create card %s: %v", c.key, err))
			continue
		}
		linkErrorLogsToCard(ctx, store, c, card.ID)
		res.Created++
	}
	return res, nil
}

// triageDecision 调 LLM 分诊（JSON Schema 校验，漂移即失败）；不可用/失败 → 规则降级分诊。
func triageDecision(ctx context.Context, llmClient *llm.Client, c *errorCluster) (*TriageDecision, bool, error) {
	if llmClient == nil {
		return ruleTriage(c), true, nil
	}
	ctxL, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	input := map[string]any{
		"system":  c.System,
		"source":  c.Source,
		"message": c.Sample.Message,
		"stack":   truncate(c.Sample.Stack, 800),
		"context": truncate(c.Sample.Context, 800),
		"count":   c.Count,
		"level":   c.Sample.Level,
	}
	b, _ := json.Marshal(input)
	raw, err := llmClient.ChatCompletion(ctxL,
		"你是资深 SRE 缺陷分诊员。判断下面的错误日志簇是否是需要修复的缺陷（vs 预期行为/第三方故障/噪音），"+
			"并给出严重度、归属、根因假设。严格只输出一个 JSON 对象（不要 markdown、不要解释），结构："+
			`{"is_defect":true,"severity":"P0|P1|P2|P3","target_name":"...","root_cause_hypothesis":"...","summary":"..."}。`+
			"severity：P0=崩溃/数据丢失/资金/安全，P1=主链路功能不可用，P2=功能降级/边缘场景，P3=体验/文案。"+
			"target_name：必须是 system 字段对应的项目名（如 pycraper、core、mailer），不要写文件路径或函数名。",
		string(b))
	if err != nil {
		// 降级（不阻断）：规则分诊 + 标注
		return ruleTriage(c), true, nil
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(extractJSONObj(raw)), &parsed); err != nil {
		return ruleTriage(c), true, nil
	}
	if verr := triageSchemaCompiled.Validate(parsed); verr != nil {
		// 漂移：降级
		return ruleTriage(c), true, nil
	}
	var d TriageDecision
	if err := json.Unmarshal([]byte(extractJSONObj(raw)), &d); err != nil {
		return ruleTriage(c), true, nil
	}
	d.TargetName = normalizeTargetName(d.TargetName, c.System)
	return &d, false, nil
}

// normalizeTargetName 归一化 LLM 返回的 target_name：多值（逗号分隔）逐项取项目名
// （去文件路径/函数名后缀）+ 部署单元别名映射（api / go-worker → core）+ 去空/去重/保序。
// 空值回退 system 字段（可部署单元名，同样过别名映射与归一）。
// 如 "pycraper/adapters/x.py" → "pycraper"，"api,core,core-ui" → "core,core-ui"。
func normalizeTargetName(target, system string) string {
	if strings.TrimSpace(target) == "" {
		target = system
	}
	return project.NormalizeTargets(target)
}

// ruleTriage 降级规则分诊（§7.4 红线：LLM 不可用 → 规则粗分 + 标注，不假装智能）。
// target_name 取 system（可部署单元，projects 表按它解析沙箱）而非 source
// （source 是日志模块名如 fetch:x，未注册 → 调度器会误判“目标不可运行”旁路 rejected）；
// 同时过部署单元别名映射（api / go-worker → core，2026-10-18）。
func ruleTriage(c *errorCluster) *TriageDecision {
	severity := severityFromLevelToBand(SeverityFromLevel(c.Sample.Level))
	return &TriageDecision{
		IsDefect:   true,
		Severity:   severity,
		TargetName: project.NormalizeTargets(c.System),
		RootCause:  "（降级分诊：LLM 不可用，按级别粗判）",
		Summary:    truncate(c.NormMsg, 80),
	}
}

// dismissCluster 标记该簇的错误日志为 resolved（非缺陷，人工可复核）。
func dismissCluster(ctx context.Context, store *Store, c *errorCluster) {
	updateClusterRows(ctx, store, c, map[string]any{"resolved": true})
}

// updateClusterRows 按本簇行 ID 分批更新（仅更新仍未归并/未解决的行，幂等防并发轮次重复处理）。
// 旧实现用 `message LIKE '%归一化消息%'` 匹配：多行消息被折叠成单行后 LIKE 会漏匹配，
// 且 NULL work_item_no 会被 `= ”` 过滤掉——改用本轮扫描到的精确行 ID。
func updateClusterRows(ctx context.Context, store *Store, c *errorCluster, updates map[string]any) {
	const batch = 500
	for i := 0; i < len(c.IDs); i += batch {
		end := i + batch
		if end > len(c.IDs) {
			end = len(c.IDs)
		}
		store.database.WithContext(ctx).Model(&db.ErrorLog{}).
			Where("id IN ? AND resolved = ? AND (work_item_no IS NULL OR work_item_no = '')", c.IDs[i:end], false).
			Updates(updates)
	}
}

// clusterKey 是错误簇的归并键。
func clusterKey(system, source, message string) string {
	return system + "|" + source + "|" + normalizeMessage(message)
}

var (
	wsRe  = regexp.MustCompile(`\s+`)
	digRe = regexp.MustCompile(`\d+`)
	hexRe = regexp.MustCompile(`0x[0-9a-f]+`)
)

// normalizeMessage 归一化错误信息用于聚簇：小写 + 折叠空白 + 数字→N + 十六进制→H + 截断。
// 数字归一化让同一 Bug 因超时值/ID/次数不同产生的变体归入同一簇（如 60000ms / 120000ms → Nms）。
func normalizeMessage(msg string) string {
	s := strings.ToLower(strings.TrimSpace(msg))
	s = wsRe.ReplaceAllString(s, " ")
	s = hexRe.ReplaceAllString(s, "H")
	s = digRe.ReplaceAllString(s, "N")
	return truncate(s, 200)
}

// truncate 按 rune 截断（UTF-8 安全）：按字节截断会把多字节汉字切成半个，
// 产生非法 UTF-8，MySQL 报 1366（Incorrect string value）导致 Bug 工作项建不出来。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ---- 分诊输出 JSON Schema（漂移即失败 → 降级）----

const triageSchema = `{
  "type": "object",
  "properties": {
    "is_defect": { "type": "boolean" },
    "severity": { "type": "string", "enum": ["P0", "P1", "P2", "P3"] },
    "target_name": { "type": "string" },
    "root_cause_hypothesis": { "type": "string" },
    "summary": { "type": "string" }
  },
  "required": ["is_defect", "severity"]
}
`

var triageSchemaCompiled = compileTriageSchema()

func compileTriageSchema() *jsonschema.Schema {
	c := jsonschema.NewCompiler()
	c.UseLoader(dataURLLoader{})
	s, err := c.Compile("data:application/json," + url.QueryEscape(triageSchema))
	if err != nil {
		panic(fmt.Sprintf("compile triage schema: %v", err))
	}
	return s
}

// dataURLLoader 支持 data:application/json 内嵌 schema（与 internal/product/decompose.go 同法）。
type dataURLLoader struct{}

func (dataURLLoader) Load(loc string) (any, error) {
	if strings.HasPrefix(loc, "data:application/json,") {
		payload, err := url.QueryUnescape(loc[len("data:application/json,"):])
		if err != nil {
			return nil, err
		}
		var v any
		if err := json.Unmarshal([]byte(payload), &v); err != nil {
			return nil, err
		}
		return v, nil
	}
	return jsonschema.FileLoader{}.Load(loc)
}

// extractJSONObj 从 LLM 输出抽取第一个 JSON 对象（容忍代码块围栏/前后杂文）。
func extractJSONObj(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end < 0 || end < start {
		return s
	}
	return s[start : end+1]
}

// severityFromBand 把 P0-P3 档位映射到 0-4 严重度。
func severityFromBand(p string) int {
	switch p {
	case "P0":
		return 4
	case "P1":
		return 3
	case "P3":
		return 1
	default:
		return 2
	}
}

// severityFromLevelToBand 把 0-4 严重度映射回 P0-P3 档位（降级分诊用）。
func severityFromLevelToBand(sev int) string {
	switch sev {
	case 4:
		return "P0"
	case 3:
		return "P1"
	case 1:
		return "P3"
	default:
		return "P2"
	}
}
