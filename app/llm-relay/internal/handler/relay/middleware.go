package relay

import (
	"context"
	"net/http"

	"llm-relay/app/llm-relay/internal/relay"
	"llm-relay/app/llm-relay/internal/svc"
)

type ctxKey int

const tokenInfoKey ctxKey = 1

// TokenFromContext handler 里取鉴权结果
func TokenFromContext(ctx context.Context) *relay.TokenInfo {
	v, _ := ctx.Value(tokenInfoKey).(*relay.TokenInfo)
	return v
}

// TokenAuthMiddleware 令牌鉴权中间件，错误统一 OpenAI 格式（03-API设计.md 1.1）：
// 401 key 无效/过期/被禁用；402 余额不足
func TokenAuthMiddleware(svcCtx *svc.ServiceContext, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		info, err := relay.Authenticate(r.Context(), r, svcCtx.DB, svcCtx.RDB, svcCtx.Config.Relay.TokenCacheSec)
		if err != nil {
			status := http.StatusUnauthorized
			if err == relay.ErrQuotaZero {
				status = http.StatusPaymentRequired
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(relay.ErrorBody(err.Error(), "invalid_request_error", "auth_failed"))
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), tokenInfoKey, info)))
	}
}
