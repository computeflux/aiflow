package pipeline

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// ---------- 泛型函数测试（表驱动） ----------

func TestMap(t *testing.T) {
	tests := []struct {
		name string
		in   []int
		fn   func(int) string
		want []string
	}{
		{"empty", []int{}, func(i int) string { return "x" }, []string{}},
		{"to-string", []int{1, 2, 3}, func(i int) string { return string(rune('a' + i - 1)) }, []string{"a", "b", "c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Map(tt.in, tt.fn); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Map() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFilter(t *testing.T) {
	in := []int{1, 2, 3, 4, 5, 6}
	want := []int{2, 4, 6}
	got := Filter(in, func(i int) bool { return i%2 == 0 })
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Filter() = %v, want %v", got, want)
	}
}

func TestReduce(t *testing.T) {
	in := []int{1, 2, 3, 4}
	got := Reduce(in, 1, func(acc, x int) int { return acc * x })
	if got != 24 { // 1*2*3*4 = 24（初始值 1）
		t.Errorf("Reduce() = %d, want 24", got)
	}
}

func TestSum(t *testing.T) {
	if got := Sum([]int{1, 2, 3}); got != 6 {
		t.Errorf("Sum(int) = %d, want 6", got)
	}
	if got := Sum([]float64{1.5, 2.5}); got != 4.0 {
		t.Errorf("Sum(float64) = %f, want 4.0", got)
	}
}

func TestMax(t *testing.T) {
	less := func(a, b int) bool { return a < b }
	if m, ok := Max([]int{3, 9, 2}, less); !ok || m != 9 {
		t.Errorf("Max() = %d,%v, want 9,true", m, ok)
	}
	if _, ok := Max([]int{}, less); ok {
		t.Error("Max(empty) should return ok=false")
	}
}

// ---------- 管道测试 ----------

func TestPipelineSquare(t *testing.T) {
	ctx := context.Background()
	got := Collect(Square(ctx, Generate(ctx, 5)))
	want := []int{1, 4, 9, 16, 25}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Square(Generate(5)) = %v, want %v", got, want)
	}
}

func TestPipelineCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	// 消费一个数后取消，管道应终止且不泄漏
	got := Collect(Square(ctx, Generate(ctx, 1_000_000)))
	if len(got) >= 1_000_000 {
		t.Errorf("pipeline not canceled: got %d items", len(got))
	}
}

func TestFanOut(t *testing.T) {
	ctx := context.Background()
	in := Generate(ctx, 10)
	got := Collect(FanOut(ctx, 3, in, func(x int) int { return x * 10 }))
	if len(got) != 10 {
		t.Fatalf("FanOut got %d items, want 10", len(got))
	}
	// 并行消费顺序不定，排序后比较集合
	sum := Sum(got)
	if sum != 550 { // (1+2+...+10)*10
		t.Errorf("FanOut sum = %d, want 550", sum)
	}
}
