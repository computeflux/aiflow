// Package main is the skillctl binary.
//
// skillctl 是 P1 Skill 框架的命令行工具（docs/plan/transform-plan.md §6.7）。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/wetee/ai-workflow/internal/skillgateway"
)

const usage = `skillctl — P1 Skill 框架命令行工具

用法:
  skillctl validate <skill.json>              本地校验 skill.json
  skillctl register <skill.json>              注册/刷新 skill
  skillctl list                               列出所有 skill 及版本状态
  skillctl invoke <name> --input <file|->     调用 skill（- 表示从 stdin 读输入）
  skillctl traffic <name> <version> <n>       设置版本流量 0~100（>0 转 live）

M4-1 跨服务切流（x-scan / like → social；写端点全部要鉴权，见下）:
  skillctl cutover list                                  列出全部切流状态
  skillctl cutover show <skill>                          切流状态 + 当前质量证据
  skillctl cutover target <skill> <addr> [--note <文本>]  登记切流目标（已放量后禁改）
  skillctl cutover stage <skill> <version> <10|50|100>    放量到指定档位（护栏不过则拒绝）
  skillctl cutover complete <skill>                       全量收尾（收敛 skills.addr）
  skillctl cutover rollback <skill> --reason <文本>       回滚到旧实现（原因必填）
  skillctl cutover parity <skill> <version> --group <组名> --input-hash <64hex> \
            --summary <JSON文本|@文件|-> [--fail]          上报一条离线双跑探针
  skillctl cutover decommission [--window-sec <秒>]        旧服务缩容下线清单

鉴权:
  WORKFLOW_API_TOKEN   Bearer Token；切流/下线是「改生产流量」的动作，必须带。
                       （board 运维中心走同源 Cookie 登录；CLI 走这个变量。）

环境变量:
  WORKFLOW_API_URL     workflow API 地址（默认 http://127.0.0.1:8001）
`

func apiBase() string {
	if v := os.Getenv("WORKFLOW_API_URL"); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return "http://127.0.0.1:8001"
}

func httpDo(method, path string, body any) (*http.Response, []byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, apiBase()+path, reader)
	if err != nil {
		return nil, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// M4-1 切流/下线端点强制鉴权：有 WORKFLOW_API_TOKEN 就带 Bearer（无 token 时由服务端
	// 直接 401/403，CLI 原样回显状态码 —— 绝不在客户端做「未登录也发出去」的兜底）。
	if tok := strings.TrimSpace(os.Getenv("WORKFLOW_API_TOKEN")); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	// 不设 client timeout：invoke 可能长达 limits.timeout_sec（最长 300s）。
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, nil, err
	}
	return resp, data, nil
}

