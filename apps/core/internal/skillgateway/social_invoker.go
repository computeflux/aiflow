// social_invoker.go：把 skill 网关的 service 调用适配到 social 的**渠道路由**上
// （MIG-1，工作项 wi-1795853890）。
//
// 背景：M4-1 之前，x-scan 由 pycraper 的 POST :9504/invoke 提供、like 由 pyreplay 的
// skill_server.py POST :9500/invoke 提供 —— 两者都是「一个进程只服务一个 skill」的
// 专用 /invoke 端点。social 不是这种形状：它是一个多渠道服务，能力挂在
// /api/social/{channel}/{action} 上（见 apps/social/main.go 的 route）。于是把
// x-scan / like 的 addr 切到 social 之后，网关不能再机械地往 addr 后面拼 /invoke：
//
//	x-scan → POST http://social:8010/x-scan/invoke → 转发 /api/social/x/search?mode=user
//	like   → POST http://social:8010/like/invoke   → 转发 /api/social/x/interact (action=like)
//
// 这一层的职责就是完成那次转发 + **响应归一**：skill 的 output_schema（钉在
// apps/pycraper/skill.json 与 apps/pyreplay/skill.json，网关会逐字段校验）要求的字段
// 是 {ok, handle, count, items[]} / {ok, tweet_id, attempts, success_count, results[]}，
// 而 social 返回的是 posts[] / {results, success, total}。归一规则集中在这里一处，
// 新旧两版「同一入参出同一摘要」才成立 —— M4-1 的一致率探针（migration.ParitySummaryHash）
// 比的就是它，两侧各写一套归一，一致率就成了比格式而不是比行为。
package skillgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// Skill 名（x-scan 的能力名；like 的权威定义在 workflows.SkillLikeName，两侧同值）。
const (
	// SkillXScan 是 X 用户主页推文抓取能力（apps/pycraper/skill.json 的 name）。
	SkillXScan = "x-scan"
	// SkillLike 是 X 点赞能力（apps/pyreplay/skill.json 的 name）。
	SkillLike = "like"
)

// socialInvokePathSuffix 是旧实现（pycraper / pyreplay 的 skill server）的统一调用后缀。
// social 没有这个端点，转发链路必须剥掉它（见 socialBaseOf）。
const socialInvokePathSuffix = "/invoke"

// isSocialAddr 判断一个 skill addr 是否指向 social 服务（按 host 判定，不看端口/路径）。
//
// 为什么按 host：切流过程中同一个 skill 的 addr 会在旧实现（http://pycraper:9504/scan、
// http://pyreplay:9500/like）与 social（http://social:8010/x-scan、/like）之间逐档切换，
// 而 skills.addr 与版本 config.addr 两处都可能落在任意一侧。判定必须对「带不带尾斜杠、
// 带不带路径段」都稳定，否则一档切流就会让调用打到不存在的 /invoke 子路径上。
// isSocialAddr 判断一个 skill addr 是否指向 social 服务（按 host 判定，不看端口/路径）。
//
// 为什么按 host：切流过程中同一个 skill 的 addr 会在旧实现（http://pycraper:9504/scan、
// http://pyreplay:9500/like）与 social（http://social:8010/x-scan、/like）之间逐档切换，
// 而 skills.addr 与版本 config.addr 两处都可能落在任意一侧。判定必须对「带不带尾斜杠、
// 带不带路径段」都稳定，否则一档切流就会让调用打到不存在的 /invoke 子路径上。
func isSocialAddr(addr string) bool {
	switch socialHostOf(addr) {
	case "social", "localhost", "127.0.0.1", "host.docker.internal":
		// 生产只认 compose 服务名 social；环回与 host.docker.internal 是本地/冒烟环境起
		// 假 social 的形态（单测与 deployctl 冒烟都走这两条）。旧实现（pycraper:9504 /
		// pyreplay:9500）与 S5 灰度版本实例（skill-like-1.4.1:19501）的主机名都不在其中。
		return true
	default:
		return false
	}
}

