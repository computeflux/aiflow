package flex

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// GraphDef 是子图注册单元：名称 + 图 + 入/出参 JSON Schema。
type GraphDef struct {
	Name      string
	Graph     *Graph
	InSchema  json.RawMessage // 可空：不做入参校验
	OutSchema json.RawMessage // 可空：不做出参校验
}

var (
	mu       sync.RWMutex
	registry = map[string]GraphDef{}
)

// Register 白名单注册子图（S2：flex-server 只运行已注册图，不动态加载外部文件）。
func Register(def GraphDef) {
	if def.Graph == nil {
		panic("flex: register with nil graph")
	}
	if def.Name != def.Graph.Name() {
		panic(fmt.Sprintf("flex: def name %q != graph name %q", def.Name, def.Graph.Name()))
	}
	mu.Lock()
	defer mu.Unlock()
	registry[def.Name] = def
}

// Get 按名称取已注册子图。
func Get(name string) (GraphDef, bool) {
	mu.RLock()
	defer mu.RUnlock()
	def, ok := registry[name]
	return def, ok
}

// List 返回全部已注册子图名（排序）。
func List() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ValidateSchema 用 JSON Schema 校验值；schema 为空时跳过。
func ValidateSchema(schema json.RawMessage, v any) error {
	if len(schema) == 0 {
		return nil
	}
	// v6 要求 doc 为已解析的 JSON 值（传 string 会被当成 JSON 字符串值）
	var doc any
	if err := json.Unmarshal(schema, &doc); err != nil {
		return fmt.Errorf("parse schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("flex-schema.json", doc); err != nil {
		return fmt.Errorf("add schema resource: %w", err)
	}
	compiled, err := c.Compile("flex-schema.json")
	if err != nil {
		return fmt.Errorf("compile schema: %w", err)
	}
	return compiled.Validate(v)
}
