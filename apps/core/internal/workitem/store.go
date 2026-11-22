package workitem

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"gorm.io/gorm"
)

// Store 是 work_items 表访问层（直接取 *gorm.DB，与 product.Store 同风格）。
type Store struct {
	database *gorm.DB
}

// NewStore 构造 work_items 表访问层。
func NewStore(database *gorm.DB) *Store { return &Store{database: database} }

// IsRunnableTarget 目标是否可自动执行：主目标（第一个）是**已注册且启用**项目（projects 表，
// S9 §7.8 项目注册：apps/ 目录注册为项目后可作 target_name）。S9.8 §7.10 起目标**只按项目识别**：
// S1 沙箱（evolve-driver runSandbox）按 projects 表解析主目标的测试镜像/命令，未注册的自动执行必失败
// （烧掉一轮 coder/reviewer）→ 调度前先拦。查询失败返回 true（不误杀，交给后续步骤处理）。
//
// 2026-10-18：不再有「core 系人工维护 → 默认停用」这套（项目一律默认启用，见 project.ScanApps），
// active 就是纯粹的启停开关：用户关掉某个项目的自动执行，指向它的工作项才会被拦。
func (s *Store) IsRunnableTarget(ctx context.Context, name string) bool {
	primary := PrimaryTarget(name)
	if primary == "" {
		return false
	}
	var n int64
	if err := s.database.WithContext(ctx).Table("projects").
		Where("name = ? AND active = ?", primary, true).Count(&n).Error; err != nil {
		return true // 查询失败不误杀
	}
	return n > 0
}

// ensureDDL 建表 DDL：与 apps/core/hacks/sql/s9_work_item.sql 保持一致（幂等，进程启动时调用）。
// 注：error_logs.work_item_no / cards.work_item_no 两列由 workflow-api 的 AutoMigrate 补齐，
// 本 DDL 不写 ALTER（MySQL 8.0 不支持 ADD COLUMN IF NOT EXISTS）。
var ensureDDL = `CREATE TABLE IF NOT EXISTS work_items (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  no VARCHAR(32) NOT NULL UNIQUE,
  kind VARCHAR(16) NOT NULL,
  title VARCHAR(255) NOT NULL,
  description LONGTEXT,
  source VARCHAR(32) NOT NULL,
  source_ref VARCHAR(255) DEFAULT '',
  target_name VARCHAR(128) DEFAULT '',
  score FLOAT DEFAULT 0,
  priority VARCHAR(8) DEFAULT 'P2',
  severity TINYINT DEFAULT 0,
  urgency  TINYINT DEFAULT 0,
  impact   TINYINT DEFAULT 0,
  frequency TINYINT DEFAULT 0,
  status VARCHAR(32) NOT NULL DEFAULT 'open',
  due_at DATETIME,
  checklist TEXT,
  assignee VARCHAR(128) DEFAULT '',
  locked TINYINT(1) DEFAULT 0,
  triage_degraded TINYINT(1) DEFAULT 0,
  card_id BIGINT,
  evolve_task_no VARCHAR(32) DEFAULT '',
  rollback_count TINYINT DEFAULT 0,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  triaged_at DATETIME,
  picked_at DATETIME,
  shipped_at DATETIME,
  updated_at DATETIME ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`

// EnsureSchema 建表。幂等，可在进程启动时调用。
func (s *Store) EnsureSchema() error {
	return s.database.Exec(ensureDDL).Error
}

// ---- CRUD ----

// Create 新建工作项（no 为空则生成；status 为空则 open）。
func (s *Store) Create(ctx context.Context, item *db.WorkItem) error {
	if item.No == "" {
		item.No = NewNo()
	}
	if item.Status == "" {
		item.Status = StatusOpen
	}
	return s.database.WithContext(ctx).Create(item).Error
}

