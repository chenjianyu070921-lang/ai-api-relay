package convert

import (
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"
)

// ---------- 非流式：Anthropic 响应 → OpenAI 响应 ----------

var anthropicStopToOpenAI = map[string]string{
	"end_turn":      "stop",
	"max_tokens":    "length",
	"stop_sequence": "stop",
	"tool_use":      "tool_calls",
}

// AnthropicToOpenAIResponse 非流式响应转换（OpenAI 客户端 + Anthropic 上游）
func AnthropicToOpenAIResponse(raw []byte) ([]byte, *Usage, error) {
	var resp AnthropicResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, nil, fmt.Errorf("parse anthropic response: %w", err)
	}

	msg := ChatMessage{Role: "assistant"}
	var texts []string
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "tool_use":
			args := string(b.Input)
			if args == "" || args == "null" {
				args = "{}"
			}
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID:   b.ID,
				Type: "function",
				Function: struct {
					Name      string `json:"name,omitempty"`
					Arguments string `json:"arguments,omitempty"`
				}{Name: b.Name, Arguments: args},
			})
		}
	}
	if len(texts) > 0 {
		msg.Content = mustJSON(joinLines(texts))
	}

	usage := &Usage{
		PromptTokens:     resp.Usage.InputTokens,
		CompletionTokens: resp.Usage.OutputTokens,
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens

	finish := anthropicStopToOpenAI[resp.StopReason]
	if finish == "" {
		finish = "stop"
	}
	out := ChatResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   resp.Model,
		Choices: []ChatChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: finish,
		}},
		Usage: usage,
	}
	b, err := json.Marshal(out)
	return b, usage, err
}

// ---------- 流式：Anthropic SSE → OpenAI chunks 状态机 ----------

// StreamConverter 流式转换状态机：逐行喂上游 SSE 行，吐出客户端侧 SSE 行。
// Feed 返回的行不含换行符（转发层逐行 +"\n" 写出并 flush）。
type StreamConverter interface {
	Feed(line []byte) []string
	// Finish 上游 EOF 后调用（上游异常断流时也要调），返回收尾行
	Finish() []string
	// Usage 上游提取到的 usage；ok=false 表示上游没给
	Usage() (prompt, completion int, ok bool)
	// ContentChars 累计的输出文本字符数，供估算兜底
	ContentChars() int
}

// AnthropicToOpenAIStream Anthropic SSE → OpenAI chunk 流。
// 事件序（01 文档 3.2③）：message_start → content_block_start →
// content_block_delta* → content_block_stop → message_delta → message_stop
type AnthropicToOpenAIStream struct {
	id      string
	model   string
	prompt  int
	completion int
	sawUsage   bool
	finished   bool

	// tool_use 块状态：anthropic block index → 已发过 start
	toolBlocks map[int]bool
}

func NewAnthropicToOpenAIStream() *AnthropicToOpenAIStream {
	return &AnthropicToOpenAIStream{toolBlocks: map[int]bool{}}
}

func (s *AnthropicToOpenAIStream) chunk(delta ChatMessage, finish *string) []ChatChunkChoice {
	return []ChatChunkChoice{{Index: 0, Delta: delta, FinishReason: finish}}
}

func (s *AnthropicToOpenAIStream) emit(c ChatChunk) []string {
	b, err := json.Marshal(c)
	if err != nil {
		return nil
	}
	return []string{"data: " + string(b), ""}
}

