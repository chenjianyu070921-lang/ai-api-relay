package convert

import (
	"encoding/json"
	"fmt"
	"time"
)

var openAIFinishToAnthropic = map[string]string{
	"stop":       "end_turn",
	"length":     "max_tokens",
	"tool_calls": "tool_use",
}

// OpenAIToAnthropicResponse 非流式响应转换（Anthropic 客户端 + OpenAI 上游）
func OpenAIToAnthropicResponse(raw []byte, publicModel string) ([]byte, *Usage, error) {
	var resp ChatResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, nil, fmt.Errorf("parse openai response: %w", err)
	}

	var content []AnthropicBlock
	if len(resp.Choices) > 0 {
		msg := resp.Choices[0].Message
		if t := ExtractText(msg.Content); t != "" {
			content = append(content, AnthropicBlock{Type: "text", Text: t})
		}
		for _, tc := range msg.ToolCalls {
			input := json.RawMessage(tc.Function.Arguments)
			if len(input) == 0 || string(input) == "null" {
				input = json.RawMessage(`{}`)
			}
			content = append(content, AnthropicBlock{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: input,
			})
		}
	}
	if len(content) == 0 {
		content = []AnthropicBlock{{Type: "text", Text: ""}}
	}

	stop := "end_turn"
	if len(resp.Choices) > 0 {
		if v, ok := openAIFinishToAnthropic[resp.Choices[0].FinishReason]; ok {
			stop = v
		}
	}

	model := publicModel
	if model == "" {
		model = resp.Model
	}

	var usage Usage
	if resp.Usage != nil {
		usage = *resp.Usage
	}

	out := AnthropicResponse{
		ID:         resp.ID,
		Type:       "message",
		Role:       "assistant",
		Model:      model,
		Content:    content,
		StopReason: stop,
		Usage: AnthropicUsage{
			InputTokens:  usage.PromptTokens,
			OutputTokens: usage.CompletionTokens,
		},
	}
	b, err := json.Marshal(out)
	return b, &usage, err
}

// ---------- 流式：OpenAI chunks → Anthropic SSE 事件 ----------

// OpenAIToAnthropicStream OpenAI chunk 流 → Anthropic 事件流。
// 首个含 role 的 chunk 触发 message_start + text 块 start；
// 文本 delta → text_delta；tool_calls → tool_use 块 start + input_json_delta；
// 收到 [DONE]（或 Finish）时补 content_block_stop + message_delta + message_stop。
type OpenAIToAnthropicStream struct {
	model       string
	msgStarted  bool
	textOpen    bool
	toolOpenIdx int          // 当前打开的 anthropic block index（-1 = 无）
	textBlkIdx  int          // text 块占用 index 0
	toolBlkIdx  map[int]int  // openai tool_calls index → anthropic block index
	nextBlkIdx  int
	finish      string
	prompt      int
	completion  int
	sawUsage    bool
	stopped     bool
}

func NewOpenAIToAnthropicStream(publicModel string) *OpenAIToAnthropicStream {
	return &OpenAIToAnthropicStream{
		model:      publicModel, // 客户端无感知：回显请求的对外模型名，而非上游真实名
		toolOpenIdx: -1,
		textBlkIdx:  0,
		toolBlkIdx:  map[int]int{},
		nextBlkIdx:  1, // index 0 留给 text 块
	}
}

// event 输出标准 Anthropic SSE（event 行 + data 行 + 空行）
func (s *OpenAIToAnthropicStream) event(name string, payload any) []string {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return []string{"event: " + name, "data: " + string(b), ""}
}

func (s *OpenAIToAnthropicStream) blockStart(idx int, block AnthropicBlock) []string {
	return s.event("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": idx,
		"content_block": block,
	})
}

func (s *OpenAIToAnthropicStream) blockStop(idx int) []string {
	return s.event("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": idx,
	})
}

