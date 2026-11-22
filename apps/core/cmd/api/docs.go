package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wetee/ai-workflow/internal/authn"
	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/docstore"
	"github.com/wetee/ai-workflow/internal/llm"
)

// 产品板块「文档」：仓库 docs/ 目录的树形浏览 / 编辑 / 预览 + AI 生成（2026-10-19）。
//
//	GET    /docs/tree             目录树（含 raw 前缀）
//	POST   /docs/raw-url          取单个文件的 raw 预览 URL（短时签名，见下）
//	GET    /docs/file?path=       读文件（文本；二进制/超大只回元信息）
//	PUT    /docs/file             保存（覆盖）
//	POST   /docs/file             新建文件/目录
//	DELETE /docs/file?path=       删除（目录须为空）
//	POST   /docs/rename           重命名/移动
//	GET    /docs/raw/:sig/*path   raw 静态（图片/HTML；**不走 Bearer**，见签名说明）
//	POST   /docs/ai               AI 生成/改写文档正文（不落盘，返回内容由用户确认后保存）
//
// raw 为什么要签名：HTML 预览要用 iframe 直出 `docs/ppt/xx/index.html`，其中的相对引用
// （images/xxx.png）由浏览器直接请求——这些请求**带不上 Authorization 头**，而 board 的
// nginx 同源代理对公网开放，裸放等于把 docs 全量公开。故用短时 HMAC 签名（签名写在**路径**里，
// 相对引用会连路径前缀一起保留），TTL 1h，仅在需要预览时按需签发。
//
// 拆解为任务：直接复用 POST /product/decompose（前端把文档正文当 text 传），本文件不重复实现。
func registerDocsRoutes(r *gin.Engine, store *docstore.Store, llmClient *llm.Client, database *db.DB, requireAuth gin.HandlerFunc) {
	if store == nil {
		r.GET("/docs/tree", func(ctx *gin.Context) {
			ctx.JSON(http.StatusServiceUnavailable, gin.H{"error": "文档功能未启用（DOCS_DIR/REPO_ROOT 未配置）"})
		})
		return
	}
	signer := newRawSigner()

	// bad 统一错误映射：路径非法/不存在 → 400/404，其余 → 500（细节进 error_logs）。
	fail := func(ctx *gin.Context, err error) {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "非法路径"),
			strings.Contains(msg, "越界"),
			strings.Contains(msg, "仅支持相对"),
			strings.Contains(msg, "不可访问"),
			strings.Contains(msg, "不允许写"),
			strings.Contains(msg, "已存在"),
			strings.Contains(msg, "超过上限"),
			strings.Contains(msg, "目录非空"),
			strings.Contains(msg, "目标是目录"):
			ctx.JSON(http.StatusBadRequest, gin.H{"error": msg})
		case strings.Contains(msg, "不存在"):
			ctx.JSON(http.StatusNotFound, gin.H{"error": msg})
		default:
			serverError(database, "docs", err)
		}
	}

	r.GET("/docs/tree", requireAuth, func(ctx *gin.Context) {
		tree, err := store.Tree()
		if err != nil {
			fail(ctx, err)
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"root": store.Root(), "tree": tree})
	})

	// raw-url：按需签发预览 URL（token 只代表"短时可读 docs"，路径拼在 token 后，
	// HTML 里的相对引用会连 token 段一起保留 → 图片/CSS 能正常加载）。
	r.POST("/docs/raw-url", requireAuth, func(ctx *gin.Context) {
		var body struct {
			Path string `json:"path"`
		}
		if err := ctx.ShouldBindJSON(&body); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "path is required"})
			return
		}
		abs, err := store.ResolveForRaw(body.Path)
		if err != nil {
			fail(ctx, err)
			return
		}
		token, exp := signer.sign()
		ctx.JSON(http.StatusOK, gin.H{
			"url":        "/docs/raw/" + token + "/" + store.Rel(abs),
			"expires_at": exp.Format(time.RFC3339),
		})
	})

	r.GET("/docs/file", requireAuth, func(ctx *gin.Context) {
		fc, err := store.Read(ctx.Query("path"))
		if err != nil {
			fail(ctx, err)
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"file": fc})
	})

	r.PUT("/docs/file", requireAuth, func(ctx *gin.Context) {
		var body struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := ctx.ShouldBindJSON(&body); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "path/content is required"})
			return
		}
		fc, err := store.Write(body.Path, body.Content)
		if err != nil {
			fail(ctx, err)
			return
		}
		log.Printf("docs: 保存 %s（%d 字节，by %s）", fc.Path, fc.Size, authUserOr(ctx, "board"))
		ctx.JSON(http.StatusOK, gin.H{"file": fc})
	})

	r.POST("/docs/file", requireAuth, func(ctx *gin.Context) {
		var body struct {
			Path  string `json:"path"`
			IsDir bool   `json:"dir"`
		}
		if err := ctx.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.Path) == "" {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "path is required"})
			return
		}
		node, err := store.Create(body.Path, body.IsDir)
		if err != nil {
			fail(ctx, err)
			return
		}
		log.Printf("docs: 新建 %s（%s，by %s）", node.Path, node.Type, authUserOr(ctx, "board"))
		ctx.JSON(http.StatusCreated, gin.H{"node": node})
	})

	r.DELETE("/docs/file", requireAuth, func(ctx *gin.Context) {
		p := ctx.Query("path")
		if err := store.Delete(p); err != nil {
			fail(ctx, err)
			return
		}
		log.Printf("docs: 删除 %s（by %s）", p, authUserOr(ctx, "board"))
		ctx.JSON(http.StatusOK, gin.H{"message": "deleted"})
	})

	r.POST("/docs/rename", requireAuth, func(ctx *gin.Context) {
		var body struct {
			From string `json:"from"`
			To   string `json:"to"`
		}
		if err := ctx.ShouldBindJSON(&body); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "from/to is required"})
			return
		}
		node, err := store.Rename(body.From, body.To)
		if err != nil {
			fail(ctx, err)
			return
		}
		log.Printf("docs: 重命名 %s → %s（by %s）", body.From, body.To, authUserOr(ctx, "board"))
		ctx.JSON(http.StatusOK, gin.H{"node": node})
	})

	// raw 静态：签名校验（不做 Bearer，见文件头说明）；路径仍走 docstore 红线。
	r.GET("/docs/raw/:sig/*rel", func(ctx *gin.Context) {
		if err := signer.verify(ctx.Param("sig")); err != nil {
			ctx.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
			return
		}
		reqPath := strings.TrimPrefix(ctx.Param("rel"), "/")
		abs, err := store.ResolveForRaw(reqPath)
		if err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		ctx.Header("Cache-Control", "private, max-age=300")
		ctx.File(abs)
	})

	r.POST("/docs/ai", requireAuth, func(ctx *gin.Context) {
		if llmClient == nil {
			ctx.JSON(http.StatusServiceUnavailable, gin.H{"error": "LLM 未配置"})
			return
		}
		var body struct {
			Mode   string `json:"mode"` // generate | edit
			Prompt string `json:"prompt"`
			Path   string `json:"path"` // edit 模式：被改写文件；generate 模式：可选的落点提示
		}
		if err := ctx.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.Prompt) == "" {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "prompt is required"})
			return
		}
		sys, user, err := buildDocAIPrompt(store, body.Mode, body.Prompt, body.Path)
		if err != nil {
			fail(ctx, err)
			return
		}
		out, err := llmClient.ChatCompletion(ctx, sys, user)
		if err != nil {
			log.Printf("docs/ai: LLM 失败：%v", err)
			ctx.JSON(http.StatusBadGateway, gin.H{"error": "AI 生成失败：" + err.Error()})
			return
		}
		content := stripCodeFence(out)
		log.Printf("docs/ai: mode=%s path=%s prompt=%d字 → %d字（by %s）",
			body.Mode, body.Path, len([]rune(body.Prompt)), len([]rune(content)), authUserOr(ctx, "board"))
		ctx.JSON(http.StatusOK, gin.H{"content": content, "mode": body.Mode, "path": body.Path})
	})
}

