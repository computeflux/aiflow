package workitem

import (
	"reflect"
	"testing"
)

// TestFinishPath 锁定「人工结束」的合法状态阶梯：
// in_progress 不能直接跳到 shipped / open（状态机禁止），必须逐级补齐；
// 否则看板拖 Done 会被拒、5 分钟镜像后卡片回弹（2026-10-28 实测）。
func TestFinishPath(t *testing.T) {
	cases := []struct {
		name         string
		from, target string
		want         []string
		wantErr      bool
	}{
		{"执行中→验收", StatusInProgress, StatusShipped, []string{StatusInReview, StatusApproved, StatusShipped}, false},
		{"审核中→验收", StatusInReview, StatusShipped, []string{StatusApproved, StatusShipped}, false},
		{"已批准→验收", StatusApproved, StatusShipped, []string{StatusShipped}, false},
		{"已完成→验收（幂等）", StatusShipped, StatusShipped, nil, false},
		{"执行中→终止", StatusInProgress, StatusRejected, []string{StatusRejected}, false},
		{"审核中→终止", StatusInReview, StatusRejected, []string{StatusRejected}, false},
		{"已批准→终止", StatusApproved, StatusRejected, []string{StatusRejected}, false},
		{"已完成→终止（禁）", StatusShipped, StatusRejected, nil, true},
		// 2026-11-03 看板「完成执行」：执行过且有 diff 的卡可以直接拉到 in_review
		{"执行中→完成执行", StatusInProgress, StatusInReview, []string{StatusInReview}, false},
		{"已驳回→完成执行（复活）", StatusRejected, StatusInReview, []string{StatusInReview}, false},
		{"审核中→完成执行（幂等）", StatusInReview, StatusInReview, nil, false},
		{"排队中→完成执行（禁：没执行过）", StatusQueued, StatusInReview, nil, true},
		{"已完成→完成执行（禁）", StatusShipped, StatusInReview, nil, true},
		{"排队中→验收（禁）", StatusQueued, StatusShipped, nil, true},
		{"已过期→验收（禁）", StatusExpired, StatusShipped, nil, true},
		{"未知目标", StatusInProgress, "weird", nil, true},
	}
	for _, c := range cases {
		got, err := finishPath(c.from, c.target)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: 期望报错，got path=%v", c.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: 意外报错: %v", c.name, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: path=%v, want %v", c.name, got, c.want)
		}
		// 每一步都必须是合法迁移；终点必须等于目标态
		cur := c.from
		for _, to := range got {
			if !IsValidTransition(cur, to) {
				t.Errorf("%s: 非法迁移 %s -> %s", c.name, cur, to)
			}
			cur = to
		}
		if len(got) > 0 && cur != c.target {
			t.Errorf("%s: 终点 %s != %s", c.name, cur, c.target)
		}
	}
}
