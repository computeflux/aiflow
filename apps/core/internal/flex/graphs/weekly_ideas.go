// Package graphs 持有 flex 子图定义（S2 §3.3）。
// 每个图在 init() 中向 flex 白名单注册，由 cmd/flex-server 空白导入本包。
package graphs

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/wetee/ai-workflow/internal/flex"
)

// weekly_ideas：多源汇总脑暴示例（collect → summarize → brainstorm → rank → final，5 节点）。
// 纯 Go、确定性、无外部 LLM 调用（LLM 化留给各图自行扩展，经 internal/llm 调用）。
//
// 输入：{"sources": [{"name": "x", "items": ["..."]}, ...]}
// 输出：{"final": {"ideas": [...], "count": N}}

type source struct {
	Name  string   `json:"name"`
	Items []string `json:"items"`
}

type rawItem struct {
	Source string `json:"source"`
	Item   string `json:"item"`
}

type finalOut struct {
	Ideas []string `json:"ideas"`
	Count int      `json:"count"`
}

type ideasState struct {
	Sources  []source  `json:"sources"`
	RawItems []rawItem `json:"rawItems"`
	Summary  string    `json:"summary"`
	Ideas    []string  `json:"ideas"`
	Ranked   []string  `json:"ranked"`
	Final    finalOut  `json:"final"`
}

// decode 把 State（map）按 JSON 解码为结构体（节点间的类型桥接）。
func decode[T any](st flex.State) T {
	var v T
	b, _ := json.Marshal(st)
	_ = json.Unmarshal(b, &v)
	return v
}

func collect(_ context.Context, st flex.State) (flex.State, error) {
	s := decode[ideasState](st)
	raw := make([]rawItem, 0, len(s.RawItems)+len(s.Sources))
	for _, src := range s.Sources {
		for _, item := range src.Items {
			raw = append(raw, rawItem{Source: src.Name, Item: item})
		}
	}
	return flex.State{"rawItems": raw}, nil
}

func summarize(_ context.Context, st flex.State) (flex.State, error) {
	s := decode[ideasState](st)
	perSource := map[string]int{}
	for _, r := range s.RawItems {
		perSource[r.Source]++
	}
	summary := fmt.Sprintf("共 %d 条素材，来自 %d 个来源", len(s.RawItems), len(perSource))
	if top := topWords(wordFreq(s.RawItems), 5); top != "" {
		summary += "；高频词: " + top
	}
	return flex.State{"summary": summary}, nil
}

func brainstorm(_ context.Context, st flex.State) (flex.State, error) {
	s := decode[ideasState](st)
	ideas := make([]string, 0, len(s.RawItems)+3)
	for _, r := range s.RawItems {
		ideas = append(ideas, fmt.Sprintf("深挖「%s」（来源 %s）", r.Item, r.Source))
	}
	for _, w := range topWordList(wordFreq(s.RawItems), 3) {
		ideas = append(ideas, fmt.Sprintf("围绕关键词「%s」做系列内容", w))
	}
	seen := map[string]bool{}
	unique := make([]string, 0, len(ideas))
	for _, idea := range ideas {
		if !seen[idea] {
			seen[idea] = true
			unique = append(unique, idea)
		}
	}
	return flex.State{"ideas": unique}, nil
}

func rank(_ context.Context, st flex.State) (flex.State, error) {
	s := decode[ideasState](st)
	// 确定性简单打分：越长越具体越靠前（示意，可替换为 LLM/向量相似度）
	ranked := append([]string(nil), s.Ideas...)
	sort.SliceStable(ranked, func(i, j int) bool { return len(ranked[i]) > len(ranked[j]) })
	return flex.State{"ranked": ranked}, nil
}

func final(_ context.Context, st flex.State) (flex.State, error) {
	s := decode[ideasState](st)
	return flex.State{"final": finalOut{Ideas: s.Ranked, Count: len(s.Ranked)}}, nil
}

func wordFreq(items []rawItem) map[string]int {
	words := map[string]int{}
	for _, r := range items {
		for _, w := range strings.Fields(strings.ToLower(r.Item)) {
			if len(w) > 2 {
				words[w]++
			}
		}
	}
	return words
}

// topWordList 按频次（同频次按词序）取前 n 个高频词。
func topWordList(words map[string]int, n int) []string {
	type kv struct {
		w string
		c int
	}
	list := make([]kv, 0, len(words))
	for w, c := range words {
		list = append(list, kv{w, c})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].c != list[j].c {
			return list[i].c > list[j].c
		}
		return list[i].w < list[j].w
	})
	out := make([]string, 0, n)
	for i, e := range list {
		if i >= n {
			break
		}
		out = append(out, e.w)
	}
	return out
}

// topWords 按频次（同频次按词序）取前 n 个高频词，拼成 "w1(2), w2(1)"。
func topWords(words map[string]int, n int) string {
	parts := make([]string, 0, n)
	for _, w := range topWordList(words, n) {
		parts = append(parts, fmt.Sprintf("%s(%d)", w, words[w]))
	}
	return strings.Join(parts, ", ")
}

var weeklyIdeasInSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"sources": {
			"type": "array",
			"minItems": 1,
			"items": {
				"type": "object",
				"properties": {
					"name": {"type": "string", "minLength": 1},
					"items": {"type": "array", "items": {"type": "string", "minLength": 1}}
				},
				"required": ["name", "items"]
			}
		}
	},
	"required": ["sources"]
}`)

var weeklyIdeasOutSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"final": {
			"type": "object",
			"properties": {
				"ideas": {"type": "array", "items": {"type": "string"}},
				"count": {"type": "integer", "minimum": 0}
			},
			"required": ["ideas", "count"]
		}
	},
	"required": ["final"]
}`)

func init() {
	g := flex.New("weekly_ideas", "collect")
	g.AddNode("collect", collect)
	g.AddNode("summarize", summarize)
	g.AddNode("brainstorm", brainstorm)
	g.AddNode("rank", rank)
	g.AddNode("final", final)
	g.AddEdge("collect", "summarize")
	g.AddEdge("summarize", "brainstorm")
	g.AddEdge("brainstorm", "rank")
	g.AddEdge("rank", "final")
	if err := g.Compile(); err != nil {
		panic("compile weekly_ideas: " + err.Error())
	}
	flex.Register(flex.GraphDef{
		Name:      "weekly_ideas",
		Graph:     g,
		InSchema:  weeklyIdeasInSchema,
		OutSchema: weeklyIdeasOutSchema,
	})
}
