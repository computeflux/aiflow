// Package main is the evolve-driver binary.
//
// evolve-driver：S1 进化执行链编排（docs/plan/S1-进化闭环.md §3.8）。
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
	"github.com/wetee/ai-workflow/internal/approval"
	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/dockerapi"
	"github.com/wetee/ai-workflow/internal/notify"
)

// Config 是本进程的运行时配置（从环境变量 / .env 加载）。
type Config struct {
	// MySQL
	MySQLHost     string `env:"MYSQL_HOST,required"`
	MySQLPort     string `env:"MYSQL_PORT,required"`
	MySQLUser     string `env:"MYSQL_USER,required"`
	MySQLPassword string `env:"MYSQL_PASSWORD,required"`
	MySQLDatabase string `env:"MYSQL_DATABASE,required"`

	// 仓库 / 进化工作区
	RepoDir        string `env:"REPO_DIR" envDefault:"/repo"`
	EvolveCacheDir string `env:"EVOLVE_CACHE_DIR" envDefault:"cache/evolve"` // 相对 REPO_DIR（容器内 /repo/... 与 pi-web /work/... 同宿主目录）
	BaseBranch     string `env:"EVOLVE_BASE_BRANCH" envDefault:"main"`
	PollSec        int    `env:"EVOLVE_POLL_SEC" envDefault:"60"`

	// 执行器
	DockerHost       string `env:"DOCKER_HOST" envDefault:"unix:///run/docker.sock"`
	SandboxRunnerURL string `env:"SANDBOX_RUNNER_URL" envDefault:"http://sandbox-runner:8123"`
	CIRunnerImage    string `env:"CI_RUNNER_IMAGE" envDefault:"computeflux/ci-runner:latest"`
	CIVolume         string `env:"CI_CACHE_VOLUME" envDefault:"workflow_ci-cache"`
	// 宿主仓库根（与 sandbox-runner 同一约定）：CI 容器经**宿主 docker daemon** 起，
	// bind 挂载源必须是宿主路径，不能用容器内 /repo/...（2026-09-30 实测 HTTP 400）。
	HostRepo string `env:"SANDBOX_REPO_PREFIX" envDefault:"/work"`

	// 绑定回退（agent_bindings 缺失时）
	AgentBindingCoder    string `env:"AGENT_BINDING_CODER" envDefault:"pi"`
	AgentBindingReviewer string `env:"AGENT_BINDING_REVIEWER" envDefault:"ocr"`

	ApprovalExpireHours int `env:"APPROVAL_EXPIRE_HOURS" envDefault:"24"`

	// 写范围放宽（2026-10-08）：diff 允许**小范围越界**（非 apps/* 或 apps/board* 的文件）——
	// 同时满足「越界文件数 ≤ ScopeAllowFiles」且「越界改动行数 ≤ ScopeAllowLines」时放行
	//（仍经 reviewer + 人工审批双重把关），超阈值才打回人工 PR（红线：大范围核心改动只走人工）。
	// 置 0 = 一律不允许越界（退回旧行为）。
	ScopeAllowFiles int `env:"EVOLVE_SCOPE_ALLOW_FILES" envDefault:"2"`
	ScopeAllowLines int `env:"EVOLVE_SCOPE_ALLOW_LINES" envDefault:"50"`

	// 目标镜像构建（2026-10-08）：灰度要跑真新代码——CI 全绿后从 worktree 构建 <app>:<version>。
	// 用 deployctl 镜像（docker CLI + compose）一次性容器挂宿主 docker.sock + worktree 跑 docker build。
	BuilderImage string `env:"EVOLVE_BUILDER_IMAGE" envDefault:"computeflux/deployctl:latest"`

	// 发布跟进（2026-10-08）：local（默认）= released 后本地快进 main（无需 token）；
	// pr = 推分支 + 建 GitHub PR；both = 两者；off = 不做（人工）。
	MergeMode string `env:"EVOLVE_MERGE_MODE" envDefault:"local"`
	// MergeRetrySec：base（main）有未提交改动 → 暂停合入后的重试检查间隔（秒）。
	// 期间静默跳过（不提交/不合并/不刷 audit），到点再查一次 base 工作区是否干净。
	MergeRetrySec int `env:"EVOLVE_MERGE_RETRY_SEC" envDefault:"300"`

	// 陈旧单回收：运行态（coding/reviewing/sandboxing/testing）超过该秒数未更新 → 重置 open 重跑
	//（进程中途死亡：重启/OOM。approving 不回收——人工审批合法长时，过期由 api TickExpiry 驱动）。
	StaleSec int `env:"EVOLVE_TASK_STALE_SEC" envDefault:"7200"`

	// worktree 兜底清理：独立 ticker 每 CleanScanSec 秒（默认 1800 = 30 分钟）扫一次
	// cache/evolve/，对「分支已合并进 main 且非进行中且无未提交改动」的目录执行清理
	//（git worktree remove + 删目录 + audit 留痕）。合并成功路径已即时清理，这里是兜底
	//（进程崩溃/历史遗留/漏网目录）。置 0 = 关闭周期扫描。
	CleanScanSec int `env:"EVOLVE_CLEAN_SCAN_SEC" envDefault:"1800"`

	// coder→reviewer 最大轮次：reviewer 发现 error 级评论 → 把评论反馈给 coder 重改 → 再 review，
	// 直到通过或达到上限（超限转人工）。默认 10 轮（1 初始 + 9 修复）。
	MaxRounds int `env:"EVOLVE_MAX_ROUNDS" envDefault:"10"`

	HealthPort string `env:"EVOLVE_HEALTH_PORT" envDefault:"8121"`

	// Coder 执行器（经 pi-web HTTP API 调用，对话记录在 pi-web UI 可见）
	CoderAdapterPath string `env:"CODER_ADAPTER_PATH" envDefault:"/bin/agent-pi"`
	CoderHostHome    string `env:"CODER_HOST_HOME" envDefault:"/host"`
	CoderWebURL      string `env:"CODER_WEB_URL" envDefault:"http://pi-web:30141"`
	CoderWebPassword string `env:"CODER_WEB_PASSWORD" envDefault:""`
	CoderProvider    string `env:"CODER_PROVIDER" envDefault:"local"`
	CoderModel       string `env:"CODER_MODEL" envDefault:"Qwen3.8-27B"`
	// CoderAPIKey：模型管理（S8.1）同步进 pi-web models.json 时的兜底 key
	//（档案未填 api_key 时用它；与 CODER_PROVIDER/CODER_MODEL 同源 .env）。
	CoderAPIKey string `env:"CODER_API_KEY" envDefault:""`

	// Reviewer 执行器（同上，宿主执行）
	ReviewerAdapterPath string `env:"REVIEWER_ADAPTER_PATH" envDefault:"/bin/agent-ocr"`
	ReviewerHostHome    string `env:"REVIEWER_HOST_HOME" envDefault:"/host"`
	ReviewerProvider    string `env:"REVIEWER_PROVIDER" envDefault:"local"`
	ReviewerModel       string `env:"REVIEWER_MODEL" envDefault:""`
	ReviewerAPIKey      string `env:"REVIEWER_API_KEY" envDefault:""`
	// ReviewerRetries：reviewer **执行失败**（provider/HTTP/超时/空输出/schema 漂移）时的重试次数（不含首次）。
	// 口径：reviewer 不可用 ≠ 代码被拒（2026-10-25：OCR provider 400 曾直接把任务 cancel 掉）。
	ReviewerRetries int `env:"REVIEWER_RETRIES" envDefault:"3"`
	// ReviewerRetryBackoffSec：reviewer 重试首次退避秒数（之后翻倍：15s → 30s → 60s）。
	ReviewerRetryBackoffSec int `env:"REVIEWER_RETRY_BACKOFF_SEC" envDefault:"15"`

	// ---- 合并冲突自动解决（2026-10-31）----
	// 把 base（main）合入 evolve 分支时若产生**文本**冲突，用 LLM 逐文件合并（保留双方意图），
	// 校验无冲突标记后提交合并提交；失败/超限则照旧中止交人工（绝不静默取舍）。
	AutoResolveConflicts bool `env:"EVOLVE_AUTO_RESOLVE_CONFLICTS" envDefault:"true"`
	// ConflictMaxFiles 单次最多自动解决多少个冲突文件（超过 → 直接交人工，避免大范围误改）。
	ConflictMaxFiles int `env:"EVOLVE_CONFLICT_MAX_FILES" envDefault:"5"`
	// ConflictTimeoutSec 单个冲突文件的解决超时（秒）。
	// 本地 27B 模型重写 60KB / 600 行的文件要数分钟，3 分钟会直接 deadline exceeded
	// （2026-10-31 实测：apps/core-ui/src/kit/api.ts 连续三次超时）。
	ConflictTimeoutSec int `env:"EVOLVE_CONFLICT_TIMEOUT_SEC" envDefault:"900"`
	// ConflictLLM* 与 ai-workflow 主服务同一套 OpenAI 兼容配置（根 .env）。
	ConflictLLMAPIKey  string `env:"OPENAI_API_KEY" envDefault:""`
	ConflictLLMBaseURL string `env:"OPENAI_BASE_URL" envDefault:""`
	ConflictLLMModel   string `env:"LLM_MODEL" envDefault:""`
}