// ---- AI 提示词（模式：生成新文档 / 按指示改写现有文档） ----

func buildDocAIPrompt(store *docstore.Store, mode, prompt, filePath string) (system, user string, err error) {
	treeText := "(目录树不可用)"
	if tree, terr := store.Tree(); terr == nil {
		treeText = docTreePaths(&tree)
	}
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "generate":
		system = "你是仓库文档助手（文档都以 Markdown 存放在 docs/ 目录）。请按用户要求直接产出**完整可用的文档正文**：\n" +
			"- 只输出正文本身：不要寒暄、不要解释、不要用代码围栏把整篇包起来；\n" +
			"- 结构清晰：一级标题 + 小节；清单、表格、代码块按需使用；\n" +
			"- 中文、简洁、以事实为准；不确定的内容标注「（待确认）」，**不要编造**接口名/文件名/数字；\n" +
			"- 需要引用代码或配置时给出真实路径（相对仓库根，如 apps/core/internal/...）。"
		user = "docs/ 现有文件（供你参考命名与结构，避免重复造轮子）：\n" + treeText + "\n\n用户要求：\n" + prompt
		return system, user, nil
	case "edit":
		if strings.TrimSpace(filePath) == "" {
			return "", "", fmt.Errorf("edit 模式需要 path")
		}
		fc, rerr := store.Read(filePath)
		if rerr != nil {
			return "", "", rerr
		}
		if !fc.Editable {
			return "", "", fmt.Errorf("该文件不可编辑（二进制或超大）")
		}
		content := fc.Content
		if len(content) > 60_000 {
			content = content[:60_000] + "\n\n…（正文过长已截断）"
		}
		system = "你是仓库文档助手。用户会给你**现有文档全文**与修改要求；请输出**修改后的完整文档**：\n" +
			"- 只输出文档正文本身（不要解释、不要代码围栏包裹整篇）；\n" +
			"- 未要求改动的部分保持原样；保持原文的 Markdown 风格与语言。"
		user = "文档路径：" + filePath + "\n\n修改要求：\n" + prompt + "\n\n现有正文：\n" + content
		return system, user, nil
	default:
		return "", "", fmt.Errorf("未知模式：%s（支持 generate / edit）", mode)
	}
}

