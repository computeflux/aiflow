package skillgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

const (
	maxSkillOutputBytes = 1 << 20 // 1MB：skill 输出上限，防止内存失控
	maxSkillStderrBytes = 8 << 10 // 8KB：cli stderr 仅用于错误信息
)

// invokeService 通过 HTTP POST {addr}/invoke 调用 service 型 skill。
// trace 传播由调用方（runner）在 ctx 中携带，这里只负责执行与解析。
func invokeService(ctx context.Context, spec *SkillSpec, input map[string]any) (map[string]any, error) {
	addr := strings.TrimSuffix(spec.Addr, "/")
	if addr == "" {
		return nil, fmt.Errorf("skill addr 为空")
	}

	body, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("序列化输入失败: %w", err)
	}

	client := &http.Client{}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, addr+"/invoke", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// 传播 W3C traceparent 到 skill 服务，链路贯穿 workflow → api → skill
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("调用 skill service 失败: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSkillOutputBytes))
	if err != nil {
		return nil, fmt.Errorf("读取 skill 响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("skill HTTP %d: %s", resp.StatusCode, truncateForError(string(data)))
	}

	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("skill 响应不是 JSON object: %w", err)
	}
	return out, nil
}

// invokeCLI 通过子进程执行 cli 型 skill：JSON 输入走 stdin，输出读 stdout。
//
// 执行方式优先级：
//  1. env SKILL_CLI_WRAPPER 提供时用它作为命令模板，占位符 {container} / {cmd}；
//  2. spec.Container 非空时默认 docker exec -i {container} {entrypoint...}（必须 -i，
//     否则 stdin 不进容器）；
//  3. 否则直接本机执行 entrypoint（按空格拆分为 argv）。
func invokeCLI(ctx context.Context, spec *SkillSpec, input map[string]any) (map[string]any, error) {
	args := buildCLIArgs(spec)

	body, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("序列化输入失败: %w", err)
	}

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdin = bytes.NewReader(body)

	var stdout, stderr cappedBuffer
	stdout.max = maxSkillOutputBytes
	stderr.max = maxSkillOutputBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("cli skill 超时: %w", ctx.Err())
		}
		msg := stderr.String()
		if msg == "" {
			msg = stdout.String()
		}
		return nil, fmt.Errorf("cli skill 执行失败: %v: %s", err, truncateForError(msg))
	}

	var out map[string]any
	dec := json.NewDecoder(strings.NewReader(stdout.String()))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("cli skill 输出不是 JSON object: %v: %s", err, truncateForError(stdout.String()))
	}
	return out, nil
}

// buildCLIArgs 按优先级组装 cli 执行命令。
func buildCLIArgs(spec *SkillSpec) []string {
	if wrapper := os.Getenv("SKILL_CLI_WRAPPER"); wrapper != "" {
		fields := strings.Fields(wrapper)
		out := make([]string, 0, len(fields))
		for _, f := range fields {
			switch f {
			case "{container}":
				out = append(out, spec.Container)
			case "{cmd}":
				out = append(out, spec.Entrypoint)
			default:
				out = append(out, f)
			}
		}
		if len(out) > 0 {
			return out
		}
	}

	if spec.Container != "" {
		args := []string{"docker", "exec", "-i", spec.Container}
		return append(args, strings.Fields(spec.Entrypoint)...)
	}
	return strings.Fields(spec.Entrypoint)
}

// cappedBuffer 是带上限的字节缓冲：超过 max 后丢弃新增内容，
// 但 Write 恒返回 len(p)（从不短写），防止子进程因管道写满而阻塞。
type cappedBuffer struct {
	max int
	buf bytes.Buffer
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := c.max - c.buf.Len()
	if room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		_, _ = c.buf.Write(p)
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }

// truncateForError 截断错误信息，避免把大段输出写进 error。
func truncateForError(s string) string {
	if len(s) > 2048 {
		return s[:2048] + "...(truncated)"
	}
	return s
}

// ensureTimeoutCtx 为 skill 执行套上 limits.timeout_sec 超时。
func ensureTimeoutCtx(parent context.Context, l Limits) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, time.Duration(l.TimeoutSec)*time.Second)
}
