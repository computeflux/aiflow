// judge.go — MK-14 judge 主流程（docs/marketing/plan/P2-判断服务地基.md MK-14）。
//
// 把 MK-11/12/13/15 串成完整判断主流程：
//
//	组装上下文（MK-13）→ llm.ChatCompletion → 解析（1 次重试，MK-12 规则1）
//	→ validate（MK-12 规则2–6）→ 落库（MK-11）→ 返回。
//
// 硬要求（规格 §2.1 护栏）：
//   - 判废也要落库：任一校验规则命中（verdict=reject）同样写 mk_judgments，
//     保证「当时模型看到了什么」+ 模型原始输出可回放；
//   - intervene=no 也要落库（判断结论不是落库条件）。
//
// LLM 以接口（LLMClient）注入，便于 mock；现有实现是 internal/llm.Client
// （ChatCompletion 签名一致，*llm.Client 可直接传入，本包不 import internal/llm，
// 靠 Go 结构化类型隐式满足，避免耦合）。
//
// 边界：不做队列、不做 Temporal 编排（后续卡）；不写 prompt 正文（MK-15，
// 本卡只调用 LoadJudgeLeadPrompt）。一卡一组文件（P2 串行红线）：
// 本卡只动 judge.go / judge_test.go。
package marketing

import (
	"context"
	"encoding/json"
	"fmt"
)

