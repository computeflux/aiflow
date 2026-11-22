// Package product 是 S7 产品情报核心：文档入库/向量化、产品 Agent 对话与任务拆解（secret 脱敏红线）。
package product

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/wetee/ai-workflow/internal/llm"
	"github.com/wetee/ai-workflow/internal/qdrant"
)

// Agent 是产品 Agent（S7 §3.2）：product 角色，业务智能体，**不写代码**。
// 上下文 = 用户角色（决定可见密级）+ RAG 命中（CRAG，按密级过滤）+ 产品文档上下文。
// 红线：secret 级内容不送外部 LLM（仅本地检索 / 脱敏后引用）。
type Agent struct {
	Store  *Store
	Qdrant *qdrant.Client
	LLM    *llm.Client
}

// NewAgent 构造产品角色业务智能体（RAG 检索 + LLM 对话/文档生成）。
func NewAgent(store *Store, qc *qdrant.Client, llmClient *llm.Client) *Agent {
	return &Agent{Store: store, Qdrant: qc, LLM: llmClient}
}

// Message 是对话消息。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// SourceRef 是回答的可溯源来源（RAG 命中文档 + 时间戳）。
// secret 来源 Snippet 为空（红线：正文不暴露），仅保留标题/密级/时间。
type SourceRef struct {
	DocTitle        string `json:"doc_title"`
	SourceType      string `json:"source_type"`
	Confidentiality string `json:"confidentiality"`
	UpdatedAt       string `json:"updated_at,omitempty"`
	Snippet         string `json:"snippet,omitempty"`
}

// ChatResult 是产品 Agent 对话结果。
type ChatResult struct {
	Answer  string      `json:"answer"`
	Sources []SourceRef `json:"sources"`
	Found   bool        `json:"found"`
}

// Chat 产品 Agent 对话：按用户角色过滤密级 → RAG 检索 → 组装上下文（secret 脱敏）→ LLM 归纳。
// 无命中时明确回答"知识库中没有找到相关信息"。
func (a *Agent) Chat(ctx context.Context, roles []string, query string, k int) (*ChatResult, error) {
	if a.LLM == nil || !a.LLM.EmbeddingsEnabled() {
		return &ChatResult{Answer: "知识库搜索不可用（嵌入模型未配置）。", Found: false}, nil
	}
	if k <= 0 {
		k = 5
	}
	levels := ConfidentialityLevelsFor(roles)

	vector, err := a.LLM.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	docs, err := a.Qdrant.SearchFiltered(ctx, vector, uint64(k), nil, levels)
	if err != nil {
		return nil, fmt.Errorf("rag search: %w", err)
	}
	if len(docs) == 0 {
		return &ChatResult{
			Answer:  "知识库中没有找到相关信息。",
			Sources: []SourceRef{},
			Found:   false,
		}, nil
	}

	// 组装上下文：secret 级脱敏（红线），其余用原文
	var sb strings.Builder
	for i, d := range docs {
		content := d.Chunk.Content
		if d.Chunk.Confidentiality == "secret" {
			content = SecretMask(d.Chunk)
		}
		sb.WriteString(fmt.Sprintf("%d. [%s｜%s] %s\n", i+1, d.Chunk.DocTitle, d.Chunk.SourceType, content))
	}

	ctxL, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	answer, err := a.LLM.ChatCompletion(ctxL,
		"你是产品助手。基于下面的内部资料回答用户问题，用中文、简洁、可溯源。若资料不足以回答，明确说明资料中没有相关信息，不要编造。",
		fmt.Sprintf("用户问题：%s\n\n资料：\n%s", query, sb.String()))
	if err != nil {
		return nil, fmt.Errorf("chat: %w", err)
	}

	return &ChatResult{
		Answer:  strings.TrimSpace(answer),
		Sources: buildSources(docs),
		Found:   true,
	}, nil
}

// buildSources 把 RAG 命中转成可溯源来源列表（secret 正文不暴露，仅标题/密级/时间）。
func buildSources(docs []qdrant.SearchResult) []SourceRef {
	out := make([]SourceRef, 0, len(docs))
	for _, d := range docs {
		ref := SourceRef{
			DocTitle:        d.Chunk.DocTitle,
			SourceType:      d.Chunk.SourceType,
			Confidentiality: d.Chunk.Confidentiality,
		}
		if d.Chunk.UpdatedAt != nil {
			ref.UpdatedAt = d.Chunk.UpdatedAt.Format("2006-01-02 15:04")
		}
		if d.Chunk.Confidentiality != "secret" {
			ref.Snippet = truncate(d.Chunk.Content, 200)
		}
		out = append(out, ref)
	}
	return out
}

// DocInput 是产品文档生成输入（S7 §3.2）。
type DocInput struct {
	BoardID uint   `json:"board_id"`
	Prompt  string `json:"prompt"`
	DocType string `json:"doc_type"` // product_doc | change_request | meeting_minutes
}

