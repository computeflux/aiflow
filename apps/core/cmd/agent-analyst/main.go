// Package main is the agent-analyst binary.
//
// agent-analyst：需求分析 Agent adapter（pi-web 实现），统一 JSON 协议（stdin → stdout）。
package main

import (
	"os"

	"github.com/wetee/ai-workflow/internal/agentcli"
)

func main() {
	if err := agentcli.RunAnalyze(os.Stdin, os.Stdout, os.Stderr); err != nil {
		os.Exit(1)
	}
}
