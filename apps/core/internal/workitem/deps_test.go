package workitem

import (
	"context"
	"strings"
	"testing"

	"github.com/wetee/ai-workflow/internal/db"
)

// TestParseDependencyVerdict 锁定 LLM 输出解析：容忍代码块围栏/前后杂文；非法输出 → ok=false。
func TestParseDependencyVerdict(t *testing.T) {
	v, ok := parseDependencyVerdict("```json\n{\"blocked\":true,\"blocked_by\":[3,5],\"reason\":\"B 依赖 A\"}\n```")
	if !ok || !v.Blocked || len(v.BlockedBy) != 2 || v.Reason != "B 依赖 A" {
		t.Fatalf("解析失败: ok=%v v=%+v", ok, v)
	}
	if v, ok := parseDependencyVerdict("好的，结果如下：{\"blocked\":false,\"blocked_by\":[],\"reason\":\"\"} 以上"); !ok || v.Blocked {
		t.Fatalf("容忍杂文失败: ok=%v v=%+v", ok, v)
	}
	if _, ok := parseDependencyVerdict("not json"); ok {
		t.Fatal("非法输出应 ok=false")
	}
}

// TestDependencyUserPrompt 锁定快照内容：目标任务 + 未完成任务（含状态），描述压平截断。
func TestDependencyUserPrompt(t *testing.T) {
	target := db.BoardCard{ID: 9, Title: "B 接入 c2c 法币", Description: "依赖 A 的支付通道", TargetName: "minishop"}
	unfinished := []db.BoardCard{
		{ID: 1, Title: "A 支付通道", Description: "先做通道\n\n再联调", Status: "in_progress"},
		{ID: 2, Title: "C 页面文案", Description: strings.Repeat("字", 500), Status: "todo"},
	}
	p := dependencyUserPrompt(unfinished, target)
	for _, want := range []string{"id=9", "B 接入 c2c 法币", "id=1", "状态: in_progress", "A 支付通道", "先做通道 再联调"} {
		if !strings.Contains(p, want) {
			t.Errorf("快照缺少 %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, strings.Repeat("字", dependencyDescRunes+10)) {
		t.Error("超长描述应被截断")
	}
}

// TestCheckDependenciesFailOpen 锁定 fail-open：LLM 未配置 → 不阻塞（返回空）。
func TestCheckDependenciesFailOpen(t *testing.T) {
	cards := []db.BoardCard{{ID: 1, Title: "A", Status: "todo"}, {ID: 2, Title: "B", Status: "todo"}}
	blockers, reason := CheckDependencies(context.Background(), nil, cards, cards[1])
	if len(blockers) != 0 || reason != "" {
		t.Fatalf("nil LLM 应放行，got blockers=%v reason=%q", blockers, reason)
	}
}

// TestCheckDependenciesNoUnfinished 锁定：没有其他未完成任务时不做 LLM 调用（直接放行）。
func TestCheckDependenciesNoUnfinished(t *testing.T) {
	cards := []db.BoardCard{{ID: 1, Title: "A", Status: "done"}, {ID: 2, Title: "B", Status: "todo"}}
	blockers, reason := CheckDependencies(context.Background(), nil, cards, cards[1])
	if len(blockers) != 0 || reason != "" {
		t.Fatalf("无未完成任务应放行，got blockers=%v reason=%q", blockers, reason)
	}
}

// TestIsCardDone 锁定完成态判定（大小写/空白容忍）。
func TestIsCardDone(t *testing.T) {
	for _, s := range []string{"done", "DONE", " Done "} {
		if !isCardDone(db.BoardCard{Status: s}) {
			t.Errorf("%q 应视为完成", s)
		}
	}
	for _, s := range []string{"todo", "in_progress", "in_review", ""} {
		if isCardDone(db.BoardCard{Status: s}) {
			t.Errorf("%q 不应视为完成", s)
		}
	}
}
