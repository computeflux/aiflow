package workitem

import (
	"testing"

	"github.com/wetee/ai-workflow/internal/db"
)

// TestCardStatusFromItem 锁定"工作项状态 → 看板卡状态"镜像表（§7.6 双向同步）：
// 中间态实时反映（排队中/执行中/审核中），终结态收敛（shipped→done，rejected/expired→todo 回 Backlog）。
func TestCardStatusFromItem(t *testing.T) {
	cases := []struct {
		itemStatus string
		want       string
		name       string
	}{
		{StatusOpen, "todo", "open 候选/被收回（Backlog，可重新 hand-off）"},
		{StatusTriaged, "todo", "triaged 候选（已分诊未入队）"},
		{StatusQueued, "ready_for_evolve", "queued 排队中"},
		{StatusInProgress, "in_progress", "in_progress 执行中"},
		{StatusInReview, "in_review", "in_review 审核中"},
		{StatusApproved, "in_review", "approved 审核中（审批卡点）"},
		{StatusShipped, "done", "shipped 完成"},
		{StatusRejected, "todo", "rejected 跑失败（回 Backlog，可重新 hand-off）"},
		{StatusExpired, "todo", "expired 超时（回 Backlog，可重新入队）"},
	}
	for _, c := range cases {
		if got := CardStatusFromItem(c.itemStatus); got != c.want {
			t.Errorf("CardStatusFromItem(%q) = %q, want %q（%s）", c.itemStatus, got, c.want, c.name)
		}
	}
}

// TestColumnFromItem 锁定"工作项状态 → 看板列"映射（§7.6 看板为核心：卡随流水线在列间移动）。
func TestColumnFromItem(t *testing.T) {
	cases := []struct {
		itemStatus string
		want       string
		name       string
	}{
		{StatusQueued, "Todo", "queued → Todo（待执行）"},
		{StatusTriaged, "Backlog", "triaged → Backlog（候选，已分诊未入队）"},
		{StatusOpen, "Backlog", "open → Backlog（候选/被收回）"},
		{StatusInProgress, "In Progress", "in_progress → In Progress（执行中）"},
		{StatusInReview, "Review", "in_review → Review（审核中）"},
		{StatusApproved, "Review", "approved → Review（审批卡点）"},
		{StatusShipped, "Done", "shipped → Done"},
		{StatusRejected, "Backlog", "rejected → Backlog（跑失败回候选）"},
		{StatusExpired, "Backlog", "expired → Backlog"},
	}
	for _, c := range cases {
		if got := ColumnFromItem(c.itemStatus); got != c.want {
			t.Errorf("ColumnFromItem(%q) = %q, want %q（%s）", c.itemStatus, got, c.want, c.name)
		}
	}
}

// TestCardItemTargetStatus 锁定卡工作项入队末态：无明确目标 → rejected（沙箱无法定位）；
// 其余（含 core 系，S9.8 §7.10 起不再旁路）→ queued。
func TestCardItemTargetStatus(t *testing.T) {
	cases := []struct {
		targetName string
		want       string
		name       string
	}{
		{"like", StatusQueued, "正常项目目标 → 入队"},
		{"pycraper", StatusQueued, "正常项目目标 → 入队"},
		{"", StatusRejected, "无明确目标 → rejected（S1 沙箱无法定位）"},
		{"   ", StatusRejected, "纯空白目标名 → rejected"},
		{"core", StatusQueued, "core 系 → 入队（不再旁路）"},
		{"core-ui", StatusQueued, "core-ui 系 → 入队（不再旁路）"},
		{"pycraper,core", StatusQueued, "多选含 core → 入队（主目标 pycraper 可执行）"},
	}
	for _, c := range cases {
		if got := cardItemTargetStatus(c.targetName); got != c.want {
			t.Errorf("cardItemTargetStatus(%q) = %q, want %q（%s）", c.targetName, got, c.want, c.name)
		}
	}
}

// TestBuildCardItemBug 锁定 §7.4 重设：Bug 看板卡（source=error_log + 簇键）→ kind=bug 工作项，
// source_ref 用簇键（非 card-<id>），frequency 取卡上累计次数；普通需求卡保持 kind=requirement。
func TestBuildCardItemBug(t *testing.T) {
	bugCard := &db.BoardCard{ID: 42, Title: "X 抓取代理全部超时", Labels: "bug", TargetName: "pycraper",
		Priority: "high", Source: "error_log", SourceRef: "pycraper|fetch:x|x scraper failed",
		Frequency: 28, Description: "根因假设：代理池耗尽"}
	it := buildCardItem(nil, bugCard)
	if it.Kind != KindBug || it.Source != SourceErrorLog {
		t.Errorf("bug card → kind=%q source=%q, want %q/%q", it.Kind, it.Source, KindBug, SourceErrorLog)
	}
	if it.SourceRef != bugCard.SourceRef {
		t.Errorf("bug card source_ref=%q, want 簇键 %q", it.SourceRef, bugCard.SourceRef)
	}
	if it.Frequency != 28 {
		t.Errorf("bug card frequency=%d, want 28", it.Frequency)
	}
	if it.TargetName != "pycraper" {
		t.Errorf("bug card target=%q, want pycraper", it.TargetName)
	}

	reqCard := &db.BoardCard{ID: 7, Title: "新增 retry_count 字段", Labels: "manual,product",
		Priority: "medium", Source: "manual"}
	rit := buildCardItem(nil, reqCard)
	if rit.Kind != KindRequirement || rit.Source != SourceManual || rit.SourceRef != "card-7" || rit.Frequency != 0 {
		t.Errorf("requirement card → kind=%q source=%q ref=%q freq=%d, want requirement/manual/card-7/0",
			rit.Kind, rit.Source, rit.SourceRef, rit.Frequency)
	}
}

// TestSeverityToPriority 锁定严重度 → 看板卡优先级映射（priorityToSeverityInt 的逆）。
func TestSeverityToPriority(t *testing.T) {
	cases := []struct {
		sev  int
		want string
	}{{4, "urgent"}, {3, "high"}, {2, "medium"}, {1, "low"}, {0, "low"}}
	for _, c := range cases {
		if got := severityToPriority(c.sev); got != c.want {
			t.Errorf("severityToPriority(%d) = %q, want %q", c.sev, got, c.want)
		}
	}
}

// TestCardTarget 锁定 S9.8 §7.10 目标推导（只按项目识别，多选）：
// target_name 归一为多值串（主目标=第一个驱动 S1）；空 → 无目标（不回退 labels）。
func TestCardTarget(t *testing.T) {
	cases := []struct {
		name     string
		card     *db.BoardCard
		wantName string
	}{
		{"单目标", &db.BoardCard{TargetName: "pycraper"}, "pycraper"},
		{"单目标 core", &db.BoardCard{TargetName: "core"}, "core"},
		{"未注册目标", &db.BoardCard{TargetName: "unknown"}, "unknown"},
		{"多选归一（主目标=第一个）", &db.BoardCard{TargetName: "pycraper,core"}, "pycraper,core"},
		{"多选 trim 去重保序", &db.BoardCard{TargetName: " pycraper , core ,pycraper"}, "pycraper,core"},
		{"空 target_name = 无目标", &db.BoardCard{TargetName: ""}, ""},
		{"空 target_name 不回退 labels", &db.BoardCard{Labels: "app,pycraper"}, ""},
	}
	for _, c := range cases {
		gotName := cardTarget(nil, c.card)
		if gotName != c.wantName {
			t.Errorf("cardTarget(target_name=%q, labels=%q) = %q, want %q（%s）",
				c.card.TargetName, c.card.Labels, gotName, c.wantName, c.name)
		}
	}
}

// TestCardLane 锁定「未合入主分支不算 Done」的口径（2026-10-31，2026-11-03 收紧）。
// shipped 只代表已审批/已验收；**必须有 merge_commit 才能进 Done**。
//   - 进化单 released 但 merge_commit 空（冲突/暂停/PR 待合并）→ Review
//   - 人工「完成执行」后验收（任务是 cancelled/stopped，根本没合并）→ Review
//   - 无进化单 = 无法证明合并 → Review
//
// 否则看板显示已完成，而主干上根本没有这段代码，下游卡还会因此误放行。
func TestCardLane(t *testing.T) {
	shipped := db.WorkItem{No: "wi-1", Status: StatusShipped}
	mergedTask := &db.EvolveTask{TaskNo: "WI-1", Status: "released", MergeCommit: "abc123"}
	unmergedTask := &db.EvolveTask{TaskNo: "WI-1", Status: "released", MergeCommit: ""}
	blankMerge := &db.EvolveTask{TaskNo: "WI-1", Status: "released", MergeCommit: "   "}
	stoppedTask := &db.EvolveTask{TaskNo: "WI-1", Status: "stopped", MergeCommit: ""}
	stoppedMerged := &db.EvolveTask{TaskNo: "WI-1", Status: "stopped", MergeCommit: "def456"}

	cases := []struct {
		name       string
		item       db.WorkItem
		task       *db.EvolveTask
		wantStatus string
		wantColumn string
	}{
		{"已发布且已合并 → Done", shipped, mergedTask, "done", "Done"},
		{"已发布未合并 → Review", shipped, unmergedTask, "in_review", "Review"},
		{"merge_commit 只有空白也算未合并", shipped, blankMerge, "in_review", "Review"},
		// 2026-11-03 收紧：「完成执行」停掉的任务没有合并记录 → 不能 Done
		{"任务已停但未合并 → Review", shipped, stoppedTask, "in_review", "Review"},
		{"任务已停但已合并 → Done", shipped, stoppedMerged, "done", "Done"},
		// 2026-11-03 收紧：无进化单 = 无法证明代码已合入 → 停在 Review
		{"无进化单 → Review（无法证明合并）", shipped, nil, "in_review", "Review"},
		{"执行中 → In Progress", db.WorkItem{Status: StatusInProgress}, unmergedTask, "in_progress", "In Progress"},
		{"排队中 → Todo", db.WorkItem{Status: StatusQueued}, nil, "ready_for_evolve", "Todo"},
		{"审核中 → Review", db.WorkItem{Status: StatusInReview}, nil, "in_review", "Review"},
	}
	for _, c := range cases {
		gotStatus, gotColumn := CardLane(c.item, c.task)
		if gotStatus != c.wantStatus || gotColumn != c.wantColumn {
			t.Errorf("%s: got (%s,%s)，期望 (%s,%s)", c.name, gotStatus, gotColumn, c.wantStatus, c.wantColumn)
		}
	}
}
