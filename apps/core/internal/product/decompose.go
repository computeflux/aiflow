package product

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/llm"
	"github.com/wetee/ai-workflow/internal/workitem"
	"gorm.io/gorm"
)

// Task 是拆解出的单条任务（S7 §3.3 输出 JSON Schema 校验，漂移即失败）。
type Task struct {
	Title             string   `json:"title"`
	Description       string   `json:"description"`
	Priority          string   `json:"priority"` // P0 | P1 | P2 | P3
	DueAt             string   `json:"due_at"`
	Checklist         []string `json:"checklist"`
	SuggestedOwner    string   `json:"suggested_owner"`
	TargetName        string   `json:"target_name"` // 目标项目（可推导时填，可逗号分隔多选；只按项目识别，S9.8 §7.10）
	IsSelfImprovement bool     `json:"is_self_improvement"`
	EstimateHours     float64  `json:"estimate_hours"` // 预计工时（小时）；≤maxTaskHours，超过必须拆卡
}

// DecomposeResult 是拆解结果。
type DecomposeResult struct {
	Tasks []Task `json:"tasks"`
}

// taskSchema 是拆解输出的 JSON Schema（S7 §3.3：校验，漂移即失败）。
const taskSchema = `{
  "type": "object",
  "properties": {
    "tasks": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "title": { "type": "string" },
          "description": { "type": "string" },
          "priority": { "type": "string", "enum": ["P0", "P1", "P2", "P3"] },
          "due_at": { "type": "string" },
          "checklist": { "type": "array", "items": { "type": "string" } },
          "suggested_owner": { "type": "string" },
          "target_name": { "type": "string" },
          "is_self_improvement": { "type": "boolean" },
          "estimate_hours": { "type": "number" }
        },
        "required": ["title", "estimate_hours"]
      }
    }
  },
  "required": ["tasks"]
}
`

// maxTaskHours 是单张任务卡的工时上限（2026-10-27 小颗粒红线）：
// 一个需求超过 3 小时就必须拆成多张卡——否则工作流难以稳定完成、审核与合入。
const maxTaskHours = 3.0

// defaultDecomposeTimeout 是单次拆解 LLM 调用的默认上限。
//
// 2026-11-01 修正：原来是写死的 120s，本地大模型冷预填动辄几分钟 → 文档拆解必超时。
// 超时不再重试（重试只会把等待翻倍），所以上限要一次给够：
//   - 后端 DECOMPOSE_TIMEOUT_SEC（默认 600s）
//   - nginx proxy_read_timeout 必须更大（core-ui.conf，900s）
//   - 前端拆解请求超时必须更大（kit/api.ts 的 productDecompose，950s）
//
// 不等式：后端 < nginx < 前端。这样出错时总是最内层先返回一个可读的错误。
const defaultDecomposeTimeout = 600 * time.Second

// ErrDecomposeTimeout 标记「拆解被超时打断」，供 HTTP 层映射为 504（而不是 422 参数错）。
var ErrDecomposeTimeout = errors.New("decompose timeout")

// decomposeTimeout 读取 DECOMPOSE_TIMEOUT_SEC（秒）；非法或未设置时用默认值。
func decomposeTimeout() time.Duration {
	if v := strings.TrimSpace(os.Getenv("DECOMPOSE_TIMEOUT_SEC")); v != "" {
		if sec, err := strconv.Atoi(v); err == nil && sec > 0 {
			return time.Duration(sec) * time.Second
		}
	}
	return defaultDecomposeTimeout
}

// decomposeSystemPrompt 拆解提示词（含 3 小时小颗粒红线）。
const decomposeSystemPrompt = "你是任务拆解助手。把下面的对话记录/会议纪要拆解为可执行的任务卡，用中文。" +
	"严格只输出一个 JSON 对象（不要 markdown 代码块、不要解释），结构：{\"tasks\":[{...}]}，" +
	"每个任务字段：title, description, priority(P0|P1|P2|P3), due_at(ISO 日期，可空), " +
	"checklist(字符串数组), suggested_owner(可空), target_name(目标项目名，可空，可逗号分隔多选；只填项目名如 pycraper/core，不要填技能), " +
	"is_self_improvement(布尔，是否对产品/系统自身的修改), estimate_hours(数字，预计工时/小时，必填)。\n\n" +
	"小颗粒红线（必须遵守）：\n" +
	"1) 每张任务卡必须由一名工程师在 3 小时内完成并自测（estimate_hours ≤ 3，建议 1–3）。\n" +
	"2) 超过 3 小时的需求必须拆成多张卡；禁止把多天/多阶段的需求塞进一张卡。\n" +
	"3) 按「可独立验收的交付物」拆（一个行为/接口/界面改动 + 自己的验收标准），不要按文件或技术层机械拆。\n" +
	"4) 每张卡要能独立评审、独立合入、独立部署；也不要拆成无意义的碎片。\n" +
	"5) 典型需求 2–5 张卡；只有确实很小的需求才输出 1 张。"

