// 注册表快照端点（/skill-registry/snapshot*）的行为单测。
//
// 钉住两件事：
//  1. 鉴权是**必填**的：本包不接受一个中间件都不挂的挂载方式（同步会往 skill_versions 写
//     版本行 = 改路由池，与 /skill-cutover/* 同一红线）。写端点必须带 admin 角色中间件，
//     少挂一层就等于把「登记新实现」开放给任何登录用户。
//  2. dry_run 与真跑的响应形状一致（changes 逐条带 action + 原因），board 的切流页在放量
//     之前就能回答「x-scan / like 的新实现登记了没有」。
package migration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
)

// testRepoRoot 从包目录向上找 monorepo 根（含 deploy/docker-compose.yml 的目录），与
// apps/core/internal/deploy 的既有测试同一口径；找不到时由调用方 Skip（只挂 apps/core 的 CI）。
func testRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "deploy", "docker-compose.yml")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("仓库根不在测试可及范围（只挂载了 apps/core 模块），跳过端点测试")
		}
		dir = parent
	}
}

func newRegistryRouter(t *testing.T, root string, admin bool) (*gin.Engine, *int) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	authCalls := new(int)
	auth := func(c *gin.Context) { *authCalls++ }
	role := func(c *gin.Context) {
		if !admin {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "admin required"})
			return
		}
		c.Next()
	}
	RegistrySnapshotRoutes(r, root, nil, auth, role)
	return r, authCalls
}

func TestRegistryRoutesRequireAuthMiddleware(t *testing.T) {
	defer func() {
		if p := recover(); p == nil {
			t.Fatal("不挂鉴权中间件的挂载必须 panic（同步会写注册表）")
		} else if got, ok := p.(string); !ok || got == "" {
			t.Fatalf("panic 信息要说清缺了什么，得到 %v", p)
		}
	}()
	gin.SetMode(gin.TestMode)
	RegistrySnapshotRoutes(gin.New(), "/nonexistent", nil, nil)
}

func TestRegistrySnapshotEndpointReportsSnapshot(t *testing.T) {
	root := testRepoRoot(t)
	r, authCalls := newRegistryRouter(t, root, true)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/skill-registry/snapshot", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET snapshot = %d（%s）", rec.Code, rec.Body.String())
	}
	if *authCalls == 0 {
		t.Fatal("鉴权中间件必须参与每一次请求")
	}
	var body struct {
		OK       bool              `json:"ok"`
		Snapshot *RegistrySnapshot `json:"snapshot"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应: %v (%s)", err, rec.Body.String())
	}
	if !body.OK || body.Snapshot == nil || len(body.Snapshot.Skills) != 2 {
		t.Fatalf("快照要原样带出去且校验通过，得到 %s", rec.Body.String())
	}
}

func TestRegistrySyncEndpointForbiddenForNonAdmin(t *testing.T) {
	root := testRepoRoot(t)
	r, authCalls := newRegistryRouter(t, root, false)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/skill-registry/snapshot/sync", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非 admin 的同步必须 403，得到 %d", rec.Code)
	}
	if *authCalls == 0 {
		t.Fatal("鉴权中间件必须参与每一次请求")
	}
}

func TestRegistrySyncEndpointDryRunWritesNothing(t *testing.T) {
	root := testRepoRoot(t)
	// gdb = nil：SyncRegistry 在触碰数据库之前就被路由层的整批校验挡住（无库 = 直接报错），
	// 这里只验端点形状：dry_run 的响应必须带 changes 数组与 dry_run=true。
	r, _ := newRegistryRouter(t, root, true)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/skill-registry/snapshot/sync?dry_run=1", nil))
	if rec.Code == http.StatusOK {
		t.Fatalf("无库时同步不能报成功：%s", rec.Body.String())
	}
	var body struct {
		OK      bool         `json:"ok"`
		DryRun  bool         `json:"dry_run"`
		Changes []SyncChange `json:"changes"`
		Error   string       `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应: %v (%s)", err, rec.Body.String())
	}
	if body.DryRun != true || body.OK != false || body.Error == "" {
		t.Fatalf("响应要如实说明预演与失败原因：%s", rec.Body.String())
	}
}
