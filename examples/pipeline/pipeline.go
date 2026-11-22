// Package pipeline 演示 Go 泛型（Generics）与管道（Pipeline/Fan-out）模式。
//
// 涵盖知识点：
//   - 泛型函数（类型参数 [T any]）
//   - 泛型约束（constraints：自定义 Number 数值约束）
//   - 单向通道管道（stage 串接，惰性求值）
//   - Fan-out 并行加速（多 worker 消费同一通道）
//   - 表驱动单元测试（go test 标准写法）
//
// 运行测试：go test ./pipeline/ -v
package pipeline

import (
	"context"
	"sync"
)

// ============================================================
// 1. 泛型函数：Map / Filter / Reduce
// ============================================================

// Map 把 f 应用到切片每个元素，返回新切片。
func Map[T, U any](xs []T, f func(T) U) []U {
	out := make([]U, len(xs))
	for i, x := range xs {
		out[i] = f(x)
	}
	return out
}

// Filter 保留满足谓词 pred 的元素。
func Filter[T any](xs []T, pred func(T) bool) []T {
	out := make([]T, 0, len(xs))
	for _, x := range xs {
		if pred(x) {
			out = append(out, x)
		}
	}
	return out
}

// Reduce 把切片折叠成单个值（带初始值 acc）。
func Reduce[T, U any](xs []T, acc U, f func(U, T) U) U {
	for _, x := range xs {
		acc = f(acc, x)
	}
	return acc
}

// ============================================================
// 2. 泛型约束：数值与可比较类型
// ============================================================

// Number 是数值类型约束（Go 1.18+ 泛型约束示例）。
type Number interface {
	~int | ~int64 | ~float64
}

// Sum 对任意数值切片求和。
func Sum[T Number](xs []T) T {
	var total T
	for _, x := range xs {
		total += x
	}
	return total
}

// Max 返回切片最大值（要求元素可比较 <）。
func Max[T any](xs []T, less func(a, b T) bool) (T, bool) {
	var zero T
	if len(xs) == 0 {
		return zero, false
	}
	m := xs[0]
	for _, x := range xs[1:] {
		if less(m, x) {
			m = x
		}
	}
	return m, true
}

// ============================================================
// 3. 通道管道（Pipeline）：每个 stage 惰性生产
// ============================================================

// Generate 产生 [1, n] 的整数流。
func Generate(ctx context.Context, n int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for i := 1; i <= n; i++ {
			select {
			case out <- i:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// Square 把输入通道的每个数平方后转发。
func Square(ctx context.Context, in <-chan int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for x := range in {
			select {
			case out <- x * x:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// FanOut 用 workers 个 goroutine 并行执行 fn，结果汇入一个通道。
func FanOut[T, U any](ctx context.Context, workers int, in <-chan T, fn func(T) U) <-chan U {
	out := make(chan U)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for x := range in {
				select {
				case out <- fn(x):
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}

// Collect 把通道里的值收敛成切片（辅助函数）。
func Collect[T any](in <-chan T) []T {
	var out []T
	for x := range in {
		out = append(out, x)
	}
	return out
}