func (s *OpenAIToAnthropicStream) textDelta(idx int, text string) []string {
	return s.event("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": idx,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

func (s *OpenAIToAnthropicStream) messageStart() []string {
	if s.msgStarted {
		return nil
	}
	s.msgStarted = true
	s.textOpen = true
	out := s.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":      "msg_relay_" + time.Now().Format("20060102150405.000000000"),
			"type":    "message",
			"role":    "assistant",
			"model":   s.model,
			"content": []any{},
			"usage":   map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
	return append(out, s.blockStart(s.textBlkIdx, AnthropicBlock{Type: "text"})...)
}

// closeOpenBlock 关闭当前打开的块（text 或 tool_use）
func (s *OpenAIToAnthropicStream) closeOpenBlock() []string {
	if s.toolOpenIdx >= 0 {
		out := s.blockStop(s.toolOpenIdx)
		s.toolOpenIdx = -1
		return out
	}
	if s.textOpen {
		out := s.blockStop(s.textBlkIdx)
		s.textOpen = false
		return out
	}
	return nil
}

func (s *OpenAIToAnthropicStream) Feed(line []byte) []string {
	trimmed := trimSpace(line)
	if len(trimmed) == 0 {
		return nil
	}
	data, ok := cutPrefix(trimmed, "data:")
	if !ok {
		return nil
	}
	data = trimSpace(data)
	if string(data) == "[DONE]" {
		return s.stop()
	}

	var chunk ChatChunk
	if json.Unmarshal(data, &chunk) != nil {
		return nil
	}
	if s.model == "" {
		s.model = chunk.Model
	} // model 为空才取上游名（正常路径已由 publicModel 预置）

	var out []string
	if chunk.Usage != nil {
		s.prompt = chunk.Usage.PromptTokens
		s.completion = chunk.Usage.CompletionTokens
		s.sawUsage = true
	}

	for _, ch := range chunk.Choices {
		if ch.FinishReason != nil {
			s.finish = *ch.FinishReason
		}
		d := ch.Delta
		if d.Role != "" {
			out = append(out, s.messageStart()...)
		}
		if t := ExtractText(d.Content); t != "" {
			out = append(out, s.textDelta(s.textBlkIdx, t)...)
		}
		for i := range d.ToolCalls {
			tc := &d.ToolCalls[i]
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			blkIdx, seen := s.toolBlkIdx[idx]
			if !seen {
				out = append(out, s.closeOpenBlock()...)
				blkIdx = s.nextBlkIdx
				s.nextBlkIdx++
				s.toolBlkIdx[idx] = blkIdx
				out = append(out, s.blockStart(blkIdx, AnthropicBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: json.RawMessage(`{}`),
				})...)
				s.toolOpenIdx = blkIdx
			}
			if tc.Function.Arguments != "" {
				out = append(out, s.event("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": blkIdx,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
				})...)
			}
		}
	}
	return out
}

// stop 收尾：关块 + message_delta(stop_reason+usage) + message_stop
func (s *OpenAIToAnthropicStream) stop() []string {
	if s.stopped {
		return nil
	}
	s.stopped = true
	if !s.msgStarted {
		// 上游没吐任何内容就结束了：仍要给客户端完整事件序
		out := s.messageStart()
		out = append(out, s.closeOpenBlock()...)
		out = append(out, s.messageDeltaStop()...)
		out = append(out, s.event("message_stop", map[string]any{"type": "message_stop"})...)
		return out
	}
	out := s.closeOpenBlock()
	out = append(out, s.messageDeltaStop()...)
	out = append(out, s.event("message_stop", map[string]any{"type": "message_stop"})...)
	return out
}

func (s *OpenAIToAnthropicStream) messageDeltaStop() []string {
	stop := openAIFinishToAnthropic[s.finish]
	if stop == "" {
		stop = "end_turn"
	}
	return s.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop},
		"usage": map[string]any{"output_tokens": s.completion},
	})
}

func (s *OpenAIToAnthropicStream) Finish() []string { return s.stop() }

func (s *OpenAIToAnthropicStream) Usage() (int, int, bool) {
	return s.prompt, s.completion, s.sawUsage
}

// ContentChars 由转发层在客户端侧累计（这里返回 0 交给外层）
func (s *OpenAIToAnthropicStream) ContentChars() int { return 0 }
