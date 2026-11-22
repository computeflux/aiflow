package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func (dataURLLoader) Load(loc string) (any, error) {
	if strings.HasPrefix(loc, "data:application/json,") {
		payload, err := url.QueryUnescape(loc[len("data:application/json,"):])
		if err != nil {
			return nil, err
		}
		// 必须返回解析后的 JSON 值（jsonschema.UnmarshalJSON 返回 json.RawMessage，
		// compiler 会报 "invalid jsonType json.RawMessage"）
		var v any
		if err := json.Unmarshal([]byte(payload), &v); err != nil {
			return nil, err
		}
		return v, nil
	}
	return jsonschema.FileLoader{}.Load(loc)
}

func compileSchema(m map[string]any) *jsonschema.Schema {
	b, _ := json.Marshal(m)
	c := jsonschema.NewCompiler()
	c.UseLoader(dataURLLoader{})
	s, err := c.Compile("data:application/json," + url.QueryEscape(string(b)))
	if err != nil {
		panic(fmt.Sprintf("compile schema: %v", err))
	}
	return s
}

type coderOut struct {
	OK           bool     `json:"ok"`
	Diff         string   `json:"diff"`
	FilesChanged []string `json:"files_changed"`
	SessionID    string   `json:"session_id"`
}

type reviewComment struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

type reviewerOut struct {
	OK       bool            `json:"ok"`
	Comments []reviewComment `json:"comments"`
}
