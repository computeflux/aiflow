package marketing

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// seedDir 是本包 testdata/seed 的绝对路径（MK-40 投的 20 条样本）。
func seedDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join("testdata", "seed")
	if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
		t.Fatalf("seed 目录不存在: %s", d)
	}
	return d
}

// TestLoadSeedReal 验收（MK-40）：testdata/seed 下 20 条样本（10 intl + 10 cn），
// 全部通过枚举校验 + 脱敏红线（无用户名 / 链接参数）。
func TestLoadSeedReal(t *testing.T) {
	samples, err := LoadSeed(seedDir(t))
	if err != nil {
		t.Fatalf("LoadSeed() = %v, want nil", err)
	}
	if len(samples) != 20 {
		t.Fatalf("样本数 = %d, want 20", len(samples))
	}
	var intl, cn int
	for _, s := range samples {
		switch s.Region {
		case RegionIntl:
			intl++
		case RegionCN:
			cn++
		}
		// 脱敏红线逐条复核（LoadSeed 已校验，这里独立断言防回归）
		if strings.ContainsAny(s.URL, "?#") {
			t.Errorf("seed %s URL 含链接参数: %q", s.ID, s.URL)
		}
		if !strings.HasPrefix(s.Author, "user-") {
			t.Errorf("seed %s author 疑似真实用户名: %q", s.ID, s.Author)
		}
	}
	if intl != 10 || cn != 10 {
		t.Errorf("region 分布 = intl %d / cn %d, want 10 / 10", intl, cn)
	}
}

// TestLoadSeedCounts 验收（MK-40）：条数不符（缺样本）必须报错，
// 防「用种子冒充金标集」（P3 §七：条数写进验收）。
func TestLoadSeedCounts(t *testing.T) {
	dir := t.TempDir()
	writeSeed := func(id, region string) {
		t.Helper()
		body := `{"id":"` + id + `","region":"` + region + `","product":"network",` +
			`"source":"reddit","source_ref":"ref-1","url":"https://x.com/i/status/1",` +
			`"author":"user-001","excerpt":"test","found_at":"2026-10-02T00:00:00Z"}`
		if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 9 intl + 10 cn → 报错
	for i := 1; i <= 9; i++ {
		writeSeed("seed-intl-"+pad2(i), RegionIntl)
	}
	for i := 1; i <= 10; i++ {
		writeSeed("seed-cn-"+pad2(i), RegionCN)
	}
	if _, err := LoadSeed(dir); err == nil {
		t.Fatal("LoadSeed(9 intl + 10 cn) = nil, want error（条数不符）")
	}
	// 补满 10 + 10 → 通过
	writeSeed("seed-intl-10", RegionIntl)
	if _, err := LoadSeed(dir); err != nil {
		t.Fatalf("LoadSeed(10 intl + 10 cn) = %v, want nil", err)
	}
}

// TestLoadSeedDesensitize 反例：URL 带 query 参数 / author 是真实用户名 → 判废。
func TestLoadSeedDesensitize(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*SeedSample)
		wantErr string
	}{
		{"URL带query参数", func(s *SeedSample) { s.URL = "https://x.com/i/status/1?utm_source=mail" }, "链接参数"},
		{"URL带fragment", func(s *SeedSample) { s.URL = "https://x.com/i/status/1#comments" }, "链接参数"},
		{"真实用户名", func(s *SeedSample) { s.Author = "satoshi_fan_42" }, "用户名"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := validSeed()
			c.mutate(&s)
			err := s.Validate()
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, c.wantErr)
			}
		})
	}
}

// TestLoadSeedEnums 反例：非法 region / product → 判废。
func TestLoadSeedEnums(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*SeedSample)
		wantErr string
	}{
		{"非法region", func(s *SeedSample) { s.Region = "us" }, "region"},
		{"非法product", func(s *SeedSample) { s.Product = "minishop" }, "product"},
		{"空source", func(s *SeedSample) { s.Source = "" }, "source"},
		{"空excerpt", func(s *SeedSample) { s.Excerpt = "" }, "excerpt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := validSeed()
			c.mutate(&s)
			err := s.Validate()
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, c.wantErr)
			}
		})
	}
}

// TestLoadSeedDuplicateID 反例：id 重复 → 报错。
func TestLoadSeedDuplicateID(t *testing.T) {
	dir := t.TempDir()
	body := `{"id":"seed-intl-01","region":"intl","product":"network","source":"reddit",` +
		`"source_ref":"ref-1","url":"https://x.com/i/status/1","author":"user-001",` +
		`"excerpt":"test","found_at":"2026-10-02T00:00:00Z"}`
	for _, name := range []string{"a.json", "b.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := LoadSeed(dir); err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("LoadSeed(重复 id) = %v, want error containing 重复", err)
	}
}

// TestLoadSeedInvalidJSON 反例：非法 JSON → 报错。
func TestLoadSeedInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSeed(dir); err == nil || !strings.Contains(err.Error(), "JSON") {
		t.Fatalf("LoadSeed(非法 JSON) = %v, want error containing JSON", err)
	}
}

// validSeed 返回一条合法样本（反例测试的基线）。
func validSeed() SeedSample {
	return SeedSample{
		ID:        "seed-intl-01",
		Region:    RegionIntl,
		Product:   ProductNetwork,
		Source:    "reddit",
		SourceRef: "r/ethdev/thread-1001",
		URL:       "https://reddit.com/r/ethdev/comments/1001",
		Author:    "user-001",
		Excerpt:   "Infura 把我限速了，有替代的 RPC 方案吗？",
	}
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}