// ---- adapter 输出 JSON Schema（协议漂移立即失败，§3.8 步骤 2/4）

var coderSchema = compileSchema(map[string]any{
	"type":     "object",
	"required": []string{"ok"},
	"properties": map[string]any{
		"ok":            map[string]any{"type": "boolean"},
		"diff":          map[string]any{"type": "string"},
		"files_changed": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"session_id":    map[string]any{"type": "string"},
	},
})

var reviewerSchema = compileSchema(map[string]any{
	"type":     "object",
	"required": []string{"ok"},
	"properties": map[string]any{
		"ok": map[string]any{"type": "boolean"},
		"comments": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":     "object",
				"required": []string{"level", "message"},
				"properties": map[string]any{
					"file":    map[string]any{"type": "string"},
					"line":    map[string]any{"type": "integer"},
					"level":   map[string]any{"enum": []string{"error", "warning", "info"}},
					"message": map[string]any{"type": "string"},
				},
			},
		},
	},
})

// dataURLLoader：jsonschema v6 默认 loader 仅支持 file scheme，
// adapter 输出 Schema 以 data: URL 内嵌在代码中，需自定义加载器。
type dataURLLoader struct{}

func main() {
	_ = godotenv.Load()
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	database, err := openDB(cfg)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	apvCfg := &approval.Config{}
	if err := env.Parse(apvCfg); err != nil {
		log.Fatalf("failed to load approval config: %v", err)
	}
	// evolve-driver 不处理 workflow_signal 审批单（S2 桥在 api 侧），无 Signal 能力
	store := approval.New(database, apvCfg, notify.New(), nil, nil)
	if err := store.EnsureSchema(); err != nil {
		log.Fatalf("ensure approval schema: %v", err)
	}
	docker, err := dockerapi.New(cfg.DockerHost)
	if err != nil {
		log.Fatalf("docker client: %v", err)
	}
	d := &driver{db: database, store: store, docker: docker, cfg: cfg, notify: notify.New()}
	d.ensureEvolveColumns() // 幂等补列 pr_url/pr_error（旧库升级）
	d.prEnabled = strings.TrimSpace(os.Getenv("GITHUB_TOKEN")) != ""
	switch cfg.MergeMode {
	case "local":
		log.Printf("发布跟进：local 模式（released 后本地快进 main，不碰工作区）")
	case "pr", "both":
		if d.prEnabled {
			log.Printf("发布跟进：%s 模式（自动 PR 已启用）", cfg.MergeMode)
		} else {
			log.Printf("发布跟进：%s 模式，但 GITHUB_TOKEN 未配置 → PR 跳过", cfg.MergeMode)
		}
	default:
		log.Printf("发布跟进：%s（不做自动合并/PR）", cfg.MergeMode)
	}

	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintln(w, "ok")
		})
		log.Printf("evolve-driver health on 127.0.0.1:%s", cfg.HealthPort)
		log.Fatal(http.ListenAndServe("127.0.0.1:"+cfg.HealthPort, mux))
	}()

	log.Printf("evolve-driver started: repo=%s cache=%s poll=%ds base=%s",
		cfg.RepoDir, cfg.EvolveCacheDir, cfg.PollSec, cfg.BaseBranch)
	d.recoverInterruptedTasks() // 启动恢复：上次中断的运行态任务立即重置 open（不等 StaleSec 阈值）
	d.cleanMergedWorktrees()    // 启动一次性兜底清理：历史积压的已合并 worktree 目录立即清（不等首个 30 分钟周期）

	// S8.1 模型目录同步：把 model_profiles（sync_agents=1）写进 pi-web/ocr 的宿主
	// 模型配置（$CODER_HOST_HOME/.pi/agent/models.json + settings.json、
	// ~/.opencodereview/config.json）。board 点「应用」即时推一次（api 侧），这里是
	// 5 分钟兜底 ticker（api 重启丢推送/离线改库时的收敛保障）+ 启动时一次。
	go func() {
		syncAgents := func() {
			res, err := store.SyncAgentsToFrameworks()
			if err != nil {
				log.Printf("WARN: model-config: sync agent frameworks: %v", err)
				return
			}
			for k, v := range res {
				log.Printf("model-config: sync %s → %s", k, v)
			}
		}
		syncAgents()
		t := time.NewTicker(5 * time.Minute)
		for range t.C {
			syncAgents()
		}
	}()

	// 发布跟进 / 陈旧回收独立 ticker：
	// runTask（coder → reviewer → 沙箱 → CI）可能跑数小时，不能跟它在同一循环里串行——
	// 否则已审批通过（released）的改动的合并要等**当前任务跑完**才执行（卡 #97 实测：
	// 审批通过后 10 小时仍未合并，因为 driver 卡在另一个长任务里）。
	// 两者操作不相交：mergeToMain 只动 ref + 各任务自己的 worktree；被领取的任务都是 open 态。
	go func() {
		t := time.NewTicker(time.Duration(cfg.PollSec) * time.Second)
		for range t.C {
			d.maybePublish()   // released → 本地快进 main / 已审批 → 推分支建 PR（幂等）
			d.reapStaleTasks() // 陈旧单回收（进程中途死亡 → 重置 open 重跑）
		}
	}()

	// worktree 兜底清理独立 ticker（EVOLVE_CLEAN_SCAN_SEC，默认 30 分钟）：
	// 与 60s 主循环解耦——扫描要跑 rev-parse/merge-base/status 等 git 调用，
	// 不能每分钟刷一次（额外 git 开销 + 日志噪声）。
	if cfg.CleanScanSec > 0 {
		go func() {
			t := time.NewTicker(time.Duration(cfg.CleanScanSec) * time.Second)
			for range t.C {
				d.cleanMergedWorktrees()
			}
		}()
	}

	d.tick()
	ticker := time.NewTicker(time.Duration(cfg.PollSec) * time.Second)
	for range ticker.C {
		d.tick() // 串行：上一轮未结束前不拾取新单（max_concurrency=1）
	}
}

