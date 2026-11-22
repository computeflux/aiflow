package deploy

// svcops_recreate.go：运维中心运行服务后端（原 internal/serviceops，批次 G 并入 deploy 域）。

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/wetee/ai-workflow/internal/dockerapi"
)

// serviceops_recreate.go：容器重建原语（先建新→启动→删旧→rename）与网络配置还原（从 serviceops.go 拆出，纯机械搬迁）。

// recreate 安全重建容器（回滚 / 改参共用）：
//
//  0. per-service 互斥锁（防并发 recreate 争用临时名 / 旧容器）；
//  1. 处理残留临时容器：原名容器存在 → 删临时；原名不存在而临时存在 → rename 临时回原名（可恢复）；
//  2. inspect 旧容器取基线配置（Config/HostConfig/NetworkingConfig 原样保留，含网络别名）；
//  3. 停旧容器（释放宿主端口，避免新容器 "port is already allocated"）；
//  4. 以临时名创建新容器（覆盖 image / env / ports）并启动；
//     失败 → 删临时容器 + 重启旧容器（回滚到原状，服务恢复）；
//  5. 轮询确认新容器 Running（防「启动成功但立刻退出」的假成功）；
//     未达标 → 删临时容器 + 重启旧容器；
//  6. 新容器运行 → 删旧（不删匿名卷）→ 临时容器 rename 为原名。
//
// 接受停旧→起新之间的短暂中断（秒级）。image 为空 = 沿用当前镜像；
// env 为 nil = 沿用当前 Env；portBindings 为 nil = 沿用当前端口。
func (s *ServiceOps) recreate(ctx context.Context, svc *Service, image string, env []string, portBindings map[string][]dockerapi.PortBinding, actor string) error {
	containerName := svc.ContainerName
	tmpName := containerName + "-recreate-tmp"

	// 清理/回滚/审计必须在客户端断开（请求 ctx 取消）后仍能执行，故脱离取消；
	// 但脱离取消也去掉了 deadline，而底层 dockerapi http.Client 是 Timeout:0（完全依赖 ctx 超时），
	// 一旦 docker daemon 卡住，stop/create/start/waitRunning 任一调用可能长时间不返回，
	// 而 recreate 期间持有 withServiceLock 的 per-service 互斥锁 → 该服务后续写操作被无限期阻塞。
	// 故在脱离取消的同时加显式超时兜底（stop 30s + create + start + waitRunning 30s + 余量 ≈ 3min）。
	// 注意：per-service 写互斥锁由调用方（RollbackService/UpdateServiceParams）经 withServiceLock 持有，
	// 此处不再加锁（sync.Mutex 不可重入，重复加锁会死锁）。
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
	defer cancel()

	// 1. 处理残留临时容器（上一轮 rename 失败遗留）：
	//    - 原名容器存在 → 临时是多余残留，删除（删匿名卷，临时容器不承载有效数据）；
	//    - 原名不存在而临时存在 → 临时就是该服务唯一实例（上轮 rename 失败），rename 回原名恢复。
	tmpExists, _ := s.docker.ContainerExists(opCtx, tmpName)
	if tmpExists {
		origExists, _ := s.docker.ContainerExists(opCtx, containerName)
		if origExists {
			_ = s.docker.RemoveContainerWithVolumes(opCtx, tmpName, true)
		} else {
			if err := s.docker.RenameContainer(opCtx, tmpName, containerName); err != nil {
				return fmt.Errorf("恢复残留临时容器 %s → %s 失败: %w（请人工 docker rename）", tmpName, containerName, err)
			}
			log.Printf("recreate %s: 检测到上轮 rename 失败残留，已将 %s rename 回 %s", containerName, tmpName, containerName)
		}
	}

	// 2. inspect 旧容器（获取基线配置）
	full, err := s.docker.InspectContainerFull(opCtx, containerName)
	if err != nil {
		return fmt.Errorf("inspect %s: %w（容器不存在，无法重建）", containerName, err)
	}

	// 3. 停旧容器（释放宿主端口 + 阻止 restart 策略在重建期间拉起旧实例争用端口/别名；接受短暂中断）。
	//    终态（created/exited/dead）无需停；其余（running/restarting/paused）都停。
	//    oldWasActive = 本次停过旧容器 → 失败时需重启旧容器恢复原状。
	oldStatus := ""
	if full.State != nil {
		oldStatus = full.State.Status
	}
	oldWasActive := oldStatus != "" && oldStatus != "created" && oldStatus != "exited" && oldStatus != "dead"
	if oldWasActive {
		if err := s.docker.StopContainer(opCtx, containerName, 30); err != nil {
			return fmt.Errorf("stop old %s: %w（未删旧容器，服务仍在原状）", containerName, err)
		}
	}
	// 恢复提示与实际动作一致：停过旧容器才说「已重启恢复」，终态未停则如实说明。
	recoverSuffix := func() string {
		if oldWasActive {
			return "（已重启旧容器恢复）"
		}
		return "（旧容器原为终态，未重启）"
	}

	// 4. 组装创建参数（基于当前容器配置 + 覆盖）
	cfg := full.Config
	hostCfg := full.HostConfig
	if image != "" {
		cfg["Image"] = image
	}
	if env != nil {
		cfg["Env"] = env
	}
	if portBindings != nil {
		pb := map[string]any{}
		for containerPort, bindings := range portBindings {
			arr := make([]map[string]string, 0, len(bindings))
			for _, b := range bindings {
				arr = append(arr, map[string]string{"HostIp": b.HostIP, "HostPort": b.HostPort})
			}
			pb[containerPort] = arr
		}
		hostCfg["PortBindings"] = pb
	}
	createBody := map[string]any{}
	for k, v := range cfg {
		createBody[k] = v
	}
	createBody["HostConfig"] = hostCfg
	// NetworkingConfig：保留基容器的网络别名（服务名 != 容器名时，同行服务按服务名解析依赖它）
	if nc := buildNetworkingConfig(full); nc != nil {
		createBody["NetworkingConfig"] = nc
	}

	// 5. 以临时名创建新容器
	id, err := s.docker.CreateContainerRaw(opCtx, tmpName, createBody)
	if err != nil {
		// 创建失败：旧容器已停，重启旧容器恢复服务
		if oldWasActive {
			if rerr := s.docker.StartContainer(opCtx, containerName); rerr != nil {
				return fmt.Errorf("create %s: %w；且重启旧容器失败: %v（服务不可用，请人工介入）", tmpName, err, rerr)
			}
		}
		return fmt.Errorf("create %s: %w%s", tmpName, err, recoverSuffix())
	}
	cleanupTmp := func() {
		_ = s.docker.RemoveContainerWithVolumes(opCtx, id, true) // 临时容器清理：删匿名卷
	}

	// 6. 启动新容器；失败 → 删临时容器 + 重启旧容器（回滚到原状）
	if err := s.docker.StartContainer(opCtx, id); err != nil {
		cleanupTmp()
		if oldWasActive {
			if rerr := s.docker.StartContainer(opCtx, containerName); rerr != nil {
				return fmt.Errorf("start %s: %w；且重启旧容器失败: %v（服务不可用，请人工介入）", tmpName, err, rerr)
			}
		}
		return fmt.Errorf("start %s: %w%s", tmpName, err, recoverSuffix())
	}

	// 7. 轮询确认新容器 Running（StartContainer 仅校验 OCI 能拉起进程，
	//    「启动后立刻退出」不会报错；不确认就删旧会把服务改坏）
	if werr := s.waitRunning(opCtx, id, 30*time.Second); werr != nil {
		cleanupTmp()
		if oldWasActive {
			if rerr := s.docker.StartContainer(opCtx, containerName); rerr != nil {
				return fmt.Errorf("新容器 %s 未稳定运行: %w；且重启旧容器失败: %v（服务不可用，请人工介入）", tmpName, werr, rerr)
			}
		}
		return fmt.Errorf("新容器 %s 未稳定运行: %w%s", tmpName, werr, recoverSuffix())
	}

	// 8. 新容器已运行 → 删旧（不删匿名卷）
	if err := s.docker.RemoveContainer(opCtx, containerName, true); err != nil {
		cleanupTmp()
		if oldWasActive {
			if rerr := s.docker.StartContainer(opCtx, containerName); rerr != nil {
				return fmt.Errorf("remove old %s: %w；且重启旧容器失败: %v（服务不可用，请人工介入）", containerName, err, rerr)
			}
		}
		return fmt.Errorf("remove old %s: %w%s", containerName, err, recoverSuffix())
	}

	// 9. 临时容器 rename 为原名
	if err := s.docker.RenameContainer(opCtx, tmpName, containerName); err != nil {
		// 旧容器已删、rename 失败：临时容器仍在运行（服务可用，名字带 -recreate-tmp 后缀），
		// 线上状态已被实际改变（破坏性半成功）→ 必须写失败审计留痕（红线：全部写操作留痕）。
		// 下轮 recreate 的第 1 步会自动 rename 回原名恢复。
		log.Printf("ERROR: rename %s → %s 失败: %v（临时容器仍在运行，请人工 docker rename）", tmpName, containerName, err)
		s.audit(opCtx, actor, "service_recreate_failed", containerName, map[string]any{
			"stage": "rename",
			"note":  "旧容器已删、新容器以临时名运行，需人工 docker rename 或下轮重建自动恢复",
			"tmp":   tmpName,
			"error": err.Error(),
		})
		return fmt.Errorf("rename %s → %s: %w（临时容器仍在运行，请人工处理）", tmpName, containerName, err)
	}
	return nil
}