// ByNo 按 no 取工作项（不存在 → gorm.ErrRecordNotFound）。
func (s *Store) ByNo(ctx context.Context, no string) (*db.WorkItem, error) {
	var item db.WorkItem
	if err := s.database.WithContext(ctx).Where("no = ?", no).First(&item).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

// FindOpenByTarget 查同一目标已存在的非终态工作项（去重：同一 target_name 只保留一个在途工作项）。
// 返回最老的非终态项 no（"" = 无）。终态 = shipped/rejected（IsTerminal）+ expired。
func (s *Store) FindOpenByTarget(ctx context.Context, targetName string) (string, error) {
	if strings.TrimSpace(targetName) == "" {
		return "", nil
	}
	var item db.WorkItem
	err := s.database.WithContext(ctx).
		Where("target_name = ? AND status IN ?", targetName,
			[]string{StatusOpen, StatusTriaged, StatusQueued, StatusInProgress, StatusInReview, StatusApproved}).
		Order("created_at ASC").First(&item).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return item.No, nil
}

// Filter 是 List 的过滤条件。
type Filter struct {
	Kind          string  // requirement | bug（空 = 全部）
	Source        string  // product | error_log | audit | manual（空 = 全部）
	Status        string  // 精确状态（空 = 全部）
	OnlyOpen      bool    // 仅 IsOpen 状态
	ExcludeLocked bool    // 排除人工锁定项（调度用）
	MinScore      float64 // 最低分（0 = 不限）
	Limit         int     // 0 = 不限
}

// List 列出工作项（按 score desc, created_at asc——统一队列顺序，§6.4）。
func (s *Store) List(ctx context.Context, f Filter) ([]db.WorkItem, error) {
	q := s.database.WithContext(ctx)
	if f.Kind != "" {
		q = q.Where("kind = ?", f.Kind)
	}
	if f.Source != "" {
		q = q.Where("source = ?", f.Source)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.OnlyOpen {
		q = q.Where("status IN ?", []string{StatusTriaged, StatusQueued, StatusInProgress, StatusInReview, StatusApproved})
	}
	if f.ExcludeLocked {
		q = q.Where("locked = ?", false)
	}
	if f.MinScore > 0 {
		q = q.Where("score >= ?", f.MinScore)
	}
	if f.Limit > 0 {
		q = q.Limit(f.Limit)
	}
	var items []db.WorkItem
	if err := q.Order("score DESC, created_at ASC").Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

// FindOpenByRef 按 (source, source_ref) 找未完结工作项（分诊去重用，§7.4）。
// 找到返回该工作项；未找到返回 (nil, nil)。
func (s *Store) FindOpenByRef(ctx context.Context, source, sourceRef string) (*db.WorkItem, error) {
	var item db.WorkItem
	err := s.database.WithContext(ctx).
		Where("source = ? AND source_ref = ? AND status IN ?",
			source, sourceRef, []string{StatusTriaged, StatusQueued, StatusInProgress, StatusInReview, StatusApproved}).
		Order("id DESC").First(&item).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

// ---- 状态与分 ----

// UpdateStatus 带状态机校验的状态转移（§4.2）；非法转移返回错误并拒绝写入。
func (s *Store) UpdateStatus(ctx context.Context, no, to string) error {
	item, err := s.ByNo(ctx, no)
	if err != nil {
		return fmt.Errorf("get work item %s: %w", no, err)
	}
	if !IsValidTransition(item.Status, to) {
		return fmt.Errorf("invalid work item transition %s -> %s (no=%s)", item.Status, to, no)
	}
	updates := map[string]any{"status": to, "updated_at": time.Now()}
	switch to {
	case StatusShipped:
		updates["shipped_at"] = time.Now()
	case StatusInProgress:
		if item.PickedAt == nil {
			updates["picked_at"] = time.Now()
		}
	}
	return s.database.WithContext(ctx).Model(&db.WorkItem{}).Where("no = ?", no).Updates(updates).Error
}

// UpdateScores 更新四维原始分 + 综合分 + 分档（动态升降级，§6.3）。
func (s *Store) UpdateScores(ctx context.Context, no string, severity, urgency, impact, frequency int, w ScoreWeights) error {
	score := ComputeScore(w, severity, urgency, impact, frequency)
	updates := map[string]any{
		"severity":   severity,
		"urgency":    urgency,
		"impact":     impact,
		"frequency":  frequency,
		"score":      score,
		"priority":   BandFromScore(score),
		"updated_at": time.Now(),
	}
	res := s.database.WithContext(ctx).Model(&db.WorkItem{}).Where("no = ?", no).Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("work item %s not found", no)
	}
	return nil
}

// RequeueScores 重算所有 queued 工作项的综合分（动态升降级，§6.3）：
// 按 due_at 重算 urgency（SLA 超时自动升档），保留 severity/impact/frequency。返回重算条数。
func (s *Store) RequeueScores(ctx context.Context, w ScoreWeights) (int, error) {
	items, err := s.List(ctx, Filter{Status: StatusQueued})
	if err != nil {
		return 0, err
	}
	now := time.Now()
	n := 0
	for _, it := range items {
		urgency := UrgencyFromDueAt(it.DueAt, now)
		score := ComputeScore(w, it.Severity, urgency, it.Impact, it.Frequency)
		_ = s.database.WithContext(ctx).Model(&db.WorkItem{}).Where("no = ?", it.No).Updates(map[string]any{
			"urgency": urgency, "score": score, "priority": BandFromScore(score), "updated_at": now,
		})
		n++
	}
	return n, nil
}

// AddFrequency 同簇错误累加复现频率并重新算分（去重归并，§7.4）：
// 不新建工作项，只把 frequency +add、重算 score/priority。
func (s *Store) AddFrequency(ctx context.Context, no string, add int, w ScoreWeights) (*db.WorkItem, error) {
	item, err := s.ByNo(ctx, no)
	if err != nil {
		return nil, err
	}
	item.Frequency += add
	score := ComputeScore(w, item.Severity, item.Urgency, item.Impact, item.Frequency)
	res := s.database.WithContext(ctx).Model(&db.WorkItem{}).Where("no = ?", no).Updates(map[string]any{
		"frequency": item.Frequency, "score": score, "priority": BandFromScore(score), "updated_at": time.Now(),
	})
	if res.Error != nil {
		return nil, res.Error
	}
	item.Score = score
	item.Priority = BandFromScore(score)
	return item, nil
}

// SetLocked 人工锁定/解锁（§6.4：locked=true 时调度器不自动领取）。
func (s *Store) SetLocked(ctx context.Context, no string, locked bool) error {
	res := s.database.WithContext(ctx).Model(&db.WorkItem{}).Where("no = ?", no).
		Updates(map[string]any{"locked": locked, "updated_at": time.Now()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("work item %s not found", no)
	}
	return nil
}

// SetAssignee 更新负责人。
func (s *Store) SetAssignee(ctx context.Context, no, assignee string) error {
	return s.database.WithContext(ctx).Model(&db.WorkItem{}).Where("no = ?", no).
		Updates(map[string]any{"assignee": assignee, "updated_at": time.Now()}).Error
}

// ---- 领取与关联 ----

// Claim 原子领取一个 queued 工作项 → in_progress + picked_at（§7.5 调度）。
// 用条件 UPDATE 抢占，RowsAffected==0 表示已被别的领取者抢走（并发/重复触发防御）。
func (s *Store) Claim(ctx context.Context, no string) (*db.WorkItem, error) {
	now := time.Now()
	res := s.database.WithContext(ctx).Model(&db.WorkItem{}).
		Where("no = ? AND status = ?", no, StatusQueued).
		Updates(map[string]any{"status": StatusInProgress, "picked_at": now, "updated_at": now})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, fmt.Errorf("work item %s not claimable (not queued)", no)
	}
	return s.ByNo(ctx, no)
}

// SetEvolveTaskNo 回填关联的 S1 进化单号（领取时）。
func (s *Store) SetEvolveTaskNo(ctx context.Context, no, taskNo string) error {
	return s.database.WithContext(ctx).Model(&db.WorkItem{}).Where("no = ?", no).
		Updates(map[string]any{"evolve_task_no": taskNo, "updated_at": time.Now()}).Error
}

// LinkCard 回填关联看板卡 id（需求入队时，§7.2）。
func (s *Store) LinkCard(ctx context.Context, no string, cardID uint) error {
	return s.database.WithContext(ctx).Model(&db.WorkItem{}).Where("no = ?", no).
		Updates(map[string]any{"card_id": cardID, "updated_at": time.Now()}).Error
}

// InProgressCount 统计当前 in_progress 工作项数（调度并发上限，§7.5）。
func (s *Store) InProgressCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.database.WithContext(ctx).Model(&db.WorkItem{}).
		Where("status = ?", StatusInProgress).Count(&n).Error
	return n, err
}

// QueuedWaiting 返回等待最久的 queued 工作项（饥饿保护，§7.5）。
func (s *Store) QueuedWaiting(ctx context.Context, minWait time.Duration) (*db.WorkItem, error) {
	cutoff := time.Now().Add(-minWait)
	var item db.WorkItem
	err := s.database.WithContext(ctx).
		Where("status = ? AND locked = ? AND created_at < ?", StatusQueued, false, cutoff).
		Order("created_at ASC").First(&item).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

// OpenWithEvolveTask 返回所有关联了进化单且未完结的工作项（回流扫描，§7.7）。
func (s *Store) OpenWithEvolveTask(ctx context.Context) ([]db.WorkItem, error) {
	var items []db.WorkItem
	err := s.database.WithContext(ctx).
		Where("evolve_task_no <> '' AND status IN ?",
			[]string{StatusInProgress, StatusInReview, StatusApproved}).
		Find(&items).Error
	if err != nil {
		return nil, err
	}
	return items, nil
}