// runCutover 是 M4-1 跨服务切流的 CLI 面（工作项 wi-1795489658-2d59）：全部动作都落到
// core 的 /skill-cutover/*，护栏判定在服务端（本 CLI 只负责把逐条原因原样打出来，绝不在
// 客户端「放宽」任何门槛）。
//
// 路径前缀为什么不带 /api：WORKFLOW_API_URL 指向的是 core API 本体（默认
// http://127.0.0.1:8001，compose 里是 http://workflow-api:8001），**不经过** core-ui 的
// nginx（只有 nginx 会剥掉 /api/ 前缀）。migration.RegisterRoutes 把路由组挂在根上
// （gin.IRouter + r.Group("/skill-cutover")），所以直连 core 的正确前缀就是 /skill-cutover；
// 写成 /api/skill-cutover 会让整族命令一律 404。
func runCutover(args []string) {
	if len(args) == 0 {
		fatalf("用法: skillctl cutover list|show|target|stage|complete|rollback|parity|decommission …（见 skillctl -h）")
	}
	cmd := args[0]
	// flags：--k v 形式收进 map，位置参数按顺序留在 pos。
	flags := map[string]string{}
	var pos []string
	for i := 1; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--") {
			key := strings.TrimPrefix(args[i], "--")
			if key == "fail" { // --fail = 布尔开关（探针记一条失败）
				flags[key] = "true"
				continue
			}
			if i+1 >= len(args) {
				fatalf("缺少 --%s 的取值", key)
			}
			flags[key] = args[i+1]
			i++
			continue
		}
		pos = append(pos, args[i])
	}

	switch cmd {
	case "list":
		cutoverDo(http.MethodGet, "/skill-cutover", nil, "切流清单")
	case "show":
		if len(pos) < 1 {
			fatalf("用法: skillctl cutover show <skill>")
		}
		cutoverDo(http.MethodGet, "/skill-cutover/"+pos[0], nil, "切流状态")
	case "target":
		if len(pos) < 2 {
			fatalf("用法: skillctl cutover target <skill> <addr> [--note <文本>]")
		}
		cutoverDo(http.MethodPost, "/skill-cutover/"+pos[0]+"/target",
			map[string]string{"target_addr": pos[1], "note": flags["note"]}, "登记切流目标")
	case "stage":
		if len(pos) < 3 {
			fatalf("用法: skillctl cutover stage <skill> <version> <10|50|100>")
		}
		n, err := strconv.Atoi(pos[2])
		if err != nil {
			fatalf("档位必须是 %s 之一，得到 %q", "10|50|100", pos[2])
		}
		cutoverDo(http.MethodPost, "/skill-cutover/"+pos[0]+"/stage",
			map[string]any{"version": pos[1], "traffic_split": n}, "放量")
	case "complete":
		if len(pos) < 1 {
			fatalf("用法: skillctl cutover complete <skill>")
		}
		cutoverDo(http.MethodPost, "/skill-cutover/"+pos[0]+"/complete", map[string]any{}, "全量收尾")
	case "rollback":
		if len(pos) < 1 {
			fatalf("用法: skillctl cutover rollback <skill> --reason <文本>")
		}
		cutoverDo(http.MethodPost, "/skill-cutover/"+pos[0]+"/rollback",
			map[string]string{"reason": flags["reason"]}, "回滚")
	case "parity":
		if len(pos) < 2 {
			fatalf("用法: skillctl cutover parity <skill> <version> --group <组名> --input-hash <64hex> --summary <JSON|@文件|-> [--fail]")
		}
		group := flags["group"]
		if group == "" {
			fatalf("探针必须带 --group（组标识；批次号在尾部，用 ParityProbeGroup 的格式）")
		}
		sum, err := readTextArg(flags["summary"])
		if err != nil {
			fatalf("读取 --summary: %v", err)
		}
		if sum == "" {
			fatalf("探针必须带 --summary（可比对的结果摘要 JSON）：空摘要在一致率里一律算不一致")
		}
		if _, ok := flags["fail"]; ok {
			cutoverDoProbe(pos, group, flags["input-hash"], sum, false)
			return
		}
		cutoverDoProbe(pos, group, flags["input-hash"], sum, true)
	case "decommission":
		path := "/skill-decommission/plan"
		if v := flags["window-sec"]; v != "" {
			path += "?window_sec=" + v
		}
		cutoverDo(http.MethodGet, path, nil, "旧服务下线计划")
	default:
		fatalf("未知 cutover 子命令 %q（见 skillctl -h）", cmd)
	}
}

// cutoverDoProbe 上报一条离线双跑探针。ok=false 只影响「这次探针是否成功」，
// 一致与否由服务端逐组比对结果摘要得出 —— CLI 不接受「我宣布一致」这种输入。
func cutoverDoProbe(pos []string, group, inputHash, summary string, ok bool) {
	cutoverDo(http.MethodPost, "/skill-cutover/"+pos[0]+"/parity", map[string]any{
		"version": pos[1], "group": group, "input_hash": inputHash, "summary": summary, "ok": ok,
	}, "上报探针")
}

