package relay

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"llm-relay/app/llm-relay/internal/model"
	"llm-relay/app/llm-relay/internal/relay"
	"llm-relay/app/llm-relay/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

// 日志状态码，对齐 02-数据库设计.md lr_relay_log.status 注释
const (
	logStatusSuccess      = 1
	logStatusUpstreamFail = 2
	logStatusQuotaFail    = 4
)

const defaultUpstreamPath = "/v1/chat/completions"

// ChatCompletionsHandler 对外 OpenAI 兼容入口，P0 完整流水线：
// 鉴权(中间件) → 模型白名单 → 预扣费(Lua) → 注入include_usage → 转发/SSE透传 → 结算 → 异步日志
func ChatCompletionsHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		start := time.Now()

		entry := &model.RelayLog{
			TraceID:  newTraceID(),
			ClientIP: clientIP(r),
		}

		// ---- 解析请求（P0 透传协议，只取路由/计费需要的字段）----
		rawBody, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "read request body failed", "invalid_request_error", "bad_request")
			return
		}
		var reqMeta struct {
			Model     string `json:"model"`
			Stream    bool   `json:"stream"`
			MaxTokens *int   `json:"max_tokens"`
		}
		if err := json.Unmarshal(rawBody, &reqMeta); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid json body", "invalid_request_error", "bad_request")
			return
		}
		entry.ModelRequest = reqMeta.Model
		entry.Stream = b2i(reqMeta.Stream)

		info := TokenFromContext(ctx)
		if info == nil {
			writeJSONError(w, http.StatusUnauthorized, "authentication required", "invalid_request_error", "auth_required")
			return
		}
		entry.UserID, entry.TokenID = info.UserID, info.ID

		// ---- 模型白名单 ----
		if !info.CheckModelWhitelist(reqMeta.Model) {
			finishLog(svcCtx, entry, start, logStatusUpstreamFail, 403, "model not in token whitelist")
			writeJSONError(w, http.StatusForbidden, "model not allowed for this token", "invalid_request_error", "model_not_allowed")
			return
		}

		// ---- P0 单渠道：模型名映射（配置了 Upstream.Model 则替换）----
		realModel := reqMeta.Model
		if svcCtx.Config.Upstream.Model != "" {
			realModel = svcCtx.Config.Upstream.Model
		}
		entry.ModelReal = realModel

		// ---- 预扣费（Redis Lua 原子，见 01 文档 3.4②）----
		pricing, err := loadPricing(svcCtx, reqMeta.Model, realModel)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "load pricing failed", "internal_error", "pricing_failed")
			return
		}
		maxTokens := svcCtx.Config.Relay.DefaultMaxTokens
		if reqMeta.MaxTokens != nil && *reqMeta.MaxTokens > 0 {
			maxTokens = *reqMeta.MaxTokens
		}
		promptEst := relay.EstimateTokens(rawBody)
		freeze := relay.CalcFreeze(pricing, promptEst, maxTokens, 1.0)
		if err := svcCtx.Bill.Reserve(ctx, info.User, info, freeze); err != nil {
			if err == relay.ErrInsufficientQuota {
				finishLog(svcCtx, entry, start, logStatusQuotaFail, http.StatusPaymentRequired, "insufficient quota")
				writeJSONError(w, http.StatusPaymentRequired, "insufficient quota", "insufficient_quota", "quota_exceeded")
				return
			}
			writeJSONError(w, http.StatusInternalServerError, "billing reserve failed", "internal_error", "billing_failed")
			logx.Errorf("reserve failed: %v", err)
			return
		}

		// ---- 注入 include_usage + 模型名替换 ----
		forwardBody := rawBody
		if svcCtx.Config.Relay.EnforceIncludeUsage && reqMeta.Stream {
			if injected, _, err := relay.InjectStreamUsage(rawBody); err == nil {
				forwardBody = injected
			}
		}
		if realModel != reqMeta.Model {
			if replaced, ok := replaceModelName(forwardBody, realModel); ok {
				forwardBody = replaced
			}
		}

		// ---- 转发 ----
		result, ferr := relay.Forward(ctx, w, svcCtx.UpstreamHTTP,
			svcCtx.Config.Upstream.BaseURL, upstreamPath(svcCtx), svcCtx.Config.Upstream.APIKey,
			forwardBody)
		if ferr != nil {
			// 首字节未出失败：全额返还冻结（cost=0），日志记上游失败
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

		// ---- 结算（真实 usage × 定价，见 01 文档 3.4③）----
		cost := relay.CalcCost(pricing, result.PromptTokens, result.CompletionTokens, 1.0)
		svcCtx.Bill.Settle(ctx, info.User, info, freeze, cost)

		entry.PromptTokens = result.PromptTokens
		entry.CompletionTokens = result.CompletionTokens
		entry.UsageEstimated = b2i(result.UsageEstimated)
		entry.QuotaCost = cost
		entry.FirstByteMs = result.FirstByteMs
		finishLog(svcCtx, entry, start, logStatusSuccess, http.StatusOK, "")
	}
}

// finishLog 补齐计时/状态字段并异步投递
func finishLog(svcCtx *svc.ServiceContext, entry *model.RelayLog, start time.Time, status, httpStatus int, errMsg string) {
	entry.Status = int8(status)
	entry.HTTPStatus = httpStatus
	entry.DurationMs = int(time.Since(start).Milliseconds())
	entry.ErrorMsg = truncate(errMsg, 1000)
	svcCtx.Logs.Write(entry)
	logx.Infof("relay trace_id=%s status=%d model=%s cost=%d duration_ms=%d",
		entry.TraceID, status, entry.ModelRequest, entry.QuotaCost, entry.DurationMs)
}

// loadPricing 按请求模型名查定价，查不到再用上游真实名兜底
func loadPricing(svcCtx *svc.ServiceContext, requestModel, realModel string) (*model.ModelPricing, error) {
	if p, err := model.GetPricing(svcCtx.DB, requestModel); err != nil || p != nil {
		return p, err
	}
	if realModel == requestModel {
		return nil, nil
	}
	return model.GetPricing(svcCtx.DB, realModel)
}

// replaceModelName 请求体里的 model 换成上游真实名（map 方式保留其余字段）
func replaceModelName(body []byte, realModel string) ([]byte, bool) {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body, false
	}
	m["model"] = realModel
	out, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return out, true
}

func upstreamPath(svcCtx *svc.ServiceContext) string {
	if svcCtx.Config.Upstream.Path != "" {
		return svcCtx.Config.Upstream.Path
	}
	return defaultUpstreamPath
}

// HealthzHandler 存活探针
func HealthzHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}

func writeJSONError(w http.ResponseWriter, status int, msg, errType, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(relay.ErrorBody(msg, errType, code))
}

func newTraceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := len(host) - 1; i > 0 {
		for j := i; j >= 0; j-- {
			if host[j] == ':' {
				return host[:j]
			}
		}
	}
	return host
}

func b2i(b bool) int8 {
	if b {
		return 1
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
