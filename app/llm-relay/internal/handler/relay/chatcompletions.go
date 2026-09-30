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

// ChatCompletionsHandler 对外 OpenAI 兼容入口（P1）：
// 鉴权(中间件) → 白名单 → runRelay 公共流水线
func ChatCompletionsHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		start := time.Now()

		entry := &model.RelayLog{
			TraceID:  newTraceID(),
			ClientIP: clientIP(r),
		}

		// ---- 解析请求（只取路由/计费需要的字段）----
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

		maxTokens := svcCtx.Config.Relay.DefaultMaxTokens
		if reqMeta.MaxTokens != nil && *reqMeta.MaxTokens > 0 {
			maxTokens = *reqMeta.MaxTokens
		}

		runRelay(w, r, svcCtx, entry, info, "openai", RequestMeta{
			Model:     reqMeta.Model,
			Stream:    reqMeta.Stream,
			MaxTokens: maxTokens,
		}, rawBody)
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
	if svcCtx.Config.Upstream.Type == "anthropic" {
		return "/v1/messages"
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
