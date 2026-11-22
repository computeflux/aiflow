// Package main is the knowledge-sync binary.
//
// knowledge-sync：S7 产品信息自动汇聚进程（docs/plan/S7-产品情报.md §3.1）。
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
	"github.com/wetee/ai-workflow/internal/llm"
	"github.com/wetee/ai-workflow/internal/product"
	"github.com/wetee/ai-workflow/internal/qdrant"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Config 是本进程的运行时配置（从环境变量 / .env 加载）。
type Config struct {
	// MySQL
	MySQLHost     string `env:"MYSQL_HOST,required"`
	MySQLPort     string `env:"MYSQL_PORT,required"`
	MySQLUser     string `env:"MYSQL_USER,required"`
	MySQLPassword string `env:"MYSQL_PASSWORD,required"`
	MySQLDatabase string `env:"MYSQL_DATABASE,required"`

	// Qdrant
	QdrantHost       string `env:"QDRANT_HOST,required"`
	QdrantGRPCPort   int    `env:"QDRANT_GRPC_PORT,required"`
	QdrantCollection string `env:"QDRANT_COLLECTION,required"`

	// LLM（嵌入；不可用 → 跳过入库）
	OpenAIAPIKey   string `env:"OPENAI_API_KEY" envDefault:""`
	OpenAIBaseURL  string `env:"OPENAI_BASE_URL" envDefault:""`
	EmbeddingModel string `env:"EMBEDDING_MODEL" envDefault:""`

	// 同步
	KnowledgeDir string `env:"KNOWLEDGE_INTERNAL_DIR" envDefault:"docs/,apps/*/README.md,/meetings"`
	RepoRoot     string `env:"OPS_REPORT_REPO_ROOT" envDefault:"/work"`
	DefaultConf  string `env:"KNOWLEDGE_DEFAULT_CONFIDENTIALITY" envDefault:"internal"`
	RunOnStartup bool   `env:"KNOWLEDGE_SYNC_RUN_ON_STARTUP" envDefault:"true"`
	HealthPort   string `env:"KNOWLEDGE_SYNC_HEALTH_PORT" envDefault:"8129"`
}

// SyncResult 是一轮同步的统计。
type SyncResult struct {
	Scanned  int
	Ingested int
	Skipped  int
	Errors   []string
}

func main() {
	_ = godotenv.Load()
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	gormDB, err := openDB(cfg)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	store := product.NewStore(gormDB)
	if err := store.EnsureSchema(); err != nil {
		log.Fatalf("ensure product schema: %v", err)
	}
	qc, err := qdrant.NewClient(cfg.QdrantHost, cfg.QdrantGRPCPort, cfg.QdrantCollection)
	if err != nil {
		log.Fatalf("create qdrant client: %v", err)
	}
	qctx, qcancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := qc.EnsureCollection(qctx); err != nil {
		log.Printf("WARN: ensure qdrant collection: %v (continuing)", err)
	}
	qcancel()

	var llmClient *llm.Client
	if cfg.OpenAIAPIKey != "" {
		llmClient = llm.NewClient(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, cfg.EmbeddingModel, "")
	}

	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
		log.Printf("knowledge-sync health on 127.0.0.1:%s", cfg.HealthPort)
		log.Fatal(http.ListenAndServe("127.0.0.1:"+cfg.HealthPort, mux))
	}()

	log.Printf("knowledge-sync started: dirs=%q llm=%v default_conf=%s run_on_startup=%v",
		cfg.KnowledgeDir, llmClient != nil, cfg.DefaultConf, cfg.RunOnStartup)

	if cfg.RunOnStartup {
		runAll(cfg, gormDB, store, qc, llmClient)
	}
	for {
		next := next2AM(time.Now())
		time.Sleep(time.Until(next))
		runAll(cfg, gormDB, store, qc, llmClient)
	}
}

func runAll(cfg *Config, database *gorm.DB, store *product.Store, qc *qdrant.Client, llmClient *llm.Client) {
	ctx := context.Background()
	res := scanAndSync(ctx, cfg, store, qc, llmClient)
	log.Printf("knowledge-sync: scanned=%d ingested=%d skipped=%d errors=%d",
		res.Scanned, res.Ingested, res.Skipped, len(res.Errors))
	for _, e := range res.Errors {
		log.Printf("  WARN: %s", e)
	}
	appendOpsNote(ctx, database, res)
}

