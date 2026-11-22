// Command hc 是最小健康检查探针，用于无 shell 的镜像（distroless）做 compose healthcheck。
//
// 用法：
//
//	hc port <host:port>    TCP 拨号（3s 超时），成功 exit 0
//	hc proc                检查 PID 1 存活且未僵尸化（/proc/1/stat 状态位非 Z/X）
//
// 独立小模块（stdlib only，无外部依赖），由 deploy/Dockerfile.go 单独编译后拷入目标镜像。
package main

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

func main() {
	mode := "port"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}

	switch mode {
	case "port":
		if len(os.Args) < 3 {
			fatal("usage: hc port <host:port>")
		}
		conn, err := net.DialTimeout("tcp", os.Args[2], 3*time.Second)
		if err != nil {
			fatal(err.Error())
		}
		_ = conn.Close()
	case "proc":
		data, err := os.ReadFile("/proc/1/stat")
		if err != nil {
			fatal(err.Error())
		}
		// 格式：pid (comm) state ...；comm 可能含空格/括号，取最后一个 ')' 之后的状态位
		idx := strings.LastIndexByte(string(data), ')')
		if idx < 0 || idx+2 >= len(data) {
			fatal("unparseable /proc/1/stat")
		}
		state := string(data[idx+1 : idx+2])
		if state == "Z" || state == "X" {
			fatal("pid 1 state: " + state)
		}
	default:
		fatal("unknown mode: " + mode)
	}
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "hc: "+msg)
	os.Exit(1)
}
