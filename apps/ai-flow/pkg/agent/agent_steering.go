// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/computeflux/ai-flow/pkg/bus"
	"github.com/computeflux/ai-flow/pkg/constants"
	"github.com/computeflux/ai-flow/pkg/logger"
)

func (al *AgentLoop) processMessageSync(ctx context.Context, msg bus.InboundMessage) {
	if al.channelManager != nil {
		defer al.channelManager.InvokeTypingStop(msg.Channel, msg.ChatID)
	}

	// [workflow-fork] P3-1: the consumer loop runs this synchronously with
	// the publish context, so propagate the message's request-scoped
	// values (board chat turn) into processing.
	response, err := al.processMessage(mergeContextValues(ctx, msg.ValueCtx), msg)
	al.publishResponseOrError(ctx, msg.Channel, msg.ChatID, msg.SessionKey, response, err)
}

// markSteeringActive 在 steering 续接窗口内把本 session 的活跃 turn 槽从
// "pending-" 占位升级为正式 turn。占位阶段（activeTurnStates 里的 pending-*
// 条目）对 getActiveTurnState 不可见，新消息会被认领成独立 turn；升级后同
// session 的新消息进入 steering 队列由本 turn 续接。
func (al *AgentLoop) markSteeringActive(sessionKey string) {
	if sessionKey == "" {
		return
	}
	if cur, loaded := al.activeTurnStates.Load(sessionKey); loaded {
		if ts, ok := cur.(*turnState); ok && strings.HasPrefix(ts.turnID, pendingTurnPrefix) {
			ts.turnID = fmt.Sprintf("steer-%s-%d", sessionKey, al.turnSeq.Add(1))
		}
	}
}

// clearSteeringActive 关闭 steering 窗口：释放本 session 的活跃 turn 槽。
func (al *AgentLoop) clearSteeringActive(sessionKey string) {
	if sessionKey == "" {
		return
	}
	al.activeTurnStates.Delete(sessionKey)
}

func (al *AgentLoop) runTurnWithSteering(ctx context.Context, initialMsg bus.InboundMessage) {
	// [workflow-fork] P3-2: steer 期间（本 turn 从开始到 steering 排空完成），
	// 同 session 的新消息不再被主循环认领为独立 turn，而是留在 steering 队列由
	// 本 turn 续接（见 agent.go 主循环的 isSteeringActive 检查）。没有这个窗口
	// 检查时，board SSE 的「排队追问」会在 <100ms 内被认领成第二个并发 turn，
	// 在 BeginStream 里抢占式替换掉第一个 turn 的会话槽（turns[chatID]）→
	// 第一个 SSE 流的迭代 streamer 永远 detach 不掉 → relay 的 final outbound
	// 被 deliver 吞掉（hasStreamer && streamerDone）→ 流只能等 watchdog 超时，
	// 前端表现为一直“生成中”后报 "The model returned an empty response…"。
	al.markSteeringActive(initialMsg.SessionKey)
	defer al.clearSteeringActive(initialMsg.SessionKey)

	// Process the initial message. [workflow-fork] P3-1: merge the
	// message's request-scoped publisher values (board chat turn) into the
	// processing context; continuation publishes resolve the value via the
	// target (see continuationTarget).
	response, err := al.processMessage(mergeContextValues(ctx, initialMsg.ValueCtx), initialMsg)
	if err != nil {
		if !al.maybePublishError(ctx, initialMsg.Channel, initialMsg.ChatID, initialMsg.SessionKey, err) {
			return // context canceled
		}
		response = ""
	}
	finalResponse := response
	// [workflow-fork] P3-1: internal channels (board SSE) already publish the
	// turn's final content from runAgentLoop (the SendResponse/internal-channel
	// publish). Publishing it again here delivers the final outbound twice; the
	// second delivery arrives after the first finished the turn and detached
	// its streamer, which blanks its content → the SSE result frame ends up
	// empty. Skip the duplicate publish for internal channels.
	publishedInTurn := constants.IsInternalChannel(initialMsg.Channel)

	// Build continuation target. [workflow-fork] P3-2: 带上首条消息的请求级
	// ValueCtx（board turn），续接 turn 的 runAgentLoop 发布 final outbound 时
	// 据此把消息路由回原始 SSE 流（relay 按 ValueCtx 里的 turn 匹配）。
	target, targetErr := al.buildContinuationTarget(initialMsg)
	if target != nil {
		target.ValueCtx = initialMsg.ValueCtx
	}
	if targetErr != nil {
		logger.WarnCF("agent", "Failed to build steering continuation target",
			map[string]any{
				"channel": initialMsg.Channel,
				"error":   targetErr.Error(),
			})
		return
	}
	if target == nil {
		// System message or non-routable, response already published
		return
	}

	_, continuedTurns, continueErr := al.drainQueuedSteeringContinuations(ctx, target)
	if continueErr != nil {
		logger.WarnCF("agent", "Failed to continue queued steering",
			map[string]any{
				"channel": target.Channel,
				"chat_id": target.ChatID,
				"error":   continueErr.Error(),
			})
	}
	if len(continuedTurns) > 0 {
		publishedInTurn = false
	}

	// [workflow-fork] P3-2: 内部渠道（board SSE）的续接回答必须逐条发布：
	// 首 turn 的 final outbound 会 finish 掉 SSE turn，续接 turn 的 final
	// outbound 被 relay 丢弃（turn 已 done）；这里在续接前 Reopen 重新挂起
	// 该 session 的 SSE turn（清终态 + 唤 pump），并把每条续接答案作为
	// final outbound 补发到原始请求的流（relay 的 FinishFromRelay 可重新
	// 完成 turn）。非内部渠道靠末尾的 finalResponse 一次性发布。
	if constants.IsInternalChannel(target.Channel) && len(continuedTurns) > 0 {
		if r := streamChannelReopener; r != nil {
			r.Reopen(target.SessionKey)
		}
		for i := range continuedTurns {
			if strings.TrimSpace(continuedTurns[i].Content) == "" {
				continue
			}
			pubCtx, pubCancel := context.WithTimeout(ctx, 5*time.Second)
			al.publishContinuationResponse(pubCtx, target.Channel, target.ChatID, target.SessionKey, continuedTurns[i].Content, continuedTurns[i].ValueCtx)
			pubCancel()
		}
	}

	// Publish final response: the last continuation's answer when the queue
	// was drained, otherwise the initial turn's response.
	tail := finalResponse
	if n := len(continuedTurns); n > 0 && strings.TrimSpace(continuedTurns[n-1].Content) != "" {
		tail = continuedTurns[n-1].Content
	}
	if tail != "" && !publishedInTurn {
		al.PublishResponseIfNeeded(ctx, target.Channel, target.ChatID, target.SessionKey, tail)
	}
}

