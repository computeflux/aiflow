// seed.go — MK-40 种子样本读取约定（docs/marketing/plan/P3-金标集与回归.md）。
//
// testdata/seed/ 下 20 条真实线索样本（10 intl + 10 cn），已脱敏
// （去掉用户名 / 链接参数）。MK-18 跑批工具（cmd/mk-judge -seed）
// 以 LoadSeed 为唯一读取入口，本文件即「seed 读取约定」。
//
// 边界：本卡只定义读取 + 校验 + 脱敏检查；不做 LLM 判断（那是 MK-18）。
package marketing

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SeedSample 是一条种子线索样本（testdata/seed/*.json，一行一个文件）。
// 字段与 Lead 的输入侧对齐（region/product/source/source_ref/url/author/excerpt），
// MK-18 逐条喂给 Judge（J1）跑批。
type SeedSample struct {
	ID        string    `json:"id"`         // seed-intl-01 .. seed-intl-10 / seed-cn-01 .. seed-cn-10
	Region    string    `json:"region"`     // intl | cn
	Product   string    `json:"product"`    // network | shop
	Source    string    `json:"source"`     // github|reddit|x|telegram|zhihu|juejin|v2ex
	SourceRef string    `json:"source_ref"` // 脱敏后的来源引用（issue 号 / 帖子短 id，不含用户名）
	URL       string    `json:"url"`        // 脱敏后的 URL（裸路径，无 query 参数）
	Author    string    `json:"author"`     // 脱敏后的作者占位（user-NNN，不含真实用户名）
	Excerpt   string    `json:"excerpt"`    // 帖子 / issue 正文摘录（J1 判断输入）
	FoundAt   time.Time `json:"found_at"`   // 采集时间
}

// Validate 校验单条样本：枚举合法 + 必填字段齐全 + 脱敏红线。
func (s *SeedSample) Validate() error {
	if !ValidRegion(s.Region) {
		return fmt.Errorf("marketing: seed %s 非法 region %q（合法值：intl | cn）", s.ID, s.Region)
	}
	if !ValidProduct(s.Product) {
		return fmt.Errorf("marketing: seed %s 非法 product %q（合法值：network | shop）", s.ID, s.Product)
	}
	if s.Source == "" {
		return fmt.Errorf("marketing: seed %s source 为空", s.ID)
	}
	if s.SourceRef == "" {
		return fmt.Errorf("marketing: seed %s source_ref 为空", s.ID)
	}
	if s.Excerpt == "" {
		return fmt.Errorf("marketing: seed %s excerpt 为空", s.ID)
	}
	if err := checkDesensitized(s); err != nil {
		return err
	}
	return nil
}

// checkDesensitized 脱敏红线（MK-40 验收：无用户名 / 链接参数等敏感信息）：
//   - URL 不得带 query 参数（? 或 # 之后的部分）；
//   - author 必须是 user-NNN 占位（真实用户名一律替换）。
func checkDesensitized(s *SeedSample) error {
	if u := s.URL; u != "" {
		if i := strings.IndexAny(u, "?#"); i >= 0 {
			return fmt.Errorf("marketing: seed %s URL 含链接参数（脱敏红线）: %q", s.ID, u)
		}
	}
	if a := s.Author; a != "" {
		if !strings.HasPrefix(a, "user-") {
			return fmt.Errorf("marketing: seed %s author 疑似真实用户名（须为 user-NNN 占位）: %q", s.ID, a)
		}
	}
	return nil
}

// LoadSeed 读取 dir 下全部 *.json 种子样本（MK-18 跑批的读取约定）。
// 校验：每条样本合法 + id 唯一 + 恰好 10 intl / 10 cn（防「用种子冒充金标集」，
// 条数写进验收，P3 §七）。返回按 id 排序的样本列表。
func LoadSeed(dir string) ([]SeedSample, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("marketing: 读取 seed 目录失败: %w", err)
	}
	var samples []SeedSample
	seen := map[string]string{} // id -> 文件名
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("marketing: 读取 %s 失败: %w", path, err)
		}
		var s SeedSample
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("marketing: %s 不是合法 JSON: %w", path, err)
		}
		if s.ID == "" {
			s.ID = strings.TrimSuffix(e.Name(), ".json")
		}
		if prev, dup := seen[s.ID]; dup {
			return nil, fmt.Errorf("marketing: seed id 重复: %q（%s 与 %s）", s.ID, prev, e.Name())
		}
		seen[s.ID] = e.Name()
		if err := s.Validate(); err != nil {
			return nil, err
		}
		samples = append(samples, s)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].ID < samples[j].ID })

	var intl, cn int
	for _, s := range samples {
		switch s.Region {
		case RegionIntl:
			intl++
		case RegionCN:
			cn++
		}
	}
	if intl != 10 || cn != 10 {
		return nil, fmt.Errorf("marketing: seed 条数不符（intl=%d cn=%d，要求各 10 条）", intl, cn)
	}
	return samples, nil
}