// Decompose 把对话记录 / 会议纪要文本拆解为任务数组（S7 §3.3）：
// LLM 输出 JSON → JSON Schema 校验（漂移即失败）→ 3 小时小颗粒校验（不合格带反馈重拆一次）。
func Decompose(ctx context.Context, llmClient *llm.Client, text string) (*DecomposeResult, error) {
	if llmClient == nil {
		return nil, fmt.Errorf("llm unavailable")
	}
	var lastErr error
	// 最多两次：首版 + 带反馈重拆（只靠 prompt 不可靠，必须用校验结果回灌约束）
	for attempt := 0; attempt < 2; attempt++ {
		prompt := text
		if attempt > 0 {
			prompt = text + "\n\n（上一版不合格：" + lastErr.Error() +
				"。请重新拆解：每张卡必须是 3 小时内可独立完成并验收的小颗粒，超过就拆成多张卡；estimate_hours 必填且 ≤3。）"
		}
		timeout := decomposeTimeout()
		ctxL, cancel := context.WithTimeout(ctx, timeout)
		raw, err := llmClient.ChatCompletion(ctxL, decomposeSystemPrompt, prompt)
		// ⚠ ctxL.Err() 必须在 cancel() 之前取：cancel 会让 Err() 变成 Canceled，盖掉真实的 DeadlineExceeded。
		ctxErr := ctxL.Err()
		cancel()
		if err != nil {
			if errors.Is(ctxErr, context.DeadlineExceeded) {
				return nil, fmt.Errorf("%w：超过 %s 仍未返回。文档过长或模型过慢，"+
					"可调大 DECOMPOSE_TIMEOUT_SEC，或先把文档拆短再逐段拆解", ErrDecomposeTimeout, timeout)
			}
			if ctxErr != nil {
				return nil, fmt.Errorf("拆解已取消: %w", ctxErr)
			}
			return nil, fmt.Errorf("decompose llm: %w", err)
		}
		result, err := parseDecompose(raw)
		if err != nil {
			lastErr = err
			continue
		}
		if err := validateGranularity(result); err != nil {
			lastErr = err
			continue
		}
		return result, nil
	}
	return nil, fmt.Errorf("decompose 未通过校验（已重试 1 次）: %w", lastErr)
}

// parseDecompose 解析 + JSON Schema 校验（漂移即失败）。
func parseDecompose(raw string) (*DecomposeResult, error) {
	obj := extractJSONObj(raw)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(obj), &parsed); err != nil {
		return nil, fmt.Errorf("decompose output not valid JSON（漂移）: %w", err)
	}
	if verr := taskSchemaCompiled.Validate(parsed); verr != nil {
		return nil, fmt.Errorf("decompose output violates schema（漂移）: %w", verr)
	}
	var result DecomposeResult
	if err := json.Unmarshal([]byte(obj), &result); err != nil {
		return nil, fmt.Errorf("unmarshal tasks: %w", err)
	}
	return &result, nil
}

