// Package main is the workflow binary.
//
// workflow 是 Temporal Go Worker（注册 activities/workflows 并消费 go-worker 队列）。
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wetee/ai-workflow/internal/activities"
	"github.com/wetee/ai-workflow/internal/config"
	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/llm"
	"github.com/wetee/ai-workflow/internal/otel"
	internalotel "github.com/wetee/ai-workflow/internal/otel"
	"github.com/wetee/ai-workflow/internal/qdrant"
	"github.com/wetee/ai-workflow/internal/workflows"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	temporalotel "go.temporal.io/sdk/contrib/opentelemetry"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"gorm.io/plugin/opentelemetry/tracing"
)

func main() {
	cfg := config.Load()

	database, err := db.Open(cfg)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}

	// OTel 初始化：上报到 otel-collector（默认 http://127.0.0.1:4317）
	otelShutdown, err := internalotel.Init(context.Background(), "go-worker")
	if err != nil {
		log.Fatalf("init otel: %v", err)
	}
	defer otelShutdown(context.Background())

	// gorm 自动埋 SQL span（失败不阻断启动）
	if err := database.Use(tracing.NewPlugin(tracing.WithoutMetrics())); err != nil {
		log.Printf("enable gorm tracing plugin: %v", err)
	}

	llmClient := llm.NewClient(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, cfg.EmbeddingModel, cfg.LLMModel)

	qc, err := qdrant.NewClient(cfg.QdrantHost, cfg.QdrantGRPCPort, cfg.QdrantCollection)
	if err != nil {
		log.Fatalf("create qdrant client: %v", err)
	}
	// EnsureCollection 可能因网络问题长时间阻塞，加超时避免启动卡死
	qdrantCtx, qdrantCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := qc.EnsureCollection(qdrantCtx); err != nil {
		qdrantCancel()
		// 嵌入模型不可用时向量检索已停用，Qdrant 不可达不再阻断 worker 启动
		log.Printf("WARN: ensure qdrant collection: %v (continuing without vector search)", err)
	} else {
		qdrantCancel()
	}

	tracingInterceptor, err := temporalotel.NewTracingInterceptor(temporalotel.TracerOptions{})
	if err != nil {
		log.Fatalf("create temporal tracing interceptor: %v", err)
	}
	c, err := client.Dial(client.Options{
		HostPort:     cfg.TemporalHostPort,
		Namespace:    cfg.TemporalNamespace,
		Interceptors: []interceptor.ClientInterceptor{tracingInterceptor},
	})
	if err != nil {
		log.Fatalf("create temporal client: %v", err)
	}
	defer c.Close()

	w := worker.New(c, cfg.TemporalGoTaskQueue, worker.Options{
		Interceptors: []interceptor.WorkerInterceptor{
			&errorLogInterceptor{database: database},
			tracingInterceptor,
		},
	})
	w.RegisterWorkflow(workflows.FetchWorkflow)
	w.RegisterWorkflow(workflows.PublishWorkflow)
	w.RegisterWorkflow(workflows.ScheduledFetchWorkflow)
	w.RegisterWorkflow(workflows.EmailWorkflow)
	w.RegisterWorkflow(workflows.AutoTweetWorkflow)
	w.RegisterWorkflow(workflows.ScheduledAutoTweetWorkflow)
	w.RegisterWorkflow(workflows.FollowLikeDispatchWorkflow)
	// P1：/like 旧链路（LIKE_VIA_SKILL 未启用时）依赖的 LikeWorkflow
	w.RegisterWorkflow(workflows.LikeWorkflow)
	// P1：通用 skill 调用 workflow（任意已注册 skill 均可经 Temporal 调用）
	w.RegisterWorkflow(workflows.GenericSkillWorkflow)
	w.RegisterActivity(activities.New(qc, llmClient, database))
	// P1：Skill 桥接 activity（纯函数，不依赖 Activities 结构体）
	w.RegisterActivity(activities.InvokeSkill)

	if err := w.Start(); err != nil {
		log.Fatalf("start worker: %v", err)
	}
	log.Printf("Go worker started on task queue %s (%s)", cfg.TemporalGoTaskQueue, cfg.TemporalNamespace)

	// S2 混合编排（docs/plan/S2-混合编排.md §3.1）：agent-prod namespace 三队列。
	// skill-queue：skill 化 activity（经 Gateway 调 skill）；
	// flex-queue：柔性子图（P3 FlexActivity）+ WeeklyContentWorkflow（P2）；
	// cron-queue：Scheduled* 定时 workflow（从 default 迁移）。
	// 三个队列都注册完整 Go activity 集，workflow 内未显式指定队列的 activity 默认跑在本队列。
	// default 队列保留旧注册，双跑一个版本周期后停旧 cron（§7 回滚）。
	agentClient, err := client.Dial(client.Options{
		HostPort:     cfg.TemporalHostPort,
		Namespace:    cfg.TemporalAgentNamespace,
		Interceptors: []interceptor.ClientInterceptor{tracingInterceptor},
	})
	if err != nil {
		log.Fatalf("create %s temporal client: %v", cfg.TemporalAgentNamespace, err)
	}
	defer agentClient.Close()

	agentWorkers := []worker.Worker{
		worker.New(agentClient, cfg.SkillTaskQueue, worker.Options{
			Interceptors: []interceptor.WorkerInterceptor{
				&errorLogInterceptor{database: database},
				tracingInterceptor,
			},
		}),
		worker.New(agentClient, cfg.FlexTaskQueue, worker.Options{
			Interceptors: []interceptor.WorkerInterceptor{
				&errorLogInterceptor{database: database},
				tracingInterceptor,
			},
		}),
		worker.New(agentClient, cfg.CronTaskQueue, worker.Options{
			Interceptors: []interceptor.WorkerInterceptor{
				&errorLogInterceptor{database: database},
				tracingInterceptor,
			},
		}),
	}
	agentWorkers[0].RegisterWorkflow(workflows.GenericSkillWorkflow)
	// S2 §3.2：周内容 workflow（审批卡点示范）跑在 flex-queue
	agentWorkers[1].RegisterWorkflow(workflows.WeeklyContentWorkflow)
	agentWorkers[2].RegisterWorkflow(workflows.ScheduledFetchWorkflow)
	agentWorkers[2].RegisterWorkflow(workflows.ScheduledAutoTweetWorkflow)
	agentWorkers[2].RegisterWorkflow(workflows.FollowLikeDispatchWorkflow)
	for _, aw := range agentWorkers {
		aw.RegisterActivity(activities.New(qc, llmClient, database))
		aw.RegisterActivity(activities.InvokeSkill)
		if err := aw.Start(); err != nil {
			log.Fatalf("start %s worker: %v", cfg.TemporalAgentNamespace, err)
		}
	}
	log.Printf("Go worker started on %s queues: %s / %s / %s", cfg.TemporalAgentNamespace,
		cfg.SkillTaskQueue, cfg.FlexTaskQueue, cfg.CronTaskQueue)

	// S2：cron 迁移开关——true 时 cron workflow 在 agent-prod 的 cron 队列上
	//（syncCrons/syncFollowDispatch describe 不到即重建 schedule）；false 时维持 default 队列。
	cronClient, cronQueue := c, cfg.TemporalGoTaskQueue
	if cfg.CronQueueMigrated {
		cronClient, cronQueue = agentClient, cfg.CronTaskQueue
	}

	// Sync all scheduled ingest cron workflows on startup + periodically
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			syncCrons(cronClient, cronQueue, cfg, database)
			syncFollowDispatch(cronClient, cronQueue, cfg, database)
			<-ticker.C
		}
	}()

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
	w.Stop()
	for _, aw := range agentWorkers {
		aw.Stop()
	}
}