// waitRunning 轮询确认容器处于 Running 状态（宽限期内未达标返回错误）。
// 对齐 VersionManager.waitRunning 的做法：防「启动成功但立刻退出」的假成功。
func (s *ServiceOps) waitRunning(ctx context.Context, containerID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		info, err := s.docker.InspectContainer(ctx, containerID)
		if err == nil && info.State != nil && info.State.Running {
			return nil
		}
		if time.Now().After(deadline) {
			if info != nil && info.State != nil {
				return fmt.Errorf("容器 %s 状态 %s（exit=%d），未达 Running", containerID, info.State.Status, info.State.ExitCode)
			}
			return fmt.Errorf("容器 %s 未在 %s 内达 Running", containerID, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// buildNetworkingConfig 从基容器的 NetworkSettings.Networks 重建 NetworkingConfig，
// 保留网络别名（服务名 != 容器名时，同行服务按服务名解析依赖它，如 qdrant / temporal-pg）。
// 基容器无网络信息时返回 nil（沿用默认）。
func buildNetworkingConfig(full *dockerapi.ContainerFull) map[string]any {
	if full.NetworkSettings == nil || len(full.NetworkSettings.Networks) == 0 {
		return nil
	}
	networks := map[string]any{}
	for netName, raw := range full.NetworkSettings.Networks {
		aliases := []string{}
		if m, ok := raw.(map[string]any); ok {
			if al, ok := m["Aliases"].([]any); ok {
				for _, a := range al {
					if str, ok := a.(string); ok {
						aliases = append(aliases, str)
					}
				}
			}
		}
		entry := map[string]any{}
		if len(aliases) > 0 {
			entry["Aliases"] = aliases
		}
		networks[netName] = entry
	}
	if len(networks) == 0 {
		return nil
	}
	return map[string]any{"EndpointsConfig": networks}
}
