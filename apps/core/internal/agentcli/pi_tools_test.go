package agentcli

import (
	"io"
	"strings"
	"testing"
)

func TestLoadCoderToolPresetDefaultsToFull(t *testing.T) {
	t.Setenv("PI_TOOL_PRESET", "")
	if got := LoadCoderToolPreset(io.Discard); got != "full" {
		t.Fatalf("默认应为 full，得到 %q", got)
	}
}

func TestLoadCoderToolPresetOverride(t *testing.T) {
	for _, preset := range []string{"none", "read-only", "default", "full", " FULL "} {
		t.Setenv("PI_TOOL_PRESET", preset)
		want := strings.ToLower(strings.TrimSpace(preset))
		if got := LoadCoderToolPreset(io.Discard); got != want {
			t.Fatalf("PI_TOOL_PRESET=%q 应得到 %q，得到 %q", preset, want, got)
		}
	}
}

func TestLoadCoderToolPresetFallsBackOnInvalid(t *testing.T) {
	t.Setenv("PI_TOOL_PRESET", "everything")
	var log strings.Builder
	if got := LoadCoderToolPreset(&log); got != "full" {
		t.Fatalf("非法值应回落 full，得到 %q", got)
	}
	if !strings.Contains(log.String(), "非法") {
		t.Fatalf("应记录非法值告警，得到 %q", log.String())
	}
}
