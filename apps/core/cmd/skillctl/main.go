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

环境变量:
  WORKFLOW_API_URL   workflow API 地址（默认 http://127.0.0.1:8001）
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
