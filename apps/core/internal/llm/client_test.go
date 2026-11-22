package llm

import (
	"encoding/json"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

// TestSanitizeMessages 锁定「空 content 会被 SDK 序列化成 content:null → 上游 400」这个陷阱：
// 归一后不得再出现 null content（否则 AI 助手会静默空白，2026-10-18 实测事故）。
func TestSanitizeMessages(t *testing.T) {
	in := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
		{Role: openai.ChatMessageRoleAssistant, Content: ""}, // 空 assistant（工具调用轮次）
		{Role: openai.ChatMessageRoleUser, Content: ""},      // 空 user
	}
	out := sanitizeMessages(in)
	if len(out) != len(in) {
		t.Fatalf("消息条数不应变化：in=%d out=%d", len(in), len(out))
	}
	for i, m := range out {
		if strings.TrimSpace(m.Content) == "" {
			t.Fatalf("第 %d 条归一后仍为空内容", i)
		}
	}
	if out[0].Content != "sys" || out[1].Content != "hi" {
		t.Fatalf("非空内容不应被改写：%+v", out[:2])
	}

	// 序列化后不得出现 content:null（上游对它直接 400）
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), `"content":null`) {
		t.Fatalf("序列化出现 content:null（上游会 400）: %s", b)
	}
}
