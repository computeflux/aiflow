package approval

import "testing"

func TestSignalWhitelist(t *testing.T) {
	for _, name := range []string{"approve", "reject", "inject"} {
		if !IsSignalAllowed(name) {
			t.Errorf("signal %q should be allowed", name)
		}
	}
	for _, name := range []string{"", "hack", "APPROVE", "approve ", "cancel", "skip", "retry"} {
		if IsSignalAllowed(name) {
			t.Errorf("signal %q should NOT be allowed (whitelist 403, S2 §3.4)", name)
		}
	}
}
