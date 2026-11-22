package workitem

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/project"
	"gorm.io/gorm"
)

// CardSyncResult 是一轮"看板卡 → 工作项"同步的统计（§7.6 看板入队）。
type CardSyncResult struct {
	Scanned int
	Created int
	Errors  []string
}

// SyncCards 把看板中 status=ready_for_evolve 且尚未关联工作项（work_item_no=”）的卡，
// 转为**需求工作项**入统一队列（§7.6 看板入队）——这是"工作看板 → 工作项流水线"的桥：
// 用户在看板把卡标记为 ready_for_evolve（= 交给工作流），调度器每轮扫描一次，
// 建需求工作项（kind=requirement）并双向关联卡↔工作项；无明确目标/目标非已注册启用项目 → rejected（回 Backlog）。
// 已关联（work_item_no 非空）的卡不重复建（幂等）；需求是已定义的任务（无需 AI 分诊），
// 直接落 triaged（→ queued 合法），不经 open。
func SyncCards(ctx context.Context, store *Store, w ScoreWeights) (*CardSyncResult, error) {
	res := &CardSyncResult{}
	var cards []db.BoardCard
	// 未关联工作项：work_item_no 为 ''（GORM 建卡零值）/ NULL（裸 SQL/旧行）/ "card-<id>"（分诊卡关联）都算未关联
	if err := store.database.WithContext(ctx).
		Where("status = ? AND (work_item_no = '' OR work_item_no IS NULL OR work_item_no LIKE 'card-%')", "ready_for_evolve").
		Order("id ASC").
		Find(&cards).Error; err != nil {
		return nil, fmt.Errorf("scan ready board cards: %w", err)
	}
	res.Scanned = len(cards)

	for i := range cards {
		item, err := CreateFromCard(ctx, store, w, &cards[i])
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("card %d: %v", cards[i].ID, err))
			continue
		}
		if item == nil {
			continue // 已关联（幂等跳过）
		}
		res.Created++
	}
	return res, nil
}

// buildCardItem 把看板卡组装为工作项（§7.6 入队前完整字段）：
// 目标名由 cardTarget 推导（S9.8 §7.10：只按项目识别，卡 target_name 专门字段，支持多选）、
// severity/impact 从卡 priority、urgency 从 due_date；需求已定义（无需 AI 分诊）直接落 triaged。
// Bug 卡（source=error_log，分诊产出）→ kind=bug：source_ref 用错误簇键（去重/回填对齐），
// frequency 取卡上累计的出现次数（轻重缓急打分的一部分）。
func buildCardItem(database *gorm.DB, c *db.BoardCard) *db.WorkItem {
	targetName := cardTarget(database, c)
	sev := priorityToSeverityInt(c.Priority)
	item := &db.WorkItem{
		Kind:        KindRequirement,
		Source:      cardSource(c.Source),
		SourceRef:   fmt.Sprintf("card-%d", c.ID),
		TargetName:  targetName,
		Title:       c.Title,
		Description: valueOr(c.Description, c.Title),
		Checklist:   c.Checklist,
		Assignee:    c.Assignee,
		Severity:    sev,
		Impact:      sev,
		Frequency:   0,
		Status:      StatusTriaged,
		CardID:      &c.ID,
	}
	if c.Source == "error_log" && strings.TrimSpace(c.SourceRef) != "" {
		item.Kind = KindBug
		item.SourceRef = c.SourceRef // 错误簇键（system|source|归一化消息）
		item.Frequency = c.Frequency
	}
	if c.DueDate != nil {
		item.DueAt = c.DueDate
		item.Urgency = UrgencyFromDueAt(c.DueDate, time.Now())
	} else {
		item.Urgency = 1
	}
	return item
}