type driver struct {
	db        *db.DB
	store     *approval.Store
	docker    *dockerapi.Client
	cfg       *Config
	notify    notify.Notifier
	prEnabled bool // GITHUB_TOKEN 已配置（自动 PR 开）

	// running 是当前正在执行的进化单号（runTask 全程持有）。
	// 陈旧回收/发布跟进已拆到独立协程，不再被 runTask 阻塞（否则 released → mergeToMain 被馔死），
	// 但因此必须显式避开“正在跑的那个任务”：coder/reviewer 单阶段可能数小时不写库，
	// 光看 updated_at 会把正在跑的任务误判为陈旧而重置。
	running atomic.Value // string

	// 合入闸门：base（main）有未提交改动时「暂停合并」，每 MergeRetrySec 秒重试检查一次。
	// mergeDeferUntil：>now 表示仍在暂缓期（unix nano；0 = 未暂停）。
	// mergeDeferAudited：本轮暂缓是否已写过 audit/日志（避免每个检查周期重复刷）。
	mergeDeferUntil   atomic.Int64
	mergeDeferAudited atomic.Bool

	// mergeBlockAudited：「上一个任务未合入 main → 阻塞新任务」是否已写过 audit/日志（避免每轮刷）。
	mergeBlockAudited atomic.Bool
}
