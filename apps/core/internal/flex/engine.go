// Package flex 是最小状态图引擎（S2 柔性分支，docs/plan/S2-混合编排.md §3.3）。
//
// Go 自研，替代原计划的 LangGraph（Python）选型：当前柔性子图是确定性数据处理
// （脑暴/汇总），LLM 节点直接用 internal/llm 调用。/runs HTTP 契约由
// cmd/flex-server 实现；FlexActivity（internal/activities/flex.go）经 HTTP 调用。
package flex

import (
	"context"
	"fmt"
	"sort"
)

// State 是子图状态：键值袋，节点按键读写。
type State map[string]any

// NodeFunc 是单个节点：读当前状态，返回部分更新（合并入状态）；返回 error 终止本次运行。
type NodeFunc func(ctx context.Context, state State) (State, error)

// Graph 是定向无环状态图：入口 + 有向边，按拓扑序执行。
type Graph struct {
	name  string
	entry string
	nodes map[string]NodeFunc
	edges map[string][]string
	order []string // 拓扑序（Compile 后固定）
}

// New 创建图；entry 必须是后续 AddNode 注册的节点名。
func New(name, entry string) *Graph {
	return &Graph{
		name:  name,
		entry: entry,
		nodes: map[string]NodeFunc{},
		edges: map[string][]string{},
	}
}

func (g *Graph) Name() string { return g.name }

func (g *Graph) NodeCount() int { return len(g.nodes) }

func (g *Graph) AddNode(name string, fn NodeFunc) *Graph {
	if _, exists := g.nodes[name]; exists {
		panic(fmt.Sprintf("flex graph %s: duplicate node %q", g.name, name))
	}
	g.nodes[name] = fn
	if _, exists := g.edges[name]; !exists {
		g.edges[name] = nil
	}
	return g
}

func (g *Graph) AddEdge(from, to string) *Graph {
	if _, ok := g.nodes[from]; !ok {
		panic(fmt.Sprintf("flex graph %s: edge from unknown node %q", g.name, from))
	}
	if _, ok := g.nodes[to]; !ok {
		panic(fmt.Sprintf("flex graph %s: edge to unknown node %q", g.name, to))
	}
	g.edges[from] = append(g.edges[from], to)
	return g
}

// Compile 做拓扑排序（Kahn，候选按节点名排序保证确定性），
// 环、悬挂节点、不可达节点均报错。重复 Compile 结果不变。
func (g *Graph) Compile() error {
	indeg := make(map[string]int, len(g.nodes))
	for n := range g.nodes {
		indeg[n] = 0
	}
	for _, tos := range g.edges {
		for _, to := range tos {
			indeg[to]++
		}
	}

	var ready []string
	for n, d := range indeg {
		if d == 0 {
			ready = append(ready, n)
		}
	}
	sort.Strings(ready)
	var order []string
	for len(ready) > 0 {
		n := ready[0]
		ready = ready[1:]
		order = append(order, n)
		for _, to := range g.edges[n] {
			indeg[to]--
			if indeg[to] == 0 {
				ready = append(ready, to)
				sort.Strings(ready)
			}
		}
	}
	if len(order) != len(g.nodes) {
		return fmt.Errorf("flex graph %s: cycle detected", g.name)
	}

	// 全部节点必须从入口可达（不允许存在第二个事实入口）
	reached := map[string]bool{g.entry: true}
	var q []string
	q = append(q, g.entry)
	for len(q) > 0 {
		n := q[0]
		q = q[1:]
		for _, to := range g.edges[n] {
			if !reached[to] {
				reached[to] = true
				q = append(q, to)
			}
		}
	}
	for n := range g.nodes {
		if !reached[n] {
			return fmt.Errorf("flex graph %s: node %q not reachable from entry %q", g.name, n, g.entry)
		}
	}

	g.order = order
	return nil
}

// Invoke 运行子图：从入口按拓扑序执行，返回最终状态。
// ctx 的 deadline 作为本次运行硬超时（cmd/flex-server 设为 10 分钟）；
// 超时返回 ctx 的 DeadlineExceeded，由调用方映射为 504。
func (g *Graph) Invoke(ctx context.Context, input State) (State, error) {
	if g.order == nil {
		return nil, fmt.Errorf("flex graph %s: not compiled", g.name)
	}
	state := make(State, len(input))
	for k, v := range input {
		state[k] = v
	}
	for _, n := range g.order {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		update, err := g.nodes[n](ctx, state)
		if err != nil {
			return nil, fmt.Errorf("flex graph %s node %s: %w", g.name, n, err)
		}
		for k, v := range update {
			state[k] = v
		}
	}
	return state, nil
}
