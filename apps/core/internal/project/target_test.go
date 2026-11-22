package project

import "testing"

// TestNormalizeTargets 多值目标归一 + 部署单元别名映射：
// 卡 #102 根因是 error_logs.system（部署单元名 api / go-worker）当项目名用 → 调度秒拒；
// 卡 #97 根因是多值目标串直接查注册表 → CD skill_not_found。两者共用本归一函数。
func TestNormalizeTargets(t *testing.T) {
	cases := []struct {
		in   string
		want string
		name string
	}{
		{"api", "core", "API 服务 → core（apps/core）"},
		{"go-worker", "core", "Temporal worker → core"},
		{"workflow-api", "core", "容器名 workflow-api → core"},
		{"API", "core", "大小写不敏感"},
		{"api,core,core-ui", "core,core-ui", "多值逐项映射 + 去重保序（卡 #102 的目标）"},
		{"go-worker, core", "core", "不同别名映射到同一项目 → 去重"},
		{"board-ops,board-product", "core-ui", "board 系 → core-ui"},
		{"pycraper", "pycraper", "已注册项目原样保留"},
		{"pycraper/adapters/x.py", "pycraper", "去文件路径后缀"},
		{"pycraper.FetchActivities.fetch", "pycraper", "去函数名后缀"},
		{"minishop, pi-web", "minishop,pi-web", "多项目原样保留（保序）"},
		{"actives,tee-node,dapp,core-ui,core", "actives,tee-node,dapp,core-ui,core", "审批 payload 典型多值目标（卡 #97）"},
		{"unknown", "unknown", "未识别名字原样返回，交给注册表判定"},
		{"", "", "空 → 空（无目标）"},
		{"  ,  , ", "", "纯空白/逗号 → 空"},
		{" api , api ", "core", "trim + 去重"},
	}
	for _, c := range cases {
		if got := NormalizeTargets(c.in); got != c.want {
			t.Errorf("NormalizeTargets(%q) = %q, want %q（%s）", c.in, got, c.want, c.name)
		}
	}
}

// TestPrimaryTargetName 主目标（归一 + 别名后的第一个）：S1 执行链（沙箱/镜像/CD）只认主目标。
func TestPrimaryTargetName(t *testing.T) {
	cases := map[string]string{
		"actives,tee-node,dapp,core-ui,core": "actives",
		"api,core,core-ui":                   "core",
		"core":                               "core",
		" pycraper , core ":                  "pycraper",
		"":                                   "",
		"  ":                                 "",
	}
	for in, want := range cases {
		if got := PrimaryTargetName(in); got != want {
			t.Errorf("PrimaryTargetName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestParseTargets 拆分归一：trim/去空/去重/保序。
func TestParseTargets(t *testing.T) {
	got := ParseTargets(" pycraper , core ,pycraper, ,")
	want := []string{"pycraper", "core"}
	if len(got) != len(want) {
		t.Fatalf("ParseTargets = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ParseTargets = %v, want %v", got, want)
		}
	}
}
