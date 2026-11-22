package workitem

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"gorm.io/gorm"
)

// BugBoardName 是工作看板里的 Bug 看板名（§7.4 重设：Bug 队列 = Bug 看板）——
// 分诊产出**卡**（Backlog 列，status=todo，等人工拖到 In Progress = 交给工作流），
// 入队/状态镜像/PR 查看/执行记录全部复用看板机制，相同的功能只做一次。
const BugBoardName = "Bug"

// bugBoardColumns 是 Bug 看板标准五列（与 cmd/api POST /boards 一致）。
var bugBoardColumns = []string{"Backlog", "Todo", "In Progress", "Review", "Done"}

// ensureBugBoard 幂等确保 Bug 看板存在（标准五列，缺列补建），返回看板 ID + Backlog 列 ID。
// 列缺失（用户改名/删除）时补建 Backlog，其余列缺失不影响（moveCardToColumn 有容错）。
func ensureBugBoard(ctx context.Context, store *Store) (boardID, backlogListID uint, err error) {
	var board db.Board
	if err := store.database.WithContext(ctx).Where("name = ?", BugBoardName).First(&board).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, 0, fmt.Errorf("query bug board: %w", err)
		}
		board = db.Board{Name: BugBoardName, Description: "错误日志 AI 分诊 → Bug 卡（拖到 In Progress = 交给工作流）"}
		if err := store.database.WithContext(ctx).Create(&board).Error; err != nil {
			return 0, 0, fmt.Errorf("create bug board: %w", err)
		}
		for i, name := range bugBoardColumns {
			if err := store.database.WithContext(ctx).Create(&db.BoardList{BoardID: board.ID, Name: name, Position: i}).Error; err != nil {
				return 0, 0, fmt.Errorf("create bug board column %s: %w", name, err)
			}
		}
	}
	var backlog db.BoardList
	if err := store.database.WithContext(ctx).Where("board_id = ? AND name = ?", board.ID, "Backlog").First(&backlog).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, 0, fmt.Errorf("query bug board Backlog: %w", err)
		}
		backlog = db.BoardList{BoardID: board.ID, Name: "Backlog", Position: 99}
		if err := store.database.WithContext(ctx).Create(&backlog).Error; err != nil {
			return 0, 0, fmt.Errorf("create bug board Backlog: %w", err)
		}
	}
	return board.ID, backlog.ID, nil
}

// nextCardPosition 返回指定列的下一 position（列尾追加）。
func nextCardPosition(ctx context.Context, store *Store, listID uint) int {
	var maxPos int
	if err := store.database.WithContext(ctx).Model(&db.BoardCard{}).
		Where("list_id = ?", listID).Select("COALESCE(MAX(position),0)").Scan(&maxPos).Error; err != nil {
		return 1
	}
	return maxPos + 1
}

// findListByName 按名称找同看板内的列（不存在返回 ok=false）。
func findListByName(ctx context.Context, store *Store, boardID uint, name string) (db.BoardList, bool) {
	var l db.BoardList
	if err := store.database.WithContext(ctx).Where("board_id = ? AND name = ?", boardID, name).First(&l).Error; err != nil {
		return l, false
	}
	return l, true
}

// buildBugCard 把错误簇 + 分诊判定组装成 Bug 看板卡（§7.4）：
// target_name = 分诊判定的目标项目/技能名（S9.8 §7.10 专门字段，hand-off 时 cardTarget 推导目标）、
// labels = "bug"（纯 kind 展示）、priority 由 severity 映射、status=todo（等人工拖 In Progress 交给工作流）、
// source=error_log、source_ref = 簇键（去重键）、frequency = 出现次数。
func buildBugCard(backlogListID uint, c *errorCluster, d *TriageDecision) *db.BoardCard {
	title := fmt.Sprintf("[%s/%s] %s", c.System, c.Source, truncate(c.NormMsg, 80))
	if d.Summary != "" {
		title = fmt.Sprintf("%s（%s/%s）", truncate(d.Summary, 80), c.System, c.Source)
	}
	desc := fmt.Sprintf("system=%s source=%s 近 24h 出现 %d 次\n根因假设：%s\n样例：%s",
		c.System, c.Source, c.Count, d.RootCause, truncate(c.Sample.Message, 300))
	return &db.BoardCard{
		ListID:      backlogListID,
		Position:    0, // 调用方填（列尾追加）
		Title:       title,
		Description: desc,
		Labels:      "bug",
		TargetName:  d.TargetName,
		Priority:    severityToPriority(severityFromBand(d.Severity)),
		Status:      "todo",
		Source:      "error_log",
		SourceRef:   c.key,
		Frequency:   c.Count,
	}
}

