package workitem

import (
	"strings"
	"testing"
)

func TestIsValidTransition(t *testing.T) {
	valid := []struct{ from, to string }{
		{StatusOpen, StatusTriaged},
		{StatusTriaged, StatusQueued},
		{StatusQueued, StatusInProgress},
		{StatusQueued, StatusExpired},
		{StatusInProgress, StatusInReview},
		{StatusInProgress, StatusExpired}, // S1 进化单审批过期（系统驱动回流）
		{StatusInReview, StatusApproved},
		{StatusInReview, StatusExpired}, // 审批单 24h 过期 → task expired → 回流
		{StatusApproved, StatusShipped},
		{StatusApproved, StatusExpired}, // 灰度前审批单过期（极端竞态）
		{StatusExpired, StatusQueued},
		{StatusQueued, StatusOpen}, // 看板拖回 Backlog/Todo = 收回队列重开
		{StatusOpen, StatusOpen},   // 同态幂等
	}
	for _, c := range valid {
		if !IsValidTransition(c.from, c.to) {
			t.Errorf("IsValidTransition(%s -> %s) = false, want true", c.from, c.to)
		}
	}

	invalid := []struct{ from, to string }{
		{StatusOpen, StatusQueued},         // 跳过分诊
		{StatusOpen, StatusShipped},        // 直接完结
		{StatusShipped, StatusQueued},      // 终止态
		{StatusRejected, StatusQueued},     // 终止态
		{StatusInProgress, StatusApproved}, // 跳过 review
	}
	for _, c := range invalid {
		if IsValidTransition(c.from, c.to) {
			t.Errorf("IsValidTransition(%s -> %s) = true, want false", c.from, c.to)
		}
	}
}

func TestIsTerminalAndOpen(t *testing.T) {
	for _, s := range []string{StatusShipped, StatusRejected} {
		if !IsTerminal(s) {
			t.Errorf("IsTerminal(%s) = false, want true", s)
		}
	}
	for _, s := range []string{StatusOpen, StatusTriaged, StatusQueued, StatusInProgress, StatusInReview, StatusApproved} {
		if IsTerminal(s) {
			t.Errorf("IsTerminal(%s) = true, want false", s)
		}
	}
	for _, s := range []string{StatusTriaged, StatusQueued, StatusInProgress, StatusInReview, StatusApproved} {
		if !IsOpen(s) {
			t.Errorf("IsOpen(%s) = false, want true", s)
		}
	}
	if IsOpen(StatusShipped) || IsOpen(StatusRejected) || IsOpen(StatusExpired) {
		t.Error("terminal/expired statuses should not be open")
	}
}

func TestNewNoFormat(t *testing.T) {
	no := NewNo()
	if !strings.HasPrefix(no, "wi-") {
		t.Errorf("NewNo() = %q, want prefix wi-", no)
	}
	// wi-<unix(10)>-<4hex> = 3+10+1+4 = 18 字符（< 32）
	if len(no) > 32 {
		t.Errorf("NewNo() = %q (%d chars), want < 32", no, len(no))
	}
}
