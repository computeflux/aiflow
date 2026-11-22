package deploy

// svcops_test.go：运维中心运行服务后端（原 internal/serviceops，批次 G 并入 deploy 域）。

import (
	"reflect"
	"testing"

	"github.com/wetee/ai-workflow/internal/dockerapi"
)

// TestParsePortBindings 覆盖端口解析的各形态与非法输入（改参回传格式与 compose 对齐）。
func TestParsePortBindings(t *testing.T) {
	tests := []struct {
		name    string
		ports   []string
		want    map[string][]dockerapi.PortBinding
		wantErr bool
	}{
		{
			name:  "host:container",
			ports: []string{"8001:8001"},
			want: map[string][]dockerapi.PortBinding{
				"8001/tcp": {{HostIP: "", HostPort: "8001"}},
			},
		},
		{
			name:  "ip:host:container",
			ports: []string{"127.0.0.1:5181:80"},
			want: map[string][]dockerapi.PortBinding{
				"80/tcp": {{HostIP: "127.0.0.1", HostPort: "5181"}},
			},
		},
		{
			name:  "container-only (random host port)",
			ports: []string{"9000"},
			want: map[string][]dockerapi.PortBinding{
				"9000/tcp": {{HostIP: "", HostPort: ""}},
			},
		},
		{
			name:  "udp protocol suffix",
			ports: []string{"53:53/udp"},
			want: map[string][]dockerapi.PortBinding{
				"53/udp": {{HostIP: "", HostPort: "53"}},
			},
		},
		{
			name:  "ip:host:container/udp",
			ports: []string{"127.0.0.1:5353:53/udp"},
			want: map[string][]dockerapi.PortBinding{
				"53/udp": {{HostIP: "127.0.0.1", HostPort: "5353"}},
			},
		},
		{
			name:  "container-only with udp",
			ports: []string{"5353/udp"},
			want: map[string][]dockerapi.PortBinding{
				"5353/udp": {{HostIP: "", HostPort: ""}},
			},
		},
		{
			name:  "multiple entries",
			ports: []string{"8001:8001", "127.0.0.1:5181:80"},
			want: map[string][]dockerapi.PortBinding{
				"8001/tcp": {{HostIP: "", HostPort: "8001"}},
				"80/tcp":   {{HostIP: "127.0.0.1", HostPort: "5181"}},
			},
		},
		{
			name:  "empty lines ignored",
			ports: []string{"", "  ", "8001:8001"},
			want: map[string][]dockerapi.PortBinding{
				"8001/tcp": {{HostIP: "", HostPort: "8001"}},
			},
		},
		{
			name:    "too many segments",
			ports:   []string{"a:b:c:d"},
			wantErr: true,
		},
		{
			name:    "empty host segment",
			ports:   []string{":8001"},
			wantErr: true,
		},
		{
			name:    "empty container segment",
			ports:   []string{"8001:"},
			wantErr: true,
		},
		{
			name:    "unknown protocol",
			ports:   []string{"3478/ucp"},
			wantErr: true,
		},
		{
			name:  "empty list",
			ports: []string{},
			want:  map[string][]dockerapi.PortBinding{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePortBindings(tt.ports)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parsePortBindings(%v) expected error, got nil", tt.ports)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePortBindings(%v) unexpected error: %v", tt.ports, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parsePortBindings(%v) = %v, want %v", tt.ports, got, tt.want)
			}
		})
	}
}

// TestRedactEnv 覆盖敏感键名 + 凭据值形态的脱敏（服务端红线：不落明文密钥）。
func TestRedactEnv(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want map[string]string
	}{
		{
			name: "empty",
			env:  map[string]string{},
			want: map[string]string{},
		},
		{
			name: "sensitive keys redacted",
			env: map[string]string{
				"POSTGRES_PASSWORD": "secret123",
				"X_API_KEY":         "abc",
				"JWT_SECRET":        "xyz",
				"APP_PORT":          "8001",
			},
			want: map[string]string{
				"POSTGRES_PASSWORD": "<redacted>",
				"X_API_KEY":         "<redacted>",
				"JWT_SECRET":        "<redacted>",
				"APP_PORT":          "8001",
			},
		},
		{
			name: "credential value form redacted",
			env: map[string]string{
				"DATABASE_URL": "postgres://user:pass@host:5432/db",
				"REDIS_URL":    "redis://localhost:6379",
			},
			want: map[string]string{
				"DATABASE_URL": "<redacted>",             // 含 user:pass@
				"REDIS_URL":    "redis://localhost:6379", // 无凭据
			},
		},
		{
			name: "case-insensitive key match",
			env: map[string]string{
				"db_password": "p",
				"MY_TOKEN":    "t",
			},
			want: map[string]string{
				"db_password": "<redacted>",
				"MY_TOKEN":    "<redacted>",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redactEnv(tt.env)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("redactEnv(%v) = %v, want %v", tt.env, got, tt.want)
			}
		})
	}
}