func (s *AnthropicToOpenAIStream) Feed(line []byte) []string {
	trimmed := trimSpace(line)
	if len(trimmed) == 0 {
		return nil // SSE 空行分隔符，转发层自己补
	}
	data, ok := cutPrefix(trimmed, "data:")
	if !ok {
		return nil // "event: xxx" 行：payload 自带 type，无需依赖
	}
	data = trimSpace(data)
	if string(data) == "[DONE]" {
		return nil
	}

	var ev struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &ev) != nil || ev.Type == "" {
		return nil
	}

	switch ev.Type {
	case "message_start":
		var m struct {
			Message struct {
				ID    string `json:"id"`
				Model string `json:"model"`
				Usage struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(data, &m) == nil {
			s.id = m.Message.ID
			s.model = m.Message.Model
			s.prompt = m.Message.Usage.InputTokens
			s.sawUsage = s.prompt > 0
		}
		c := ChatChunk{ID: s.id, Object: "chat.completion.chunk", Created: time.Now().Unix(), Model: s.model}
		c.Choices = s.chunk(ChatMessage{Role: "assistant", Content: mustJSON("")}, nil)
		return s.emit(c)

	case "content_block_start":
		var m struct {
			Index int           `json:"index"`
			Block AnthropicBlock `json:"content_block"`
		}
		if json.Unmarshal(data, &m) != nil {
			return nil
		}
		if m.Block.Type != "tool_use" {
			return nil
		}
		s.toolBlocks[m.Index] = true
		c := ChatChunk{ID: s.id, Object: "chat.completion.chunk", Created: time.Now().Unix(), Model: s.model}
		idx := m.Index
		c.Choices = s.chunk(ChatMessage{ToolCalls: []ToolCall{{
			Index: &idx,
			ID:    m.Block.ID,
			Type:  "function",
			Function: struct {
				Name      string `json:"name,omitempty"`
				Arguments string `json:"arguments,omitempty"`
			}{Name: m.Block.Name},
		}}}, nil)
		return s.emit(c)

	case "content_block_delta":
		var m struct {
			Index int    `json:"index"`
			Delta struct {
				Type        string `json:"type"` // text_delta | input_json_delta
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal(data, &m) != nil {
			return nil
		}
		c := ChatChunk{ID: s.id, Object: "chat.completion.chunk", Created: time.Now().Unix(), Model: s.model}
		switch m.Delta.Type {
		case "text_delta":
			s.completion += utf8.RuneCountInString(m.Delta.Text) // 估算兜底备用
			c.Choices = s.chunk(ChatMessage{Content: mustJSON(m.Delta.Text)}, nil)
		case "input_json_delta":
			idx := m.Index
			c.Choices = s.chunk(ChatMessage{ToolCalls: []ToolCall{{
				Index: &idx,
				Function: struct {
					Name      string `json:"name,omitempty"`
					Arguments string `json:"arguments,omitempty"`
				}{Arguments: m.Delta.PartialJSON},
			}}}, nil)
		default:
			return nil
		}
		return s.emit(c)

	case "message_delta":
		var m struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(data, &m) != nil {
			return nil
		}
		if m.Usage.OutputTokens > 0 {
			s.completion = m.Usage.OutputTokens
			s.sawUsage = true
		}
		finish := anthropicStopToOpenAI[m.Delta.StopReason]
		if finish == "" {
			finish = "stop"
		}
		c := ChatChunk{ID: s.id, Object: "chat.completion.chunk", Created: time.Now().Unix(), Model: s.model}
		c.Choices = s.chunk(ChatMessage{}, &finish)
		return s.emit(c)

	case "message_stop":
		s.finished = true
		// OpenAI 惯例：最后一个 usage-only chunk（choices 为空）
		c := ChatChunk{ID: s.id, Object: "chat.completion.chunk", Created: time.Now().Unix(), Model: s.model}
		if s.sawUsage {
			c.Usage = &Usage{PromptTokens: s.prompt, CompletionTokens: s.completion, TotalTokens: s.prompt + s.completion}
		}
		return append(s.emit(c), "data: [DONE]", "")
	}
	return nil
}

func (s *AnthropicToOpenAIStream) Finish() []string {
	if s.finished {
		return nil
	}
	// 上游断流没发 message_stop：补 [DONE]，按已有内容结算
	s.finished = true
	return []string{"data: [DONE]", ""}
}

func (s *AnthropicToOpenAIStream) Usage() (int, int, bool) {
	return s.prompt, s.completion, s.sawUsage
}

func (s *AnthropicToOpenAIStream) ContentChars() int { return s.completion }
