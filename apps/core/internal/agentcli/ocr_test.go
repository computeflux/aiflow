package agentcli

import (
	"encoding/json"
	"strings"
	"testing"
)

// 真实结构取自 ocr v1.13.1 的会话输出（review_item_done.comments）：
// 正文在 content、行号在 start_line、严重度在 severity、建议补丁在 suggestion_code。
const ocrV113Fixture = `{"comments":[
 {"path":"apps/actives/messages.go","start_line":65,"end_line":68,"severity":"critical","category":"concurrency",
  "content":"并发安全：AddMessage 的未读数读-改-写会丢失更新。","suggestion_code":"s.mu.Lock()"},
 {"path":"apps/actives/cmd/server/main.go","start_line":190,"severity":"high","category":"security",
  "content":"签名可重放。","suggestion_code":"nonce"},
 {"path":"apps/actives/messages.go","start_line":88,"severity":"low","content":"命名建议：改名 CleanupMessagesBefore。"},
 {"path":"apps/actives/messages.go","start_line":65,"end_line":68,"severity":"critical","category":"concurrency",
  "content":"并发安全：AddMessage 的未读数读-改-写会丢失更新。","suggestion_code":"s.mu.Lock()"}
]}`

// TestParseOCRCommentsV113 ocr v1.13 结构：正文/行号/严重度映射 + 建议改法 + 去重。
// 回归要点：正文取错键（旧实现只认 message）会让 message 全空，返工反馈白烧一轮。
func TestParseOCRCommentsV113(t *testing.T) {
	comments := parseOCRComments(ocrV113Fixture)
	if len(comments) != 3 {
		t.Fatalf("重复评论应去重为 3 条，得到 %d 条: %+v", len(comments), comments)
	}
	for _, c := range comments {
		if strings.TrimSpace(c.Message) == "" {
			t.Fatalf("评论正文不得为空: %+v", c)
		}
	}

	first := comments[0]
	if first.File != "apps/actives/messages.go" || first.Line != 65 || first.Level != "error" {
		t.Fatalf("file/line/level 映射错误: %+v", first)
	}
	if !strings.HasPrefix(first.Message, "[concurrency] 并发安全") {
		t.Fatalf("category 应作为前缀: %q", first.Message)
	}
	if !strings.Contains(first.Message, "建议改法") || !strings.Contains(first.Message, "s.mu.Lock()") {
		t.Fatalf("suggestion_code 应附加到正文: %q", first.Message)
	}
	if second := comments[1]; second.Line != 190 || second.Level != "error" || second.File != "apps/actives/cmd/server/main.go" {
		t.Fatalf("第二条映射错误: %+v", second)
	}
	if third := comments[2]; third.Level != "warning" {
		t.Fatalf("severity=low 应归一为 warning: %+v", third)
	}
}

// TestParseOCRCommentsLegacy 旧版字段（file/line/message）仍要能解析。
func TestParseOCRCommentsLegacy(t *testing.T) {
	comments := parseOCRComments(`{"comments":[{"file":"a.go","line":7,"level":"medium","message":"旧版字段"}]}`)
	if len(comments) != 1 {
		t.Fatalf("应得 1 条，得到 %d 条", len(comments))
	}
	c := comments[0]
	if c.File != "a.go" || c.Line != 7 || c.Level != "warning" || c.Message != "旧版字段" {
		t.Fatalf("旧版字段解析错误: %+v", c)
	}
}

// TestParseOCRCommentsFallbacks 非 JSON 纯文本 / 未知结构 JSON / 空输出都要兜底且不产生空正文。
func TestParseOCRCommentsFallbacks(t *testing.T) {
	if got := parseOCRComments("plain text report from ocr"); len(got) != 1 ||
		got[0].Level != "info" || strings.TrimSpace(got[0].Message) == "" {
		t.Fatalf("非 JSON 输出兜底错误: %+v", got)
	}
	got := parseOCRComments(`{"weird":{"text":"内容在奇怪的地方"}}`)
	if len(got) != 1 || strings.TrimSpace(got[0].Message) == "" || !strings.Contains(got[0].Message, "内容在奇怪的地方") {
		t.Fatalf("未知结构 JSON 兜底错误: %+v", got)
	}
	if got := parseOCRComments("   "); len(got) != 0 {
		t.Fatalf("空输出应为空评论列表: %+v", got)
	}
}

