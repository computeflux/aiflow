package workitem

import (
	"strings"
	"testing"

	"github.com/wetee/ai-workflow/internal/db"
)

// TestParseDependsOn 容忍 JSON 数组 / 逗号 / 空格 / 中文逗号 / # 前缀。
func TestParseDependsOn(t *testing.T) {
	cases := []struct {
		in   string
		want []uint
	}{
		{"", nil},
		{"[]", nil},
		{"null", nil},
		{"[137]", []uint{137}},
		{"[137, 138]", []uint{137, 138}},
		{"137", []uint{137}},
		{"137,138", []uint{137, 138}},
		{"137 138", []uint{137, 138}},
		{"#137， #138", []uint{137, 138}},
		{"137,137,138", []uint{137, 138}}, // 去重
		{"abc", nil},
		{"0", nil},
	}
	for _, tc := range cases {
		got := ParseDependsOn(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("ParseDependsOn(%q)=%v，期望 %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("ParseDependsOn(%q)=%v，期望 %v", tc.in, got, tc.want)
				break
			}
		}
	}
}

// TestExplicitBlockers 显式依赖：未合并 → 阻塞；已合并 → 放行。
func TestExplicitBlockers(t *testing.T) {
	dep := db.BoardCard{ID: 137, Title: "地址详情页"}
	target := db.BoardCard{ID: 139, Title: "交易历史", DependsOn: "[137]"}
	byID := map[uint]db.BoardCard{137: dep}

	// 未合并 → 阻塞
	blockers, reason := explicitBlockers(target, byID, map[uint]bool{137: false})
	if len(blockers) != 1 || blockers[0].CardID != 137 {
		t.Fatalf("未合并应阻塞，得到 %+v", blockers)
	}
	if !strings.Contains(reason, "#137") || !strings.Contains(reason, "合入主分支") {
		t.Errorf("原因应含 #137 与「合入主分支」，得到 %q", reason)
	}
	if blockers[0].Merged {
		t.Error("blocker.Merged 应为 false")
	}

	// 已合并 → 放行
	if blockers, _ := explicitBlockers(target, byID, map[uint]bool{137: true}); len(blockers) != 0 {
		t.Errorf("已合并应放行，得到 %+v", blockers)
	}

	// 无显式依赖 → 不判定
	if blockers, _ := explicitBlockers(db.BoardCard{ID: 1}, byID, nil); len(blockers) != 0 {
		t.Errorf("无 depends_on 不应阻塞，得到 %+v", blockers)
	}

	// 多前置只阻塞其中一个
	multi := db.BoardCard{ID: 10, DependsOn: "137,138"}
	byID[138] = db.BoardCard{ID: 138, Title: "已完成的卡"}
	blockers, reason = explicitBlockers(multi, byID, map[uint]bool{137: true, 138: false})
	if len(blockers) != 1 || blockers[0].CardID != 138 {
		t.Fatalf("应只阻塞 #138，得到 %+v", blockers)
	}
	if strings.Contains(reason, "#137 ") {
		t.Errorf("已合并的 #137 不应出现在原因里: %q", reason)
	}

	// 前置卡不在本看板（byID 缺失）→ 仍视为未合并阻塞
	blockers, _ = explicitBlockers(db.BoardCard{ID: 11, DependsOn: "[999]"}, byID, map[uint]bool{})
	if len(blockers) != 1 || blockers[0].CardID != 999 {
		t.Errorf("缺失的前置卡应阻塞，得到 %+v", blockers)
	}
}

// TestDependencyReasonTruncate 标题过长时截断，避免提示条被撑爆。
func TestDependencyReasonTruncate(t *testing.T) {
	long := strings.Repeat("很长的标题", 20)
	r := dependencyReason([]db.CardDependency{{CardID: 7, Title: long}})
	if !strings.HasPrefix(r, "需 #7 ") || !strings.Contains(r, "合入主分支后才能开始") {
		t.Errorf("原因格式不符: %q", r)
	}
	if len([]rune(r)) > 80 {
		t.Errorf("原因过长: %d 字", len([]rune(r)))
	}
}

// TestMergedCardSetNilStore 无库时退回看列（手工卡口径）。
func TestMergedCardSetNilStore(t *testing.T) {
	cards := []db.BoardCard{
		{ID: 1, Status: "done"},
		{ID: 2, Status: "in_progress"},
	}
	got := MergedCardSet(nil, nil, cards)
	if !got[1] || got[2] {
		t.Errorf("无库时应按列判定：done=完成、in_progress=未完成，得到 %+v", got)
	}
}
