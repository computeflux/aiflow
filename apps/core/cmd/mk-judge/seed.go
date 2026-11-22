// seed.go — mk-judge -seed 子命令：种子样本跑批 + markdown 报告（MK-18）。
//
// 读 testdata/seed 下的样本（10 intl + 10 cn，MK-40 提供；读取约定 =
// marketing.LoadSeed），逐条跑 Judge（J1 判断主流程，MK-14），输出 markdown
// 报告：判定 / 置信度 / evidence 是否命中 / 判废原因；报告写到 --out 指定路径。
//
// 用法：
//
//	mk-judge -seed [seed 目录] [-out 报告路径]
//
// 约定：
//   - seed 目录为位置参数（缺省 internal/marketing/testdata/seed，相对仓库根；
//     go run ./cmd/mk-judge -seed testdata/seed 的写法也支持：目录不存在时
//     回退到仓库内 testdata）；
//   - -out 缺省 /tmp/mk-judge-seed-report.md；
//   - LLM 走根 .env 的 OPENAI_API_KEY / OPENAI_BASE_URL / LLM_MODEL；
//     未配置时用内置 stub（产出合法 J1 输出，evidence.quote 取自样本原文），
//     保证工具链离线可跑、报告格式可验收（报告头部标注 stub 模式）。
//   - 落库走内存 sqlite（跑批是离线工具，不依赖 MySQL；每条判断仍完整落
//     mk_judgments，可回放）。
//
// 独立文件（P2 串行红线：避免多张卡改同一个 main.go）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/joho/godotenv"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/wetee/ai-workflow/internal/llm"
	"github.com/wetee/ai-workflow/internal/marketing"
)

// seedDefaultDir 是 seed 目录缺省值（相对仓库根；go run ./cmd/mk-judge 在
// apps/core 下执行，testdata 在 internal/marketing 下）。
const seedDefaultDir = "internal/marketing/testdata/seed"

// seedDefaultOut 是报告缺省输出路径。
const seedDefaultOut = "/tmp/mk-judge-seed-report.md"