// TestParseOCRCommentsTopLevelArray 顶层数组（部分版本输出形态）。
func TestParseOCRCommentsTopLevelArray(t *testing.T) {
	got := parseOCRComments(`[{"path":"x.go","start_line":3,"severity":"high","content":"问题"}]`)
	if len(got) != 1 || got[0].File != "x.go" || got[0].Line != 3 || got[0].Level != "error" {
		t.Fatalf("顶层数组解析错误: %+v", got)
	}
}

// TestNormOCRLevel 严重度词表归一（未识别一律降级 info，保证 schema 不漂移）。
func TestNormOCRLevel(t *testing.T) {
	cases := map[string]string{
		"critical": "error", "FATAL": "error", "blocker": "error", "P0": "error",
		"medium": "warning", "minor": "warning", "high": "error",
		"info": "info", "nit": "info", "": "info", "未知词": "info",
	}
	for in, want := range cases {
		if got := normOCRLevel(in); got != want {
			t.Errorf("normOCRLevel(%q)=%q，期望 %q", in, got, want)
		}
	}
}

// TestReviewOutputJSON 输出结构满足 driver schema（required ok；comments[].required level+message）。
func TestReviewOutputJSON(t *testing.T) {
	out := ReviewOutput{OK: true, Comments: parseOCRComments(ocrV113Fixture)}
	b, err := jsonCompact(out)
	if err != nil {
		t.Fatalf("jsonCompact: %v", err)
	}
	var parsed struct {
		OK       bool `json:"ok"`
		Comments []struct {
			Level   string `json:"level"`
			Message string `json:"message"`
		} `json:"comments"`
	}
	if err := json.Unmarshal([]byte(b), &parsed); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if !parsed.OK || len(parsed.Comments) != 3 {
		t.Fatalf("输出结构错误: %s", b)
	}
	for _, c := range parsed.Comments {
		if c.Level == "" || c.Message == "" {
			t.Fatalf("level/message 不得为空: %s", b)
		}
	}
	if strings.Contains(b, "\\u003") {
		t.Fatalf("输出不应转义 HTML 字符: %s", b)
	}
}

// TestParseOCRCommentsRunManifest ocr v1.13 的 run manifest：
//   - comments 为空 = 评审完且无问题（不再塞兜底 info 评论）；
//   - status=skipped / failed 与「干净通过」必须可区分（failed 要报 ok:false，不能当通过放行）。
func TestParseOCRCommentsRunManifest(t *testing.T) {
	skipped := `{"status":"skipped","comments":[],"message":"Review skipped: no items were selected.",` +
		`"manifest":{"coverage":{"completed":[],"failed":[],"selected":[]}}}`
	if got := parseOCRComments(skipped); len(got) != 0 {
		t.Fatalf("空 comments 应为空结果（不塞兜底评论），得到 %+v", got)
	}
	if status, failed, ok := ocrRunManifest(skipped); !ok || status != "skipped" || failed != 0 {
		t.Fatalf("manifest 识别错误: status=%q failed=%d ok=%v", status, failed, ok)
	}

	failed := `{"status":"failed","comments":[],` +
		`"manifest":{"coverage":{"failed":[{"path":"a.go"}],"selected":[{"path":"a.go"}]}}}`
	if status, n, ok := ocrRunManifest(failed); !ok || status != "failed" || n != 1 {
		t.Fatalf("failed manifest 识别错误: status=%q failed=%d ok=%v", status, n, ok)
	}

	if _, _, ok := ocrRunManifest(ocrV113Fixture); ok {
		t.Fatalf("旧版 {comments:[...]} 结构不应被判为 run manifest")
	}
	if _, _, ok := ocrRunManifest("plain text"); ok {
		t.Fatalf("非 JSON 不应被判为 run manifest")
	}
}

// TestTruncateSuggestion suggestion_code 超长应截断（避免反馈体爆 token）。
func TestTruncateSuggestion(t *testing.T) {
	long := strings.Repeat("x", commentSuggestionMax+500)
	got := parseOCRComments(`{"comments":[{"path":"a.go","line":1,"severity":"high","content":"c","suggestion_code":"` + long + `"}]}`)
	if len(got) != 1 {
		t.Fatalf("应得 1 条: %+v", got)
	}
	if len(got[0].Message) > commentMessageMax {
		t.Fatalf("正文应被截断到 %d 字符内，实际 %d", commentMessageMax, len(got[0].Message))
	}
}
