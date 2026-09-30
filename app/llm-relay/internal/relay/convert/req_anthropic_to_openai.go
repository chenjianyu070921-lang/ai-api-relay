package convert

import (
	"encoding/json"
	"fmt"
)

// AnthropicToOpenAIRequest 把 Anthropic Messages 请求转为 OpenAI Chat 请求
//（/v1/messages 客户端入口 → OpenAI 普通话）。
func AnthropicToOpenAIRequest(anthropic []byte, realModel string) ([]byte, *AnthropicRequest, error) {
	var req AnthropicRequest
	if err := json.Unmarshal(anthropic, &req); err != nil {
		return nil, nil, fmt.Errorf("parse anthropic request: %w", err)
	}

	out := &ChatRequest{
		Model:    realModel,
		Stream:   req.Stream,
		Messages: make([]ChatMessage, 0, len(req.Messages)+1),
	}

	if realModel == "" {
		out.Model = req.Model
	}
	if req.MaxTokens > 0 {
		mt := req.MaxTokens
		out.MaxTokens = &mt
	}
	out.Temperature = req.Temperature
	out.TopP = req.TopP
	if len(req.StopSequences) > 0 {
		out.Stop = mustJSON(req.StopSequences)
	}

	// system → role=system
	if t := anthropicSystemText(req.System); t != "" {
		out.Messages = append(out.Messages, ChatMessage{
			Role:    "system",
			Content: mustJSON(t),
		})
	}

	for _, m := range req.Messages {
		blocks := ExtractTextBlocks(m.Content)
		isStringContent := false
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			isStringContent = true
		}

		switch m.Role {
		case "user":
			// tool_result 块拆成 role=tool 消息；其余合并成一条 user 消息
			var texts []string
			for _, b := range blocks {
				switch b.Type {
				case "text":
					texts = append(texts, b.Text)
				case "tool_result":
					if len(texts) > 0 {
						out.Messages = append(out.Messages, userMsg(texts))
						texts = nil
					}
					out.Messages = append(out.Messages, ChatMessage{
						Role:       "tool",
						ToolCallID: b.ToolUseID,
						Content:    mustJSON(toolResultText(b)),
					})
				}
			}
			if len(texts) > 0 {
				out.Messages = append(out.Messages, userMsg(texts))
			}
		case "assistant":
			msg := ChatMessage{Role: "assistant"}
			var texts []string
			for _, b := range blocks {
				switch b.Type {
				case "text":
					texts = append(texts, b.Text)
				case "tool_use":
					msg.ToolCalls = append(msg.ToolCalls, ToolCall{
						ID:   b.ID,
						Type: "function",
						Function: func() (f struct {
							Name      string `json:"name,omitempty"`
							Arguments string `json:"arguments,omitempty"`
						}) {
							f.Name = b.Name
							arg := string(b.Input)
							if arg == "" {
								arg = "{}"
							}
							f.Arguments = arg
							return
						}(),
					})
				}
			}
			if len(texts) > 0 {
				msg.Content = mustJSON(joinLines(texts))
			}
			out.Messages = append(out.Messages, msg)
		}
		_ = isStringContent
	}

	// tools
	for _, t := range req.Tools {
		params := t.InputSchema
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object"}`)
		}
		out.Tools = append(out.Tools, ChatTool{
			Type: "function",
			Function: ToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}

	// tool_choice
	if req.ToolChoice != nil {
		switch req.ToolChoice.Type {
		case "auto":
			out.ToolChoice = mustJSON("auto")
		case "any":
			out.ToolChoice = mustJSON("required")
		case "tool":
			out.ToolChoice = mustJSON(map[string]any{
				"type":     "function",
				"function": map[string]string{"name": req.ToolChoice.Name},
			})
		}
	}

	body, err := json.Marshal(out)
	return body, &req, err
}

func anthropicSystemText(system json.RawMessage) string {
	if len(system) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(system, &s) == nil {
		return s
	}
	var texts []string
	for _, b := range ExtractTextBlocks(system) {
		if b.Type == "text" {
			texts = append(texts, b.Text)
		}
	}
	return joinLines(texts)
}

// toolResultText tool_result 的 content（string 或 blocks）取纯文本
func toolResultText(b AnthropicBlock) string {
	if len(b.Content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(b.Content, &s) == nil {
		return s
	}
	var texts []string
	for _, sub := range ExtractTextBlocks(b.Content) {
		if sub.Type == "text" {
			texts = append(texts, sub.Text)
		}
	}
	return joinLines(texts)
}

func userMsg(texts []string) ChatMessage {
	return ChatMessage{Role: "user", Content: mustJSON(joinLines(texts))}
}

func joinLines(texts []string) string {
	out := ""
	for _, t := range texts {
		if out != "" {
			out += "\n"
		}
		out += t
	}
	return out
}
