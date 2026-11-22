package main

import (
	"github.com/wetee/ai-workflow/internal/approval"
	"github.com/wetee/ai-workflow/internal/config"
	"github.com/wetee/ai-workflow/internal/crag"
	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/llm"
	"github.com/wetee/ai-workflow/internal/notify"
	"github.com/wetee/ai-workflow/internal/ops"
	"github.com/wetee/ai-workflow/internal/product"
	"github.com/wetee/ai-workflow/internal/project"
	"github.com/wetee/ai-workflow/internal/qdrant"
	"github.com/wetee/ai-workflow/internal/workitem"
	"go.temporal.io/sdk/client"
)

// appDeps 是 cmd/api 的依赖装配根：main() 一次性构造，各域路由注册函数按需取用。
// 目的：把 2700 行的巨型 main() 收敛成「组装 + 注册」两层，业务路由按域拆分到 routes_*.go。
type appDeps struct {
	cfg *config.Config

	database      *db.DB
	llm           *llm.Client
	qdrantClient  *qdrant.Client
	cragEngine    *crag.CRAG
	temporal      client.Client // 主 namespace（default）client
	temporalAgent client.Client // agent-prod namespace client（S2 混合编排）

	productStore  *product.Store
	productAgent  *product.Agent
	workitemStore *workitem.Store
	projectStore  *project.Store
	apvStore      *approval.Store
	opsStore      *ops.Store
	notifier      notify.Notifier
}
