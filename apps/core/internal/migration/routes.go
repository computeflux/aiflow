// routes.go：M4-1 的运维 API（board 运维中心 / skillctl 用）。
//
// ⚠️ 鉴权红线：切流与下线都是「改生产流量」的动作，路由必须由调用方挂 requireAuth
// （与 gateway / intervention 同款），本包不接受无鉴权挂载 —— 未登录就能把 like 的
// 流量打到新实现上，等于把发布权交给任何能连到 API 的人。
package migration

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// RegisterRoutes 挂载 /skill-cutover/* 路由。
//
// mw 是**必填**参数（不是可变参数）：本包不接受「一个鉴权中间件都不挂」的挂载方式。
// 为什么在编译期挡而不在运行期检查：把「无鉴权就能改生产流量」做成写不出来的代码，比让它
// 跑起来才发现要早得多（也绝不会在生产的启动路径上炸进程）。调用方传 requireAuth() 即可；
// 真要放开的场景（本地开发）必须显式传一个自己的中间件，让决定落在代码里可被 review。
func RegisterRoutes(r gin.IRouter, svc *CutoverService, dec *DecommissionService, mw gin.HandlerFunc, mws ...gin.HandlerFunc) {
	if mw == nil {
		panic("migration: /skill-cutover/* 的鉴权中间件不能为 nil（切流与下线都是改生产流量的动作）")
	}
	g := r.Group("/skill-cutover", append([]gin.HandlerFunc{mw}, mws...)...)
	g.GET("", svc.list)
	g.GET("/:skill", svc.show)
	g.POST("/:skill/target", svc.setTarget)
	g.POST("/:skill/stage", svc.setStage)
	g.POST("/:skill/complete", svc.complete)
	g.POST("/:skill/rollback", svc.rollback)
	g.POST("/:skill/parity", svc.recordParity)
	if dec != nil {
		// ⚠️ 下线计划挂在**独立一级前缀** /skill-decommission/plan，绝不挂进上面的 /skill-cutover 组：
		// 那个组里已经有 `GET /:skill`，同一层再放静态段 `decommission-plan` 就是 httprouter 系
		// radix tree 的「参数段 + 静态段冲突」（gin 在注册时就 panic），最坏是 cmd/api 一启动就崩；
		// 即使某个版本容忍这种注册，`/skill-cutover/decommission-plan` 也会被 `:skill` 抢先吃掉，
		// 返回一个 skill 详情而不是下线清单 —— 单测里 dec 传 nil 完全覆盖不到这条路径。
		dg := r.Group("/skill-decommission", append([]gin.HandlerFunc{mw}, mws...)...)
		dg.GET("/plan", dec.plan)
	}
}

// list GET /skill-cutover
func (s *CutoverService) list(c *gin.Context) {
	rows, err := s.List(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": rows})
}

// show GET /skill-cutover/:skill —— 切流状态 + 当前质量证据。
func (s *CutoverService) show(c *gin.Context) {
	name := c.Param("skill")
	row, err := s.Get(c.Request.Context(), name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := gin.H{"skill": name, "cutover": row}
	if row != nil && row.TargetVer != "" {
		if ev, err := s.Evidence(c.Request.Context(), name, row.TargetVer); err == nil {
			out["evidence"] = ev
			out["gate_reasons"] = ev.GateReasons(s.cfg.effective(row))
		}
	}
	c.JSON(http.StatusOK, out)
}

// setTarget POST /skill-cutover/:skill/target  body {target_addr, note?}
func (s *CutoverService) setTarget(c *gin.Context) {
	var body struct {
		TargetAddr string `json:"target_addr"`
		Note       string `json:"note"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	row, err := s.Target(c.Request.Context(), c.Param("skill"), body.TargetAddr, actorOf(c), body.Note)
	if err != nil {
		c.JSON(statusFor(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, row)
}

// setStage POST /skill-cutover/:skill/stage  body {traffic_split, version?, note?}
//
// 护栏不过（非法档位 / 未登记目标 / 时长不足 / 数据不达标）→ 409，响应体带逐条原因，
// board 上运维能看到「差在哪」而不是一个笼统的 500。
func (s *CutoverService) setStage(c *gin.Context) {
	var body struct {
		TrafficSplit *int   `json:"traffic_split"`
		Version      string `json:"version"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if body.TrafficSplit == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "traffic_split 必填（10/50/100）"})
		return
	}
	v, ev, err := s.Stage(c.Request.Context(), c.Param("skill"), body.Version, *body.TrafficSplit, actorOf(c))
	if err != nil {
		c.JSON(statusFor(err), gin.H{"error": err.Error(), "version": v, "evidence": ev})
		return
	}
	c.JSON(http.StatusOK, gin.H{"version": v, "evidence": ev})
}