// severityToPriority 严重度 0-4 → 看板卡优先级（priorityToSeverityInt 的逆映射）。
func severityToPriority(sev int) string {
	switch sev {
	case 4:
		return "urgent"
	case 3:
		return "high"
	case 2:
		return "medium"
	default:
		return "low"
	}
}

// findBugCard 找同簇（source_ref = 簇键）已有 Bug 卡（含已完结，取最新一张）。
func findBugCard(ctx context.Context, store *Store, boardID uint, key string) (*db.BoardCard, bool) {
	var card db.BoardCard
	err := store.database.WithContext(ctx).
		Where("list_id IN (SELECT id FROM board_lists WHERE board_id = ?) AND source_ref = ?", boardID, key).
		Order("id DESC").First(&card).Error
	if err != nil {
		return nil, false // 不存在或查询失败 → 不合并（查询失败时本轮新建，下轮去重兑底）
	}
	return &card, true
}

// mergeBugCard 去重归并：同簇已有卡（含 Done）→ 累加 frequency（不新建卡）；
// 已完结的卡重新打开回 Backlog（同一 Bug 再次出现 = 复发，重修）。
// 卡描述保持首次分诊的根因假设/样例，出现次数在 frequency 列累计（卡面徽标展示）。
func mergeBugCard(ctx context.Context, store *Store, boardID uint, c *errorCluster) (bool, error) {
	card, ok := findBugCard(ctx, store, boardID, c.key)
	if !ok {
		return false, nil
	}
	updates := map[string]any{"frequency": gorm.Expr("COALESCE(frequency,0) + ?", c.Count), "updated_at": time.Now()}
	if card.Status == "done" {
		// 复发：重新打开回 Backlog（列尾追加）
		backlog, ok := findListByName(ctx, store, boardID, "Backlog")
		if ok {
			updates["list_id"] = backlog.ID
			updates["position"] = nextCardPosition(ctx, store, backlog.ID)
			updates["status"] = "todo"
		}
	}
	if err := store.database.WithContext(ctx).Model(&db.BoardCard{}).Where("id = ?", card.ID).
		Updates(updates).Error; err != nil {
		return false, err
	}
	return true, nil
}

// linkErrorLogsToCard 分诊归并留痕：簇的 error_logs work_item_no 回填 "card-<id>"。
// "card-" 前缀区分卡关联与工作项关联；hand-off 建工作项后由 relinkErrorLogsWithItem
// 改回工作项号（shipped 回填按 work_item_no 对齐，不受影响）。
func linkErrorLogsToCard(ctx context.Context, store *Store, c *errorCluster, cardID uint) {
	updateClusterRows(ctx, store, c, map[string]any{"work_item_no": fmt.Sprintf("card-%d", cardID)})
}

// MigrateBugItems 一次性迁移（幂等，work-dispatch 启动时跑）：存量 kind=bug 工作项
// （§7.4 重设前由分诊/审计直接建、无关联卡）→ 在 Bug 看板建卡
// （work_item_no 关联 + 状态/列镜像工作项状态），使 Bug 队列与 Bug 看板对齐。
// rejected（人工驳回）不迁移；已有卡的跳过（幂等）。
func MigrateBugItems(ctx context.Context, store *Store) (int, error) {
	boardID, backlogID, err := ensureBugBoard(ctx, store)
	if err != nil {
		return 0, err
	}
	var items []db.WorkItem
	if err := store.database.WithContext(ctx).
		Where("kind = ? AND status <> ?", KindBug, StatusRejected).
		Find(&items).Error; err != nil {
		return 0, fmt.Errorf("scan bug work items: %w", err)
	}
	migrated := 0
	for i := range items {
		var cnt int64
		if err := store.database.WithContext(ctx).Model(&db.BoardCard{}).
			Where("work_item_no = ?", items[i].No).Count(&cnt).Error; err != nil {
			return migrated, fmt.Errorf("check card for %s: %w", items[i].No, err)
		}
		if cnt > 0 {
			continue // 已有卡（幂等）
		}
		listID := backlogID
		if col, ok := findListByName(ctx, store, boardID, ColumnFromItem(items[i].Status)); ok {
			listID = col.ID
		}
		card := &db.BoardCard{
			ListID:      listID,
			Position:    nextCardPosition(ctx, store, listID),
			Title:       items[i].Title,
			Description: items[i].Description,
			Labels:      "bug",
			TargetName:  items[i].TargetName,
			Priority:    severityToPriority(items[i].Severity),
			Status:      CardStatusFromItem(items[i].Status),
			Source:      items[i].Source, // error_log / audit / ...
			SourceRef:   items[i].SourceRef,
			Frequency:   items[i].Frequency,
			WorkItemNo:  items[i].No,
		}
		if err := store.database.WithContext(ctx).Create(card).Error; err != nil {
			return migrated, fmt.Errorf("create card for %s: %w", items[i].No, err)
		}
		migrated++
	}
	return migrated, nil
}