// socialBaseOf 返回 social 服务的基址（scheme://host[:port]，无路径段）；
// addr 不指向 social 时返回空串。
//
// 返回基址而不是原样返回 addr：调用方（runner）拿到的是切流写入的「能力 addr」
// （http://social:8010/x-scan），而转发目标是这个基址下的 /api/social/...，所以必须剥掉
// 能力路径段 —— 直接拼会得到 /x-scan/api/social/x/search 这种不存在的路径（整档断流）。
func socialBaseOf(addr string) string {
	s := strings.TrimSpace(addr)
	if s == "" || !isSocialAddr(s) {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s // 运维手填 addr 常漏 scheme，判定口径不能因此变
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "" // 不可解析（"not-a-url"）：一律当非 social，绝不拼出打到网关自己的地址
	}
	return u.Scheme + "://" + u.Host
}

// socialHostOf 取 addr 的主机名（小写、去端口、去路径）；解析失败时退回字符串裁剪。
//
// 白名单口径：生产只认 compose 服务名 social；环回与 host.docker.internal 是本地/冒烟环境
// 起假 social 的形态（单测与 deployctl 冒烟都走这两条）。旧实现（pycraper:9504 /
// pyreplay:9500）与 S5 灰度版本实例（skill-like-1.4.1:19501）的主机名都不在其中。
func socialHostOf(addr string) string {
	s := strings.TrimSpace(addr)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		h := u.Hostname()
		if h == "" {
			h = u.Host
		}
		return strings.ToLower(h)
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "http://"), "https://")
	s = strings.Split(s, "/")[0]
	if i := strings.LastIndex(s, ":"); i > 0 {
		s = s[:i]
	}
	return strings.ToLower(s)
}

// socialRouteFor 返回某 skill 在 social 上的转发目标（基址 + 查询参数）。
// 不在收口范围（x-scan / like）内的 skill 返回 ok=false，调用方走原有 /invoke 链路。
//
// ⚠️ 入参 addr 必须是 socialBaseOf 收敛过的**基址**（scheme://host[:port]，无路径段）：
// 切流写入的 addr 带能力路径（http://social:8010/x-scan），直接拿它拼转发目标会得到
// /x-scan/api/social/x/search 这种不存在的路径（404 = 整档断流）。
func socialRouteFor(skillName string, base string, input map[string]any) (string, url.Values, bool) {
	switch skillName {
	case SkillXScan:
		handle := strings.TrimSpace(fmt.Sprint(input["handle"]))
		limit := intField(input["limit"], 10)
		q := url.Values{}
		q.Set("keyword", strings.TrimPrefix(handle, "@"))
		q.Set("mode", "user") // pycraper 的 run_scan 固定 mode="user"（用户主页时间线）
		q.Set("limit", fmt.Sprint(clampScanLimit(limit)))
		return strings.TrimSuffix(base, "/"), q, true
	case SkillLike:
		return strings.TrimSuffix(base, "/"), nil, true
	default:
		return "", nil, false
	}
}

// clampScanLimit 收口 x-scan 的条数：与 pycraper skill_entry 的 _MAX_LIMIT=50、
// skill.json 的 minimum=1 完全同口径（两侧任何一侧改动都会让单测红）。
func clampScanLimit(n int) int {
	if n < 1 {
		return 10 // skill.json 未标 default，pycraper 侧 `limit or 10` 的口径就是 10
	}
	if n > 50 {
		return 50
	}
	return n
}

// invokeSocial 把一次 skill 调用转发到 social 的渠道路由，并把响应归一成 skill 的输出契约。
//
// 与 invokeService 同一套约定：非 2xx / 非法 JSON = error（网关记 skill_runs status=fail）；
// 业务失败（ok=false）由上层 runner 依顶层 ok 字段判定，这里不改写它。
// invokeSocial 把一次 skill 调用转发到 social 的渠道路由（生产入口，transport 走注入点）。
func invokeSocial(ctx context.Context, skillName, addr string, input map[string]any) (map[string]any, error) {
	return invokeSocialWithTransport(ctx, nil, skillName, addr, input)
}

