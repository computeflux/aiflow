package marketing

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 双轨一致性是 MK-19 的核心不变量：ensureLeadsDDL / ensureActionsDDL
// （服务自启兜底）与 hacks/sql/marketing_leads.sql（bootstrap.sh）必须描述
// 同一组表，否则两条建表路径会产出结构不一致的 mk_leads / mk_actions。
// ddlColumns 复用 store_judgment_test.go 的实现（同包）。

// splitCreateTables 把多语句 DDL 文件按表名切成各表 CREATE TABLE 语句
// （文件头注释里可能含 "CREATE TABLE" 字样，不能直接 Split；先按 ; 切语句）。
func splitCreateTables(t *testing.T, raw string) map[string]string {
	t.Helper()
	re := regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?` + "`?" + `([A-Za-z0-9_]+)` + "`?")
	tables := map[string]string{}
	for _, stmt := range strings.Split(raw, ";") {
		m := re.FindStringSubmatchIndex(stmt)
		if m == nil {
			continue
		}
		name := stmt[m[2]:m[3]]
		if _, dup := tables[name]; dup {
			continue
		}
		tables[name] = stmt
	}
	return tables
}

func TestLeadAction_TableName(t *testing.T) {
	if got := (Lead{}).TableName(); got != "mk_leads" {
		t.Fatalf("Lead.TableName() = %q, want mk_leads", got)
	}
	if got := (Action{}).TableName(); got != "mk_actions" {
		t.Fatalf("Action.TableName() = %q, want mk_actions", got)
	}
}

func TestEnsureLeadsDDL_RequiredColumns(t *testing.T) {
	cols := ddlColumns(t, ensureLeadsDDL)
	required := []string{
		"id", "region", "product", "source", "source_ref", "url", "author",
		"excerpt", "is_real_need", "need_type", "intervene", "pain", "score",
		"confidence", "judgment_id", "idem_key", "status", "found_at",
	}
	have := map[string]bool{}
	for _, c := range cols {
		have[c] = true
	}
	for _, r := range required {
		if !have[r] {
			t.Fatalf("ensureLeadsDDL missing column %q (cols=%v)", r, cols)
		}
	}
}

func TestEnsureActionsDDL_RequiredColumns(t *testing.T) {
	cols := ddlColumns(t, ensureActionsDDL)
	required := []string{
		"id", "region", "product", "lead_id", "agent", "kind", "channel",
		"account", "target_url", "body", "draft_judgment_id",
		"critique_judgment_id", "compliance_judgment_id", "status",
		"idem_key", "created_at",
	}
	have := map[string]bool{}
	for _, c := range cols {
		have[c] = true
	}
	for _, r := range required {
		if !have[r] {
			t.Fatalf("ensureActionsDDL missing column %q (cols=%v)", r, cols)
		}
	}
}

func TestEnsureLeadsDDL_IdemKeyUnique(t *testing.T) {
	// 幂等硬要求：mk_leads.idem_key 必须建唯一键。
	re := regexp.MustCompile(`(?i)UNIQUE\s+KEY\s+uk_idem\s*\(\s*idem_key\s*\)`)
	if !re.MatchString(ensureLeadsDDL) {
		t.Fatalf("ensureLeadsDDL must have UNIQUE KEY uk_idem (idem_key): %s", ensureLeadsDDL)
	}
}

func TestEnsureActionsDDL_IdemKeyUnique(t *testing.T) {
	re := regexp.MustCompile(`(?i)UNIQUE\s+KEY\s+uk_idem\s*\(\s*idem_key\s*\)`)
	if !re.MatchString(ensureActionsDDL) {
		t.Fatalf("ensureActionsDDL must have UNIQUE KEY uk_idem (idem_key): %s", ensureActionsDDL)
	}
}

func TestLeadActionEnsureDDL_MatchesSQLFile(t *testing.T) {
	// 从包目录定位 hacks/sql/marketing_leads.sql（internal/marketing → 仓库 apps/core 根）。
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	sqlPath := filepath.Join(wd, "..", "..", "hacks", "sql", "marketing_leads.sql")
	raw, err := os.ReadFile(sqlPath)
	if err != nil {
		t.Fatalf("read %s: %v", sqlPath, err)
	}
	fileTables := splitCreateTables(t, string(raw))
	goTables := map[string]string{
		"mk_leads":   ensureLeadsDDL,
		"mk_actions": ensureActionsDDL,
	}
	for name, goDDL := range goTables {
		fileDDL, ok := fileTables[name]
		if !ok {
			t.Fatalf("%s missing table %q", sqlPath, name)
		}
		fileCols := ddlColumns(t, fileDDL)
		goCols := ddlColumns(t, goDDL)
		sort.Strings(fileCols)
		sort.Strings(goCols)
		if strings.Join(fileCols, ",") != strings.Join(goCols, ",") {
			t.Fatalf("%s and ensureDDL drifted for %s:\n  sql file: %v\n  ensureDDL: %v", sqlPath, name, fileCols, goCols)
		}
	}
}

// TestLeadStatusConstants 防回归：lead status 枚举非空且互异。
func TestLeadStatusConstants(t *testing.T) {
	statuses := []string{LeadStatusNew, LeadStatusReviewed, LeadStatusConverted, LeadStatusDiscarded}
	seen := map[string]bool{}
	for _, s := range statuses {
		if s == "" || seen[s] {
			t.Errorf("lead status 常量非法或重复: %q", s)
		}
		seen[s] = true
		if !ValidLeadStatus(s) {
			t.Errorf("ValidLeadStatus(%q) = false, want true", s)
		}
	}
}

// TestActionStatusConstants 防回归：action status 枚举非空且互异，
// 且 draft → reviewing 是规格 §6.2 的强制人工路径。
func TestActionStatusConstants(t *testing.T) {
	statuses := []string{
		ActionStatusDraft, ActionStatusReviewing, ActionStatusApproved,
		ActionStatusRejected, ActionStatusSent, ActionStatusFailed,
	}
	seen := map[string]bool{}
	for _, s := range statuses {
		if s == "" || seen[s] {
			t.Errorf("action status 常量非法或重复: %q", s)
		}
		seen[s] = true
		if !ValidActionStatus(s) {
			t.Errorf("ValidActionStatus(%q) = false, want true", s)
		}
	}
	for _, bad := range []string{"", "DRAFT", "done", "sent!"} {
		if ValidActionStatus(bad) {
			t.Errorf("ValidActionStatus(%q) = true, want false", bad)
		}
	}
}

// TestIsDuplicateKeyError 表驱动：唯一键冲突识别（MySQL 1062 / sqlite UNIQUE）。
func TestIsDuplicateKeyError(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"", false},
		{"Error 1062 (23000): Duplicate entry 'abc' for key 'mk_leads.uk_idem'", true},
		{"UNIQUE constraint failed: mk_actions.idem_key", true},
		{"unique constraint failed: mk_leads.idem_key", true},
		{"connection refused", false},
	}
	for _, c := range cases {
		var err error
		if c.msg != "" {
			err = &testError{c.msg}
		}
		if got := IsDuplicateKeyError(err); got != c.want {
			t.Errorf("IsDuplicateKeyError(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }
