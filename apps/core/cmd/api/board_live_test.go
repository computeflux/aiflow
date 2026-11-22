package main

import (
	"testing"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/workitem"
)

// TestEffectiveItemStatus 锁定「evolve_task 状态 → 工作项状态」的实时投影口径
// （与 workitem.Feedback 一致）：看板轮询要秒级反映执行/审核/完成，不能等 5min 镜像轮。
func TestEffectiveItemStatus(t *testing.T) {
	item := db.WorkItem{No: "wi-1", Status: workitem.StatusInProgress}
	cases := []struct {
		task string
		want string
	}{
		{"coding", workitem.StatusInProgress},
		{"reviewing", workitem.StatusInProgress},
		{"sandboxing", workitem.StatusInProgress},
		{"testing", workitem.StatusInProgress},
		{"approving", workitem.StatusInReview},
		// released 但 merge_commit 空 = 没合入主分支 → 停在 Review（不是 Done）
		{"released", workitem.StatusInReview},
		{"cancelled", workitem.StatusRejected},
		{"rolled_back", workitem.StatusRejected},
		{"expired", workitem.StatusExpired},
	}
	for _, c := range cases {
		got := effectiveItemStatus(item, db.EvolveTask{TaskNo: "WI-1", Status: c.task})
		if got != c.want {
			t.Errorf("task=%s → %s, want %s", c.task, got, c.want)
		}
	}

	// released + 已合入主分支 → 才算 Done
	if got := effectiveItemStatus(item, db.EvolveTask{TaskNo: "WI-1", Status: "released", MergeCommit: "abc123"}); got != workitem.StatusShipped {
		t.Errorf("released 且已合并应 → shipped，得到 %s", got)
	}

	// 无任务 → 回退工作项自身状态
	if got := effectiveItemStatus(db.WorkItem{Status: workitem.StatusQueued}, db.EvolveTask{}); got != workitem.StatusQueued {
		t.Errorf("无任务应回退工作项状态，got %s", got)
	}
}
