package workitem

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/llm"
	"github.com/wetee/ai-workflow/internal/project"
)

// DispatchConfig 是一轮调度的参数（§7.5）。
type DispatchConfig struct {
	MaxInProgress     int           // 并发上限（默认 1 串行）
	StarvationEvery   int           // 每 N 轮做一次饥饿保护（默认 12 ≈ 1h）
	StarvationMinWait time.Duration // 饥饿保护最小等待（默认 24h）
	Weights           ScoreWeights  // 打分权重
	// LLM：启动前依赖检查（nil = 不检查）。
	// 自动领取同样要拦「前置任务未完成」的任务，否则看板手动检查会被调度器绕过。
	LLM *llm.Client
}

// DefaultDispatchConfig 返回默认调度配置。
func DefaultDispatchConfig() DispatchConfig {
	return DispatchConfig{
		MaxInProgress:     1,
		StarvationEvery:   12,
		StarvationMinWait: 24 * time.Hour,
		Weights:           DefaultWeights(),
	}
}

// DispatchResult 是一轮调度的统计。
type DispatchResult struct {
	Recomputed        int
	Dispatched        []string
	Starved           string
	DependencyBlocked []string // 因前置任务未完成而跳过的 queued 项（"wi-xxx: 原因"）
	Errors            []string
}

// Dispatch 执行一轮优先级调度（§7.5）：
//  1. 重算所有 queued 工作项的 score（动态升降级，§6.3）；
//  2. 旁路：无明确目标 / 目标不是已注册启用项目的 queued 项 → rejected（沙箱无法执行，不进自动调度）；
//  3. 并发未满（in_progress < max）→ 领取最高分非锁定 queued 项 → 建 evolve_task（source=work_item）驱动 S1 链；
//  4. 饥饿保护：每 N 轮若存在等待 > 24h 的 queued 项 → 插队一个（防低优先级饿死）。
//
// round 是调度轮次（从 0 起），用于触发饥饿保护。
func Dispatch(ctx context.Context, store *Store, cfg DispatchConfig, round int) (*DispatchResult, error) {
	res := &DispatchResult{}

	// 1. 重算 queued 分数
	n, err := store.RequeueScores(ctx, cfg.Weights)
	if err != nil {
		return nil, fmt.Errorf("requeue scores: %w", err)
	}
	res.Recomputed = n

	// 2. 旁路：无明确目标（target_name 空 → S1 沙箱无法定位，必然失败）
	//    + 目标不是已注册启用项目（沙箱只认 projects 表，未注册/未启用的自动执行必失败）→ rejected。
	//    避免把"无法执行"的任务派给必然失败的 S1 链（浪费一次 coder→沙箱 运行）。
	//    S9.8 §7.10 起 core 系不再旁路（统一走自动进化链，人工在审批/合并 PR 处把关）。
	queued, err := store.List(ctx, Filter{Status: StatusQueued})
	if err != nil {
		return nil, err
	}
	for _, it := range queued {
		reason := bypassReason(ctx, store, it)
		if reason == "" {
			continue
		}
		if err := store.UpdateStatus(ctx, it.No, StatusRejected); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("rejected bypass %s (%s): %v", it.No, reason, err))
		}
	}

	// 3. 并发未满 → 领取最高分（跳过前置任务未完成的）
	inProgress, _ := store.InProgressCount(ctx)
	if int(inProgress) < cfg.MaxInProgress {
		// 取前几个候选：被依赖闸门拦下的跳过，继续看下一个
		candidates, err := store.List(ctx, Filter{Status: StatusQueued, ExcludeLocked: true, Limit: 5})
		if err != nil {
			return nil, err
		}
		for _, cand := range candidates {
			if blockers, reason := checkItemDependencies(ctx, store, cfg, cand); len(blockers) > 0 {
				res.DependencyBlocked = append(res.DependencyBlocked, fmt.Sprintf("%s: %s", cand.No, reason))
				continue
			}
			picked, err := claimAndCreateTask(ctx, store, cand.No)
			if err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("dispatch %s: %v", cand.No, err))
				continue
			}
			res.Dispatched = append(res.Dispatched, picked)
			break
		}
	}

	// 4. 饥饿保护（防低优先级饿死，§7.5）
	if round > 0 && cfg.StarvationEvery > 0 && round%cfg.StarvationEvery == 0 {
		inProgress, _ = store.InProgressCount(ctx)
		if int(inProgress) < cfg.MaxInProgress {
			starved, err := store.QueuedWaiting(ctx, cfg.StarvationMinWait)
			if err == nil && starved != nil {
				if blockers, reason := checkItemDependencies(ctx, store, cfg, *starved); len(blockers) > 0 {
					res.DependencyBlocked = append(res.DependencyBlocked, fmt.Sprintf("%s: %s", starved.No, reason))
				} else if picked, err := claimAndCreateTask(ctx, store, starved.No); err == nil {
					res.Starved = picked
				}
			}
		}
	}
	return res, nil
}