// TestIsSensitiveKey 覆盖敏感键判定：复合键命中、auth 独立成词命中、OAUTH_*/AUTHORITY 不误伤。
func TestIsSensitiveKey(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{"POSTGRES_PASSWORD", true},
		{"X_API_KEY", true},
		{"JWT_SECRET", true},
		{"DB_DSN", true},
		{"AUTH_TOKEN", true},       // auth 独立成词（下划线分词）
		{"MY_AUTH", true},          // auth 独立成词
		{"OAUTH_CLIENT_ID", false}, // auth 非独立词，不误伤
		{"AUTHORITY", false},       // auth 非独立词，不误伤
		{"APP_PORT", false},
		{"LOG_LEVEL", false},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			if got := isSensitiveKey(tt.key); got != tt.want {
				t.Fatalf("isSensitiveKey(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

// TestRedactLogs 覆盖日志脱敏：行首裸键、复合键、凭据值形态、非敏感内容保留。
func TestRedactLogs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "bare key at line start",
			in:   "PASSWORD=supersecret",
			want: "PASSWORD=<redacted>",
		},
		{
			name: "compound key",
			in:   "2026-01-01 DB_PASSWORD=hunter2 ready",
			want: "2026-01-01 DB_PASSWORD=<redacted> ready",
		},
		{
			name: "credential value form",
			in:   "connecting to postgres://user:pass@host:5432/db",
			want: "connecting to postgres://<redacted>@host:5432/db",
		},
		{
			name: "non-sensitive preserved",
			in:   "server started on :8001",
			want: "server started on :8001",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactLogs(tt.in); got != tt.want {
				t.Fatalf("redactLogs(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestBuildNetworkingConfig 覆盖网络别名重建（服务名 != 容器名时同行服务按服务名解析依赖它）。
func TestBuildNetworkingConfig(t *testing.T) {
	t.Run("nil network settings", func(t *testing.T) {
		full := &dockerapi.ContainerFull{}
		if got := buildNetworkingConfig(full); got != nil {
			t.Fatalf("buildNetworkingConfig(nil networks) = %v, want nil", got)
		}
	})

	t.Run("empty networks", func(t *testing.T) {
		full := &dockerapi.ContainerFull{NetworkSettings: &dockerapi.NetworkSettings{Networks: map[string]any{}}}
		if got := buildNetworkingConfig(full); got != nil {
			t.Fatalf("buildNetworkingConfig(empty) = %v, want nil", got)
		}
	})

	t.Run("aliases preserved", func(t *testing.T) {
		full := &dockerapi.ContainerFull{
			NetworkSettings: &dockerapi.NetworkSettings{
				Networks: map[string]any{
					"workflow_default": map[string]any{
						"Aliases": []any{"qdrant", "workflow-qdrant"},
					},
				},
			},
		}
		got := buildNetworkingConfig(full)
		if got == nil {
			t.Fatal("buildNetworkingConfig(aliases) = nil, want non-nil")
		}
		ec, ok := got["EndpointsConfig"].(map[string]any)
		if !ok {
			t.Fatalf("EndpointsConfig missing or wrong type: %v", got)
		}
		entry, ok := ec["workflow_default"].(map[string]any)
		if !ok {
			t.Fatalf("workflow_default entry missing: %v", ec)
		}
		aliases, ok := entry["Aliases"].([]string)
		if !ok {
			t.Fatalf("Aliases missing or wrong type: %v", entry)
		}
		want := []string{"qdrant", "workflow-qdrant"}
		if !reflect.DeepEqual(aliases, want) {
			t.Fatalf("Aliases = %v, want %v", aliases, want)
		}
	})

	t.Run("network without aliases yields empty entry", func(t *testing.T) {
		full := &dockerapi.ContainerFull{
			NetworkSettings: &dockerapi.NetworkSettings{
				Networks: map[string]any{
					"some_net": map[string]any{},
				},
			},
		}
		got := buildNetworkingConfig(full)
		if got == nil {
			t.Fatal("buildNetworkingConfig(no aliases) = nil, want non-nil (network still listed)")
		}
		ec := got["EndpointsConfig"].(map[string]any)
		entry := ec["some_net"].(map[string]any)
		if _, has := entry["Aliases"]; has {
			t.Fatalf("expected no Aliases key, got %v", entry)
		}
	})
}
