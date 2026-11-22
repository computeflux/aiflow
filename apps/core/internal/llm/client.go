// Package llm 是 LLM 调用客户端（OpenAI 兼容协议；重试/超时/token 统计/OTel span）。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sashabaranov/go-openai"
	internalotel "github.com/wetee/ai-workflow/internal/otel"
	"go.opentelemetry.io/otel/attribute"
)

// Client wraps the OpenAI-compatible client.
type Client struct {
	mu             sync.RWMutex
	inner          *openai.Client
	embeddingModel string
	llmModel       string
}

// NewClient creates a new OpenAI-compatible client.
func NewClient(apiKey, baseURL, embeddingModel, llmModel string) *Client {
	return &Client{inner: newInner(apiKey, baseURL), embeddingModel: embeddingModel, llmModel: llmModel}
}

// newInner builds the OpenAI-compatible inner client (shared by NewClient and
// ApplyConfig so the transport/timeout/debug knobs live in one place).
func newInner(apiKey, baseURL string) *openai.Client {
	cfg := openai.DefaultConfig(apiKey)
	cfg.BaseURL = baseURL
	// 单次请求超时：默认不限制（0 = 无超时）。本地大模型（如 27B GGUF ~26 tok/s）遇到长 prompt /
	// 长工具调用 JSON 时 30s 经常不够，会误报 504「AI 超时」，故去掉硬超时。
	// 客户端断开时 gin 的 ctx 会取消，请求随之中止，不会永久挂起；需要给上游设硬上限时用
	// LLM_TIMEOUT_SEC 显式指定（秒），并同步调大 nginx proxy_read_timeout 保持网关不等式。
	timeout := 0 * time.Second
	if v := strings.TrimSpace(os.Getenv("LLM_TIMEOUT_SEC")); v != "" {
		if sec, err := strconv.Atoi(v); err == nil && sec > 0 {
			timeout = time.Duration(sec) * time.Second
		}
	}
	httpClient := &http.Client{Timeout: timeout}
	// LLM_DEBUG_HTTP=1：打印请求/响应摘要（排障用；上游把错误包成 HTTP 200 +
	// {"error":"..."} 时 SDK 会静默当成空结果，只能从原始报文定位）。
	if os.Getenv("LLM_DEBUG_HTTP") == "1" {
		httpClient.Transport = &debugTransport{}
	}
	cfg.HTTPClient = httpClient
	return openai.NewClientWithConfig(cfg)
}

// ApplyConfig atomically swaps the endpoint (base URL + API key) and models.
// 模型管理（S8）：model_profiles 活动配置变更（如 /model-config/apply）后，
// 各服务 ticker 调本方法热切到活动模型，无需重启；调用方持有的 *Client
// 指针不变，进行中的请求不受影响（锁内换指针）。
func (c *Client) ApplyConfig(apiKey, baseURL, embeddingModel, llmModel string) {
	inner := newInner(apiKey, baseURL)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inner = inner
	if strings.TrimSpace(embeddingModel) != "" {
		c.embeddingModel = embeddingModel
	}
	if strings.TrimSpace(llmModel) != "" {
		c.llmModel = llmModel
	}
}

// Model 返回当前配置的 LLM 模型名（落库时标记「实际用的哪个模型」，
// 回放 /replay 用它而非原记录的 Model，避免与实际执行不一致）。
func (c *Client) Model() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.llmModel
}

// debugTransport 记录 LLM HTTP 交换摘要（请求体首尾 300 字符 / 响应状态 + 正文首 500 字符），
// 并把最近一次请求/响应完整落盘（/tmp/llm-last-req.json、/tmp/llm-last-resp.json）
// —— 上游 400 被包成 HTTP 200 + {"error":"..."} 时，只能靠原始报文定位（SDK 会静默吞掉）。
type debugTransport struct{ inner http.RoundTripper }

func (t *debugTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	inner := t.inner
	if inner == nil {
		inner = http.DefaultTransport
	}
	var reqBody []byte
	if req.Body != nil {
		reqBody, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
	}
	log.Printf("[llm-debug] → %s %s body=%dB %s", req.Method, req.URL.String(), len(reqBody), summarizeLLMRequest(reqBody))
	if len(reqBody) > 0 {
		_ = os.WriteFile("/tmp/llm-last-req.json", reqBody, 0o644)
	}
	resp, err := inner.RoundTrip(req)
	if err != nil {
		log.Printf("[llm-debug] ← error: %v", err)
		return resp, err
	}
	if resp.Body != nil {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(body))
		log.Printf("[llm-debug] ← %d body=%dB head=%q", resp.StatusCode, len(body), headTail(string(body), 500))
		_ = os.WriteFile("/tmp/llm-last-resp.json", body, 0o644)
	}
	return resp, err
}

