// main_test.go — mk-judge /judge 入口回归测试（MK-16）。
//
// 重点覆盖 reviewer 指出的「两套数据」bug：body 同时给顶层 region 与 context.region
// 且不一致时，snapshot/纯度校验（用 Context 组装）与落库记录（用 JudgeRequest 字段）
// 必须同源——顶层 region/product 为权威值，无条件覆盖 Context。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/wetee/ai-workflow/internal/marketing"
)

// mockLLM 是 marketing.LLMClient 的测试实现：返回预设的合法 J1 输出。
type mockLLM struct{ out string }

func (m *mockLLM) ChatCompletion(_ context.Context, _, _ string) (string, error) {
	return m.out, nil
}

// validJ1Out 是合法 J1（judge_lead）输出，evidence.quote 必须是 post_text 子串。
const validJ1Out = `{
  "is_real_need": true, "need_type": "demand", "intervene": "yes",
  "pain": "latency", "freshness": "fresh", "score": 80, "confidence": 0.9,
  "evidence": [{"quote": "延迟很高", "why": "性能诉求"}],
  "reason": "真实需求"
}`

func newTestStore(t *testing.T) *marketing.Store {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&marketing.Judgment{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	return marketing.NewStore(db)
}

func doJudge(t *testing.T, store *marketing.Store, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.POST("/judge", handleJudge(store, &mockLLM{out: validJ1Out}, "test-model"))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/judge", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

// TestJudgeTopLevelRegionAuthoritative 验收：顶层 region 为权威值，无条件覆盖 Context。
// body 给 region=intl + context.region=cn（冲突）→ 落库记录与 snapshot 都按 intl 走，
// 不得出现「snapshot 按 cn、落库按 intl」的两套数据。
func TestJudgeTopLevelRegionAuthoritative(t *testing.T) {
	store := newTestStore(t)
	body := `{
		"region": "intl",
		"product": "network",
		"kind": "judge_lead",
		"context": { "region": "cn", "product": "shop", "posttext": "我们的节点延迟很高，怎么解决？" }
	}`
	w := doJudge(t, store, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var j marketing.Judgment
	if err := json.Unmarshal(w.Body.Bytes(), &j); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	// 落库记录 region/product 用顶层值（intl/network），不是 context 里的 cn/shop。
	if j.Region != "intl" {
		t.Errorf("落库 Region = %q, want intl（顶层权威值）", j.Region)
	}
	if j.Product != "network" {
		t.Errorf("落库 Product = %q, want network（顶层权威值）", j.Product)
	}
	// snapshot 也按 intl 组装（同源）：解析 snapshot 校验其 region。
	var snap marketing.ContextSnapshot
	if err := json.Unmarshal([]byte(j.ContextSnapshot), &snap); err != nil {
		t.Fatalf("解析 snapshot 失败: %v", err)
	}
	if snap.Region != "intl" {
		t.Errorf("snapshot Region = %q, want intl（须与落库记录同源）", snap.Region)
	}
	if snap.Product != "network" {
		t.Errorf("snapshot Product = %q, want network（须与落库记录同源）", snap.Product)
	}
	// 落库可取回。
	if _, err := store.ByID(context.Background(), j.ID); err != nil {
		t.Fatalf("落库记录取回失败: %v", err)
	}
}

// TestJudgeMissingProduct 验收：顶层 product 为空 → Context.Product 被赋空 →
// AssembleContext 报「非法 product」（500），不再静默丢数据。
func TestJudgeMissingProduct(t *testing.T) {
	store := newTestStore(t)
	body := `{"region": "intl", "kind": "judge_lead", "context": {"posttext": "延迟很高"}}`
	w := doJudge(t, store, body)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500（product 缺失 → 非法 product）; body=%s", w.Code, w.Body.String())
	}
}
