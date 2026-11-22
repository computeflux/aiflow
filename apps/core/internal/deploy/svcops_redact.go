package deploy

// svcops_redact.go：运维中心运行服务后端（原 internal/serviceops，批次 G 并入 deploy 域）。

import (
	"regexp"
	"strings"
)

// serviceops_redact.go：日志与环境变量的敏感信息脱敏（从 serviceops.go 拆出，纯机械搬迁）。

// isSensitiveKey 判定键名是否敏感：敏感子串命中，或 auth 独立成词（避免 OAUTH_*/AUTHORITY 过度脱敏）。
func isSensitiveKey(k string) bool {
	if sensitiveSubstrRe.MatchString(k) {
		return true
	}
	for _, tok := range strings.FieldsFunc(k, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	}) {
		if strings.EqualFold(tok, "auth") {
			return true
		}
	}
	return false
}

// redactValue 判定单个值是否含凭据（键名敏感或值含 ://user:pass@）。
func redactValue(k, v string) bool {
	return isSensitiveKey(k) || credValueRe.MatchString(v)
}

// redactEnv 服务端脱敏环境变量：敏感键或含凭据的值替换为 <redacted>。
func redactEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return env
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		if redactValue(k, v) {
			out[k] = "<redacted>"
		} else {
			out[k] = v
		}
	}
	return out
}

// redactLogs 服务端脱敏容器日志：日志常打印启动期的 DSN / 口令 / Token，而详情接口仅 requireAuth，
// 原样下发会成为绕开 env 脱敏的旁路。对 `KEY=...`（敏感键，允许行首裸键）与 `://user:pass@` 片段替换。
// 注：日志正则只覆盖敏感子串词（password/token/secret/dsn 等，日志凭据泄漏的主形态）；
// auth 因需独立成词判定（防 OAUTH 误伤）不纳入日志正则，属可接受的取舍。
var logKeyValRe = regexp.MustCompile(`(?i)\b([A-Za-z0-9_]*(?:password|passwd|secret|token|api_?key|private_?key|credential|dsn)[A-Za-z0-9_]*)\s*[=:]\s*\S+`)

func redactLogs(logs string) string {
	if logs == "" {
		return logs
	}
	logs = logKeyValRe.ReplaceAllString(logs, "$1=<redacted>")
	logs = credValueRe.ReplaceAllString(logs, "://<redacted>@")
	return logs
}
