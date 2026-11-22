package deploy

// svcops_ports.go：运维中心运行服务后端（原 internal/serviceops，批次 G 并入 deploy 域）。

import (
	"fmt"
	"strings"

	"github.com/wetee/ai-workflow/internal/dockerapi"
)

// serviceops_ports.go：compose 端口字符串解析（从 serviceops.go 拆出，纯机械搬迁）。

// parsePortBindings 解析端口列表（与 compose 格式一致："host:container" / "ip:host:container" / "container"，
// 容器端口段可带协议后缀如 "8001/udp"）为 Engine API 的 PortBindings map。
// 单段 = 仅容器端口（宿主随机分配，HostPort 留空），与 parsePort 语义对齐。
// 无法解析的条目（空串 / 段数 > 3 / 端口段为空 / 非法协议）返回错误，由调用方映射为 400。
func parsePortBindings(ports []string) (map[string][]dockerapi.PortBinding, error) {
	out := map[string][]dockerapi.PortBinding{}
	for _, p := range ports {
		p = strings.TrimSpace(p)
		if p == "" {
			continue // 空行忽略
		}
		parts := strings.Split(p, ":")
		// 容器端口段是最后一段，可能带协议后缀（"8001/udp"）；拆出端口 + 协议（缺省 tcp）。
		containerSeg := parts[len(parts)-1]
		port, proto, perr := splitPortProto(containerSeg)
		if perr != nil {
			return nil, fmt.Errorf("非法端口条目 %q: %w", p, perr)
		}
		if port == "" {
			return nil, fmt.Errorf("非法端口条目 %q（容器端口段为空）", p)
		}
		key := port + "/" + proto
		switch len(parts) {
		case 1:
			// 单段 = 仅容器端口（宿主随机分配），与 compose parsePort 语义一致
			out[key] = append(out[key], dockerapi.PortBinding{HostIP: "", HostPort: ""})
		case 2:
			if parts[0] == "" {
				return nil, fmt.Errorf("非法端口条目 %q（宿主端口段为空）", p)
			}
			out[key] = append(out[key], dockerapi.PortBinding{HostIP: "", HostPort: parts[0]})
		case 3:
			if parts[1] == "" {
				return nil, fmt.Errorf("非法端口条目 %q（宿主端口段为空）", p)
			}
			out[key] = append(out[key], dockerapi.PortBinding{HostIP: parts[0], HostPort: parts[1]})
		default:
			return nil, fmt.Errorf("非法端口条目 %q（需 host:container / ip:host:container / container 格式）", p)
		}
	}
	return out, nil
}

// splitPortProto 拆出端口段的端口号与协议（"8001/udp" → ("8001","udp",nil)；无协议 → 缺省 tcp）。
// 未知协议（如 "3478/ucp"）返回 error，由调用方映射为 400——不能静默改写为 tcp（会悄悄改变端口语义）。
func splitPortProto(seg string) (port, proto string, err error) {
	if i := strings.IndexByte(seg, '/'); i >= 0 {
		port = seg[:i]
		proto = strings.ToLower(seg[i+1:])
		if proto != "tcp" && proto != "udp" && proto != "sctp" {
			return "", "", fmt.Errorf("未知协议 %q（仅支持 tcp/udp/sctp）", proto)
		}
		return port, proto, nil
	}
	return seg, "tcp", nil
}
