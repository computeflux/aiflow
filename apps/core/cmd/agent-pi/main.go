// Package main is the agent-pi binary.
//
// agent-pi：coder Agent adapter（pi-web 实现），统一 JSON 协议（stdin → stdout）。
package main

import (
	"os"

	"github.com/wetee/ai-workflow/internal/agentcli"
)

func main() {
	if err := agentcli.RunPi(os.Stdin, os.Stdout, os.Stderr); err != nil {
		os.Exit(1)
	}
}