// cutoverDo 发一个切流请求并回显响应。护栏拒绝（409）时把服务端的逐条原因原样打出来：
// 「差在哪」只有服务端算得准（样本量 / 一致率 / 成功率 / 观察窗口），CLI 复述一遍就够了。
//
// 鉴权失败单独点名：缺 token 时服务端只回一句「未登录」，直接贴出来运维会去翻服务端日志；
// 这里明确指向 WORKFLOW_API_TOKEN，把「CLI 没带凭证」（本地配置问题）与「服务端策略拒绝」
// （业务护栏）分开。
func cutoverDo(method, path string, body any, what string) {
	resp, data, err := httpDo(method, path, body)
	if err != nil {
		fatalf("请求失败: %v", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		fatalf("%s 被拒 (HTTP %d)：切流/下线端点要求鉴权，请设置 WORKFLOW_API_TOKEN（Bearer）。响应：%s",
			what, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if resp.StatusCode >= 400 {
		fatalf("%s 失败 (HTTP %d): %s", what, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	printJSON(json.RawMessage(data))
}

// readTextArg 取参数文本：@path 读文件，"-" 读 stdin，其余当字面量。
//
// JSON 文本必须能校验（探针的 summary 是「可比对的结果摘要」）：os.Stdin 与打开的文件句柄
// 都实现 io.Reader，两条路径共用一次流式解码，读到的就是原始文本（不重新序列化，避免
// key 顺序/空白变化影响调用方的自校验）。
func readTextArg(v string) (string, error) {
	switch {
	case strings.HasPrefix(v, "@"):
		f, err := os.Open(strings.TrimPrefix(v, "@"))
		if err != nil {
			return "", err
		}
		defer f.Close()
		return readValidatedJSON(f, "摘要文件")
	case v == "-":
		return readValidatedJSON(os.Stdin, "stdin")
	case v == "":
		return "", nil
	default:
		return v, nil
	}
}

// readValidatedJSON 读全流并校验是合法 JSON **对象**；返回原文（原样，不重新序列化 ——
// 重新序列化会改 key 顺序与空白，让调用方自己的哈希/自校验对不上）。
//
// 只接受对象：探针的结果摘要按 skill 的 output_schema 归一后必然是对象，数组/标量/空串
// 都是「没按契约出结果」，在服务端会被算成缺摘要 → 整组不一致；在 CLI 侧就拒掉，比让
// 运维对着一条莫名的「一致率不达标」更好查。
func readValidatedJSON(r io.Reader, what string) (string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", fmt.Errorf("%s 必须是 JSON 对象（按 skill 的 output_schema 归一后的结果摘要）", what)
	}
	var probe map[string]any
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return "", fmt.Errorf("%s 不是合法 JSON 对象: %w", what, err)
	}
	return string(b), nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "skillctl: "+format+"\n", args...)
	os.Exit(1)
}

func printJSON(v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Println(v)
		return
	}
	fmt.Println(string(b))
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Print(usage)
		os.Exit(1)
	}

	switch args[0] {
	case "validate":
		if len(args) < 2 {
			fatalf("用法: skillctl validate <skill.json>")
		}
		raw, err := os.ReadFile(args[1])
		if err != nil {
			fatalf("读取 %s: %v", args[1], err)
		}
		spec, err := skillgateway.ValidateSpec(raw)
		if err != nil {
			fatalf("校验失败: %v", err)
		}
		fmt.Printf("OK  %s v%s (%s/%s, timeout=%ds, rate=%d/min, concurrency=%d)\n",
			spec.Name, spec.Version, spec.Runtime, spec.Type,
			spec.Limits.TimeoutSec, spec.Limits.RatePerMin, spec.Limits.MaxConcurrency)

	case "register":
		if len(args) < 2 {
			fatalf("用法: skillctl register <skill.json>")
		}
		raw, err := os.ReadFile(args[1])
		if err != nil {
			fatalf("读取 %s: %v", args[1], err)
		}
		spec, err := skillgateway.ValidateSpec(raw)
		if err != nil {
			fatalf("校验失败: %v", err)
		}
		resp, data, err := httpDo("POST", "/api/skills", json.RawMessage(raw))
		if err != nil {
			fatalf("请求失败: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			fatalf("注册失败 (HTTP %d): %s", resp.StatusCode, string(data))
		}
		fmt.Printf("registered %s v%s\n", spec.Name, spec.Version)
		printJSON(json.RawMessage(data))

	case "list":
		resp, data, err := httpDo("GET", "/api/skills", nil)
		if err != nil {
			fatalf("请求失败: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			fatalf("list 失败 (HTTP %d): %s", resp.StatusCode, string(data))
		}
		printJSON(json.RawMessage(data))

	case "invoke":
		if len(args) < 2 {
			fatalf("用法: skillctl invoke <name> --input <file|->")
		}
		name := args[1]
		inputPath := ""
		for i := 2; i < len(args); i++ {
			if args[i] == "--input" && i+1 < len(args) {
				inputPath = args[i+1]
				i++
			}
		}
		var raw []byte
		var err error
		if inputPath == "" || inputPath == "-" {
			raw, err = io.ReadAll(os.Stdin)
		} else {
			raw, err = os.ReadFile(inputPath)
		}
		if err != nil {
			fatalf("读取输入: %v", err)
		}
		input := map[string]any{}
		if len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, &input); err != nil {
				fatalf("输入不是合法 JSON object: %v", err)
			}
		}
		resp, data, err := httpDo("POST", "/api/skills/"+name+"/invoke", input)
		if err != nil {
			fatalf("请求失败: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			fatalf("invoke 失败 (HTTP %d): %s", resp.StatusCode, string(data))
		}
		printJSON(json.RawMessage(data))

	case "cutover":
		runCutover(args[1:])

	case "traffic":
		if len(args) < 4 {
			fatalf("用法: skillctl traffic <name> <version> <0~100>")
		}
		name, version := args[1], args[2]
		n, err := strconv.Atoi(args[3])
		if err != nil || n < 0 || n > 100 {
			fatalf("traffic 须为 0~100 的整数")
		}
		resp, data, err := httpDo("PUT",
			"/api/skills/"+name+"/versions/"+version+"/traffic",
			map[string]int{"traffic_split": n})
		if err != nil {
			fatalf("请求失败: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			fatalf("traffic 失败 (HTTP %d): %s", resp.StatusCode, string(data))
		}
		printJSON(json.RawMessage(data))

	default:
		fmt.Print(usage)
		os.Exit(1)
	}
}
