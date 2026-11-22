// Package main is the agent-ocr binary.
//
// agent-ocr：reviewer Agent adapter（open-code-review 实现），统一 JSON 协议（stdin → stdout）。
package main

import (
	"os"

	"github.com/wetee/ai-workflow/internal/agentcli"
)

func main() {
	if err := agentcli.RunOCR(os.Stdin, os.Stdout, os.Stderr); err != nil {
		os.Exit(1)
	}
}