// summarizeLLMRequest 打印请求骨架：model / messages（角色+长度）/ tools 名称。
// 上游对消息结构敏感（system 必须首位且唯一、无 user 直接 400），骨架比正文更能定位问题。
func summarizeLLMRequest(body []byte) string {
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if json.Unmarshal(body, &req) != nil {
		return "(unparsable)"
	}
	var sb strings.Builder
	sb.WriteString("model=" + req.Model + " messages=")
	for i, m := range req.Messages {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(fmt.Sprintf("%d:%s(%dB)", i, m.Role, len(m.Content)))
	}
	sb.WriteString(" tools=")
	for i, tl := range req.Tools {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(tl.Function.Name)
	}
	return sb.String()
}

// headTail 取字符串头部 n 字符（超出时追加尾部 80 字符便于看出截断位置）。
func headTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + " … " + s[len(s)-80:]
}

// maxAPIRetries is the number of retries for transient network / server errors.
const maxAPIRetries = 3

// retryBackoff returns a backoff duration for the given attempt (0-based),
// with random jitter so concurrent callers don't retry at the same instant.
// Attempt 1 → ~1s, attempt 2 → ~2s, attempt 3 → ~4s (each +0~50% jitter).
func retryBackoff(attempt int) time.Duration {
	base := time.Duration(math.Pow(2, float64(attempt))) * time.Second
	// 附加 0 ~ base/2 的随机抖动，避免并发重试形成羊群效应
	return base + time.Duration(rand.Int63n(int64(base/2)+1))
}

// retryable 判断错误是否值得重试：网络/传输层错误、5xx、429 重试，
// 其余 4xx（参数/鉴权等）直接失败，不做无谓重试。
func retryable(err error) bool {
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		return apiErr.HTTPStatusCode == http.StatusTooManyRequests || apiErr.HTTPStatusCode >= 500
	}
	var reqErr *openai.RequestError
	if errors.As(err, &reqErr) {
		return true
	}
	// 未知错误类型：保守起见不重试
	return false
}

// EmbeddingsEnabled 报告嵌入功能是否可用（需配置 EMBEDDING_MODEL）。
// 模型不可用时（如模型列表移除），留空即可整体停用向量索引/搜索。
func (c *Client) EmbeddingsEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.embeddingModel != ""
}

// Embed returns the embedding vector for the given text.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.embeddingModel == "" {
		return nil, errors.New("embedding disabled: EMBEDDING_MODEL not set")
	}
	var lastErr error
	for attempt := 0; attempt < maxAPIRetries; attempt++ {
		resp, err := c.inner.CreateEmbeddings(ctx, openai.EmbeddingRequestStrings{
			Input: []string{text},
			Model: openai.EmbeddingModel(c.embeddingModel),
		})
		if err == nil {
			if len(resp.Data) == 0 {
				return nil, fmt.Errorf("no embedding returned")
			}
			return resp.Data[0].Embedding, nil
		}
		if !retryable(err) {
			return nil, fmt.Errorf("create embedding: %w", err)
		}
		lastErr = err
		if attempt < maxAPIRetries-1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryBackoff(attempt)):
			}
		}
	}
	return nil, fmt.Errorf("create embedding after %d retries: %w", maxAPIRetries, lastErr)
}

// EmbedBatch returns embedding vectors for multiple texts.
func (c *Client) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.embeddingModel == "" {
		return nil, errors.New("embedding disabled: EMBEDDING_MODEL not set")
	}

	var lastErr error
	for attempt := 0; attempt < maxAPIRetries; attempt++ {
		resp, err := c.inner.CreateEmbeddings(ctx, openai.EmbeddingRequestStrings{
			Input: texts,
			Model: openai.EmbeddingModel(c.embeddingModel),
		})
		if err == nil {
			out := make([][]float32, len(resp.Data))
			for i, d := range resp.Data {
				out[i] = d.Embedding
			}
			return out, nil
		}
		if !retryable(err) {
			return nil, fmt.Errorf("create embeddings: %w", err)
		}
		lastErr = err
		if attempt < maxAPIRetries-1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryBackoff(attempt)):
			}
		}
	}
	return nil, fmt.Errorf("create embeddings after %d retries: %w", maxAPIRetries, lastErr)
}

// sanitizeMessages 把空 content 换成占位文案。
//
// ⚠️ 上游陷阱（2026-10-18 实测）：go-openai SDK 对 Content=="" 的消息会序列化成
// `"content": null`，而 LLM 上游（llama.cpp 系）对 null 直接回 400；该 400 又被网关包装成
// `HTTP 200 + {"error":"upstream status 400"}`——SDK 只识别对象形式的 error，于是当成
// 「成功但空 choices」静默吞掉。表现：AI 界面空白无报错，且前端会再存一条空回复 → 死循环。
func sanitizeMessages(msgs []openai.ChatCompletionMessage) []openai.ChatCompletionMessage {
	out := make([]openai.ChatCompletionMessage, 0, len(msgs))
	for _, m := range msgs {
		if m.Content == "" && len(m.MultiContent) == 0 {
			m.Content = "（无内容）"
		}
		out = append(out, m)
	}
	return out
}