// scanAndSync 扫描 KNOWLEDGE_INTERNAL_DIR → hash diff → 增量入库。
func scanAndSync(ctx context.Context, cfg *Config, store *product.Store, qc *qdrant.Client, llmClient *llm.Client) SyncResult {
	res := SyncResult{}
	dirs := splitDirs(cfg.KnowledgeDir)
	for _, dir := range dirs {
		pattern := dir
		if !strings.HasPrefix(pattern, "/") {
			pattern = filepath.Join(cfg.RepoRoot, pattern)
		}
		matches, err := filepath.Glob(pattern)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("glob %s: %v", pattern, err))
			continue
		}
		for _, f := range matches {
			fi, err := os.Stat(f)
			if err != nil {
				continue
			}
			if fi.IsDir() {
				// 目录型条目（如 docs/）：glob 只匹配到目录本身，需递归遍历找 .md/.txt
				_ = filepath.WalkDir(f, func(p string, d os.DirEntry, werr error) error {
					if werr != nil || d.IsDir() {
						return nil
					}
					processFile(ctx, p, cfg, store, qc, llmClient, &res)
					return nil
				})
				continue
			}
			processFile(ctx, f, cfg, store, qc, llmClient, &res)
		}
	}
	return res
}

// processFile 读单个知识文件 → hash diff → 增量入库（扫描器与递归遍历共用）。
func processFile(ctx context.Context, f string, cfg *Config, store *product.Store, qc *qdrant.Client, llmClient *llm.Client, res *SyncResult) {
	if !isKnowledgeFile(f) {
		return
	}
	res.Scanned++
	data, err := os.ReadFile(f)
	if err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("read %s: %v", f, err))
		return
	}
	content := string(data)
	hash := product.ContentHash(content)
	// hash diff：内容未变则跳过
	if existing, gerr := store.KnowledgeDoc(ctx, f); gerr == nil && existing.ContentHash == hash {
		res.Skipped++
		return
	}
	if _, ierr := product.IngestInternal(ctx, store, qc, llmClient, product.IngestInput{
		SourceType:      inferSourceType(f),
		Content:         content,
		DocTitle:        filepath.Base(f),
		Confidentiality: cfg.DefaultConf,
	}); ierr != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("ingest %s: %v", f, ierr))
	} else {
		res.Ingested++
	}
}

// appendOpsNote 把同步结果附注到当日 ops_reports（type=daily，仅追加不覆盖；无当日日报则跳过）。
func appendOpsNote(ctx context.Context, database *gorm.DB, res SyncResult) {
	today := time.Now().Format("2006-01-02")
	var n int64
	database.Model(&struct{ ID uint }{}).Table("ops_reports").
		Where("report_date = ? AND type = ?", today, "daily").Count(&n)
	if n == 0 {
		return // 无当日日报（ops-report 未产出）→ 不新建，避免覆盖
	}
	note := fmt.Sprintf("\n\n## 知识同步（knowledge-sync %s）\n\n- 扫描 %d 个文件，新增/更新入库 %d，跳过（未变）%d，错误 %d\n",
		time.Now().Format("15:04:05"), res.Scanned, res.Ingested, res.Skipped, len(res.Errors))
	res2 := database.Table("ops_reports").Where("report_date = ? AND type = ?", today, "daily").
		UpdateColumn("content", gorm.Expr("CONCAT(content, ?)", note))
	if res2.Error != nil {
		log.Printf("WARN: append ops note: %v", res2.Error)
	}
}

func splitDirs(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// isKnowledgeFile 只同步文本知识文档（.md / .txt）。
func isKnowledgeFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".txt", ".markdown":
		return true
	}
	return false
}

// inferSourceType 按路径推断 source_type（会议纪要目录 → meeting_minutes，其余 → tech_doc）。
func inferSourceType(path string) string {
	if strings.Contains(strings.ToLower(path), "meeting") {
		return "meeting_minutes"
	}
	return "tech_doc"
}

// next2AM 返回下一个本地时间 02:00。
func next2AM(now time.Time) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), 2, 0, 0, 0, now.Location())
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

func openDB(cfg *Config) (*gorm.DB, error) {
	gormDB, err := gorm.Open(mysql.Open(fmt.Sprintf(
		"%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s",
		cfg.MySQLUser, cfg.MySQLPassword, cfg.MySQLHost, cfg.MySQLPort, cfg.MySQLDatabase,
	)), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	sqlDB, err := gormDB.DB()
	if err != nil {
		return nil, fmt.Errorf("get sql db: %w", err)
	}
	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(3 * time.Minute)
	return gormDB, nil
}
