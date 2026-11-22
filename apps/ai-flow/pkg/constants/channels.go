// Package constants provides shared constants across the codebase.
package constants

// internalChannels defines channels that are used for internal communication
// and should not be exposed to external users or recorded as last active channel.
var internalChannels = map[string]struct{}{
	"cli":      {},
	"system":   {},
	"subagent": {},
	// [workflow-fork] P3-1: board = board 前端 AI 侧栏虚拟渠道（POST /chat SSE）。
	// 投递完全由渠道自身的 relay goroutine 完成（manager 对内部渠道跳过 worker
	// 分发），不经过平台 worker，因此必须登记为内部渠道。
	"board": {},
}

// IsInternalChannel returns true if the channel is an internal channel.
func IsInternalChannel(channel string) bool {
	_, found := internalChannels[channel]
	return found
}
