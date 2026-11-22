// Package skillgateway 实现 P1 Skill 框架的注册表与调用网关
// （docs/plan/transform-plan.md §6）。
//
// 职责：
//   - skill.json（SkillSpec）的解析与校验（validator.go）
//   - 每 skill 的限流与并发控制（limits.go）
//   - service / cli 两种 skill 的执行（invoker.go）
//   - skills / skill_versions / skill_runs 的存储与版本路由（registry.go）
//   - 校验输入 → 限流 → 执行 → 校验输出 → 落库 的核心调用链（runner.go）
//   - HTTP API 路由（handler.go）
package skillgateway

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// 顶层 schema 注册用的伪 URL（仅用于 compiler 资源定位，不发起网络请求）。
const (
	schemaURLInput  = "https://wetee.ai/skill-schemas/input.json"
	schemaURLOutput = "https://wetee.ai/skill-schemas/output.json"
)

// namePattern：skill 名要求小写字母开头，后续可含小写字母/数字/连字符，长度 2~64。
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}$`)

// Limits 是 skill 的运行时约束（skill.json 的 limits 字段）。
// 0 值表示不限：rate_per_min=0 不限流，max_concurrency=0 不限并发。
type Limits struct {
	TimeoutSec     int `json:"timeout_sec"`
	RatePerMin     int `json:"rate_per_min"`
	MaxConcurrency int `json:"max_concurrency"`
}

// withDefaults 填充缺失的 limits 默认值：超时 60s、不限流、并发 1。
func (l Limits) withDefaults() Limits {
	if l.TimeoutSec <= 0 {
		l.TimeoutSec = 60
	}
	if l.MaxConcurrency == 0 {
		l.MaxConcurrency = 1
	}
	return l
}

// SkillSpec 是 skill.json 的解析结果（docs/plan/transform-plan.md §6.1）。
// schema 字段保留原始 JSON（RawMessage），注册与校验时按原样落库/编译。
type SkillSpec struct {
	Name         string          `json:"name"`
	Version      string          `json:"version"`
	Description  string          `json:"description"`
	Owner        string          `json:"owner"`
	Runtime      string          `json:"runtime"` // go / python / rust
	Type         string          `json:"type"`    // service / cli
	Entrypoint   string          `json:"entrypoint"`
	Addr         string          `json:"addr"`
	Container    string          `json:"container"`
	Image        string          `json:"image"`
	InputSchema  json.RawMessage `json:"input_schema"`
	OutputSchema json.RawMessage `json:"output_schema"`
	Limits       Limits          `json:"limits"`
	Permissions  json.RawMessage `json:"permissions"`
}

// versionPattern：语义化版本，形如 1.0.0 / v1.2.3。
var versionPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// ValidateSpec 校验 skill.json 的语义正确性，并预编译 input/output schema。
// 返回的 spec.Limits 已填充默认值。
func ValidateSpec(raw []byte) (*SkillSpec, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var spec SkillSpec
	if err := dec.Decode(&spec); err != nil {
		return nil, fmt.Errorf("skill.json 不是合法 JSON: %w", err)
	}

	if !namePattern.MatchString(spec.Name) {
		return nil, fmt.Errorf("name %q 非法：须匹配 %s", spec.Name, namePattern.String())
	}
	if spec.Version == "" {
		return nil, fmt.Errorf("version 不能为空")
	}
	if !versionPattern.MatchString(spec.Version) {
		return nil, fmt.Errorf("version %q 非法：须为语义化版本（如 1.0.0）", spec.Version)
	}
	switch spec.Runtime {
	case "go", "python", "rust":
	default:
		return nil, fmt.Errorf("runtime %q 非法：须为 go / python / rust", spec.Runtime)
	}
	switch spec.Type {
	case "service", "cli":
	default:
		return nil, fmt.Errorf("type %q 非法：须为 service / cli", spec.Type)
	}
	if spec.Type == "service" && strings.TrimSpace(spec.Addr) == "" {
		return nil, fmt.Errorf("type=service 时 addr 不能为空")
	}
	if spec.Type == "cli" && strings.TrimSpace(spec.Entrypoint) == "" {
		return nil, fmt.Errorf("type=cli 时 entrypoint 不能为空")
	}
	if spec.Owner == "" {
		return nil, fmt.Errorf("owner 不能为空")
	}
	if len(spec.InputSchema) == 0 {
		return nil, fmt.Errorf("input_schema 不能为空")
	}
	if len(spec.OutputSchema) == 0 {
		return nil, fmt.Errorf("output_schema 不能为空")
	}

	// 预编译两份 schema：任何一份非法都拒绝注册，避免坏 schema 入库。
	if _, err := compileSchema(schemaURLInput, spec.InputSchema); err != nil {
		return nil, fmt.Errorf("input_schema: %w", err)
	}
	if _, err := compileSchema(schemaURLOutput, spec.OutputSchema); err != nil {
		return nil, fmt.Errorf("output_schema: %w", err)
	}

	spec.Limits = spec.Limits.withDefaults()
	return &spec, nil
}

// schemaCache 缓存编译后的 schema，key 为原始 JSON 字符串。
// schema 不可变，sync.Map 并发安全。
var schemaCache sync.Map

// compileSchema 把 raw JSON 编译为可复用的 *jsonschema.Schema（带缓存）。
func compileSchema(url string, raw json.RawMessage) (*jsonschema.Schema, error) {
	key := string(raw)
	if v, ok := schemaCache.Load(key); ok {
		return v.(*jsonschema.Schema), nil
	}

	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("schema 不是合法 JSON: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(url, doc); err != nil {
		return nil, fmt.Errorf("注册 schema 失败: %w", err)
	}
	sch, err := compiler.Compile(url)
	if err != nil {
		return nil, fmt.Errorf("编译失败: %w", err)
	}
	schemaCache.Store(key, sch)
	return sch, nil
}

// validateAgainst 用 raw JSON schema 校验 value，返回首个人类可读的错误。
func validateAgainst(url string, raw json.RawMessage, value any) error {
	sch, err := compileSchema(url, raw)
	if err != nil {
		return err
	}
	if err := sch.Validate(value); err != nil {
		return fmt.Errorf("数据不符合 schema: %v", err)
	}
	return nil
}