// validateGranularity 校验「每张卡 ≤3 小时」硬红线：缺 estimate_hours 或超限都打回重拆。
func validateGranularity(result *DecomposeResult) error {
	if result == nil || len(result.Tasks) == 0 {
		return fmt.Errorf("没有拆出任何任务")
	}
	var bad []string
	for _, t := range result.Tasks {
		if t.EstimateHours <= 0 {
			bad = append(bad, fmt.Sprintf("「%s」缺少 estimate_hours", t.Title))
			continue
		}
		if t.EstimateHours > maxTaskHours {
			bad = append(bad, fmt.Sprintf("「%s」预计 %s 小时（>3）", t.Title, formatHours(t.EstimateHours)))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("%s", strings.Join(bad, "；"))
	}
	return nil
}

// formatHours 格式化工时（1.5 → "1.5"，2 → "2"）。
func formatHours(h float64) string {
	return strconv.FormatFloat(h, 'f', -1, 64)
}

// CreateCards 把拆解结果建为看板卡（走现有 board 卡逻辑），并按分流规则置状态（S7 §3.3）：
// 所有目标（含 core 系）统一 → card.Status=ready_for_evolve（可被 S1 领取）。
// S9.8 §7.10 起 core 系不再旁路（人工在审批/合并 PR 处把关）。
//
// S9 增强（§7.2 需求入队）：建卡的同时建**需求工作项**（kind=requirement, source=product），
// 双向关联卡↔工作项。
//
// ⚠️ 2026-10-29 修正：工作项**不再自动入队**（旧行为直接 queued → 卡被立刻镜像到 Todo）。
// 新需求应先停留在 **Backlog（候选）**，由人拖到 Todo/In Progress 才 hand-off 入队。
// 工作项建为 triaged（已定义、无需 AI 分诊；CardStatusFromItem→todo 候选，ColumnFromItem→Backlog），
// 拖到 Todo 时 HandOffCard 走 triaged→queued，行为与手建卡一致。
//
// wiStore 为 nil 时回退旧行为（直接建 evolve_task source=card）。
//
// 负责人取 suggested_owner，缺省为对话发起者。返回建卡结果。
func CreateCards(ctx context.Context, database *gorm.DB, store *Store, boardID, listID uint, initiator string, result *DecomposeResult, wiStore *workitem.Store) ([]CardResult, error) {
	out := make([]CardResult, 0, len(result.Tasks))
	for i, t := range result.Tasks {
		assignee := t.SuggestedOwner
		if assignee == "" {
			assignee = initiator
		}
		status := routingStatus(t.TargetName)
		// 工时标注（≤3h 红线见 Decompose 校验）：写进描述，方便审核者判断颗粒度。
		desc := t.Description
		if t.EstimateHours > 0 && !strings.Contains(desc, "预计工时") {
			note := "预计工时：" + formatHours(t.EstimateHours) + " 小时"
			if strings.TrimSpace(desc) == "" {
				desc = note
			} else {
				desc = strings.TrimSpace(desc) + "\n\n" + note
			}
		}
		card := db.BoardCard{
			ListID:      listID,
			Title:       t.Title,
			Description: desc,
			Priority:    mapPriority(t.Priority),
			Assignee:    assignee,
			Checklist:   marshalChecklist(t.Checklist),
			Source:      "product",
			Status:      status,
			Labels:      "product",    // 纯来源展示（S9.8 §7.10：不再塞 type/name）
			TargetName:  t.TargetName, // 目标项目名（专门字段，只按项目识别；hand-off 时 cardTarget 推导）
		}
		if t.DueAt != "" {
			if dt, err := time.Parse(time.RFC3339, t.DueAt); err == nil {
				card.DueDate = &dt
			}
		}
		if err := database.WithContext(ctx).Create(&card).Error; err != nil {
			return out, fmt.Errorf("create card %d: %w", i, err)
		}
		res := CardResult{CardID: card.ID, Title: card.Title, Status: status}

		if wiStore != nil {
			// S9：建需求工作项（进统一队列；无目标/不可运行由调度器旁路 rejected 回 Backlog）
			if wiNo, err := createWorkItemForCard(ctx, database, wiStore, card, t, assignee); err != nil {
				// 建工作项失败不阻断建卡（card 仍为 ready_for_evolve）
				res.WorkItemNo = ""
			} else {
				res.WorkItemNo = wiNo
			}
		} else {
			// 旧行为（过渡期）：有 target_name → 直接建 evolve_task（source=card）
			if t.TargetName != "" {
				taskNo, err := createEvolveTaskForCard(ctx, database, card.ID, t)
				if err == nil {
					res.EvolveTaskNo = taskNo
				}
			}
		}
		out = append(out, res)
	}
	return out, nil
}

// createWorkItemForCard 为拆解出的任务卡建需求工作项（S9 §7.2）：
// kind=requirement, source=product, source_ref=card-<id>；双向关联（card.work_item_no / work_item.card_id）。
// 状态固定为 triaged（候选）：**不自动入队**——新需求先落在 Backlog，由人 hand-off 后再 queued。
func createWorkItemForCard(ctx context.Context, database *gorm.DB, wiStore *workitem.Store, card db.BoardCard, t Task, assignee string) (string, error) {
	item := &db.WorkItem{
		Kind:        workitem.KindRequirement,
		Source:      workitem.SourceProduct,
		SourceRef:   fmt.Sprintf("card-%d", card.ID),
		TargetName:  t.TargetName,
		Title:       t.Title,
		Description: t.Description,
		Checklist:   card.Checklist,
		Assignee:    assignee,
		Severity:    priorityToSeverity(t.Priority),
		Impact:      priorityToSeverity(t.Priority),
		Frequency:   0,
		Status:      workitem.StatusTriaged, // 需求已定义（无需 AI 分诊）→ 可直接入队（open→queued 非法）
		CardID:      &card.ID,
	}
	if card.DueDate != nil {
		item.DueAt = card.DueDate
		item.Urgency = workitem.UrgencyFromDueAt(card.DueDate, time.Now())
	} else {
		item.Urgency = 1
	}
	if err := wiStore.Create(ctx, item); err != nil {
		return "", err
	}
	// 双向关联：回填卡的工作项号
	_ = database.WithContext(ctx).Model(&db.BoardCard{}).Where("id = ?", card.ID).
		Update("work_item_no", item.No)
	// 算分（hand-off 入队时按分数排序用）；**不自动入队**：新需求停在 Backlog（候选），
	// 由人拖到 Todo / In Progress 触发 HandOffCard 的 triaged→queued。
	// 旧行为在这里直接 UpdateStatus(queued)，导致卡片被镜像轮立刻从 Backlog 移到 Todo。
	w := workitem.DefaultWeights()
	_ = wiStore.UpdateScores(ctx, item.No, item.Severity, item.Urgency, item.Impact, item.Frequency, w)
	return item.No, nil
}
