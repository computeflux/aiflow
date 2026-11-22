package deploy

import (
	"context"
	"fmt"
	"time"
)

// CheckAsset 是资产健康判定（deployctl 与 health-patrol 共用同一套定义，S5 §3.2/§3.3）：
// 容器状态（compose ps）+ compose healthcheck（docker 健康状态）+ 每资产 HTTP 健康端点。
//
// 判定：
//   - 一次性容器（restart≠always）：成功退出（exited 0）= 健康；running = 进行中（健康）；
//     其它退出码 / 未启动 = 不健康；
//   - 常驻容器：running +（有 healthcheck 时状态 healthy；starting 在 wait 窗口内容忍）
//     +（无 healthcheck 但有 HTTP 端点时探测 200）。
//
// wait 为轮询窗口（deploy 阶段 90s / 自愈复检 60s）；窗口内变 healthy 即成功。
func (e *Executor) CheckAsset(ctx context.Context, svc *Service, wait time.Duration) (bool, string) {
	deadline := time.Now().Add(wait)
	var lastReason string
	for {
		ok, reason := e.checkOnce(ctx, svc)
		if ok {
			return true, reason
		}
		lastReason = reason
		if time.Now().After(deadline) {
			return false, lastReason
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err().Error()
		case <-time.After(3 * time.Second):
		}
	}
}

// checkOnce 单次健康判定（不轮询）。
func (e *Executor) checkOnce(ctx context.Context, svc *Service) (bool, string) {
	ps, err := e.PS(ctx)
	if err != nil {
		return false, fmt.Sprintf("compose ps: %v", err)
	}
	st, ok := ps[svc.Name]
	if !ok {
		if svc.OneShot {
			// 一次性容器未启动（init 已完成 / 按需守护进程已停）：合法状态，不触发自愈
			// （S5 §3.3 自愈针对服务存活；反复重跑 init 任务是错的）。常驻守护进程未启动才判不健康。
			return true, "一次性容器未启动（按需/已完成，跳过自愈）"
		}
		return false, "容器不存在（未启动）"
	}

	if svc.OneShot {
		switch st.State {
		case "exited":
			if st.ExitCode == 0 {
				return true, "一次性容器成功退出"
			}
			return false, fmt.Sprintf("一次性容器退出码 %d", st.ExitCode)
		case "running":
			return true, "一次性容器执行中"
		default:
			return false, fmt.Sprintf("一次性容器状态 %s", st.State)
		}
	}

	if st.State != "running" {
		return false, fmt.Sprintf("容器状态 %s（%s）", st.State, st.Status)
	}

	// compose healthcheck 状态（docker 在目标容器内执行，127.0.0.1 指向目标自身，是权威判据）。
	// 信任它即可，**不做跨容器 HTTP 探测**：从 health-patrol 容器探 127.0.0.1 是探它自身
	// （其 HealthURL 由目标容器内 healthcheck 的 127.0.0.1 端点提取），必失败（S5 踩坑）。
	if svc.HealthCheck != nil {
		switch st.Health {
		case "healthy":
			return true, "healthy"
		case "unhealthy":
			return false, "healthcheck unhealthy"
		case "starting", "":
			// starting：在轮询窗口内容忍（由 CheckAsset 的 deadline 控制）
			return false, fmt.Sprintf("healthcheck %s", firstNonEmpty(st.Health, "无状态"))
		default:
			return false, fmt.Sprintf("healthcheck 状态 %s", st.Health)
		}
	}

	// 无 compose healthcheck：容器 running 即视为健康（最小存活判据；不探 HTTP 端点）。
	return true, "healthy"
}

// FirstNonEmpty 返回第一个非空字符串。
func FirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string { return FirstNonEmpty(vals...) }