// complete POST /skill-cutover/:skill/complete
func (s *CutoverService) complete(c *gin.Context) {
	v, ev, err := s.Complete(c.Request.Context(), c.Param("skill"), actorOf(c))
	if err != nil {
		c.JSON(statusFor(err), gin.H{"error": err.Error(), "version": v, "evidence": ev})
		return
	}
	c.JSON(http.StatusOK, gin.H{"version": v, "evidence": ev})
}

// rollback POST /skill-cutover/:skill/rollback  body {reason}
func (s *CutoverService) rollback(c *gin.Context) {
	var body struct {
		Reason string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(body.Reason) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reason 必填（回滚原因进审计与告警）"})
		return
	}
	if err := s.Rollback(c.Request.Context(), c.Param("skill"), body.Reason, actorOf(c)); err != nil {
		c.JSON(statusFor(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// recordParity POST /skill-cutover/:skill/parity
// body {version, group, input_hash, summary, ok}
func (s *CutoverService) recordParity(c *gin.Context) {
	var body struct {
		Version   string `json:"version"`
		Group     string `json:"group"`
		InputHash string `json:"input_hash"`
		Summary   string `json:"summary"`
		OK        *bool  `json:"ok"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if body.OK == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ok 必填（本次探针是否成功）"})
		return
	}
	if err := s.RecordParityRun(c.Request.Context(), c.Param("skill"), body.Version, body.Group, body.InputHash, body.Summary, *body.OK); err != nil {
		c.JSON(statusFor(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"ok": true})
}

// plan GET /skill-decommission/plan?window_sec= —— 旧服务缩容下线清单。
func (d *DecommissionService) plan(c *gin.Context) {
	window := d.stableWindow
	if v := c.Query("window_sec"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			window = time.Duration(n) * time.Second
		}
	}
	dec := NewDecommission(d.db, d.cfg, window)
	pl, err := dec.Evaluate(c.Request.Context(), nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, pl)
}

// statusFor 把切流错误映射到 HTTP 状态码：护栏类 = 409（可解释的业务冲突），
// 参数类 = 400，其它 = 500。
func statusFor(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case isGuardrail(err):
		return http.StatusConflict
	case isBadParam(err):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func isGuardrail(err error) bool {
	return isErr(err, ErrCutoverStage) || isErr(err, ErrCutoverInterval) ||
		isErr(err, ErrCutoverNoTarget) || isErr(err, ErrCutoverNotParsed) ||
		isErr(err, ErrCutoverCompleted) || isErr(err, ErrCutoverVersionLocked) ||
		strings.Contains(err.Error(), "不达标") || strings.Contains(err.Error(), "禁止更换目标")
}

func isBadParam(err error) bool {
	return strings.Contains(err.Error(), "不能为空") || strings.Contains(err.Error(), "必填") ||
		strings.Contains(err.Error(), "sha256")
}

func isErr(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// actorOf 取操作者：优先用鉴权中间件解析出的登录身份（requireAuth 写进 context 的 user），
// 其次才是调用方自报的 X-Actor。
//
// 顺序不能反：X-Actor 是请求头，任何能连到 API 的调用方都能随便填。审计留痕（audit_logs
// 只增不删）是切流「谁放的量、谁批的收尾」的唯一凭据，若自报头优先，整条切流链的操作者
// 都能被伪造成任意用户名，事后追责与「人工审批」的约束一起失效。
func actorOf(c *gin.Context) string {
	if v := authedActor(c); v != "" {
		return v
	}
	return strings.TrimSpace(c.GetHeader("X-Actor"))
}

// authedActor 从鉴权中间件的产物里取登录身份。本系统唯一的 requireAuth 是
// internal/authn.Verifier.Middleware()，它把 JWT 里的 sub（用户 id）写进 context 键
// "authn_user_id"（字符串）。这里显式列键位而不是 import authn：migration 是被 cmd/api
// 装配的底层包，反向依赖鉴权包会把装配顺序锁死，也让单测必须起验签器。
// 取不到 = 空串（调用方回落到 X-Actor 或缺省 actor）。
func authedActor(c *gin.Context) string {
	for _, key := range authActorKeys {
		if v, ok := c.Get(key); ok {
			if s := stringifyIdentity(v); s != "" {
				return s
			}
		}
	}
	return ""
}

// authActorKeys 是按优先级排列的鉴权身份 context 键（authn 在首，其余是历史/兼容键位）。
var authActorKeys = []string{"authn_user_id", "username", "user_name", "user", "login", "sub"}

// stringifyIdentity 把 context 里的身份值收敛成人读字符串（只接受字符串与数字标量，
// 结构体等复杂值一律当「取不到」—— 审计字段绝不能落进一段 Go 指针地址）。
func stringifyIdentity(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case fmt.Stringer:
		return strings.TrimSpace(t.String())
	case int:
		if t <= 0 {
			return ""
		}
		return strconv.Itoa(t)
	case int64:
		if t <= 0 {
			return ""
		}
		return strconv.FormatInt(t, 10)
	case uint:
		if t == 0 {
			return ""
		}
		return strconv.FormatUint(uint64(t), 10)
	default:
		return ""
	}
}