func syncCrons(c client.Client, queue string, cfg *config.Config, database *db.DB) {
	var tasks []db.IngestTask
	if err := database.Where("schedule != '' AND enabled = true").Find(&tasks).Error; err != nil {
		log.Printf("syncCrons query: %v", err)
		return
	}

	updated := 0
	for _, task := range tasks {
		wid := fmt.Sprintf("scraper-%s-%d", task.Source, task.ID)
		descCtx, descCancel := context.WithTimeout(context.Background(), 10*time.Second)
		desc, err := c.DescribeWorkflowExecution(descCtx, wid, "")
		descCancel()
		if err != nil || isTerminalWorkflowStatus(desc) {
			// Workflow missing or finished — recreate
			cancelCtx, cancelCancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = c.CancelWorkflow(cancelCtx, wid, "")
			cancelCancel()

			execCtx, execCancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, err := c.ExecuteWorkflow(execCtx, client.StartWorkflowOptions{
				TaskQueue:                queue,
				ID:                       wid,
				CronSchedule:             task.Schedule,
				WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
				WorkflowExecutionTimeout: 30 * 24 * time.Hour,
			}, workflows.ScheduledFetchWorkflow, workflows.ScheduledFetchWorkflowInput{
				Source: task.Source, Keyword: task.Keyword, Limit: task.Limit, Mode: task.Mode, ParallelCount: task.ParallelCount,
			})
			execCancel()
			if err != nil {
				log.Printf("syncCrons task %d (%s) restart failed: %v", task.ID, task.Keyword, err)
				otel.Record(database, otel.Entry{
					System:  "go-worker",
					Source:  "syncCrons",
					Message: fmt.Sprintf("restart cron workflow %s failed: %v", wid, err),
					Context: fmt.Sprintf(`{"task_id":%d,"source":"%s","keyword":"%s"}`, task.ID, task.Source, task.Keyword),
				})
			} else {
				updated++
			}
		}
	}

	if updated > 0 {
		log.Printf("syncCrons: restarted %d missing cron workflows", updated)
	}
}

