package deploy

import "testing"

func TestVersionContainerName(t *testing.T) {
	if got := VersionContainerName("like", "1.4.1"); got != "skill-like-1.4.1" {
		t.Fatalf("got %q", got)
	}
}

func TestVersionPort(t *testing.T) {
	// 195xx 段
	for _, name := range []string{"like", "x-scan", "generate", "evaluate", "search"} {
		p := VersionPort(name)
		if p < 19500 || p >= 19500+100 {
			t.Fatalf("%s port %d not in 195xx", name, p)
		}
	}
	// 确定性
	if VersionPort("like") != VersionPort("like") {
		t.Fatal("not deterministic")
	}
}

func TestParseMemLimit(t *testing.T) {
	cases := map[string]int64{
		"512m":    512 << 20,
		"1g":      1 << 30,
		"2gb":     2 << 30,
		"256M":    256 << 20,
		"1048576": 1048576,
		"":        0,
		"abc":     0,
	}
	for in, want := range cases {
		if got := parseMemLimit(in); got != want {
			t.Fatalf("parseMemLimit(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseCPUs(t *testing.T) {
	if got := parseCPUs("0.5"); got != 500000000 {
		t.Fatalf("parseCPUs(0.5) = %d", got)
	}
	if got := parseCPUs("1.0"); got != 1000000000 {
		t.Fatalf("parseCPUs(1.0) = %d", got)
	}
	if got := parseCPUs(""); got != 0 {
		t.Fatalf("parseCPUs() = %d", got)
	}
}
