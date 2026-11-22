package ops

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
)

// KeyCount 是「键 → 计数」的聚合项（by_system / by_level 用）。
type KeyCount struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

// TopError 是错误 Top 榜的一行（按 system+source 聚类，sample 为该聚类下任一 message）。
type TopError struct {
	System string `json:"system"`
	Source string `json:"source"`
	Sample string `json:"sample"`
	Count  int64  `json:"count"`
}

// ErrorMetrics 是 error_logs 窗口聚合（S6 §3.1）。
type ErrorMetrics struct {
	TotalInWindow int64      `json:"total_in_window"` // 新增（窗口内）
	Unresolved    int64      `json:"unresolved"`      // 存量（未处理）
	BySystem      []KeyCount `json:"by_system"`
	ByLevel       []KeyCount `json:"by_level"`
	Top10         []TopError `json:"top10"`
}

// SkillMetrics 是单个技能的窗口聚合（S6 §3.1）。
type SkillMetrics struct {
	Name         string  `json:"name"`
	Total        int64   `json:"total"`
	Success      int64   `json:"success"`
	SuccessRate  float64 `json:"success_rate"`
	P99Ms        int64   `json:"p99_ms"`
	Timeouts     int64   `json:"timeouts"`
	CircuitState string  `json:"circuit_state"` // close | open | half | ""
}

// TemporalMetrics 是 Temporal workflow 概览（S6 §3.1）。
type TemporalMetrics struct {
	Active int64  `json:"active"` // 活跃（Running）
	Failed int64  `json:"failed"` // 窗口内失败
	Note   string `json:"note,omitempty"`
}

// InterventionMetrics 是 interventions 窗口聚合（S6 §3.1）。
type InterventionMetrics struct {
	NewInWindow int64 `json:"new_in_window"` // 新增（窗口内）
	Unresolved  int64 `json:"unresolved"`    // 存量（open）
}

// Metrics 是一轮聚合的完整原始数据（写入 ops_reports.metrics JSON，供对账/图表）。
type Metrics struct {
	WindowDays    int                 `json:"window_days"`
	GeneratedAt   string              `json:"generated_at"`
	Errors        ErrorMetrics        `json:"errors"`
	Skills        []SkillMetrics      `json:"skills"`
	Temporal      TemporalMetrics     `json:"temporal"`
	Interventions InterventionMetrics `json:"interventions"`
}

// skillAgg 是技能聚合的中间态（latency 数组用于 p99）。
type skillAgg struct {
	total    int64
	success  int64
	timeouts int64
	latency  []int64
}

// Aggregate 聚合近 windowDays 天的运维数据（全部来自现有表，不新增采集）。
// temporal 可为 nil（不可用时降级为 0 + note，报告仍生成）。
func Aggregate(ctx context.Context, database *db.DB, temporal client.Client, temporalNamespace string, windowDays int) (*Metrics, error) {
	now := time.Now()
	start := now.AddDate(0, 0, -windowDays)
	m := &Metrics{WindowDays: windowDays, GeneratedAt: now.Format(time.RFC3339)}

	if err := aggregateErrors(ctx, database, start, &m.Errors); err != nil {
		return nil, fmt.Errorf("aggregate errors: %w", err)
	}
	if err := aggregateSkills(ctx, database, start, &m.Skills); err != nil {
		return nil, fmt.Errorf("aggregate skills: %w", err)
	}
	m.Temporal = aggregateTemporal(ctx, temporal, temporalNamespace, start)
	if err := aggregateInterventions(ctx, database, start, &m.Interventions); err != nil {
		return nil, fmt.Errorf("aggregate interventions: %w", err)
	}
	return m, nil
}

// aggregateErrors 聚合 error_logs：窗口内计数 + 按 system/level 聚类 + Top10 + 未处理存量。
func aggregateErrors(ctx context.Context, database *db.DB, start time.Time, out *ErrorMetrics) error {
	var total, unresolved int64
	if err := database.WithContext(ctx).Model(&db.ErrorLog{}).Where("created_at >= ?", start).Count(&total).Error; err != nil {
		return err
	}
	if err := database.WithContext(ctx).Model(&db.ErrorLog{}).Where("resolved = ?", false).Count(&unresolved).Error; err != nil {
		return err
	}
	out.TotalInWindow, out.Unresolved = total, unresolved

	// system 是 MySQL 8.0.17+ 保留字，必须加反引号（见 cmd/api 的 /errors/stats）。
	var bySystem, byLevel []KeyCount
	if err := database.WithContext(ctx).Model(&db.ErrorLog{}).
		Select("`system` as `key`, count(*) as `count`").
		Where("created_at >= ?", start).Group("`system`").Order("`count` desc").Scan(&bySystem).Error; err != nil {
		return err
	}
	if err := database.WithContext(ctx).Model(&db.ErrorLog{}).
		Select("`level` as `key`, count(*) as `count`").
		Where("created_at >= ?", start).Group("`level`").Order("`count` desc").Scan(&byLevel).Error; err != nil {
		return err
	}
	out.BySystem, out.ByLevel = nilIfEmpty(bySystem), nilIfEmpty(byLevel)

	var top []TopError
	if err := database.WithContext(ctx).Model(&db.ErrorLog{}).
		Select("`system`, source, MIN(`message`) as `sample`, count(*) as `count`").
		Where("created_at >= ?", start).
		Group("`system`, source").Order("`count` desc").Limit(10).Scan(&top).Error; err != nil {
		return err
	}
	out.Top10 = nilIfEmptyTop(top)
	return nil
}

