package convert

import (
	"encoding/json"
	"fmt"
)

// OpenAIToAnthropicRequest 把 OpenAI Chat 请求转为 Anthropic Messages 请求。
// 关键差异（01-架构设计.md 3.2①）：
//   - system 消息独立提取到 system 字段
//   - max_tokens 必填（OpenAI 没传时调用方给 defaultMaxTokens）
//   - tools[].function → {name, description, input_schema}
//   - tool_choice 枚举映射
//   - role=tool 消息 → user 消息里的 tool_result 块
//   - assistant.tool_calls → tool_use 块
func OpenAIToAnthropicRequest(openai []byte, realModel string, defaultMaxTokens int) ([]byte, error) {
	var req ChatRequest
	if err := json.Unmarshal(openai, &req); err != nil {
		return nil, fmt.Errorf("parse openai request: %w", err)
	}

	if realModel == "" {
		realModel = req.Model
	}

	out := &AnthropicRequest{
		Model: realModel,
		Tools: make([]AnthropicTool, 0, len(req.Tools)),
	}

	// max_tokens：max_tokens / max_completion_tokens / 默认值，取第一个非零的
	maxTokens := defaultMaxTokens
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTokens = *req.MaxTokens
	} else if req.MaxCompletionTokens != nil && *req.MaxCompletionTokens > 0 {
		maxTokens = *req.MaxCompletionTokens
	}
	out.MaxTokens = maxTokens

	out.Temperature = req.Temperature
	out.TopP = req.TopP
	if len(req.Stop) > 0 {
		// stop 可能是 string 或 []string
		var one string
		if json.Unmarshal(req.Stop, &one) == nil {
			out.StopSequences = []string{one}
		} else {
			_ = json.Unmarshal(req.Stop, &out.StopSequences)
		}
	}

	// system 提取 + 消息转换
	for i := range req.Messages {
		m := &req.Messages[i]
		switch m.Role {
		case "system", "developer":
			if t := ExtractText(m.Content); t != "" {
				out.System = appendSystem(out.System, t)
			}
		case "user":
			out.Messages = append(out.Messages, AnthropicMessage{
				Role:    "user",
				Content: marshalTextContent(ExtractText(m.Content)),
			})
		case "assistant":
			blocks := make([]AnthropicBlock, 0, len(m.ToolCalls)+1)
			if t := ExtractText(m.Content); t != "" {
				blocks = append(blocks, AnthropicBlock{Type: "text", Text: t})
			}
			for _, tc := range m.ToolCalls {
				arg := json.RawMessage(tc.Function.Arguments)
				if len(arg) == 0 || string(arg) == "null" {
					arg = json.RawMessage(`{}`)
				}
				blocks = append(blocks, AnthropicBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: arg,
				})
			}
			if len(blocks) == 0 { // 空 assistant 消息 Anthropic 会拒绝，给个空文本兜底
				blocks = append(blocks, AnthropicBlock{Type: "text", Text: ""})
			}
			out.Messages = append(out.Messages, AnthropicMessage{
				Role:    "assistant",
				Content: mustJSON(blocks),
			})
		case "tool":
			out.Messages = append(out.Messages, AnthropicMessage{
				Role: "user",
				Content: mustJSON([]AnthropicBlock{{
					Type:      "tool_result",
					ToolUseID: m.ToolCallID,
					Content:   marshalTextContent(ExtractText(m.Content)),
				}}),
			})
		}
	}

	// tools
	for _, t := range req.Tools {
		schema := t.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out.Tools = append(out.Tools, AnthropicTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: schema,
		})
	}

	// tool_choice：auto/none/required/{type:function,...}
	if len(req.ToolChoice) > 0 {
		var tcName string
		if json.Unmarshal(req.ToolChoice, &tcName) == nil {
			switch tcName {
			case "auto":
				out.ToolChoice = &AnthropicToolChoice{Type: "auto"}
			case "required":
				out.ToolChoice = &AnthropicToolChoice{Type: "any"}
			case "none":
				out.Tools = nil // Anthropic 没有 none，直接去掉 tools
			}
		} else {
			var tcObj struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if json.Unmarshal(req.ToolChoice, &tcObj) == nil && tcObj.Function.Name != "" {
				out.ToolChoice = &AnthropicToolChoice{Type: "tool", Name: tcObj.Function.Name}
			}
		}
	}

	// stream 由转发层根据上游情况透传
	out.Stream = req.Stream
	return json.Marshal(out)
}

func appendSystem(existing json.RawMessage, text string) json.RawMessage {
	// 多条 system 用换行拼接为 string（Anthropic 两种格式都接受 string）
	var prev string
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &prev)
	}
	if prev != "" {
		prev += "\n"
	}
	return mustJSON(prev + text)
}

// marshalTextContent 文本为空时保持 content 为空串而非 nil（Anthropic 要求 content 存在）
func marshalTextContent(text string) json.RawMessage {
	return mustJSON(text)
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return b
}
