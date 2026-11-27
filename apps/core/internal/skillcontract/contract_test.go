package skillcontract

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// skillSpecFile 是 pyreplay 'like' skill 的契约文件（相对 apps/core 模块根）。
const skillSpecFile = "../pyreplay/skill.json"

// readLikeSchema 读出 skill.json 里的 input_schema / output_schema（两侧契约同源比对用）。
func readLikeSchema(t *testing.T) (input map[string]any, output map[string]any) {
	t.Helper()

	path, err := filepath.Abs(skillSpecFile)
	if err != nil {
		t.Fatalf("resolve %s: %v", skillSpecFile, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("读不到 pyreplay skill.json（%v）：跨项目契约比对跳过", err)
	}

	var spec struct {
		Name         string         `json:"name"`
		InputSchema  map[string]any `json:"input_schema"`
		OutputSchema map[string]any `json:"output_schema"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&spec); err != nil {
		t.Fatalf("decode skill.json: %v", err)
	}
	if spec.Name != "like" {
		t.Fatalf("skill.json name = %q, want like", spec.Name)
	}
	return spec.InputSchema, spec.OutputSchema
}

// props 取 schema 的 properties 映射。
func props(t *testing.T, schema map[string]any) map[string]any {
	t.Helper()
	p, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema 缺 properties: %v", schema)
	}
	return p
}

// TestLikeInputMatchesPyreplaySchema 是 M3-2 的跨语言契约测试：Go 侧构造的 like 输入
// 必须落在 pyreplay skill.json 声明的字段与取值区间内（字段名、minimum、maximum）。
func TestLikeInputMatchesPyreplaySchema(t *testing.T) {
	inputSchema, _ := readLikeSchema(t)
	p := props(t, inputSchema)

	for _, field := range []string{FieldPostURL, FieldLikeCount} {
		if _, ok := p[field]; !ok {
			t.Fatalf("input_schema.properties 缺字段 %q（Go 侧常量与 skill.json 已漂移）", field)
		}
	}

	// required：post_url 必填（Go 侧不越权填默认值，必填校验交给网关）
	required, _ := inputSchema["required"].([]any)
	found := false
	for _, r := range required {
		if r == FieldPostURL {
			found = true
		}
	}
	if !found {
		t.Fatalf("input_schema.required = %v，应包含 %q", required, FieldPostURL)
	}

	countSchema, ok := p[FieldLikeCount].(map[string]any)
	if !ok {
		t.Fatalf("input_schema.properties.%s 不是对象", FieldLikeCount)
	}
	if min, ok := countSchema["minimum"].(json.Number); !ok || min.String() != fmt.Sprint(LikeCountDefault) {
		t.Fatalf("schema minimum = %v, want LikeCountDefault=%d", countSchema["minimum"], LikeCountDefault)
	}
	if max, ok := countSchema["maximum"].(json.Number); !ok || max.String() != fmt.Sprint(LikeCountMax) {
		t.Fatalf("schema maximum = %v, want LikeCountMax=%d", countSchema["maximum"], LikeCountMax)
	}

	for _, c := range []struct {
		count int
		want  int
	}{
		{0, LikeCountDefault},
		{-7, LikeCountDefault},
		{3, 3},
		{LikeCountMax, LikeCountMax},
		{LikeCountMax + 50, LikeCountMax},
	} {
		in := LikeInput("https://x.com/i/status/1", c.count)
		got, ok := in[FieldLikeCount].(int)
		if !ok {
			t.Fatalf("LikeInput(%q, %d) 的 like_count 不是 int: %#v", "url", c.count, in[FieldLikeCount])
		}
		if got != c.want {
			t.Errorf("LikeInput count=%d → like_count=%d, want %d", c.count, got, c.want)
		}
		if got < LikeCountDefault || got > LikeCountMax {
			t.Errorf("like_count=%d 越界 [%d, %d]", got, LikeCountDefault, LikeCountMax)
		}
	}
}

// TestOutputMatchesPyreplaySchema 校验输出侧：统一契约读的是 ok / reason / tweet_id /
// attempts / success_count，这些字段必须都在 pyreplay 的 output_schema 里声明。
func TestOutputMatchesPyreplaySchema(t *testing.T) {
	_, outputSchema := readLikeSchema(t)
	p := props(t, outputSchema)

	for _, field := range []string{FieldOK, FieldReason, "tweet_id", "attempts", "success_count"} {
		if _, ok := p[field]; !ok {
			t.Fatalf("output_schema.properties 缺字段 %q（skillcontract.Output 读它）", field)
		}
	}
	if typ, _ := p[FieldOK].(map[string]any)["type"].(string); typ != "boolean" {
		t.Fatalf("output_schema.%s type = %v, want boolean", FieldOK, p[FieldOK])
	}

	// 网关返回的响应体（skill 原始输出 + 注入的元信息）按契约解析
	raw := `{"ok":true,"reason":"","tweet_id":"123","attempts":2,"success_count":2,
	        "results":[{"tweet_id":"123","success":true,"error":""}],"skill":"like","version":"1.0.0"}`
	var out Output
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("unmarshal skill output: %v", err)
	}
	if !out.OK() {
		t.Fatal("OK() = false, want true")
	}
	if out.Str("tweet_id") != "123" {
		t.Fatalf("tweet_id = %q", out.Str("tweet_id"))
	}
	if out.Int("success_count", -1) != 2 {
		t.Fatalf("success_count = %d", out.Int("success_count", -1))
	}

	failed := Output{"ok": false, "reason": "all like attempts failed"}
	if failed.OK() {
		t.Fatal("OK() = true, want false")
	}
	if failed.Reason() != "all like attempts failed" {
		t.Fatalf("Reason() = %q", failed.Reason())
	}

	// 缺 ok / 类型不对 → 一律视为失败（与 skillgateway「output missing boolean field 'ok'」同口径）
	for _, bad := range []Output{nil, {}, {"ok": "true"}, {"ok": 1}} {
		if bad.OK() {
			t.Errorf("Output(%v).OK() = true, want false", bad)
		}
	}
}

// TestInvokeInputChaining 校验通用输入构造器：With 写键、Map 交回底层 map。
func TestInvokeInputChaining(t *testing.T) {
	in := NewInvokeInput().With("keyword", "ai").With(FieldLikeCount, ClampLikeCount(99))
	m := in.Map()
	if len(m) != 2 || m["keyword"] != "ai" || m[FieldLikeCount] != LikeCountMax {
		t.Fatalf("InvokeInput = %v", m)
	}
}
