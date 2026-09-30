package convert

import (
	"encoding/json"
	"unicode/utf8"
)

// PassthroughStream OpenAI→OpenAI 透传流：原样转发每行，
// 同时统计 usage 与 delta.content 字符数（估算兜底用）。
// 也用作「openai 上游 → anthropic 客户端」链路的第一段。
type PassthroughStream struct {
	prompt     int
	completion int
	sawUsage   bool
	chars      int
}

func NewPassthroughStream() *PassthroughStream { return &PassthroughStream{} }

func (p *PassthroughStream) Feed(line []byte) []string {
	trimmed := trimSpace(line)
	if data, ok := cutPrefix(trimmed, "data:"); ok {
		data = trimSpace(data)
		if string(data) != "[DONE]" {
			var chunk ChatChunk
			if json.Unmarshal(data, &chunk) == nil {
				if chunk.Usage != nil {
					p.prompt = chunk.Usage.PromptTokens
					p.completion = chunk.Usage.CompletionTokens
					p.sawUsage = true
				}
				if len(chunk.Choices) > 0 {
					// 不能按整行字节估（JSON 结构开销高估数十倍），只累计文本
					p.chars += utf8.RuneCountInString(ExtractText(chunk.Choices[0].Delta.Content))
				}
			}
		}
	}
	return []string{string(line)}
}

func (p *PassthroughStream) Finish() []string { return nil }

func (p *PassthroughStream) Usage() (int, int, bool) {
	return p.prompt, p.completion, p.sawUsage
}

func (p *PassthroughStream) ContentChars() int { return p.chars }

// ExtractUsage 非流式响应体里抽 usage
func ExtractUsage(raw []byte) (prompt, completion int, ok bool) {
	var carrier struct {
		Usage *Usage `json:"usage"`
	}
	if json.Unmarshal(raw, &carrier) == nil && carrier.Usage != nil {
		return carrier.Usage.PromptTokens, carrier.Usage.CompletionTokens, true
	}
	return 0, 0, false
}
