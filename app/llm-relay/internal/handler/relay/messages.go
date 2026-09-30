package relay

import (
	"io"
	"net/http"
	"time"

	"llm-relay/app/llm-relay/internal/model"
	"llm-relay/app/llm-relay/internal/relay/convert"
	"llm-relay/app/llm-relay/internal/svc"
)

// MessagesHandler Anthropic 客户端入口 /v1/messages（P1 新增）：
// Anthropic 请求 → 转 OpenAI 普通话 → runRelay → 响应转回 Anthropic 格式。
// 上游同为 anthropic 系时全链路原生透传（零转换）。
func MessagesHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		start := time.Now()

		entry := &model.RelayLog{
			TraceID:  newTraceID(),
			ClientIP: clientIP(r),
		}

		rawBody, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "read request body failed", "invalid_request_error", "bad_request")
			return
		}

		// 转 OpenAI 普通话（模型名替换交给公共流水线，这里 realModel 传空）
		openaiBody, anthropicReq, cerr := convert.AnthropicToOpenAIRequest(rawBody, "")
		if cerr != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid anthropic request", "invalid_request_error", "bad_request")
			return
		}

		info := TokenFromContext(ctx)
		if info == nil {
			writeJSONError(w, http.StatusUnauthorized, "authentication required", "invalid_request_error", "auth_required")
			return
		}
		entry.UserID, entry.TokenID = info.UserID, info.ID

		publicModel := anthropicReq.Model
		entry.ModelRequest = publicModel
		entry.Stream = b2i(anthropicReq.Stream)

		if !info.CheckModelWhitelist(publicModel) {
			finishLog(svcCtx, entry, start, logStatusUpstreamFail, 403, "model not in token whitelist")
			writeJSONError(w, http.StatusForbidden, "model not allowed for this token", "invalid_request_error", "model_not_allowed")
			return
		}

		maxTokens := svcCtx.Config.Relay.DefaultMaxTokens
		if anthropicReq.MaxTokens > 0 {
			maxTokens = anthropicReq.MaxTokens
		}

		runRelay(w, r, svcCtx, entry, info, "anthropic", RequestMeta{
			Model:     publicModel,
			Stream:    anthropicReq.Stream,
			MaxTokens: maxTokens,
		}, openaiBody)
	}
}