// isTerminalWorkflowStatus 判断 workflow 是否已处于终态。
// 终态（含 TERMINATED/CANCELED/TIMED_OUT）说明 cron workflow 已停止，需要重建。
func isTerminalWorkflowStatus(desc *workflowservice.DescribeWorkflowExecutionResponse) bool {
	if desc == nil || desc.WorkflowExecutionInfo == nil {
		return true
	}
	switch desc.WorkflowExecutionInfo.Status {
	case enums.WORKFLOW_EXECUTION_STATUS_COMPLETED,
		enums.WORKFLOW_EXECUTION_STATUS_FAILED,
		enums.WORKFLOW_EXECUTION_STATUS_CANCELED,
		enums.WORKFLOW_EXECUTION_STATUS_TERMINATED,
		enums.WORKFLOW_EXECUTION_STATUS_TIMED_OUT:
		return true
	}
	return false
}

// syncFollowDispatch 保证订阅目标点赞分发 cron workflow 存在（模式与 syncCrons 一致）。
func syncFollowDispatch(c client.Client, queue string, cfg *config.Config, database *db.DB) {
	const wid = "follow-like-dispatch"
	descCtx, descCancel := context.WithTimeout(context.Background(), 10*time.Second)
	desc, err := c.DescribeWorkflowExecution(descCtx, wid, "")
	descCancel()
	if err == nil && !isTerminalWorkflowStatus(desc) {
		return // 已在运行，无需重建
	}

	cancelCtx, cancelCancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = c.CancelWorkflow(cancelCtx, wid, "")
	cancelCancel()

	execCtx, execCancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, err = c.ExecuteWorkflow(execCtx, client.StartWorkflowOptions{
		TaskQueue:                queue,
		ID:                       wid,
		CronSchedule:             cfg.FollowLikeDispatchCron,
		WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		WorkflowExecutionTimeout: 30 * 24 * time.Hour,
	}, workflows.FollowLikeDispatchWorkflow, workflows.FollowLikeDispatchInput{
		PyreplayTaskQueue: cfg.TemporalPyreplayTaskQueue,
	})
	execCancel()
	if err != nil {
		log.Printf("syncFollowDispatch: start cron workflow failed: %v", err)
		otel.Record(database, otel.Entry{
			System:  "go-worker",
			Source:  "syncFollowDispatch",
			Message: fmt.Sprintf("start follow-like-dispatch cron workflow failed: %v", err),
		})
	} else {
		log.Printf("syncFollowDispatch: started cron workflow (schedule=%s)", cfg.FollowLikeDispatchCron)
	}
}

// errorLogInterceptor 把失败的 activity 记入 error_logs 表（system=go-worker）。
// 仅记录最终失败：重试未达到上限时每次尝试的失败也会在这里出现，
// 只有在 Attempt 达到 RetryPolicy 上限（不再重试）时才写一条记录。
type errorLogInterceptor struct {
	interceptor.WorkerInterceptorBase
	database *db.DB
}

func (e *errorLogInterceptor) InterceptActivity(ctx context.Context, next interceptor.ActivityInboundInterceptor) interceptor.ActivityInboundInterceptor {
	return &errorLogActivityInterceptor{
		database: e.database,
		ActivityInboundInterceptorBase: interceptor.ActivityInboundInterceptorBase{
			Next: next,
		},
	}
}

func (e *errorLogInterceptor) InterceptWorkflow(ctx workflow.Context, next interceptor.WorkflowInboundInterceptor) interceptor.WorkflowInboundInterceptor {
	return next
}

type errorLogActivityInterceptor struct {
	interceptor.ActivityInboundInterceptorBase
	database *db.DB
}

func (a *errorLogActivityInterceptor) Init(outbound interceptor.ActivityOutboundInterceptor) error {
	return a.ActivityInboundInterceptorBase.Next.Init(outbound)
}

func (a *errorLogActivityInterceptor) ExecuteActivity(ctx context.Context, in *interceptor.ExecuteActivityInput) (interface{}, error) {
	res, err := a.ActivityInboundInterceptorBase.Next.ExecuteActivity(ctx, in)
	if err != nil {
		info := activity.GetInfo(ctx)
		// 仅记录最终失败：Attempt 达到 RetryPolicy 上限后不会再重试。
		if info.RetryPolicy != nil && info.RetryPolicy.MaximumAttempts > 0 &&
			info.Attempt >= info.RetryPolicy.MaximumAttempts {
			otel.Record(a.database, otel.Entry{
				System:     "go-worker",
				Source:     info.ActivityType.Name,
				WorkflowID: info.WorkflowExecution.ID,
				RunID:      info.WorkflowExecution.RunID,
				TraceID:    internalotel.TraceIDFrom(ctx),
				Message:    err.Error(),
				Context:    fmt.Sprintf(`{"attempt":%d}`, info.Attempt),
			})
		}
	}
	return res, err
}