// checkItemDependencies 对一个 queued 工作项做启动前依赖检查：
// 取它关联的看板卡所在看板，用 LLM 判断是否有未完成前置。
// LLM 未配置 / 卡缺失 / 查询失败 → 放行（fail-open）。
func checkItemDependencies(ctx context.Context, store *Store, cfg DispatchConfig, it db.WorkItem) ([]DependencyBlocker, string) {
	if cfg.LLM == nil || it.CardID == nil {
		return nil, ""
	}
	var card db.BoardCard
	if err := store.database.WithContext(ctx).First(&card, *it.CardID).Error; err != nil {
		return nil, ""
	}
	var curList db.BoardList
	if err := store.database.WithContext(ctx).First(&curList, card.ListID).Error; err != nil {
		return nil, ""
	}
	var cards []db.BoardCard
	if err := store.database.WithContext(ctx).
		Where("list_id IN (?)", store.database.Model(&db.BoardList{}).Select("id").Where("board_id = ?", curList.BoardID)).
		Find(&cards).Error; err != nil {
		return nil, ""
	}
	return CheckDependenciesWithStore(ctx, store, cfg.LLM, cards, card)
}

// shouldBypassRejected 判断一个 queued 工作项是否应旁路到 rejected（不进自动调度）：
// 仅当无明确目标（target_name 空/无值）：S1 沙箱无法定位测试镜像/命令，必然失败 → rejected。
// S9.8 §7.10 起 core 系不再旁路（统一走自动进化链，人工在审批/合并 PR 处把关）。
// shouldBypassRejected 纯函数：目标为空（无明确目标）→ 旁路 rejected。
// 归一/别名后再判（存量项可能存着服务名 api / go-worker，映射到 core 后不算“无目标”）。
func shouldBypassRejected(targetName string) bool {
	return project.NormalizeTargets(targetName) == ""
}

// bypassReason 返回该 queued 工作项应旁路 rejected 的原因（"" = 可自动执行）。
// 在 shouldBypassRejected（纯函数）基础上补库级检查：主目标必须是**已注册**项目
// （evolve-driver runSandbox 按 projects 表解析测试镜像/命令，没有 = 必失败）。
// 常规项目需 active=true；core 系注册即放行（见 IsRunnableTarget）。
// 判定前先归一 + 别名：把存量项里的部署单元名（api / go-worker）转成项目名（core），
// 否则会误判“目标未注册”而把本可执行的项旁路掉。
func bypassReason(ctx context.Context, store *Store, it db.WorkItem) string {
	name := project.NormalizeTargets(it.TargetName)
	if name == "" {
		return "无明确目标（沙箱无法定位）"
	}
	if !store.IsRunnableTarget(ctx, name) {
		return fmt.Sprintf("目标 %s 不是已注册启用项目（沙箱无法执行）", name)
	}
	return ""
}

// PickItem 手动领取一个 queued 工作项并建 S1 进化单（board 手动领取 / API 触发，§7.5）。
// 返回领取的工作项号；非 queued 或并发已被抢走时返回错误。
func PickItem(ctx context.Context, store *Store, no string) (string, error) {
	return claimAndCreateTask(ctx, store, no)
}

// claimAndCreateTask 领取一个 queued 工作项（→ in_progress）并建 S1 进化单（source=work_item）。
// 建单失败则回退为 queued（不丢工作项，下轮重试）。
func claimAndCreateTask(ctx context.Context, store *Store, no string) (string, error) {
	item, err := store.Claim(ctx, no)
	if err != nil {
		return "", err
	}
	taskNo, err := createEvolveTask(ctx, store, item)
	if err != nil {
		_ = store.database.WithContext(ctx).Model(&db.WorkItem{}).
			Where("no = ?", no).Update("status", StatusQueued)
		return "", err
	}
	_ = store.SetEvolveTaskNo(ctx, no, taskNo)
	return no, nil
}

// createEvolveTask 从工作项建 S1 进化单（source=work_item，status=open → evolve-driver 拾取）。
// task_no 形如 WI-<unix>-<4hex>（< 32 字符）。
// 若工作项有上一个 evolve_task（打回重跑），继承其 session_id → coder 续接上次对话。
func createEvolveTask(ctx context.Context, store *Store, item *db.WorkItem) (string, error) {
	// 归一 + 别名：存量工作项可能存着部署单元名（api / go-worker）或带路径后缀的目标，
	// 进化单（driver 按项目名解析沙箱/镜像/写范围）必须拿到干净的项目名。
	targetName := project.NormalizeTargets(item.TargetName)
	if targetName == "" {
		targetName = "unknown"
	}
	evidence := map[string]any{
		"work_item_no": item.No,
		"kind":         item.Kind,
		"source":       item.Source,
		"priority":     item.Priority,
		"task":         fmt.Sprintf("工作项 %s（%s / %s）：%s\n%s", item.No, item.Kind, item.Source, item.Title, item.Description),
		"checklist":    item.Checklist,
	}
	task := &db.EvolveTask{
		TaskNo:     fmt.Sprintf("WI-%d-%04x", time.Now().Unix(), randUint32()),
		Source:     "work_item",
		Evidence:   mustJSON(evidence),
		TargetName: targetName,
		Status:     "open",
	}
	// 继承上一个 task 的 session_id（打回重跑时续接 pi-web 会话，不重复发送任务描述）
	if item.EvolveTaskNo != "" {
		var prev db.EvolveTask
		if err := store.database.WithContext(ctx).Where("task_no = ?", item.EvolveTaskNo).First(&prev).Error; err == nil && prev.SessionID != "" {
			task.SessionID = prev.SessionID
		}
	}
	if err := store.database.WithContext(ctx).Create(task).Error; err != nil {
		return "", err
	}
	return task.TaskNo, nil
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

// randUint32 生成 4 位十六进制随机数（进化单号后缀，唯一性兜底，撞了 1062 由上层报错）。
func randUint32() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}
