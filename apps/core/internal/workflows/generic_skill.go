package workflows

import (
	"fmt"
	"os"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// GenericSkillInput 是通用 skill 调用 workflow 的输入。
// 任何已注册的 skill 都可通过 SkillName + Input 经 Temporal 调用。
type GenericSkillInput struct {
	// SkillName 是已注册 skill 的名称（如 generate / evaluate / like / x-scan）。
	SkillName string `json:"skill_name"`
	// Input 是传给 skill 的参数（对应 skill.json 的 input_schema）。
	Input map[string]any `json:"input"`
	// ViaSkill 必须为 true：通用 workflow 只走 Skill 网关。
	// （各工具的传统 Go activity 命名各异，无法在此通用，故默认走 skill。）
	ViaSkill bool `json:"via_skill"`
}

// GenericSkillWorkflow 演示任意已注册 skill 的 Temporal 调用：
//
//   - ExecuteActivity("InvokeSkill", skillName, input)
//     → internal/activities/skill_bridge.go 内部 HTTP 调 skillgateway
//     → 对应 service/cli skill → 统一契约 {ok, ...}
//
// agent 也经同一个 skillgateway（POST /api/skills/:name/invoke）调用同一 skill，
// 两条路径互不耦合。默认 ViaSkill=true。
func GenericSkillWorkflow(ctx workflow.Context, input GenericSkillInput) (map[string]any, error) {
	if input.SkillName == "" {
		return nil, fmt.Errorf("skill_name 为空")
	}
	if input.ViaSkill && os.Getenv("SKILL_VIA_DEFAULT") != "false" {
		return callViaSkill(ctx, input.SkillName, input.Input)
	}
	// 未显式要求走其它路径时，默认走 Skill 网关。
	return callViaSkill(ctx, input.SkillName, input.Input)
}

// callViaSkill 走 P1 Skill 网关：超时 5 分钟、失败重试 3 次（等比退避）。
func callViaSkill(ctx workflow.Context, name string, input map[string]any) (map[string]any, error) {
	logger := workflow.GetLogger(ctx)

	skillAO := workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts:    3,
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
		},
	}
	skillCtx := workflow.WithActivityOptions(ctx, skillAO)

	var out map[string]any
	err := workflow.ExecuteActivity(skillCtx, "InvokeSkill", name, input).Get(ctx, &out)
	if err != nil {
		return nil, err
	}
	logger.Info("skill invoked", "name", name, "keys", len(out))
	return out, nil
}
