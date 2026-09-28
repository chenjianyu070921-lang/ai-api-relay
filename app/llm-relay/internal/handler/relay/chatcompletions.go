package relay

import (
	"net/http"

	"llm-relay/app/llm-relay/internal/svc"
)

// ChatCompletionsHandler 对外 OpenAI 兼容入口。
// P0 第一步只搭骨架先返回 501；第二步在这里实现：
// 鉴权 → 转发上游 → SSE 逐 chunk flush → 客户端断连关闭上游。
func ChatCompletionsHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"error":{"message":"relay not implemented yet","type":"not_implemented"}}`))
	}
}

// HealthzHandler 存活探针
func HealthzHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}
}
