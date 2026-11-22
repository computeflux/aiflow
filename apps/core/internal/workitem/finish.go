package workitem

import (
	"context"
	"fmt"
)

// finishPath 返回把工作项从 from 推到目标终态需要依次经过的状态（纯函数）。
//
//   - shipped：必须逐级补齐 in_progress → in_review → approved → shipped（不得跳级）
//   - rejected：一步直达（in_progress / in_review / approved → rejected）
//   - in_review：仅从 in_progress 往前推一步（看板「完成执行」：任务停住，人去看 diff）
//
// 已处于目标态 → 返回空（幂等）；其它起点（queued/open/expired/未知）→ error。
// 红线：看板「结束任务」也必须走合法迁移，不能绕过状态机。
func finishPath(from, target string) ([]string, error) {
	if from == target {
		return nil, nil
	}
	if target == StatusRejected {
		if !IsValidTransition(from, StatusRejected) {
			return nil, fmt.Errorf("工作项当前状态 %s 无法终止", from)
		}
		return []string{StatusRejected}, nil
	}
	if target == StatusInReview {
		// 「完成执行」：只要任务执行过（有 diff/审查记录），就允许拉到 Review 看改动。
		// 包括已驳回（rejected）的任务 —— 此时是「复活」到 in_review，不重新跑执行链。
		switch from {
		case StatusInProgress, StatusRejected:
			return []string{StatusInReview}, nil
		}
		return nil, fmt.Errorf("工作项当前状态 %s 无法推到 in_review（仅 in_progress / rejected 可以）", from)
	}
	if target != StatusShipped {
		return nil, fmt.Errorf("不支持的结束目标 %q", target)
	}
	start := 0
	for i, stage := range releasedLadder {
		if stage == from {
			start = i + 1
			break
		}
	}
	if start == 0 && from != StatusInProgress {
		return nil, fmt.Errorf("工作项当前状态 %s 无法验收完成", from)
	}
	path := make([]string, 0, len(releasedLadder)-start)
	cur := from
	for _, to := range releasedLadder[start:] {
		if !IsValidTransition(cur, to) {
			return path, fmt.Errorf("非法状态迁移 %s -> %s", cur, to)
		}
		path = append(path, to)
		cur = to
	}
	return path, nil
}

// FinishTo 人工结束工作项（看板「结束任务」）：按合法阶梯推到目标终态并落库。
// 返回实际经过的状态序列（调用方写 audit）；已处于目标态返回空。
//
// 调用方负责先停掉关联进化单（cancelled）——否则流水线会继续把工作项往前推。
func FinishTo(ctx context.Context, store *Store, no, target string) ([]string, error) {
	item, err := store.ByNo(ctx, no)
	if err != nil {
		return nil, fmt.Errorf("get work item %s: %w", no, err)
	}
	path, err := finishPath(item.Status, target)
	if err != nil {
		return nil, err
	}
	for _, to := range path {
		if err := store.UpdateStatus(ctx, no, to); err != nil {
			return path, err
		}
	}
	return path, nil
}
