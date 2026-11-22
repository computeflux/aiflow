package main

// 演示 pipeline 包的用法：go run ./pipeline-demo
import (
	"context"
	"fmt"
	"strings"

	"examples/pipeline"
)

func main() {
	ctx := context.Background()

	// 1. 泛型函数式三连：生成 → 过滤偶数 → 平方 → 求和
	nums := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	evens := pipeline.Filter(nums, func(x int) bool { return x%2 == 0 })
	squares := pipeline.Map(evens, func(x int) int { return x * x })
	total := pipeline.Sum(squares)
	fmt.Printf("偶数平方和: %v -> %d\n", squares, total) // 4+16+36+64+100 = 220

	// 2. 通道管道：Generate -> Square，惰性流式处理
	ch := pipeline.Square(ctx, pipeline.Generate(ctx, 8))
	fmt.Printf("管道平方:   %v\n", pipeline.Collect(ch))

	// 3. Fan-out：3 个 worker 并行把单词转大写
	words := make(chan string, 5)
	for _, w := range []string{"alpha", "bravo", "charlie", "delta", "echo"} {
		words <- w
	}
	close(words)
	upper := pipeline.Collect(pipeline.FanOut(ctx, 3, words, strings.ToUpper))
	fmt.Printf("Fan-out:    %v\n", upper)

	// 4. Max：泛型约束 + 自定义比较器
	if m, ok := pipeline.Max(squares, func(a, b int) bool { return a < b }); ok {
		fmt.Printf("Max:        %d\n", m)
	}
}