// aggregateSkills 聚合 skill_runs：按 skill_name 聚合成功率/p99/timeout 次数，并 join skill_circuit 熔断态。
func aggregateSkills(ctx context.Context, database *db.DB, start time.Time, out *[]SkillMetrics) error {
	var runs []struct {
		SkillName string `gorm:"column:skill_name"`
		Status    string `gorm:"column:status"`
		LatencyMs *int64 `gorm:"column:latency_ms"`
	}
	if err := database.WithContext(ctx).
		Raw("SELECT skill_name, status, latency_ms FROM skill_runs WHERE created_at >= ?", start).
		Scan(&runs).Error; err != nil {
		return err
	}
	aggs := map[string]*skillAgg{}
	for _, r := range runs {
		a := aggs[r.SkillName]
		if a == nil {
			a = &skillAgg{}
			aggs[r.SkillName] = a
		}
		a.total++
		if r.Status == "ok" {
			a.success++
		}
		if r.Status == "timeout" {
			a.timeouts++
		}
		if r.LatencyMs != nil {
			a.latency = append(a.latency, *r.LatencyMs)
		}
	}

	// 熔断态（skill_circuit：S3 持久化，skill_name 主键）
	var circuits []db.SkillCircuit
	if err := database.WithContext(ctx).Find(&circuits).Error; err != nil {
		return err
	}
	circuitState := map[string]string{}
	for _, c := range circuits {
		circuitState[c.SkillName] = c.State
	}

	names := make([]string, 0, len(aggs))
	for name := range aggs {
		names = append(names, name)
	}
	sort.Strings(names)
	*out = make([]SkillMetrics, 0, len(names))
	for _, name := range names {
		a := aggs[name]
		rate := 0.0
		if a.total > 0 {
			rate = float64(a.success) / float64(a.total)
		}
		p99, _ := percentile(a.latency, 0.99)
		*out = append(*out, SkillMetrics{
			Name:         name,
			Total:        a.total,
			Success:      a.success,
			SuccessRate:  round4(rate),
			P99Ms:        p99,
			Timeouts:     a.timeouts,
			CircuitState: circuitState[name],
		})
	}
	return nil
}

// aggregateTemporal 查询 Temporal 活跃 / 窗口内失败 workflow 数。
// 客户端不可用或查询失败时降级为 0 + note（不阻断报告生成）。
func aggregateTemporal(ctx context.Context, temporal client.Client, namespace string, start time.Time) TemporalMetrics {
	if temporal == nil {
		return TemporalMetrics{Note: "temporal client unavailable"}
	}
	out := TemporalMetrics{}
	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// ListWorkflow 不直接返回总数：先探测一次（顺带校验 namespace/查询语法），
	// 再用分页计数（上限 200，运维概览够用）。
	if _, err := temporal.ListWorkflow(tctx, &workflowservice.ListWorkflowExecutionsRequest{
		Namespace: namespace, Query: "ExecutionStatus = 'Running'", PageSize: 1,
	}); err != nil {
		out.Note = fmt.Sprintf("list active: %v", err)
		return out
	}
	out.Active = countWorkflows(tctx, temporal, namespace, "ExecutionStatus = 'Running'")
	out.Failed = countWorkflows(tctx, temporal, namespace,
		fmt.Sprintf("ExecutionStatus = 'Failed' AND CloseTime >= '%s'", start.Format(time.RFC3339)))
	return out
}

// countWorkflows 分页计数（上限 200，避免全表扫描；运维概览够用）。
func countWorkflows(ctx context.Context, temporal client.Client, namespace, query string) int64 {
	const maxCount = 200
	var token []byte
	var n int64
	for i := 0; i < maxCount; i++ {
		resp, err := temporal.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
			Namespace: namespace, Query: query, PageSize: 100, NextPageToken: token,
		})
		if err != nil {
			return n
		}
		n += int64(len(resp.GetExecutions()))
		token = resp.GetNextPageToken()
		if len(token) == 0 {
			break
		}
	}
	return n
}

// aggregateInterventions 聚合 interventions：窗口内新增 + 未解决（open）存量。
func aggregateInterventions(ctx context.Context, database *db.DB, start time.Time, out *InterventionMetrics) error {
	var total, unresolved int64
	if err := database.WithContext(ctx).Model(&db.Intervention{}).Where("created_at >= ?", start).Count(&total).Error; err != nil {
		return err
	}
	if err := database.WithContext(ctx).Model(&db.Intervention{}).Where("status = ?", "open").Count(&unresolved).Error; err != nil {
		return err
	}
	out.NewInWindow, out.Unresolved = total, unresolved
	return nil
}

// percentile 计算有序数组的 p 分位（p∈(0,1)）；空数组返回 (0,false)。
func percentile(vals []int64, p float64) (int64, bool) {
	if len(vals) == 0 {
		return 0, false
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	idx := int(math.Ceil(p*float64(len(vals)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(vals) {
		idx = len(vals) - 1
	}
	return vals[idx], true
}

func round4(f float64) float64 { return math.Round(f*10000) / 10000 }

func nilIfEmpty(in []KeyCount) []KeyCount {
	if in == nil {
		return []KeyCount{}
	}
	return in
}

func nilIfEmptyTop(in []TopError) []TopError {
	if in == nil {
		return []TopError{}
	}
	return in
}