// LLMClient 是 LLM 依赖接口（注入便于 mock）。
// 现有实现是 internal/llm.Client（ChatCompletion 签名一致，可直接传入）。
type LLMClient interface {
	ChatCompletion(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// JudgeLeadPromptVersion 是 J1（judge_lead）prompt 的当前版本（代码内嵌）。
// 换 prompt 正文时 bump 此常量；落库 prompt_version 用它标记「实际用的哪个 prompt」，
// 金标回归 / prompt 对比按 prompt_version 分组（换 prompt 必跑金标回归，规格 §2）。
const JudgeLeadPromptVersion = "judge_lead/v1"

// JudgeRequest 是 judge 主流程的输入。
type JudgeRequest struct {
	Kind          string       // 判断点 kind（J1–J18，见 Kind* 常量）
	Region        string       // intl | cn
	Product       string       // network | shop
	Source        string       // 判断输入来源（github|reddit|x|zhihu|...）
	SourceRef     string       // 来源引用（issue 号 / 帖子 URL / 线索 id）
	Model         string       // 模型名（回放时确认「哪个模型判的」）
	PromptVersion string       // prompt 版本（换 prompt 必跑金标回归）
	Context       ContextInput // 上下文包原料（MK-13）
}

// Judge 是判断主流程（MK-14）：
//
//	组装上下文（MK-13）→ llm.ChatCompletion → 解析（1 次重试，MK-12 规则1）
//	→ validate（MK-12 规则2–6）→ 落库（MK-11）→ 返回。
//
// 返回约定：
//   - 通过：(Judgment{Verdict: pass}, nil)；
//   - 判废（任一校验规则命中）：(Judgment{Verdict: reject, Reason: 判废理由}, nil)——
//     判废也要落库（规格 §2.1 护栏：intervene=no 同样落库）；
//   - 硬失败（上下文组装 / LLM 调用 / 落库出错）：(Judgment{}, err)。
func Judge(ctx context.Context, store *Store, llm LLMClient, req JudgeRequest) (Judgment, error) {
	// 1. 组装上下文（MK-13）：非法枚举 / 上下文串味 → 不产出上下文包，硬失败。
	snapshot, hash, err := AssembleContext(req.Context)
	if err != nil {
		return Judgment{}, fmt.Errorf("marketing: judge 组装上下文: %w", err)
	}

	// 2. 加载 prompt（MK-15）：当前只实现 J1，其他 kind 属后续卡。
	// promptVersion 由实际加载的 prompt 决定（代码内嵌版本），不取 req.PromptVersion——
	// 落库版本必须反映「这次实际用的哪个 prompt」，金标回归按 prompt_version 分组才正确。
	systemPrompt, promptVersion, err := loadPrompt(req.Kind, req.Region)
	if err != nil {
		return Judgment{}, err
	}

	// 3. 调模型：user prompt = 上下文包快照（「当时模型看到了什么」）。
	userPrompt, err := json.Marshal(snapshot)
	if err != nil {
		return Judgment{}, fmt.Errorf("marketing: judge 序列化上下文包: %w", err)
	}
	raw, err := llm.ChatCompletion(ctx, systemPrompt, string(userPrompt))
	if err != nil {
		return Judgment{}, fmt.Errorf("marketing: judge LLM 调用: %w", err)
	}

	// 4. 解析（1 次重试）+ 校验（MK-12）：规则1 命中时把解析错误回灌、重试一次。
	// latestRaw 跟踪最后一次模型原始输出（ValidateResult 不回传 raw，落库 RawOutput 用）。
	latestRaw := raw
	retry := func(feedback string) (string, error) {
		r, err := llm.ChatCompletion(ctx, systemPrompt, string(userPrompt)+"\n\n"+feedback)
		if err == nil {
			latestRaw = r
		}
		return r, err
	}
	res := ValidateWithRetry(ValidateInput{
		Region: req.Region,
		Kind:   req.Kind,
		Raw:    raw,
		Source: sourceText(req.Context),
	}, retry)

	// 5. 组装判断记录（判废也要落库，规格 §2.1 护栏）。
	// PromptVersion 用实际加载的 prompt 版本（非 req.PromptVersion）：换 prompt 后
	// 新记录自动标记新版本，金标回归 / prompt 对比按 prompt_version 分组才正确。
	j := Judgment{
		Region:          req.Region,
		Product:         req.Product,
		Kind:            req.Kind,
		Source:          req.Source,
		SourceRef:       req.SourceRef,
		Model:           req.Model,
		PromptVersion:   promptVersion,
		ContextHash:     hash,
		ContextSnapshot: string(userPrompt),
		RawOutput:       latestRaw,
	}
	if res.OK {
		j.Verdict = VerdictPass
		if r, ok := res.Parsed["reason"].(string); ok {
			j.Reason = r
		}
	} else {
		j.Verdict = VerdictReject
		j.Reason = fmt.Sprintf("[%s] %s", res.Rule, res.Reason)
	}

	// 6. 落库（MK-11）。
	if err := store.Create(ctx, &j); err != nil {
		return Judgment{}, fmt.Errorf("marketing: judge 落库: %w", err)
	}
	return j, nil
}

// JudgeFromSnapshot 是「按 snapshot 直接重跑」入口（MK-16 /replay 用）：
//
//	原 snapshot（当时的 user prompt）→ 当前 prompt 正文 → llm.ChatCompletion
//	→ 解析（1 次重试）→ validate → 落库（新记录）→ 返回。
//
// 与 Judge 的区别：不重新组装上下文（snapshot 本身就是「当时模型看到了什么」，
// 直接复用为 user prompt），因此不存在「反序列化回 ContextInput 丢原料」的问题；
// 重跑用的是**当前** prompt 正文，prompt_version 记当前版本，便于对比 prompt 改动。
//
// 返回约定同 Judge：通过 (pass, nil) / 判废 (reject, nil) / 硬失败 (Judgment{}, err)。
// 重跑结果同样落库（新记录，原记录不动）。
func JudgeFromSnapshot(ctx context.Context, store *Store, llm LLMClient, orig Judgment, model string) (Judgment, error) {
	// 1. 解析原 snapshot（落库的 context_snapshot 是 ContextSnapshot JSON）。
	var snap ContextSnapshot
	if err := json.Unmarshal([]byte(orig.ContextSnapshot), &snap); err != nil {
		return Judgment{}, fmt.Errorf("marketing: replay 解析 snapshot: %w", err)
	}
	if !ValidKind(snap.Kind) {
		return Judgment{}, fmt.Errorf("marketing: replay 非法 kind %q", snap.Kind)
	}
	if !ValidRegion(snap.Region) {
		return Judgment{}, fmt.Errorf("marketing: replay 非法 region %q（合法值：intl | cn）", snap.Region)
	}

	// 2. 加载**当前** prompt 正文（换 prompt 后重跑即对比新 prompt）。
	systemPrompt, promptVersion, err := loadPrompt(snap.Kind, snap.Region)
	if err != nil {
		return Judgment{}, err
	}

	// 3. 调模型：user prompt = 原 snapshot（「当时模型看到了什么」，直接复用）。
	raw, err := llm.ChatCompletion(ctx, systemPrompt, orig.ContextSnapshot)
	if err != nil {
		return Judgment{}, fmt.Errorf("marketing: replay LLM 调用: %w", err)
	}

	// 4. 解析（1 次重试）+ 校验（MK-12）：原文从 snapshot sections 还原（规则3 quote 匹配用）。
	latestRaw := raw
	retry := func(feedback string) (string, error) {
		r, err := llm.ChatCompletion(ctx, systemPrompt, orig.ContextSnapshot+"\n\n"+feedback)
		if err == nil {
			latestRaw = r
		}
		return r, err
	}
	res := ValidateWithRetry(ValidateInput{
		Region: snap.Region,
		Kind:   snap.Kind,
		Raw:    raw,
		Source: snap.SourceText(),
	}, retry)

	// 5. 组装重跑记录（新记录，原记录不动）。PromptVersion 用当前版本（非 orig 的旧版本）。
	j := Judgment{
		Region:          snap.Region,
		Product:         orig.Product,
		Kind:            snap.Kind,
		Source:          orig.Source,
		SourceRef:       orig.SourceRef,
		Model:           model, // 实际使用的模型（当前 LLM_MODEL），非 orig.Model
		PromptVersion:   promptVersion,
		ContextHash:     orig.ContextHash,
		ContextSnapshot: orig.ContextSnapshot,
		RawOutput:       latestRaw,
	}
	if res.OK {
		j.Verdict = VerdictPass
		if r, ok := res.Parsed["reason"].(string); ok {
			j.Reason = r
		}
	} else {
		j.Verdict = VerdictReject
		j.Reason = fmt.Sprintf("[%s] %s", res.Rule, res.Reason)
	}

	// 6. 落库（新记录）。
	if err := store.Create(ctx, &j); err != nil {
		return Judgment{}, fmt.Errorf("marketing: replay 落库: %w", err)
	}
	return j, nil
}

// loadPrompt 按 (kind, region) 加载 prompt 正文（MK-15）。
// 返回 (正文, 版本, err)：版本由实际加载的 prompt 决定（代码内嵌版本常量），
// 落库时用它标记「这次实际用的哪个 prompt」，不取调用方传入的 PromptVersion。
// 当前只实现 J1（judge_lead）；其他 kind 属后续卡。
func loadPrompt(kind, region string) (string, string, error) {
	switch kind {
	case KindJudgeLead:
		body, err := LoadJudgeLeadPrompt(region)
		if err != nil {
			return "", "", err
		}
		return body, JudgeLeadPromptVersion, nil
	default:
		return "", "", fmt.Errorf("marketing: kind %q prompt 未实现（后续卡）", kind)
	}
}

// sourceText 从上下文原料里取「原文」（MK-12 规则3 quote 子串匹配用）。
func sourceText(in ContextInput) string {
	switch in.Kind {
	case KindJudgeLead, KindCommentSlot, KindDraftReply:
		return in.PostText
	case KindAnswerOrEscalate:
		return in.Question
	case KindCritiqueReply, KindComplianceCheck:
		return in.DraftText
	case KindStorefrontCopy:
		return in.StoreInfo
	case KindAnalyzePerformance:
		return in.Performance
	}
	return ""
}