// CreateFromCard 看板卡 → 工作项 + 双向关联（§7.6 看板入队；幂等：已关联返回 nil,nil）。
// Bug 卡（source=error_log）→ kind=bug；需求卡 → kind=requirement。
// 无明确目标（target_name 空）/ 目标非已注册启用项目 → rejected（回 Backlog）；其余 → queued 入统一队列。
func CreateFromCard(ctx context.Context, store *Store, w ScoreWeights, c *db.BoardCard) (*db.WorkItem, error) {
	if !IsCardRef(c.WorkItemNo) && strings.TrimSpace(c.WorkItemNo) != "" {
		return nil, nil // 已关联工作项（幂等跳过）；"card-<id>" 是分诊卡关联，不算
	}
	item := buildCardItem(store.database, c)
	if err := store.Create(ctx, item); err != nil {
		return nil, err
	}
	// 双向关联：回填卡的工作项号（幂等守卫：仅当仍未关联——空/NULL/"card-<id>"——才回填，防并发重复建）
	link := store.database.WithContext(ctx).Model(&db.BoardCard{}).
		Where("id = ? AND (work_item_no = '' OR work_item_no IS NULL OR work_item_no LIKE 'card-%')", c.ID).
		Update("work_item_no", item.No)
	if link.Error != nil {
		return nil, link.Error
	}
	if link.RowsAffected == 0 {
		// 已被别的轮次/进程关联 → 本轮建的工作项冗余，回退为 rejected（不占队列）
		_ = store.UpdateStatus(ctx, item.No, StatusRejected)
		return nil, nil
	}
	relinkErrorLogsWithItem(ctx, store, c, item)
	if err := queueCardItem(ctx, store, w, item); err != nil {
		return nil, err
	}
	return item, nil
}

// relinkErrorLogsWithItem hand-off 建工作项后，把簇的 error_logs 关联从 "card-<id>" 改回工作项号
// （分诊阶段是卡关联；shipped 回填按 work_item_no 对齐，故在此改回）。仅 Bug 卡（source=error_log）。
func relinkErrorLogsWithItem(ctx context.Context, store *Store, c *db.BoardCard, item *db.WorkItem) {
	if item.Source != SourceErrorLog {
		return
	}
	store.database.WithContext(ctx).Model(&db.ErrorLog{}).
		Where("work_item_no = ?", fmt.Sprintf("card-%d", c.ID)).
		Update("work_item_no", item.No)
}

// IsCardRef 卡关联标记（"card-<id>"）：分诊阶段 Bug 卡的 work_item_no 存的是卡号
// （error_logs 同步用），不是工作项号——各处"已关联工作项"判断需排除它。
func IsCardRef(s string) bool {
	return strings.HasPrefix(s, "card-")
}

// HandOffCard 看板卡被移动到 **In Progress 列 = 交给工作流**（§7.6 看板为核心）：
//   - 未关联工作项（含仅卡关联 "card-<id>"）→ 建工作项（Bug 卡 kind=bug）+ 双向关联；
//   - 已关联且未完结（open/triaged → 入队 queued；queued/in_progress/in_review/approved → 不操作）；
//   - 已关联且 expired → 重新入队（queued）；
//   - 已关联且终结（rejected/shipped）→ 建**新**工作项重跑 + 重新关联（旧项留终结态历史）；
//   - 工作项不存在（已清理）→ 按未关联重建。
//
// 返回入队/重开的工作项号；无需操作时返回 ""。
func HandOffCard(ctx context.Context, store *Store, w ScoreWeights, c *db.BoardCard) (string, error) {
	if IsCardRef(c.WorkItemNo) || strings.TrimSpace(c.WorkItemNo) == "" {
		item, err := CreateFromCard(ctx, store, w, c)
		if err != nil || item == nil {
			return "", err
		}
		return item.No, nil
	}
	var item db.WorkItem
	if err := store.database.WithContext(ctx).Where("no = ?", c.WorkItemNo).First(&item).Error; err != nil {
		newItem, rerr := createAndRelinkCard(ctx, store, w, c) // 工作项已清理 → 重建
		if rerr != nil || newItem == nil {
			return "", rerr
		}
		return newItem.No, nil
	}
	switch {
	case item.Status == StatusExpired:
		return item.No, store.UpdateStatus(ctx, item.No, StatusQueued)
	case IsTerminal(item.Status):
		newItem, rerr := createAndRelinkCard(ctx, store, w, c) // 终结态重跑：建新工作项
		if rerr != nil || newItem == nil {
			return "", rerr
		}
		return newItem.No, nil
	case item.Status == StatusOpen || item.Status == StatusTriaged:
		return item.No, QueueFromPreTriage(ctx, store, item.No, item.Status)
	default:
		return "", nil // queued/in_progress/in_review/approved：已在流水线
	}
}

// QueueFromPreTriage 把"未入队"的工作项推进到 queued（人工交给工作流 / 确认分诊 / 审计提案）。
// 状态机**不允许 open → queued**（跳过分诊，见 validTransitions 注释与 item_test）：
// 故 open 先补 triaged 一步，triaged 直接入队。status 传工作项当前状态。
func QueueFromPreTriage(ctx context.Context, store *Store, no, status string) error {
	if status == StatusOpen {
		if err := store.UpdateStatus(ctx, no, StatusTriaged); err != nil {
			return err
		}
	}
	return store.UpdateStatus(ctx, no, StatusQueued)
}

