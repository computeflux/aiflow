package workitem

import "testing"

// releasedLadderStart 复刻 feedback.go released 分支的「从哪一级开始补」逻辑：
// 当前状态在 ladder 内 → 只补它之后的；不在 ladder 内（如 in_progress）→ 从第一级补齐。
func releasedLadderStart(cur string) int {
	for i, stage := range releasedLadder {
		if stage == cur {
			return i + 1
		}
	}
	return 0
}

// TestReleasedLadderIsLegal 锁定「任务 released → 工作项 shipped」必须逐级补齐且每步合法：
// 审批常在两个回流周期（5 分钟）之间完成，工作项会停在 in_progress 而任务已 released；
// 旧实现只在 in_review 时补 approved，否则直接 in_progress → shipped → 状态机拒绝 →
// 每轮回流报错、工作项永久卡在 in_progress（2026-10-25 实测 wi-1792908636-6b61 卡 20+ 轮）。
func TestReleasedLadderIsLegal(t *testing.T) {
	for _, from := range []string{StatusInProgress, StatusInReview, StatusApproved} {
		cur := from
		for _, to := range releasedLadder[releasedLadderStart(from):] {
			if !IsValidTransition(cur, to) {
				t.Fatalf("ladder 需逐级合法：%s → %s 非法（from=%s）", cur, to, from)
			}
			cur = to
		}
		if cur != StatusShipped {
			t.Errorf("from=%s 沿 ladder 未到达 shipped（停在 %s）", from, cur)
		}
	}

	// 起点判定：approved 不应再回头补 in_review/approved（否则是非法倒退）
	if got := releasedLadderStart(StatusApproved); got != len(releasedLadder)-1 {
		t.Errorf("approved 应从最后一级（shipped）开始，got=%d", got)
	}
	if got := releasedLadderStart(StatusInReview); got != 1 {
		t.Errorf("in_review 应从 approved 开始，got=%d", got)
	}
	if got := releasedLadderStart(StatusInProgress); got != 0 {
		t.Errorf("in_progress 不在 ladder 内 → 应从第一级开始，got=%d", got)
	}

	// 反向锁定：正是旧实现的跳级路径非法（否则逐级补齐没有意义）
	if IsValidTransition(StatusInProgress, StatusShipped) {
		t.Error("in_progress → shipped 不应合法（否则无需逐级补齐）")
	}
}
