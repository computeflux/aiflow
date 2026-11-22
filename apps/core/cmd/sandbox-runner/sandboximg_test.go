package main

import "testing"

// TestAllowed 锁定白名单匹配：精确 + `repo:*` tag 通配（灰度版本容器为版本化 tag）。
func TestAllowed(t *testing.T) {
	entries := sandboximgSplit(" pycraper:* , pyreplay:latest , golang:1.26 ")
	cases := []struct {
		img  string
		want bool
		name string
	}{
		{"pyreplay:latest", true, "精确命中"},
		{"pyreplay:evolve-card-20-1790984255", false, "仓库通配未开 → 版本化 tag 不命中"},
		{"pycraper:latest", true, "通配命中 latest"},
		{"pycraper:evolve-card-20-1790984255", true, "通配命中版本化 tag"},
		{"golang:1.26", true, "精确命中（go 项目）"},
		{"golang:1.25", false, "不同 tag"},
		{"pyreplay", false, "无 tag 不等价"},
		{"", false, "空镜像"},
		{"evil/pyreplay:latest", false, "前缀无关仓库不误命中"},
	}
	for _, c := range cases {
		if got := sandboximgAllowed(entries, c.img); got != c.want {
			t.Errorf("sandboximgAllowed(%q) = %v, want %v（%s）", c.img, got, c.want, c.name)
		}
	}
}
