// Package main 演示一个并发任务调度器（Worker Pool）的 Go 语言案例。
//
// 涵盖知识点：
//   - struct + interface（接口抽象）
//   - goroutine + channel（并发通信）
//   - sync.WaitGroup（协程同步）
//   - context（超时/取消控制）
//   - select（多路复用）
//   - 错误处理与自定义 error
//
// 运行：go run worker_pool.go
package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// ============================================================
// 1. 接口 + 结构体：定义一个可执行的“任务”
// ============================================================

// Task 表示一个可执行的工作单元。
type Task interface {
	Name() string
	Execute(ctx context.Context) (string, error)
}

// SleepTask 是 Task 的一个具体实现，模拟一段耗时工作。
type SleepTask struct {
	name    string
	seconds int
	fail    bool // 是否模拟失败
}

func NewSleepTask(name string, seconds int, fail bool) *SleepTask {
	return &SleepTask{name: name, seconds: seconds, fail: fail}
}

func (t *SleepTask) Name() string { return t.name }

func (t *SleepTask) Execute(ctx context.Context) (string, error) {
	// 用 select 监听取消信号，模拟可中断的长任务
	select {
	case <-time.After(time.Duration(t.seconds) * 100 * time.Millisecond):
	case <-ctx.Done():
		return "", fmt.Errorf("task %q canceled: %w", t.name, ctx.Err())
	}
	if t.fail {
		return "", &TaskError{Task: t.name, Reason: "simulated failure"}
	}
	return fmt.Sprintf("%s done in %d00ms", t.name, t.seconds), nil
}

// ============================================================
// 2. 自定义错误类型
// ============================================================

// TaskError 携带任务名与失败原因。
type TaskError struct {
	Task   string
	Reason string
}

func (e *TaskError) Error() string {
	return fmt.Sprintf("task %q failed: %s", e.Task, e.Reason)
}

// ============================================================
// 3. Worker Pool：并发调度器
// ============================================================

// Result 是一次任务执行的结果。
type Result struct {
	Task  string
	Out   string
	Err   error
	Took  time.Duration
}

// WorkerPool 消费任务并把结果写入 results 通道。
type WorkerPool struct {
	workers int
	tasks   <-chan Task
	results chan<- Result
}

func NewWorkerPool(workers int, tasks <-chan Task, results chan<- Result) *WorkerPool {
	return &WorkerPool{workers: workers, tasks: tasks, results: results}
}

// Run 启动若干 worker，每个 worker 从 tasks 通道读取任务并执行。
func (p *WorkerPool) Run(ctx context.Context, wg *sync.WaitGroup) {
	for i := 0; i < p.workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for t := range p.tasks {
				start := time.Now()
				out, err := t.Execute(ctx)
				p.results <- Result{
					Task: t.Name(),
					Out:  out,
					Err:  err,
					Took: time.Since(start),
				}
			}
		}(i)
	}
}

// ============================================================
// 4. main：组装并运行
// ============================================================

func main() {
	tasks := []Task{
		NewSleepTask("fetch-user", 3, false),
		NewSleepTask("send-email", 5, false),
		NewSleepTask("resize-image", 8, false),
		NewSleepTask("broken-job", 2, true),
		NewSleepTask("slow-job", 20, false), // 会被超时取消
	}

	// 整体上下文：超过 1.5s 未完成的任务将被取消
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	taskCh := make(chan Task, len(tasks))
	resultCh := make(chan Result, len(tasks))

	var wg sync.WaitGroup
	pool := NewWorkerPool(3, taskCh, resultCh)

	// 启动 worker
	pool.Run(ctx, &wg)

	// 投递任务
	for _, t := range tasks {
		taskCh <- t
	}
	close(taskCh)

	// 等待所有 worker 结束后关闭结果通道
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	// 收集并打印结果
	fmt.Println("===== Worker Pool Results =====")
	var success, failed, canceled int
	for r := range resultCh {
		switch {
		case errors.Is(r.Err, context.DeadlineExceeded), errors.Is(r.Err, context.Canceled):
			canceled++
			fmt.Printf("[CANCEL] %-12s %v\n", r.Task, r.Err)
		case r.Err != nil:
			failed++
			var te *TaskError
			if errors.As(r.Err, &te) {
				fmt.Printf("[FAIL]   %-12s %s\n", te.Task, te.Reason)
			}
		default:
			success++
			fmt.Printf("[OK]     %-12s %s (took %v)\n", r.Task, r.Out, r.Took.Round(time.Millisecond))
		}
	}

	fmt.Println("-------------------------------")
	fmt.Printf("Success: %d | Failed: %d | Canceled: %d\n", success, failed, canceled)

	_ = rand.Int() // 占位，避免未使用 import（如需随机可启用）
}