// invokeSocialWithTransport 是带 transport 注入的转发实现：rt = nil 时依次回落到
// serviceTransport（包内测试注入点）与 http.DefaultTransport。生产只会走前两条中的缺省路径。
func invokeSocialWithTransport(ctx context.Context, rt http.RoundTripper, skillName, addr string, input map[string]any) (map[string]any, error) {
	// 先收敛基址：调用方给的是切流写入的「能力 addr」（http://social:8010/x-scan），
	// 直接拿它拼转发目标会得到 /x-scan/api/social/x/search 这种不存在的路径。
	socialAddr, query, ok := socialRouteFor(skillName, socialBaseOf(addr), input)
	if !ok {
		return nil, fmt.Errorf("skill %s 没有 social 路由映射", skillName)
	}
	if strings.TrimSpace(socialAddr) == "" {
		return nil, fmt.Errorf("skill addr 为空")
	}

	var (
		method string
		body   []byte
		target string
	)
	switch skillName {
	case SkillXScan:
		// social 的用户主页抓取是 GET + query（M2-2 的直连链路，不落库、超时更短，
		// 正是 skill 单次调用的形态）。
		method = http.MethodGet
		target = strings.TrimSuffix(socialAddr, "/") + "/api/social/x/search"
		if query != nil {
			target += "?" + query.Encode()
		}
	case SkillLike:
		method = http.MethodPost
		target = strings.TrimSuffix(socialAddr, "/") + "/api/social/x/interact"
		post, err := json.Marshal(map[string]any{
			"action":   "like",
			"video_id": strings.TrimSpace(fmt.Sprint(input["post_url"])),
		})
		if err != nil {
			return nil, fmt.Errorf("序列化 like 输入失败: %w", err)
		}
		body = post
	default:
		return nil, fmt.Errorf("skill %s 没有 social 路由映射", skillName)
	}

	if rt == nil {
		rt = serviceTransport
	}
	client := &http.Client{Transport: rt}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, fmt.Errorf("构建请求失败: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// trace 传播与 invokeService 同口径：链路贯穿 workflow → api → social。
	// trace 传播与 invokeService 同口径：链路贯穿 workflow → api → social。
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("调用 social 失败: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSkillOutputBytes))
	if err != nil {
		return nil, fmt.Errorf("读取 social 响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("social HTTP %d: %s", resp.StatusCode, truncateForError(string(data)))
	}

	var raw map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("social 响应不是 JSON object: %w", err)
	}
	return normalizeSocialOutput(skillName, raw)
}

// normalizeSocialOutput 把 social 的响应收敛成 skill 的 output_schema 形状。
//
// 只补齐 schema 要求的字段，其余原样保留（不丢信息，board 排障要看原始计数）。
func normalizeSocialOutput(skillName string, raw map[string]any) (map[string]any, error) {
	if raw == nil {
		return nil, fmt.Errorf("social 响应为空")
	}
	switch skillName {
	case SkillXScan:
		return normalizeScanOutput(raw), nil
	case SkillLike:
		return normalizeLikeOutput(raw), nil
	default:
		return raw, nil
	}
}

// normalizeScanOutput：social 的 posts[]（M2-2 标准化结果）→ pycraper 的 items[]。
//
// pycraper 的 items 是 RawItem JSON（source/external_id/url/content/...），其中
// external_id = "x_<tweet_id>"。一致性比对取的是 id，所以这里必须产出同样的键，
// 否则新旧两版的摘要永远不相等（一致率恒为 0，第一档就切不上去）。
func normalizeScanOutput(raw map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range raw {
		out[k] = v
	}
	if raw["ok"] == nil {
		out["ok"] = true // social 的成功响应不带 ok 字段（HTTP 200 即成功）
	}
	items := raw["posts"]
	if items == nil {
		items = raw["items"] // GET 直连链路返回的是 items（channel.SearchItem）
	}
	normalized := make([]any, 0)
	for _, it := range asSlice(items) {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		id := firstString(m, "id", "external_id")
		item := map[string]any{
			"source":      "x",
			"external_id": xExternalID(id),
			"url":         firstString(m, "url"),
			"content":     firstString(m, "text", "title", "content"),
		}
		if a := firstString(m, "author"); a != "" {
			item["keyword"] = a
		}
		normalized = append(normalized, item)
	}
	out["items"] = normalized
	if _, ok := out["count"]; !ok {
		out["count"] = len(normalized)
	} else {
		out["count"] = intField(out["count"], len(normalized))
	}
	if h := firstString(raw, "keyword"); h != "" {
		out["handle"] = h // pycraper 的 handle（social 侧参数名是 keyword）
	}
	return out
}

// normalizeLikeOutput：social 的互动结果 → pyreplay 的点赞结果。
//
// social 的 /interact 返回 channel.PublishResult（含 Raw.results/success/total），
// pyreplay 的契约是 {ok, tweet_id, attempts, success_count, results[{tweet_id,success,error}]}。
func normalizeLikeOutput(raw map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range raw {
		out[k] = v
	}
	if raw["ok"] == nil {
		out["ok"] = true
	}
	results := raw["results"]
	if results == nil {
		if inner, ok := raw["raw"].(map[string]any); ok {
			results = inner["results"]
		}
	}
	normalized := make([]any, 0)
	success := 0
	for _, it := range asSlice(results) {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		liked := boolField(m, "liked", "success")
		if liked {
			success++
		}
		reason := firstString(m, "reason", "error")
		entry := map[string]any{
			"tweet_id": xExternalID(firstString(m, "tweet", "tweet_id", "video_id", "id")),
			"success":  liked,
		}
		if reason != "" {
			entry["error"] = reason
		}
		normalized = append(normalized, entry)
	}
	out["results"] = normalized
	if v := intField(raw["success_count"], 0); v > 0 {
		out["success_count"] = v
	} else {
		out["success_count"] = success
	}
	if v := intField(raw["attempts"], 0); v > 0 {
		out["attempts"] = v
	} else if v := intField(raw["total"], 0); v > 0 {
		out["attempts"] = v
	} else {
		out["attempts"] = len(normalized)
	}
	if t := firstString(raw, "video_id", "tweet_id", "id"); t != "" {
		out["tweet_id"] = xExternalID(t)
	}
	return out
}

// xExternalID 把推文标识归一成 pycraper 的去重键口径（x_<id>，已是则原样）。
//
// 标识可以是纯 tweet ID，也可以是完整推文 URL（pyreplay 的 like 契约里 tweet_id 常直接
// 回显入参 URL）：优先取 URL 里的 /status/<id> 段，取不到就退回原值（不猜、不截断）。
func xExternalID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if m := tweetStatusIDRe.FindStringSubmatch(id); len(m) == 2 {
		return "x_" + m[1]
	}
	if strings.HasPrefix(id, "x_") {
		return id
	}
	return "x_" + id
}

// tweetStatusIDRe 与 apps/social/internal/search/normalize.go 的 tweetIDRe 同一条规则
// （推文 URL → 纯数字 tweet ID）：新旧两版的去重键必须同源，否则一致率永远对不上。
var tweetStatusIDRe = regexp.MustCompile(`/status/(\d+)`)

// asSlice 把 any 收成 []any（非切片返回 nil）。
func asSlice(v any) []any {
	s, ok := v.([]any)
	if !ok {
		return nil
	}
	return s
}

// firstString 取第一个非空字符串字段。
func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok {
			if s = strings.TrimSpace(s); s != "" && s != "<nil>" {
				return s
			}
		}
	}
	return ""
}

// boolField 取第一个布尔字段（缺失 = false）。
func boolField(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if b, ok := m[k].(bool); ok {
			return b
		}
	}
	return false
}

// intField 取整数字段（JSON 数字经 UseNumber 解码是 json.Number）。
func intField(v any, fallback int) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(n)
		}
	}
	return fallback
}
