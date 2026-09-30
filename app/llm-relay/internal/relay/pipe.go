package relay

import (
	"llm-relay/app/llm-relay/internal/relay/convert"
)

// RelayPipe 一次转发请求的协议转换管道。
// 设计（01-架构设计.md 3.2「OpenAI 为普通话」）：
//
//	客户端格式 ──转普通话──▶ OpenAI body ──ConvertRequest──▶ 上游格式
//	上游响应 ──FromUpstream/NonStream──▶ OpenAI ──ToClient──▶ 客户端格式
//
// 同格式链路（openai↔openai、anthropic↔anthropic）走透传快路径，零转换。
type RelayPipe struct {
	// ConvertRequest openai 普通话请求体 → 上游请求体（模型名替换已完成）
	ConvertRequest func(openaiBody []byte) ([]byte, error)
	// NonStream 非流式：上游响应体 → (客户端响应体, usage, usage是否可信, err)
	NonStream func(raw []byte) (out []byte, usage *convert.Usage, usageOK bool, err error)
	// FromUpstream 流式第一段：上游 SSE 行 → OpenAI 行
	FromUpstream convert.StreamConverter
	// ToClient 流式第二段：OpenAI 行 → 客户端 SSE 行；nil = 客户端就是 OpenAI 格式
	ToClient convert.StreamConverter
	// UpstreamOpenAI 上游是否 OpenAI 系（决定是否注入 stream_options.include_usage）
	UpstreamOpenAI bool
}

// 支持 "openai" / "anthropic"，空视为 openai
func normalizeFormat(f string) string {
	if f == "anthropic" {
		return "anthropic"
	}
	return "openai"
}

// NewPipe 按 (客户端格式, 上游类型) 组装管道。publicModel 用于
// OpenAI→Anthropic 响应回填客户端可见模型名。
func NewPipe(clientFormat, upstreamType, publicModel string, defaultMaxTokens int) *RelayPipe {
	client := normalizeFormat(clientFormat)
	upstream := normalizeFormat(upstreamType)

	p := &RelayPipe{UpstreamOpenAI: upstream == "openai"}

	switch {
	case client == "openai" && upstream == "openai":
		// P0 快路径：全透传
		p.ConvertRequest = func(b []byte) ([]byte, error) { return b, nil }
		p.NonStream = identityNonStream
		p.FromUpstream = convert.NewPassthroughStream()

	case client == "openai" && upstream == "anthropic":
		p.ConvertRequest = func(b []byte) ([]byte, error) {
			return convert.OpenAIToAnthropicRequest(b, "", defaultMaxTokens)
		}
		p.NonStream = func(raw []byte) ([]byte, *convert.Usage, bool, error) {
			out, usage, err := convert.AnthropicToOpenAIResponse(raw)
			return out, usage, usage != nil, err
		}
		p.FromUpstream = convert.NewAnthropicToOpenAIStream()

	case client == "anthropic" && upstream == "openai":
		p.ConvertRequest = func(b []byte) ([]byte, error) { return b, nil }
		p.NonStream = func(raw []byte) ([]byte, *convert.Usage, bool, error) {
			out, usage, err := convert.OpenAIToAnthropicResponse(raw, publicModel)
			return out, usage, usage != nil, err
		}
		p.FromUpstream = convert.NewPassthroughStream()
		p.ToClient = convert.NewOpenAIToAnthropicStream(publicModel)

	default: // anthropic ↔ anthropic：原生透传
		p.ConvertRequest = func(b []byte) ([]byte, error) { return b, nil }
		p.NonStream = identityNonStream
		p.FromUpstream = convert.NewPassthroughStream()
	}
	return p
}

func identityNonStream(raw []byte) ([]byte, *convert.Usage, bool, error) {
	prompt, completion, ok := convert.ExtractUsage(raw)
	if !ok {
		return raw, nil, false, nil
	}
	return raw, &convert.Usage{PromptTokens: prompt, CompletionTokens: completion,
		TotalTokens: prompt + completion}, true, nil
}
