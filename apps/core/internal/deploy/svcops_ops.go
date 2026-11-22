package deploy

// svcops_ops.go：运维中心运行服务后端（原 internal/serviceops/serviceops.go，批次 G 并入 deploy 域）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/dockerapi"
	"gorm.io/gorm"
)

// 哨兵错误：调用方（cmd/api）据此区分 404（服务不存在）/ 403（不纳管）/ 400（参数非法）/ 409（冲突）/ 500（内部失败）。
var (
	ErrServiceNotFound = errors.New("service not found")
	ErrNotManaged      = errors.New("service not managed")
	ErrInvalidRequest  = errors.New("invalid request")
	ErrSelfOperation   = errors.New("self operation not allowed")
)

// ServiceView 是服务列表的一行（compose 定义 + docker 实时状态 + 资产画像）。
type ServiceView struct {
	Name        string   `json:"name"`
	Container   string   `json:"container"`
	Image       string   `json:"image"`
	State       string   `json:"state"`        // running | exited | paused | restarting | dead | not_created
	Status      string   `json:"status"`       // docker 状态描述
	Health      string   `json:"health"`       // healthy | unhealthy | starting | ""
	Ports       []string `json:"ports"`        // compose 原始端口字符串（"8001:8001" / "127.0.0.1:8001:8001"，与改参回传格式一致）
	StartedAt   string   `json:"started_at"`   // 容器启动时间（RFC3339）
	Managed     bool     `json:"managed"`      // 是否纳管（external = false）
	StartMethod string   `json:"start_method"` // compose | dynamic | external
	OneShot     bool     `json:"one_shot"`     // 一次性容器
	ExitCode    int      `json:"exit_code"`
}

// ServiceDetail 是服务详情（列表字段 + 日志 + compose 配置 + 环境变量 + 版本历史）。
type ServiceDetail struct {
	ServiceView
	Logs         string            `json:"logs"`
	ComposeFile  string            `json:"compose_file"`
	Environment  map[string]string `json:"environment"`
	Volumes      []string          `json:"volumes"`
	Networks     []string          `json:"networks"`
	Restart      string            `json:"restart"`
	HealthCheck  *HealthCheckSpec  `json:"health_check"` // json tag 为 snake_case（test/interval/timeout/retries/start_period）
	Versions     []VersionEntry    `json:"versions"`
	AssetProfile string            `json:"asset_profile"` // software_assets.profile JSON
}