// docTreePaths 把目录树渲染成紧凑的路径清单（喂给 LLM 做上下文；限制深度与条数防爆）。
func docTreePaths(n *docstore.Node) string {
	const (
		maxLines  = 400
		maxDepth  = 3
		indentStr = "  "
	)
	var out []string
	var walk func(node *docstore.Node, depth int)
	walk = func(node *docstore.Node, depth int) {
		for _, c := range node.Children {
			if len(out) >= maxLines {
				return
			}
			line := strings.Repeat(indentStr, depth) + c.Name
			if c.Type == "dir" {
				line += "/"
			}
			out = append(out, line)
			if c.Type == "dir" && depth+1 < maxDepth {
				walk(&c, depth+1)
			}
		}
	}
	walk(n, 0)
	if len(out) == 0 {
		return "(空目录)"
	}
	if len(out) >= maxLines {
		out = append(out, "…（仅列前 400 项）")
	}
	return strings.Join(out, "\n")
}

// stripCodeFence 去掉 LLM 偶发把整篇包进 ``` 围栏的情况（对比产品情报的同类处理）。
func stripCodeFence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return t
	}
	lines := strings.Split(t, "\n")
	if len(lines) < 2 {
		return t
	}
	// 首行 ```lang → 去首行；末行 ``` → 去末行
	lines = lines[1:]
	if last := strings.TrimSpace(lines[len(lines)-1]); last == "```" {
		lines = lines[:len(lines)-1]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// ---- raw 短时签名（HMAC） ----

// rawSigner 给 /docs/raw 签发短时访问凭据：token = "<expUnix>-<hex(HMAC(exp))>"。
// 密钥默认进程内随机（重启失效，页面每次预览按需重签即可）；可用 DOCS_RAW_SECRET 固定。
type rawSigner struct {
	secret []byte
	ttl    time.Duration
}

func newRawSigner() *rawSigner {
	secret := []byte(strings.TrimSpace(os.Getenv("DOCS_RAW_SECRET")))
	if len(secret) == 0 {
		secret = make([]byte, 32)
		_, _ = rand.Read(secret)
	}
	return &rawSigner{secret: secret, ttl: time.Hour}
}

func (s *rawSigner) mac(exp int64) string {
	h := hmac.New(sha256.New, s.secret)
	fmt.Fprintf(h, "%d", exp)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// sign 返回 URL 段（"<exp>-<sig>"）与过期时间。凭据代表"短时可读 docs"（不绑定具体文件：
// HTML 内相对引用需要同一凭据前缀），TTL 1h。
func (s *rawSigner) sign() (string, time.Time) {
	exp := time.Now().Add(s.ttl)
	return fmt.Sprintf("%d-%s", exp.Unix(), s.mac(exp.Unix())), exp
}

// verify 校验签名（过期/伪造 → 错误）。
func (s *rawSigner) verify(token string) error {
	segs := strings.SplitN(token, "-", 2)
	if len(segs) != 2 {
		return fmt.Errorf("非法的预览签名")
	}
	exp, err := strconv.ParseInt(segs[0], 10, 64)
	if err != nil {
		return fmt.Errorf("非法的预览签名")
	}
	if time.Now().Unix() >= exp {
		return fmt.Errorf("预览链接已过期（%s），请重新打开预览", time.Unix(exp, 0).Format("15:04:05"))
	}
	if !hmac.Equal([]byte(segs[1]), []byte(s.mac(exp))) {
		return fmt.Errorf("预览签名无效")
	}
	return nil
}

// authUserOr 取操作人（未鉴权时用兜底串）。
func authUserOr(ctx *gin.Context, def string) string {
	if u := authn.UserID(ctx); strings.TrimSpace(u) != "" {
		return u
	}
	return def
}