// createAndRelinkCard 为卡建新工作项并**覆盖**卡的工作项号（重跑/重建场景）。
func createAndRelinkCard(ctx context.Context, store *Store, w ScoreWeights, c *db.BoardCard) (*db.WorkItem, error) {
	item := buildCardItem(store.database, c)
	if err := store.Create(ctx, item); err != nil {
		return nil, err
	}
	if err := store.database.WithContext(ctx).Model(&db.BoardCard{}).
		Where("id = ?", c.ID).Update("work_item_no", item.No).Error; err != nil {
		return nil, err
	}
	relinkErrorLogsWithItem(ctx, store, c, item)
	if err := queueCardItem(ctx, store, w, item); err != nil {
		return nil, err
	}
	return item, nil
}

// queueCardItem 算分入队：无明确目标（target_name 空 → S1 沙箱无法定位测试镜像/命令）
// / 目标不是已注册启用项目（沙箱无法执行）→ 直接 rejected（看板可重新 hand-off 重跑）；
// 其余 → queued。末态回写 item.Status（供调用方统计/即时反馈）。
func queueCardItem(ctx context.Context, store *Store, w ScoreWeights, item *db.WorkItem) error {
	_ = store.UpdateScores(ctx, item.No, item.Severity, item.Urgency, item.Impact, item.Frequency, w)
	to := cardItemTargetStatus(item.TargetName)
	if to == StatusQueued && !store.IsRunnableTarget(ctx, item.TargetName) {
		to = StatusRejected // 目标不是已注册启用项目（沙箱无法执行）→ rejected
	}
	if err := store.UpdateStatus(ctx, item.No, to); err != nil {
		return err
	}
	item.Status = to
	return nil
}

// cardItemTargetStatus 卡工作项入队末态：无明确目标（target_name 空，沙箱无法定位）→ rejected；其余 → queued。
// S9.8 §7.10 起 core 系不再旁路（统一走自动进化链，人工在审批/合并 PR 处把关）。
func cardItemTargetStatus(targetName string) string {
	if shouldBypassRejected(targetName) {
		return StatusRejected
	}
	return StatusQueued
}

// SyncOneCardStatus 立即把一张卡的状态**和所在列**对齐到工作项状态（hand-off 后即时反馈，不等 5min 镜像轮）。
// 定列口径见 CardLane（shipped 但未合入主干 → 停在 Review）。
func SyncOneCardStatus(ctx context.Context, store *Store, cardID uint, workItemNo string) error {
	var item db.WorkItem
	if err := store.database.WithContext(ctx).Where("no = ?", workItemNo).First(&item).Error; err != nil {
		return err
	}
	var card db.BoardCard
	if err := store.database.WithContext(ctx).First(&card, cardID).Error; err != nil {
		return err
	}
	laneStatus, laneColumn := CardLane(item, loadEvolveTask(ctx, store, item.EvolveTaskNo))
	// 列跟随工作项状态（queued→Todo / in_progress→In Progress …），避免卡片停在用户拖动前的列
	if _, err := moveCardToColumn(ctx, store, card, laneColumn); err != nil {
		return err
	}
	return store.database.WithContext(ctx).Model(&db.BoardCard{}).
		Where("id = ?", cardID).
		Updates(map[string]any{"status": laneStatus, "updated_at": time.Now()}).Error
}

// cardWorkItemNos 取卡关联的工作项号（去空/去重/剔除分诊卡关联 card-<id>）。
func cardWorkItemNos(cards []db.BoardCard) []string {
	seen := make(map[string]bool, len(cards))
	out := make([]string, 0, len(cards))
	for _, c := range cards {
		no := strings.TrimSpace(c.WorkItemNo)
		if no == "" || IsCardRef(no) || seen[no] {
			continue
		}
		seen[no] = true
		out = append(out, no)
	}
	return out
}