// runSeed 执行 -seed 跑批：读样本 → 逐条 Judge → markdown 报告写 --out。
func runSeed() {
	// 手动解析参数（flag 包遇到第一个位置参数就停止解析 flag，
	// 而验收写法 `-seed testdata/seed -out /tmp/report.md` 的 -out 在位置参数之后）：
	// 位置参数 = seed 目录（可缺省），-out <path> = 报告路径（可缺省）。
	dir := seedDefaultDir
	out := seedDefaultOut
	for i := 2; i < len(os.Args); i++ {
		arg := os.Args[i]
		switch {
		case arg == "-out" || arg == "--out":
			if i+1 >= len(os.Args) {
				log.Fatal("mk-judge -seed: -out 缺路径")
			}
			i++
			out = os.Args[i]
		case strings.HasPrefix(arg, "-out="):
			out = strings.TrimPrefix(arg, "-out=")
		case strings.HasPrefix(arg, "-"):
			log.Fatalf("mk-judge -seed: 未知参数 %q（可用：-out <path>）", arg)
		default:
			dir = arg
		}
	}
	// 显式给的目录不存在时回退仓库内缺省目录（兼容相对 cwd 的写法）。
	if dir != seedDefaultDir {
		if _, err := os.Stat(dir); err != nil {
			if _, err2 := os.Stat(seedDefaultDir); err2 == nil {
				dir = seedDefaultDir
			}
		}
	}

	samples, err := marketing.LoadSeed(dir)
	if err != nil {
		log.Fatalf("mk-judge -seed: 读取 seed 样本: %v", err)
	}

	// LLM：配置了 OPENAI_API_KEY 走真模型；否则用 stub（离线可跑，报告标注）。
	var (
		llmClient marketing.LLMClient
		modelName string
		stub      bool
	)
	_ = godotenv.Load()
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		client := llm.NewClient(key, os.Getenv("OPENAI_BASE_URL"), "", os.Getenv("LLM_MODEL"))
		llmClient = client
		modelName = client.Model()
	} else {
		llmClient = &stubLLM{}
		modelName = "stub"
		stub = true
	}

	// 落库：内存 sqlite（离线跑批不依赖 MySQL；判断记录完整落库可回放）。
	// 用 AutoMigrate（GORM 结构体 → DDL）而非 Store.EnsureSchema（MySQL 专用 DDL，
	// sqlite 不认 ENGINE=InnoDB / KEY 语法）——与 main_test.go 同写法。
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		log.Fatalf("mk-judge -seed: open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&marketing.Judgment{}); err != nil {
		log.Fatalf("mk-judge -seed: auto migrate: %v", err)
	}
	store := marketing.NewStore(db)

	// 逐条跑 Judge（J1）。
	ctx := context.Background()
	results := make([]seedResult, 0, len(samples))
	for _, s := range samples {
		req := marketing.JudgeRequest{
			Kind:      marketing.KindJudgeLead,
			Region:    s.Region,
			Product:   s.Product,
			Source:    s.Source,
			SourceRef: s.SourceRef,
			Model:     modelName,
			Context: marketing.ContextInput{
				Kind:     marketing.KindJudgeLead,
				Region:   s.Region,
				Product:  s.Product,
				PostText: s.Excerpt,
			},
		}
		j, jerr := marketing.Judge(ctx, store, llmClient, req)
		results = append(results, seedResult{sample: s, judgment: j, judgeErr: jerr})
	}

	report := renderSeedReport(results, modelName, stub, dir)
	if err := os.WriteFile(out, []byte(report), 0o644); err != nil {
		log.Fatalf("mk-judge -seed: 写报告 %s: %v", out, err)
	}

	var pass, reject, failed int
	for _, r := range results {
		switch {
		case r.judgeErr != nil:
			failed++
		case r.judgment.Verdict == marketing.VerdictPass:
			pass++
		default:
			reject++
		}
	}
	log.Printf("mk-judge -seed: %d 条样本（pass=%d reject=%d failed=%d，model=%s）→ %s",
		len(results), pass, reject, failed, modelName, out)
}

// seedResult 是一条样本的跑批结果（样本 + 判断记录 + 硬失败错误）。
type seedResult struct {
	sample   marketing.SeedSample
	judgment marketing.Judgment
	judgeErr error
}

// renderSeedReport 生成 markdown 报告：每条样本一行（判定 / 置信度 /
// evidence 是否命中 / 判废原因）+ 汇总。
func renderSeedReport(results []seedResult, modelName string, stub bool, dir string) string {
	var b strings.Builder
	b.WriteString("# MK-18 种子样本跑批报告\n\n")
	fmt.Fprintf(&b, "- 生成时间：%s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&b, "- 样本目录：%s（%d 条）\n", dir, len(results))
	fmt.Fprintf(&b, "- 模型：%s\n", modelName)
	if stub {
		b.WriteString("- 模式：stub（OPENAI_API_KEY 未配置，离线占位输出，仅验证工具链；判断质量验证须配真模型）\n")
	}
	b.WriteString("- prompt 版本：judge_lead/v1\n\n")
	b.WriteString("| # | 样本 | region | product | source | 判定 | 置信度 | evidence 命中 | 判废原因 |\n")
	b.WriteString("|---|------|--------|---------|--------|------|--------|---------------|----------|\n")
	for i, r := range results {
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			i+1,
			r.sample.ID,
			r.sample.Region,
			r.sample.Product,
			r.sample.Source,
			verdictCell(r),
			confidenceCell(r),
			evidenceCell(r),
			reasonCell(r),
		)
	}
	var pass, reject, failed int
	for _, r := range results {
		switch {
		case r.judgeErr != nil:
			failed++
		case r.judgment.Verdict == marketing.VerdictPass:
			pass++
		default:
			reject++
		}
	}
	b.WriteString("\n## 汇总\n\n")
	fmt.Fprintf(&b, "- 总数：%d（intl 10 + cn 10）\n", len(results))
	fmt.Fprintf(&b, "- pass：%d\n", pass)
	fmt.Fprintf(&b, "- reject：%d\n", reject)
	fmt.Fprintf(&b, "- 硬失败：%d\n", failed)
	return b.String()
}

