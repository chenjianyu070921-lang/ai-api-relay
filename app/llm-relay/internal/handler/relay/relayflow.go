package relay

import (
	"net/http"
	"time"

	"llm-relay/app/llm-relay/internal/model"
	"llm-relay/app/llm-relay/internal/relay"
	"llm-relay/app/llm-relay/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

// RequestMeta 两个入口统一后的请求元信息
type RequestMeta struct {
	Model     string // 客户端请求的对外模型名
	Stream    bool
	MaxTokens int // 已解析：客户端传值或 DefaultMaxTokens
}

// runRelay 公共转发流水线（P1 起，/v1/chat/completions 与 /v1/messages 共用）：
// 模型映射 → 定价 → 预扣费 → 转换管道 → 转发 → 结算 → 异步日志
func runRelay(w http.ResponseWriter, r *http.Request, svcCtx *svc.ServiceContext,
	entry *model.RelayLog, info *relay.TokenInfo,
	clientFormat string, meta RequestMeta, openaiBody []byte) {

	ctx := r.Context()
	start := time.Now()

	// ---- 模型映射：DB 映射表优先，其次配置覆盖，最后原样透传 ----
	realModel := meta.Model
	mapping, err := model.GetModelMapping(svcCtx.DB, meta.Model)
	if err != nil {
		logx.Errorf("query model mapping failed: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "model mapping query failed", "internal_error", "mapping_failed")
		return
	}
	if mapping != nil {
		realModel = mapping.RealName
	} else if svcCtx.Config.Upstream.Model != "" {
		realModel = svcCtx.Config.Upstream.Model
	}
	entry.ModelReal = realModel

	// ---- 定价：请求名优先，真实名兜底 ----
	pricing, err := loadPricing(svcCtx, meta.Model, realModel)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "load pricing failed", "internal_error", "pricing_failed")
		return
	}

	// ---- 预扣费（Redis Lua 原子，见 01 文档 3.4②）----
	promptEst := relay.EstimateTokens(openaiBody)
	freeze := relay.CalcFreeze(pricing, promptEst, meta.MaxTokens, 1.0)
	if err := svcCtx.Bill.Reserve(ctx, info.User, info, freeze); err != nil {
		if err == relay.ErrInsufficientQuota {
			finishLog(svcCtx, entry, start, logStatusQuotaFail, http.StatusPaymentRequired, "insufficient quota")
			writeJSONError(w, http.StatusPaymentRequired, "insufficient quota", "insufficient_quota", "quota_exceeded")
			return
		}
		logx.Errorf("reserve failed: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "billing reserve failed", "internal_error", "billing_failed")
		return
	}

	// ---- 组装转换管道（协议转换 + usage 提取策略由 pipe 决定）----
	pipe := relay.NewPipe(clientFormat, svcCtx.Config.Upstream.Type, meta.Model, meta.MaxTokens)

	// ---- 请求体处理：模型名替换 → include_usage 注入 → 协议转换 ----
	forwardBody := openaiBody
	if realModel != meta.Model {
		if replaced, ok := replaceModelName(forwardBody, realModel); ok {
			forwardBody = replaced
		}
	}
	if pipe.UpstreamOpenAI && svcCtx.Config.Relay.EnforceIncludeUsage && meta.Stream {
		if injected, _, e := relay.InjectStreamUsage(forwardBody); e == nil {
			forwardBody = injected
		}
	}
	forwardBody, err = pipe.ConvertRequest(forwardBody)
	if err != nil {
		svcCtx.Bill.Settle(ctx, info.User, info, freeze, 0)
		logx.Errorf("convert request conversion failed: %v", err)
		writeJSONError(w, http.StatusBadRequest, "convert request failed", "invalid_request_error", "bad_request")
		return
	}

	// ---- 转发 ----
	result, ferr := relay.Forward(ctx, w, svcCtx.UpstreamHTTP, pipe,
		svcCtx.Config.Upstream.BaseURL, upstreamPath(svcCtx), svcCtx.Config.Upstream.APIKey,
		forwardBody)
	if ferr != nil {
		svcCtx.Bill.Settle(ctx, info.User, info, freeze, 0)
		httpStatus := 502
		if result != nil && result.UpstreamStatus != 0 {
			httpStatus = result.UpstreamStatus
		}
		finishLog(svcCtx, entry, start, logStatusUpstreamFail, httpStatus, ferr.Error())
		if ctx.Err() == nil {
			writeJSONError(w, http.StatusBadGateway, "upstream request failed", "upstream_error", "bad_gateway")
		}
		return
	}

	// ---- 结算 + 日志 ----
	cost := relay.CalcCost(pricing, result.PromptTokens, result.CompletionTokens, 1.0)
	svcCtx.Bill.Settle(ctx, info.User, info, freeze, cost)
	entry.PromptTokens = result.PromptTokens
	entry.CompletionTokens = result.CompletionTokens
	entry.UsageEstimated = b2i(result.UsageEstimated)
	entry.QuotaCost = cost
	entry.FirstByteMs = result.FirstByteMs
	finishLog(svcCtx, entry, start, logStatusSuccess, http.StatusOK, "")
}