// CardStatusFromItem 工作项状态 → 看板卡状态（镜像表，§7.6）。
func CardStatusFromItem(status string) string {
	switch status {
	case StatusShipped:
		return "done"
	case StatusRejected, StatusExpired:
		return "todo"
	case StatusOpen, StatusTriaged:
		// 候选（未入队）/被收回 → Backlog，不能显示成“排队中”（那样卡看着像已交给工作流，实际不会被调度）。
		return "todo"
	case StatusInProgress:
		return "in_progress"
	case StatusInReview, StatusApproved:
		return "in_review"
	default: // queued
		return "ready_for_evolve"
	}
}

// ColumnFromItem 工作项状态 → 看板列名（§7.6 看板为核心：卡随流水线在列间移动）。
//
// 列语义（2026-10-28 对齐）：
//   - **Backlog** = 候选任务：未入队（open/triaged）或失败/超时回收（rejected/expired）；
//   - **Todo**    = 待执行：已入队 queued，等调度器领取；
//   - **In Progress** = 正在执行：in_progress；
//   - **Review**  = 正在审核：in_review / approved（有待审批单）；
//   - **Done**    = 完成：shipped。
//
// 列不存在时不动卡（容错）。
func ColumnFromItem(status string) string {
	switch status {
	case StatusShipped:
		return "Done"
	case StatusRejected, StatusExpired, StatusOpen, StatusTriaged:
		return "Backlog" // 候选 / 失败回收（可重新 hand-off）
	case StatusQueued:
		return "Todo" // 待执行（已入队）
	case StatusInProgress:
		return "In Progress" // 正在执行
	case StatusInReview, StatusApproved:
		return "Review" // 正在审核
	default:
		return "Backlog"
	}
}

// moveCardToColumn 把卡移到同看板内指定名称的列（追加到列尾）。返回新 list_id；
// 当前列不存在或目标列不存在（用户自定义列名）时返回 0（不动卡，幂等容错）。
func moveCardToColumn(ctx context.Context, store *Store, c db.BoardCard, column string) (uint, error) {
	var curList db.BoardList
	if err := store.database.WithContext(ctx).Where("id = ?", c.ListID).First(&curList).Error; err != nil {
		return 0, nil // 当前列不存在 → 不动
	}
	var target db.BoardList
	if err := store.database.WithContext(ctx).
		Where("board_id = ? AND name = ?", curList.BoardID, column).First(&target).Error; err != nil {
		return 0, nil // 目标列不存在（自定义列名）→ 不动
	}
	if target.ID == c.ListID {
		return target.ID, nil // 已在目标列
	}
	var maxPos int
	if err := store.database.WithContext(ctx).Model(&db.BoardCard{}).
		Where("list_id = ?", target.ID).Select("COALESCE(MAX(position),0)").Scan(&maxPos).Error; err != nil {
		return 0, fmt.Errorf("max position: %w", err)
	}
	if err := store.database.WithContext(ctx).Model(&db.BoardCard{}).Where("id = ?", c.ID).
		Updates(map[string]any{"list_id": target.ID, "position": maxPos + 1, "updated_at": time.Now()}).Error; err != nil {
		return 0, err
	}
	return target.ID, nil
}

// cardTarget 推导卡的目标名（S9.8 §7.10：target_name 专门字段，只按项目识别，支持多选）：
// 归一为多值串（trim/去空/去重/保序，如 "pycraper,core"）；主目标（第一个）驱动 S1 单项目执行；
// 只按项目识别，不回退 labels；空 = 无目标 → rejected（沙箱无法定位）。
func cardTarget(database *gorm.DB, c *db.BoardCard) string {
	_ = database // S9.8 §7.10 起目标只按项目名识别，不再查表（保留参数以稳定签名）
	// 归一 + 部署单元别名：存量卡可能存着服务名（api / go-worker）或带路径后缀的目标，
	// 领取时统一转成注册项目名，否则工作项主目标未注册 → 调度秒拒（卡 #102 实测）。
	return project.NormalizeTargets(c.TargetName)
}

// cardSource 把卡 source 映射到工作项 source（对齐 workitem 来源常量）。
func cardSource(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "product":
		return SourceProduct
	case "error_log":
		return SourceErrorLog // Bug 卡（分诊产出）
	case "audit":
		return SourceAudit
	default: // manual / ops / 未知 → 人工
		return SourceManual
	}
}

// priorityToSeverityInt 把看板卡优先级（low/medium/high/urgent）映射到 0-4 严重度。
func priorityToSeverityInt(p string) int {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "urgent":
		return 4
	case "high":
		return 3
	case "low":
		return 1
	default: // medium 及未知
		return 2
	}
}

func valueOr(v, fallback string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}