// verdictCell 判定列：pass / reject / error（硬失败）。
func verdictCell(r seedResult) string {
	if r.judgeErr != nil {
		return "error"
	}
	return r.judgment.Verdict
}

// confidenceCell 置信度列：从模型原始输出解析 confidence（J1 契约字段）；
// 解析不到 / 硬失败 → —。
func confidenceCell(r seedResult) string {
	if r.judgeErr != nil || r.judgment.RawOutput == "" {
		return "—"
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(r.judgment.RawOutput), &m); err != nil {
		return "—"
	}
	c, ok := m["confidence"].(float64)
	if !ok {
		return "—"
	}
	return fmt.Sprintf("%.2f", c)
}

// evidenceCell evidence 是否命中列：模型输出的每条 evidence.quote 去空白后
// 在样本原文子串匹配（MK-12 规则3 同款逻辑）；全部命中 → 是，
// 有未命中 → 否（n/m），无 evidence → —。
func evidenceCell(r seedResult) string {
	if r.judgeErr != nil || r.judgment.RawOutput == "" {
		return "—"
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(r.judgment.RawOutput), &m); err != nil {
		return "—"
	}
	ev, ok := m["evidence"].([]any)
	if !ok || len(ev) == 0 {
		return "—"
	}
	src := stripAllWS(r.sample.Excerpt)
	hit, total := 0, len(ev)
	for _, item := range ev {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		quote, _ := obj["quote"].(string)
		if q := stripAllWS(quote); q != "" && strings.Contains(src, q) {
			hit++
		}
	}
	if hit == total {
		return "是"
	}
	return fmt.Sprintf("否（%d/%d）", hit, total)
}

// reasonCell 判废原因列：reject 时给判废理由（[规则] 前缀），pass / 硬失败
// 时给说明；markdown 表格内的换行与竖线转义。
func reasonCell(r seedResult) string {
	var s string
	switch {
	case r.judgeErr != nil:
		s = "硬失败：" + r.judgeErr.Error()
	case r.judgment.Verdict == marketing.VerdictPass:
		s = "—"
	default:
		s = r.judgment.Reason
	}
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "\\|")
	return s
}

// stripAllWS 去掉全部空白字符（MK-12 规则3 quote 子串匹配同款归一化）。
func stripAllWS(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// stubLLM 是离线占位 LLM（OPENAI_API_KEY 未配置时用）：从 user prompt
// （上下文包快照）里取 post 原文，产出一条**合法** J1 输出
// （evidence.quote 是原文子串，枚举全部合法）——保证跑批工具链与报告格式
// 离线可验收；判断质量验证必须配真模型（报告头部会标注 stub 模式）。
type stubLLM struct{}

func (s *stubLLM) ChatCompletion(_ context.Context, _, userPrompt string) (string, error) {
	quote := ""
	var snap marketing.ContextSnapshot
	if err := json.Unmarshal([]byte(userPrompt), &snap); err == nil {
		quote = snap.Sections["post"]
	}
	if len([]rune(quote)) > 12 {
		quote = string([]rune(quote)[:12])
	}
	out := map[string]any{
		"is_real_need": true,
		"need_type":    "question",
		"intervene":    "monitor",
		"pain":         "latency",
		"freshness":    "fresh",
		"score":        50,
		"confidence":   0.5,
		"evidence":     []map[string]any{{"quote": quote, "why": "stub 占位：取自原文"}},
		"reason":       "stub 占位输出（OPENAI_API_KEY 未配置，仅验证工具链）",
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// 编译期断言：stubLLM 满足 marketing.LLMClient 接口。
var _ marketing.LLMClient = (*stubLLM)(nil)
