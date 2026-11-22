package skillgateway

import "testing"

func TestVersionConfigAddr(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`{"name":"like","version":"1.4.1","addr":"http://skill-like-1.4.1:19501"}`, "http://skill-like-1.4.1:19501"},
		{`{"name":"like","version":"1.0.0"}`, ""},
		{"", ""},
		{"null", ""},
		{`not-json`, ""},
	}
	for _, c := range cases {
		if got := VersionConfigAddr(c.in); got != c.want {
			t.Fatalf("VersionConfigAddr(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
