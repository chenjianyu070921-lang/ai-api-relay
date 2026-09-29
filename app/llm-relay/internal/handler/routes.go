package handler

import (
	"net/http"

	"github.com/zeromicro/go-zero/rest"

	"llm-relay/app/llm-relay/internal/handler/relay"
	"llm-relay/app/llm-relay/internal/svc"
)

// RegisterHandlers 对外 OpenAI 兼容接口不走 goctl 生成，手写注册，
// 直接持有 http.ResponseWriter 做 SSE 逐 chunk flush
func RegisterHandlers(server *rest.Server, svcCtx *svc.ServiceContext) {
	server.AddRoutes([]rest.Route{
		{
			Method:  http.MethodPost,
			Path:    "/v1/chat/completions",
			Handler: relay.TokenAuthMiddleware(svcCtx, relay.ChatCompletionsHandler(svcCtx)),
		},
		{
			Method:  http.MethodGet,
			Path:    "/healthz",
			Handler: relay.HealthzHandler(),
		},
	})
}
