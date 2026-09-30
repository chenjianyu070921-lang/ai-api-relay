package convert

import "encoding/json"

// Anthropic Messages API 类型（/v1/messages）

type AnthropicRequest struct {
	Model         string             `json:"model"`
	System        json.RawMessage    `json:"system,omitempty"` // string 或 blocks 数组
	Messages      []AnthropicMessage `json:"messages"`
	MaxTokens     int                `json:"max_tokens"` // Anthropic 必填
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Tools         []AnthropicTool    `json:"tools,omitempty"`
	ToolChoice    *AnthropicToolChoice `json:"tool_choice,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
}

type AnthropicMessage struct {
	Role    string          `json:"role"` // user | assistant
	Content json.RawMessage `json:"content"` // string 或 blocks 数组
}

type AnthropicBlock struct {
	Type string `json:"type"` // text | tool_use | tool_result

	Text string `json:"text,omitempty"` // text

	ID    string          `json:"id,omitempty"`   // tool_use
	Name  string          `json:"name,omitempty"` // tool_use
	Input json.RawMessage `json:"input,omitempty"` // tool_use：对象

	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_result
	Content   json.RawMessage `json:"content,omitempty"`     // tool_result：string 或 blocks
	IsError   bool            `json:"is_error,omitempty"`
}

type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// AnthropicToolChoice: {type: auto|any|tool|none, name?}
type AnthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type AnthropicResponse struct {
	ID         string           `json:"id"`
	Type       string           `json:"type"` // message
	Role       string           `json:"role"` // assistant
	Model      string           `json:"model"`
	Content    []AnthropicBlock `json:"content"`
	StopReason string           `json:"stop_reason"` // end_turn|max_tokens|stop_sequence|tool_use
	Usage      AnthropicUsage   `json:"usage"`
}

// ExtractTextBlocks 从 Anthropic content（string 或 blocks）提取文本块列表
func ExtractTextBlocks(content json.RawMessage) []AnthropicBlock {
	if len(content) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(content, &s) == nil {
		if s == "" {
			return nil
		}
		return []AnthropicBlock{{Type: "text", Text: s}}
	}
	var blocks []AnthropicBlock
	if json.Unmarshal(content, &blocks) == nil {
		return blocks
	}
	return nil
}
