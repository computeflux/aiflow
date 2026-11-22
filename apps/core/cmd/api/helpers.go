package main

import (
	"context"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/otel"
	"go.temporal.io/sdk/client"
)

// maskSecret 对敏感字段（auth_token / ct0）脱敏，避免在管理接口中泄露完整 cookie。
func maskSecret(s string) string {
	if len(s) <= 8 {
		if s == "" {
			return ""
		}
		return "****"
	}
	return s[:4] + "****" + s[len(s)-4:]
}

// serverError 对外统一返回通用错误文案，内部细节写入 error_logs，避免向客户端泄露 LLM/DB 内部信息。
func serverError(database *db.DB, source string, err error) gin.H {
	otel.Record(database, otel.Entry{System: "api", Source: source, Message: err.Error()})
	return gin.H{"error": "internal error"}
}

// apiEnvOr 读取环境变量（trim 后非空才生效），否则返回默认值。
func apiEnvOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// gitRemoteURL 返回仓库 web 地址（人工 PR compare 链接拼接用）。
func gitRemoteURL() string {
	if v := strings.TrimSpace(os.Getenv("GIT_REMOTE_URL")); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return "https://github.com/computeflux/workflow"
}

// gitBaseBranch 返回基线分支名（人工 PR compare 用；与 EVOLVE_BASE_BRANCH 同键）。
func gitBaseBranch() string {
	if v := strings.TrimSpace(os.Getenv("EVOLVE_BASE_BRANCH")); v != "" {
		return v
	}
	return "main"
}

// approvalSignalSender 把 Temporal client（agent-prod）包装成审批 Signal 发送器（S2 §3.2）。
type approvalSignalSender struct {
	c client.Client
}

func (a approvalSignalSender) SendSignal(ctx context.Context, workflowID, signalName string, payload any) error {
	// 本 SDK 版本 SignalWorkflow 带 runID 参数（空 = 当前 run）
	return a.c.SignalWorkflow(ctx, workflowID, "", signalName, payload)
}
