// Package main is the agentctl binary.
//
// agentctl 是 S1 Agent 注册与绑定的命令行工具（docs/plan/S1-进化闭环.md §3.7）。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const usage = `agentctl — S1 Agent 注册与绑定命令行工具

用法:
  agentctl register <agent.json>      注册/刷新 Agent manifest
  agentctl list                       列出已注册 Agent 与角色绑定
  agentctl bind <role> <agent>        绑定角色 coder|reviewer → Agent
  agentctl show                       当前绑定详情

环境变量:
  WORKFLOW_API_URL   workflow API 地址（默认 http://127.0.0.1:8001）
  AGENTCTL_UPDATED_BY  bind 操作的 updated_by（默认 agentctl）
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

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "agentctl: "+format+"\n", args...)
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
	case "register":
		if len(args) < 2 {
			fatalf("用法: agentctl register <agent.json>")
		}
		raw, err := os.ReadFile(args[1])
		if err != nil {
			fatalf("读取 %s: %v", args[1], err)
		}
		// manifest 校验：name/role/entrypoint/container 必填（复用 skillgateway 的 json 解析习惯）
		// role：coder | reviewer | analyst（analyst = 需求分析/拆卡，复用 pi-web，只读工具）
		var m struct {
			Name       string `json:"name"`
			Role       string `json:"role"`
			Entrypoint string `json:"entrypoint"`
			Container  string `json:"container"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			fatalf("manifest 不是合法 JSON: %v", err)
		}
		if m.Name == "" || (m.Role != "coder" && m.Role != "reviewer" && m.Role != "analyst") || m.Entrypoint == "" || m.Container == "" {
			fatalf("manifest 缺必填字段（name/role(coder|reviewer|analyst)/entrypoint/container）")
		}
		resp, data, err := httpDo("POST", "/agents", json.RawMessage(raw))
		if err != nil {
			fatalf("请求失败: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			fatalf("注册失败 (HTTP %d): %s", resp.StatusCode, string(data))
		}
		fmt.Printf("registered %s (role=%s, container=%s)\n", m.Name, m.Role, m.Container)

	case "list", "show":
		resp, data, err := httpDo("GET", "/agents", nil)
		if err != nil {
			fatalf("请求失败: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			fatalf("list 失败 (HTTP %d): %s", resp.StatusCode, string(data))
		}
		printJSON(json.RawMessage(data))

	case "bind":
		if len(args) < 3 {
			fatalf("用法: agentctl bind <role> <agent>")
		}
		role, agent := args[1], args[2]
		if role != "coder" && role != "reviewer" {
			fatalf("role 须为 coder|reviewer")
		}
		body := map[string]string{
			"agent_name": agent,
			"updated_by": envDefault("AGENTCTL_UPDATED_BY", "agentctl"),
		}
		resp, data, err := httpDo("POST", "/agents/"+role+"/bind", body)
		if err != nil {
			fatalf("请求失败: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			fatalf("绑定失败 (HTTP %d): %s", resp.StatusCode, string(data))
		}
		fmt.Printf("bound %s -> %s\n", role, agent)

	default:
		fmt.Print(usage)
		os.Exit(1)
	}
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