// drainQueuedSteeringContinuations drains the session's steering queue after
// the initial turn ends, running each batch as a continuation turn. Returns
// the last continuation's response plus the per-continuation inbound messages
// (carrying each continuation's request-scoped ValueCtx) so internal-channel
// relay publishing can route every answer back to its originating stream.
func (al *AgentLoop) drainQueuedSteeringContinuations(
	ctx context.Context,
	target *continuationTarget,
) (string, []bus.InboundMessage, error) {
	if target == nil {
		return "", nil, nil
	}

	finalResponse := ""
	var continuedTurns []bus.InboundMessage
	for al.pendingSteeringCountForScope(target.SessionKey) > 0 {
		if err := ctx.Err(); err != nil {
			return finalResponse, continuedTurns, err
		}

		logger.InfoCF("agent", "Continuing queued steering after turn end",
			map[string]any{
				"channel":     target.Channel,
				"chat_id":     target.ChatID,
				"session_key": target.SessionKey,
				"queue_depth": al.pendingSteeringCountForScope(target.SessionKey),
			})

		continued, continuedMsgs, continueErr := al.Continue(ctx, target.SessionKey, target.Channel, target.ChatID, target.ValueCtx)
		if continueErr != nil {
			return finalResponse, continuedTurns, continueErr
		}
		if continued == "" && len(continuedMsgs) == 0 {
			break
		}
		if continued != "" {
			finalResponse = continued
		}
		if len(continuedMsgs) > 0 {
			continuedTurns = append(continuedTurns, continuedMsgs...)
		}
	}

	return finalResponse, continuedTurns, nil
}

func (al *AgentLoop) resolveSteeringTarget(msg bus.InboundMessage) (string, string, bool) {
	if msg.Channel == "system" {
		return "", "", false
	}

	route, agent, err := al.resolveMessageRoute(msg)
	if err != nil || agent == nil {
		return "", "", false
	}
	allocation := al.allocateRouteSession(route, msg)

	return resolveScopeKey(allocation.SessionKey, msg.SessionKey), agent.ID, true
}
