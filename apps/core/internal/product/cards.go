package product

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/wetee/ai-workflow/internal/db"
	"gorm.io/gorm"
)

// cards.go：拆解结果 → 看板卡/工作项/进化单（建卡分流，S7 产品情报 → S9 工作项）。
// 自 decompose.go 拆出（批次 F，纯机械搬迁，行为无变更）。

// priorityToSeverity 把拆解优先级 P0-P3 映射到 0-4 严重度。
func priorityToSeverity(p string) int {
	switch strings.ToUpper(strings.TrimSpace(p)) {
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

// CardResult 是单条建卡结果。
type CardResult struct {
	CardID       uint   `json:"card_id"`
	Title        string `json:"title"`
	Status       string `json:"status"` // ready_for_evolve
	EvolveTaskNo string `json:"evolve_task_no,omitempty"`
	WorkItemNo   string `json:"work_item_no,omitempty"` // S9 关联统一工作项
}

// routingStatus 分流规则（S7 §3.3）：所有目标（含 core 系）统一 → ready_for_evolve（可被 S1 领取）。
// S9.8 §7.10 起 core 系不再旁路（人工在审批/合并 PR 处把关）。
// routingStatus 拆解建卡时的卡片状态。
// 2026-10-29 起统一返回 "todo"（= 候选）：需求不再自动入队，先停在 Backlog；
// hand-off 后由卡片同步（CardStatusFromItem）改成 ready_for_evolve / in_progress / …。
func routingStatus(targetName string) string {
	return "todo"
}

// mapPriority 把拆解优先级 P0-P3 映射到看板卡优先级（urgent/high/medium/low）。
func mapPriority(p string) string {
	switch strings.ToUpper(strings.TrimSpace(p)) {
	case "P0":
		return "urgent"
	case "P1":
		return "high"
	case "P3":
		return "low"
	default: // P2 及未知
		return "medium"
	}
}

// marshalChecklist 把字符串数组序列化为看板卡 checklist JSON（[{text,done}]）。
func marshalChecklist(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	type item struct {
		Text string `json:"text"`
		Done bool   `json:"done"`
	}
	arr := make([]item, 0, len(items))
	for _, s := range items {
		arr = append(arr, item{Text: s, Done: false})
	}
	b, _ := json.Marshal(arr)
	return string(b)
}

// createEvolveTaskForCard 为 ready_for_evolve 卡建进化单（source=card，status=open），
// 供 S1 evolve-driver 领取。TaskNo 形如 card-<cardid>-<ts>。
func createEvolveTaskForCard(ctx context.Context, database *gorm.DB, cardID uint, t Task) (string, error) {
	taskNo := fmt.Sprintf("card-%d-%d", cardID, time.Now().Unix())
	evidence, _ := json.Marshal(map[string]any{
		"card_id":             cardID,
		"source":              "card",
		"description":         t.Description,
		"checklist":           t.Checklist,
		"is_self_improvement": t.IsSelfImprovement,
	})
	task := db.EvolveTask{
		TaskNo:     taskNo,
		Source:     "card",
		Evidence:   string(evidence),
		TargetName: t.TargetName,
		Status:     "open",
	}
	if err := database.WithContext(ctx).Create(&task).Error; err != nil {
		return "", err
	}
	return taskNo, nil
}

// taskSchemaCompiled 是拆解输出的已编译 JSON Schema（进程内编译一次）。
var taskSchemaCompiled = compileTaskSchema()

// compileTaskSchema 用 data: URL 内嵌 schema 编译（jsonschema v6 默认 loader 仅支持 file scheme，
// 需自定义加载器，与 cmd/evolve-driver 的 compileSchema 同法）。
func compileTaskSchema() *jsonschema.Schema {
	c := jsonschema.NewCompiler()
	c.UseLoader(dataURLLoader{})
	s, err := c.Compile("data:application/json," + url.QueryEscape(taskSchema))
	if err != nil {
		panic(fmt.Sprintf("compile task schema: %v", err))
	}
	return s
}

// dataURLLoader 支持 data:application/json, 内嵌 schema（其余回退 file loader）。
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