// GenerateDoc 生成产品文档 / 变更需求：RAG 上下文 + LLM 生成 →
// 变更需求必须结构化含五要素（改什么/为什么/预期效果/风险/涉及模块），缺项打回重写（重试 1 次）→ 落 product_docs。
func (a *Agent) GenerateDoc(ctx context.Context, roles []string, in DocInput) (*ProductDoc, error) {
	if a.LLM == nil {
		return nil, fmt.Errorf("llm unavailable")
	}
	docType := in.DocType
	if docType == "" {
		docType = "product_doc"
	}
	// RAG 上下文（按密级过滤）
	ctxText, sources, err := a.ragContext(ctx, roles, in.Prompt)
	if err != nil {
		return nil, err
	}

	system := docSystemPrompt(docType)
	var content string
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		prompt := in.Prompt
		if attempt > 0 && lastErr != nil {
			prompt = in.Prompt + "\n\n（上一版缺项：" + lastErr.Error() + "，请补全后重新生成完整文档）"
		}
		ctxL, cancel := context.WithTimeout(ctx, 120*time.Second)
		gen, gerr := a.LLM.ChatCompletion(ctxL, system, prompt+"\n\n内部资料：\n"+ctxText)
		cancel()
		if gerr != nil {
			return nil, fmt.Errorf("generate doc: %w", gerr)
		}
		content = strings.TrimSpace(gen)
		if verr := validateChangeRequest(docType, content); verr != nil {
			lastErr = verr
			continue
		}
		break
	}
	if verr := validateChangeRequest(docType, content); verr != nil {
		return nil, fmt.Errorf("变更需求缺项（打回）：%w", verr)
	}

	doc := &ProductDoc{
		BoardID: in.BoardID,
		Title:   truncate(in.Prompt, 120),
		DocType: docType,
		Content: content,
		Sources: marshalSources(sources),
		Status:  "draft",
	}
	if doc.Title == "" {
		doc.Title = docType
	}
	if err := a.Store.CreateDoc(ctx, doc); err != nil {
		return nil, fmt.Errorf("create product doc: %w", err)
	}
	return doc, nil
}

// ragContext 检索 RAG 上下文 + 来源（按用户角色密级过滤）；无命中返回空上下文。
func (a *Agent) ragContext(ctx context.Context, roles []string, prompt string) (string, []SourceRef, error) {
	if !a.LLM.EmbeddingsEnabled() {
		return "", nil, nil
	}
	vector, err := a.LLM.Embed(ctx, prompt)
	if err != nil {
		return "", nil, err
	}
	docs, err := a.Qdrant.SearchFiltered(ctx, vector, 5, nil, ConfidentialityLevelsFor(roles))
	if err != nil || len(docs) == 0 {
		return "", nil, err
	}
	var sb strings.Builder
	for i, d := range docs {
		content := d.Chunk.Content
		if d.Chunk.Confidentiality == "secret" {
			content = SecretMask(d.Chunk)
		}
		sb.WriteString(fmt.Sprintf("%d. [%s｜%s] %s\n", i+1, d.Chunk.DocTitle, d.Chunk.SourceType, content))
	}
	return sb.String(), buildSources(docs), nil
}

// docSystemPrompt 按文档类型生成 LLM system prompt（变更需求强制五要素）。
func docSystemPrompt(docType string) string {
	switch docType {
	case "change_request":
		return "你是产品变更需求撰写助手。根据用户需求 + 内部资料，生成一份结构化的变更需求文档，用中文。" +
			"必须包含以下五个章节（用 Markdown 二级标题）：\n" +
			"## 改什么\n## 为什么\n## 预期效果\n## 风险\n## 涉及模块\n" +
			"每个章节都要有实质内容，不要留空。只输出文档正文。"
	case "meeting_minutes":
		return "你是会议纪要整理助手。根据输入整理结构化会议纪要（背景/讨论/结论/待办），用中文。只输出文档正文。"
	default:
		return "你是产品文档撰写助手。根据用户需求 + 内部资料，生成一份产品文档，用中文、结构清晰。只输出文档正文。"
	}
}

// changeRequestElements 是变更需求的五要素（S7 §3.2：缺项打回重写）。
var changeRequestElements = []string{"改什么", "为什么", "预期效果", "风险", "涉及模块"}

// validateChangeRequest 校验变更需求是否含五要素；非 change_request 直接通过。
func validateChangeRequest(docType, content string) error {
	if docType != "change_request" {
		return nil
	}
	var missing []string
	for _, e := range changeRequestElements {
		if !strings.Contains(content, e) {
			missing = append(missing, e)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("缺少：%s", strings.Join(missing, "、"))
	}
	return nil
}

// marshalSources 把来源列表序列化为 JSON 字符串（写入 product_docs.sources）。
func marshalSources(sources []SourceRef) string {
	if len(sources) == 0 {
		return "[]"
	}
	b, err := json.Marshal(sources)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// truncate 截断长文本（用于标题/片段）。
func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n]
}
