package marketing

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 双轨一致性是 MK-11 的核心不变量：ensureDDL（服务自启兜底）与
// hacks/sql/marketing_judgments.sql（bootstrap.sh）必须描述同一张表，
// 否则两条建表路径会产出结构不一致的 mk_judgments。

// ddlColumns 从 CREATE TABLE 语句里提取列名（跳过 KEY/UNIQUE/PRIMARY/CONSTRAINT/INDEX 行）。
func ddlColumns(t *testing.T, ddl string) []string {
	t.Helper()
	body := ddl
	if i := strings.Index(body, "("); i >= 0 {
		body = body[i+1:]
	}
	if i := strings.LastIndex(body, ")"); i >= 0 {
		body = body[:i]
	}
	var cols []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ","))
		if line == "" {
			continue
		}
		upper := strings.ToUpper(line)
		if strings.HasPrefix(upper, "KEY ") || strings.HasPrefix(upper, "UNIQUE ") ||
			strings.HasPrefix(upper, "PRIMARY ") || strings.HasPrefix(upper, "CONSTRAINT ") ||
			strings.HasPrefix(upper, "INDEX ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		cols = append(cols, strings.Trim(fields[0], "`"))
	}
	return cols
}

func TestEnsureDDL_RequiredColumns(t *testing.T) {
	cols := ddlColumns(t, ensureDDL)
	required := []string{
		"id", "region", "product", "kind", "source", "source_ref",
		"model", "prompt_version", "context_hash", "context_snapshot",
		"raw_output", "verdict", "reason", "created_at",
	}
	have := map[string]bool{}
	for _, c := range cols {
		have[c] = true
	}
	for _, r := range required {
		if !have[r] {
			t.Fatalf("ensureDDL missing column %q (cols=%v)", r, cols)
		}
	}
}

func TestEnsureDDL_ContextHashIndexed(t *testing.T) {
	// 回放按 context_hash 查，必须建索引。
	re := regexp.MustCompile(`(?i)KEY\s+idx_context_hash\s*\(\s*context_hash\s*\)`)
	if !re.MatchString(ensureDDL) {
		t.Fatalf("ensureDDL must index context_hash for replay: %s", ensureDDL)
	}
}

func TestEnsureDDL_MatchesSQLFile(t *testing.T) {
	// 从包目录定位 hacks/sql/marketing_judgments.sql（internal/marketing → 仓库 apps/core 根）。
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	sqlPath := filepath.Join(wd, "..", "..", "hacks", "sql", "marketing_judgments.sql")
	raw, err := os.ReadFile(sqlPath)
	if err != nil {
		t.Fatalf("read %s: %v", sqlPath, err)
	}
	fileCols := ddlColumns(t, string(raw))
	goCols := ddlColumns(t, ensureDDL)

	sort.Strings(fileCols)
	sort.Strings(goCols)
	if strings.Join(fileCols, ",") != strings.Join(goCols, ",") {
		t.Fatalf("ensureDDL and %s drifted:\n  sql file: %v\n  ensureDDL: %v", sqlPath, fileCols, goCols)
	}
}

func TestJudgment_TableName(t *testing.T) {
	if got := (Judgment{}).TableName(); got != "mk_judgments" {
		t.Fatalf("TableName() = %q, want mk_judgments", got)
	}
}