// ChatCompletion sends a chat request and returns the content of the first choice.
func (c *Client) ChatCompletion(ctx context.Context, systemPrompt, userPrompt string) (content string, err error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ctx, span := internalotel.Start(ctx, "llm.chat_completion",
		attribute.String("llm.model", c.llmModel))
	defer func() {
		if err != nil {
			span.RecordError(err)
		}
		span.End()
	}()
	var lastErr error
	for attempt := 0; attempt < maxAPIRetries; attempt++ {
		resp, err := c.inner.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
			Model: c.llmModel,
			Messages: sanitizeMessages([]openai.ChatCompletionMessage{
				{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
				{Role: openai.ChatMessageRoleUser, Content: userPrompt},
			}),
		})
		if err == nil {
			if len(resp.Choices) == 0 {
				return "", fmt.Errorf("no choices returned")
			}
			return resp.Choices[0].Message.Content, nil
		}
		if !retryable(err) {
			return "", fmt.Errorf("chat completion: %w", err)
		}
		lastErr = err
		if attempt < maxAPIRetries-1 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(retryBackoff(attempt)):
			}
		}
	}
	return "", fmt.Errorf("chat completion after %d retries: %w", maxAPIRetries, lastErr)
}

// ChatCompletionWithTools sends a chat request with function calling tools.
func (c *Client) ChatCompletionWithTools(ctx context.Context, messages []openai.ChatCompletionMessage, tools []openai.Tool) (resp *openai.ChatCompletionResponse, err error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ctx, span := internalotel.Start(ctx, "llm.chat_completion_tools",
		attribute.String("llm.model", c.llmModel))
	defer func() {
		if err != nil {
			span.RecordError(err)
		}
		span.End()
	}()
	var lastErr error
	for attempt := 0; attempt < maxAPIRetries; attempt++ {
		resp, err := c.inner.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
			Model:    c.llmModel,
			Messages: sanitizeMessages(messages),
			Tools:    tools,
		})
		if err == nil {
			return &resp, nil
		}
		if !retryable(err) {
			return nil, fmt.Errorf("chat completion with tools: %w", err)
		}
		lastErr = err
		if attempt < maxAPIRetries-1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryBackoff(attempt)):
			}
		}
	}
	return nil, fmt.Errorf("chat completion with tools after %d retries: %w", maxAPIRetries, lastErr)
}

// ChatCompletionStream 流式带工具对话（board AI 用）：
//   - 文本增量经 onDelta 实时回调（前端 SSE 逐字渲染）；
//   - 工具调用的 arguments 是分片下发的，这里按 index 拼装成完整 ToolCall；
//   - 返回拼装好的 assistant 消息（Content + ToolCalls），调用方可直接进工具循环。
//
// 流式请求**不做自动重试**：已经吐给前端的 token 无法回滚，重试会造成内容重复。
// 调用方（SSE handler）在出错时直接结束本轮。
func (c *Client) ChatCompletionStream(ctx context.Context, messages []openai.ChatCompletionMessage, tools []openai.Tool, onDelta func(string)) (*openai.ChatCompletionMessage, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	req := openai.ChatCompletionRequest{
		Model:    c.llmModel,
		Messages: sanitizeMessages(messages),
	}
	if len(tools) > 0 {
		req.Tools = tools
	}
	stream, err := c.inner.CreateChatCompletionStream(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("chat completion stream: %w", err)
	}
	defer func() { _ = stream.Close() }()

	var content strings.Builder
	// 按 index 累积工具调用分片（同一调用的 name / arguments 会拆成多段下发）
	type toolAcc struct {
		id   string
		name string
		args strings.Builder
	}
	calls := map[int]*toolAcc{}
	var order []int

	for {
		resp, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return nil, fmt.Errorf("chat completion stream recv: %w", rerr)
		}
		if len(resp.Choices) == 0 {
			continue
		}
		d := resp.Choices[0].Delta
		if d.Content != "" {
			content.WriteString(d.Content)
			if onDelta != nil {
				onDelta(d.Content)
			}
		}
		for _, tc := range d.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			a, ok := calls[idx]
			if !ok {
				a = &toolAcc{}
				calls[idx] = a
				order = append(order, idx)
			}
			if tc.ID != "" {
				a.id = tc.ID
			}
			if tc.Function.Name != "" {
				a.name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				a.args.WriteString(tc.Function.Arguments)
			}
		}
	}

	msg := &openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleAssistant,
		Content: content.String(),
	}
	for _, idx := range order {
		a := calls[idx]
		if a.name == "" {
			continue
		}
		msg.ToolCalls = append(msg.ToolCalls, openai.ToolCall{
			ID:   a.id,
			Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{
				Name:      a.name,
				Arguments: a.args.String(),
			},
		})
	}
	return msg, nil
}
