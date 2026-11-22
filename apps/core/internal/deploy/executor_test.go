package deploy

import (
	"testing"
)

func TestParsePSOut(t *testing.T) {
	// NDJSON（compose v2.38+ 默认：每行一个对象，无数组包裹）
	ndjson := `{"ID":"abc","Name":"workflow-mysql","Service":"mysql","State":"running","Health":"healthy"}
{"ID":"def","Name":"board","Service":"board","State":"running","Health":"healthy"}`
	rows, err := parsePSOut(ndjson)
	if err != nil {
		t.Fatalf("ndjson: %v", err)
	}
	if len(rows) != 2 || rows[0].Service != "mysql" || rows[1].Name != "board" {
		t.Fatalf("ndjson rows = %+v", rows)
	}

	// JSON 数组（旧版 compose 格式）
	arr := `[{"ID":"abc","Name":"workflow-mysql","Service":"mysql","State":"running","Health":"healthy"},{"ID":"def","Name":"board","Service":"board","State":"running","Health":"healthy"}]`
	rows, err = parsePSOut(arr)
	if err != nil {
		t.Fatalf("array: %v", err)
	}
	if len(rows) != 2 || rows[0].Service != "mysql" || rows[1].Name != "board" {
		t.Fatalf("array rows = %+v", rows)
	}

	// 空输出
	if rows, err := parsePSOut("   \n  "); err != nil || len(rows) != 0 {
		t.Fatalf("empty: rows=%+v err=%v", rows, err)
	}

	// 坏 JSON
	if _, err := parsePSOut(`{not json}`); err == nil {
		t.Fatal("expected error for bad json")
	}
}