// VersionEntry 是版本历史的一条记录。
type VersionEntry struct {
	Version   string `json:"version"`
	Image     string `json:"image"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

// ParamUpdate 是修改启动参数的请求体。
type ParamUpdate struct {
	Env    map[string]string `json:"env"`    // 要设置/覆盖的环境变量（nil = 不改）
	Ports  []string          `json:"ports"`  // 要替换的端口列表（nil = 不改），格式与 compose 一致："host:container" / "ip:host:container"
	Reason string            `json:"reason"` // 修改原因（审计留痕）
}

// ServiceOps 是运维中心服务操作器（API 容器内运行，经 Docker Engine API 操作容器）。
type ServiceOps struct {
	project  *Project
	docker   *dockerapi.Client
	assetSt  *Store
	database *gorm.DB
	root     string   // 仓库根（读 compose 文件 + .env）
	writeMu  sync.Map // map[string]*sync.Mutex：per-service 写操作互斥（防并发写操作争用容器）
	selfHost string   // 自身容器 hostname（防自操作：stop/recreate 自身会终止本进程）
}

// NewServiceOps 创建服务操作器。
// dockerHost 形如 "unix:///run/docker.sock"；repoRoot 指向统一 compose 仓库根。
func NewServiceOps(database *gorm.DB, dockerHost, repoRoot string) (*ServiceOps, error) {
	docker, err := dockerapi.New(dockerHost)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	project, err := LoadProject(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("load compose project: %w", err)
	}
	assetSt := NewStore(database)
	selfHost, _ := os.Hostname() // 容器内 hostname = 容器 ID 前缀
	return &ServiceOps{
		project:  project,
		docker:   docker,
		assetSt:  assetSt,
		database: database,
		root:     repoRoot,
		selfHost: selfHost,
	}, nil
}

// ListServices 返回全部 compose 服务的实时状态（纳管 + 不纳管）。
func (s *ServiceOps) ListServices(ctx context.Context) ([]ServiceView, error) {
	// 1. 查 docker 容器状态
	containers, err := s.docker.ListContainers(ctx, true, map[string][]string{
		"label": {"part-of=compute-flux-workflow"},
	})
	if err != nil {
		// docker 不可达时降级：只返回 compose 定义（状态 = not_created）
		log.Printf("WARN: list containers: %v (degraded mode)", err)
		containers = nil
	}
	// 按容器名索引
	byName := map[string]*dockerapi.ContainerBrief{}
	for i := range containers {
		c := &containers[i]
		byName[c.ContainerName()] = c
	}

	// 2. 按拓扑序组装视图（并发取 inspect，限并发 8，避免串行 N+1 往返）
	order, err := s.project.TopoOrder()
	if err != nil {
		return nil, fmt.Errorf("topo order: %w", err)
	}

	var (
		wg  sync.WaitGroup
		sem = make(chan struct{}, 8) // 限并发，避免对 docker/DB 压测
		mu  sync.Mutex
		out = make([]ServiceView, 0, len(order))
	)
	for _, name := range order {
		svc := s.project.Services[name]
		if svc.IsTools() {
			continue // tools profile 不展示
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(name string, brief *dockerapi.ContainerBrief) {
			defer wg.Done()
			defer func() { <-sem }()
			v := s.buildView(ctx, name, brief)
			mu.Lock()
			out = append(out, v)
			mu.Unlock()
		}(name, byName[svc.ContainerName])
	}
	wg.Wait()
	// 按拓扑序排序（并发写入后恢复顺序）
	sort.SliceStable(out, func(a, b int) bool {
		return orderIndex(order, out[a].Name) < orderIndex(order, out[b].Name)
	})
	return out, nil
}

// orderIndex 返回服务名在拓扑序中的下标（不存在返回大值）。
func orderIndex(order []string, name string) int {
	for i, n := range order {
		if n == name {
			return i
		}
	}
	return len(order)
}

// buildView 构建单个服务的视图（asset Get + inspect，复用调用方已查的 containerBrief）。
// 一次 assetSt.Get 同时判定 Managed + StartMethod，避免重复 DB 往返。
func (s *ServiceOps) buildView(ctx context.Context, name string, brief *dockerapi.ContainerBrief) ServiceView {
	svc := s.project.Services[name]
	v := ServiceView{
		Name:        name,
		Container:   svc.ContainerName,
		Image:       svc.Image,
		State:       "not_created",
		Ports:       []string{},
		StartMethod: "compose",
		OneShot:     svc.OneShot,
	}

	// 端口：用 compose 原始字符串（Raw），与改参回传格式一致，保证往返无歧义
	for _, p := range svc.Ports {
		if p.Raw != "" {
			v.Ports = append(v.Ports, p.Raw)
		} else if p.Host != "" {
			v.Ports = append(v.Ports, p.Host+":"+p.Container)
		} else {
			v.Ports = append(v.Ports, p.Container)
		}
	}

	// docker 实时状态（来自 ListContainers 的 brief，避免额外 inspect）
	if brief != nil {
		v.State = brief.State
		v.Status = brief.Status
	}

	// 补充 inspect 数据（健康状态、启动时间；brief 为空即详情路径，需从 inspect 回填 State）。
	// 注意：详情路径只回填 State（机器态），不回填 Status——列表路径的 Status 是 docker 可读描述
	//（如 "Up 5 minutes"），详情路径无 brief 拿不到该描述，若回填 info.State.Status（机器态 "running"）
	// 会导致同一服务在列表与详情显示不同文案。前端详情面板用 `status || state` 兜底展示。
	if info, ierr := s.docker.InspectContainer(ctx, svc.ContainerName); ierr == nil && info.State != nil {
		if brief == nil {
			v.State = info.State.Status // 详情接口未传 brief，需从 inspect 回填，否则 State 恒为 not_created
		}
		v.Health = healthStatus(info.State.Health)
		v.StartedAt = info.State.StartedAt
		v.ExitCode = info.State.ExitCode
	}

	// 资产（一次 Get，同时判定 Managed + StartMethod）
	if a, aerr := s.assetSt.Get(ctx, name); aerr == nil {
		v.StartMethod = a.StartMethod
		v.Managed = a.StartMethod != "external" && a.AutoManaged == 1
	} else {
		v.Managed = DefaultManagedSet(s.project)[name]
	}

	return v
}

// healthStatus 从 docker HealthInfo 提取状态字符串。
func healthStatus(h *dockerapi.HealthInfo) string {
	if h == nil {
		return ""
	}
	return h.Status
}

// 服务端脱敏（统一工具）：读接口仅 requireAuth（任意已登录用户），前端掩码无法阻止直接读响应，
// 故 env 与 logs 都必须在服务端脱敏。敏感词作为子串匹配（命中 API_KEY/DB_PASSWORD 等复合键）；
// 仅 auth 需独立成词（按非字母数字分词后精确匹配），避免误伤 OAUTH_*/AUTHORITY；
// 值形态判定命中 ://user:pass@ 凭据串。
var sensitiveSubstrRe = regexp.MustCompile(`(?i)(password|passwd|secret|token|api_?key|private_?key|credential|dsn)`)
var credValueRe = regexp.MustCompile(`://[^:@/]+:[^@/]+@`)

// GetServiceDetail 返回服务详情（日志 + compose 配置 + 环境变量 + 版本历史）。
func (s *ServiceOps) GetServiceDetail(ctx context.Context, name string, logTail int) (*ServiceDetail, error) {
	svc, ok := s.project.Services[name]
	if !ok {
		return nil, fmt.Errorf("%w: compose 服务 %q 不存在", ErrServiceNotFound, name)
	}
	if logTail <= 0 {
		logTail = 200
	}

	d := &ServiceDetail{
		ComposeFile: svc.File,
		Environment: redactEnv(svc.Environment), // 服务端脱敏，避免字面量密钥（如 postiz 的 X_API_SECRET）明文下发
		Volumes:     svc.Volumes,
		Networks:    svc.Networks,
		Restart:     svc.Restart,
		HealthCheck: svc.HealthCheck,
		Versions:    []VersionEntry{},
	}

	// 基础视图（单次 buildView，不再跑完整 ListServices）
	d.ServiceView = s.buildView(ctx, name, nil)

	// 日志（Tty 容器返回未多路复用流，需按 Config.Tty 区分解析）
	if d.State != "not_created" {
		tty := false
		if full, ierr := s.docker.InspectContainerFull(ctx, svc.ContainerName); ierr == nil {
			if t, ok := full.Config["Tty"].(bool); ok {
				tty = t
			}
		}
		if logs, lerr := s.docker.ContainerLogs(ctx, svc.ContainerName, logTail, tty); lerr == nil {
			d.Logs = redactLogs(logs) // 服务端脱敏：日志常含 DSN/Token，读接口仅 requireAuth
		}
	}

	// 版本历史（skill_versions 表，按 container 名匹配）
	s.loadVersions(ctx, name, d)

	// 资产画像
	if a, aerr := s.assetSt.Get(ctx, name); aerr == nil {
		d.AssetProfile = a.Profile
	}

	return d, nil
}

// RestartService 重启服务（docker restart）。
func (s *ServiceOps) RestartService(ctx context.Context, name, actor string) error {
	svc, err := s.managedService(ctx, name)
	if err != nil {
		return err
	}
	return s.withServiceLock(ctx, svc.ContainerName, func(ctx context.Context) error {
		if err := s.docker.RestartContainer(ctx, svc.ContainerName, 30); err != nil {
			return fmt.Errorf("restart %s: %w", name, err)
		}
		s.audit(ctx, actor, "service_restart", name, nil)
		return nil
	})
}

// StopService 停止服务（docker stop；镜像与数据保留，可再启动）。
func (s *ServiceOps) StopService(ctx context.Context, name, actor string) error {
	svc, err := s.managedService(ctx, name)
	if err != nil {
		return err
	}
	return s.withServiceLock(ctx, svc.ContainerName, func(ctx context.Context) error {
		if err := s.docker.StopContainer(ctx, svc.ContainerName, 30); err != nil {
			return fmt.Errorf("stop %s: %w", name, err)
		}
		s.audit(ctx, actor, "service_stop", name, nil)
		return nil
	})
}

// StartService 启动已停止的服务（docker start）。
func (s *ServiceOps) StartService(ctx context.Context, name, actor string) error {
	svc, err := s.managedService(ctx, name)
	if err != nil {
		return err
	}
	return s.withServiceLock(ctx, svc.ContainerName, func(ctx context.Context) error {
		if err := s.docker.StartContainer(ctx, svc.ContainerName); err != nil {
			return fmt.Errorf("start %s: %w", name, err)
		}
		s.audit(ctx, actor, "service_start", name, nil)
		return nil
	})
}

// withServiceLock 按容器名加 per-service 写互斥锁，串行化同一容器的全部写操作
// （start/stop/restart/rollback/params/recreate），防并发写操作在重建中间态互相干扰。
func (s *ServiceOps) withServiceLock(ctx context.Context, containerName string, fn func(context.Context) error) error {
	muIface, _ := s.writeMu.LoadOrStore(containerName, &sync.Mutex{})
	mu := muIface.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	return fn(ctx)
}

// RollbackService 回滚到指定镜像版本（安全重建：先建新 → 启动成功 → 删旧 → rename）。
func (s *ServiceOps) RollbackService(ctx context.Context, name, oldImage, actor string) error {
	svc, err := s.managedService(ctx, name)
	if err != nil {
		return err
	}
	if oldImage == "" {
		return fmt.Errorf("%w: 必须指定回滚目标镜像（image）", ErrInvalidRequest)
	}
	// 前置校验：镜像本地存在（Engine API create 不会自动 pull）
	exists, ierr := s.docker.ImageExists(ctx, oldImage)
	if ierr != nil {
		return fmt.Errorf("检查镜像 %s 是否存在: %w", oldImage, ierr)
	}
	if !exists {
		return fmt.Errorf("%w: 镜像 %s 本地不存在（Engine API 不会自动 pull，请先 docker pull）", ErrInvalidRequest, oldImage)
	}
	return s.withServiceLock(ctx, svc.ContainerName, func(ctx context.Context) error {
		if err := s.recreate(ctx, svc, oldImage, nil, nil, actor); err != nil {
			return fmt.Errorf("rollback %s → %s: %w", name, oldImage, err)
		}
		s.audit(ctx, actor, "service_rollback", name, map[string]any{"image": oldImage})
		return nil
	})
}

// UpdateServiceParams 修改启动参数（env / 端口）并重建容器生效。
//
// env 基线 = 当前容器 inspect 的 Config["Env"]（docker compose CLI 已做 ${VAR} 插值的
// 运行时值），仅做增量覆盖——不重读 compose 原始值（其含未插值的 ${...} 字面量，
// 直接注入会写坏服务）。upd.Env == nil 时保持原 Env 不动（只改端口）。
//
// 注意：API 容器仓库只读，修改通过容器重建实现（运行时生效）；
// 持久化需同步修改 compose 文件 / 根 .env（deployctl 或人工），避免与 auto-update 冲突。
func (s *ServiceOps) UpdateServiceParams(ctx context.Context, name string, upd ParamUpdate, actor string) error {
	svc, err := s.managedService(ctx, name)
	if err != nil {
		return err
	}
	if upd.Env == nil && upd.Ports == nil {
		return fmt.Errorf("%w: env 和 ports 至少改一项", ErrInvalidRequest)
	}

	// env：nil = 不改（recreate 保持原 Env）；非 nil = 以运行时 Env 为基线做增量覆盖
	var env []string
	if upd.Env != nil {
		base, eerr := s.currentEnv(ctx, svc.ContainerName)
		if eerr != nil {
			// 取基线失败不能退化为空基线（会丢失原有 DB 口令/Token 等 env），直接终止改参
			return fmt.Errorf("%w: %v", ErrInvalidRequest, eerr)
		}
		for k, v := range upd.Env {
			base[k] = v
		}
		env = make([]string, 0, len(base))
		keys := make([]string, 0, len(base))
		for k := range base {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			env = append(env, k+"="+base[k])
		}
	}

	// 端口（解析失败 → 400）
	var portBindings map[string][]dockerapi.PortBinding
	if upd.Ports != nil {
		var perr error
		portBindings, perr = parsePortBindings(upd.Ports)
		if perr != nil {
			return fmt.Errorf("%w: %v", ErrInvalidRequest, perr)
		}
	}

	return s.withServiceLock(ctx, svc.ContainerName, func(ctx context.Context) error {
		if err := s.recreate(ctx, svc, "", env, portBindings, actor); err != nil {
			return fmt.Errorf("update params %s: %w", name, err)
		}
		// 审计：env 只记键名（不落值，env 常含口令/Token，audit_logs 180 天不可删）
		envKeys := make([]string, 0, len(upd.Env))
		for k := range upd.Env {
			envKeys = append(envKeys, k)
		}
		sort.Strings(envKeys)
		s.audit(ctx, actor, "service_update_params", name, map[string]any{
			"env_keys": envKeys,
			"ports":    upd.Ports,
			"reason":   upd.Reason,
		})
		return nil
	})
}

// ---- 内部方法 ----

// managedService 取纳管的 compose 服务；不存在 → ErrServiceNotFound，不纳管 → ErrNotManaged，
// 目标容器 == 自身容器 → ErrSelfOperation（stop/recreate 自身会终止本进程，必须拒绝）。
func (s *ServiceOps) managedService(ctx context.Context, name string) (*Service, error) {
	svc, ok := s.project.Services[name]
	if !ok {
		return nil, fmt.Errorf("%w: compose 服务 %q 不存在", ErrServiceNotFound, name)
	}
	if !s.isManaged(ctx, name) {
		return nil, fmt.Errorf("%w: 服务 %q 不纳管（external），不可执行写操作", ErrNotManaged, name)
	}
	if s.isSelfContainer(ctx, svc.ContainerName) {
		return nil, fmt.Errorf("%w: 服务 %q 即本 API 自身容器，stop/restart/recreate 会终止本进程，请改用宿主机 deployctl 操作", ErrSelfOperation, name)
	}
	return svc, nil
}

// isSelfContainer 判定容器是否为本 API 自身容器。
// 容器内 hostname = 容器 ID 前缀（12 位 hex）；inspect 目标容器取其 ID，
// 比对 ID 前缀是否等于 hostname（或 hostname 前缀等于 ID 前缀）。
func (s *ServiceOps) isSelfContainer(ctx context.Context, containerName string) bool {
	if s.selfHost == "" || containerName == "" {
		return false
	}
	info, err := s.docker.InspectContainer(ctx, containerName)
	if err != nil || info.ID == "" {
		return false
	}
	// hostname 是 ID 前缀（≤12 位）；ID 是完整 64 位。取两者较短前缀比对。
	n := len(s.selfHost)
	if n > len(info.ID) {
		n = len(info.ID)
	}
	return strings.HasPrefix(info.ID, s.selfHost[:n]) || strings.HasPrefix(s.selfHost, info.ID[:n])
}

// isManaged 判定服务是否纳管（透传调用方 ctx，保留取消/超时/追踪）。
func (s *ServiceOps) isManaged(ctx context.Context, name string) bool {
	if a, err := s.assetSt.Get(ctx, name); err == nil {
		return a.StartMethod != "external" && a.AutoManaged == 1
	}
	// 资产表无记录时，用 compose 默认纳管集（排除 external 红线）
	return DefaultManagedSet(s.project)[name]
}

// currentEnv 取当前容器运行时 Env（inspect Config["Env"]，已插值）为 map。
// inspect 失败时返回 error（调用方必须终止改参，不能退化为空基线——否则会丢失原有 env）。
func (s *ServiceOps) currentEnv(ctx context.Context, containerName string) (map[string]string, error) {
	out := map[string]string{}
	full, err := s.docker.InspectContainerFull(ctx, containerName)
	if err != nil {
		return nil, fmt.Errorf("inspect %s 取运行时 env 失败: %w", containerName, err)
	}
	raw, ok := full.Config["Env"].([]any)
	if !ok {
		return out, nil // 容器无 Env（合法：全走 env_file）
	}
	for _, e := range raw {
		str, ok := e.(string)
		if !ok {
			continue
		}
		if k, v, found := strings.Cut(str, "="); found {
			out[k] = v
		}
	}
	return out, nil
}

// audit 写审计日志。用脱离取消的上下文：长耗时写操作（重建最长约 60s）期间客户端断开/代理超时
// 会取消请求 ctx，若审计绑定该 ctx 则 Create 立即失败——「全部写操作留痕」红线在破坏性半成功路径上失效。
func (s *ServiceOps) audit(ctx context.Context, actor, action, target string, detail any) {
	var detailJSON string
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			detailJSON = string(b)
		}
	}
	if err := s.database.WithContext(context.WithoutCancel(ctx)).Create(&db.AuditLog{
		Actor: actor, Action: action, Target: target, Detail: detailJSON,
	}).Error; err != nil {
		log.Printf("ERROR: write audit_log (%s %s %s): %v", actor, action, target, err)
	}
}

// loadVersions 从 skill_versions 表加载版本历史（按 container 名 / skill 名直接查询，避免全表扫描）。
func (s *ServiceOps) loadVersions(ctx context.Context, serviceName string, d *ServiceDetail) {
	var sk db.Skill
	if err := s.database.WithContext(ctx).
		Where("container = ? OR name = ?", serviceName, serviceName).
		First(&sk).Error; err != nil {
		return // 无匹配 skill（非技能服务），无版本历史
	}
	var versions []db.SkillVersion
	s.database.WithContext(ctx).Where("skill_id = ?", sk.ID).Order("id DESC").Find(&versions)
	for _, v := range versions {
		d.Versions = append(d.Versions, VersionEntry{
			Version:   v.Version,
			Image:     v.Image,
			Status:    v.Status,
			CreatedAt: v.CreatedAt.Format(time.RFC3339),
		})
	}
}
