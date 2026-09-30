package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---------- 请求转换 ----------

func TestOpenAIToAnthropicRequest(t *testing.T) {
	mt := 512
	body, _ := json.Marshal(map[string]any{
		"model":    "gpt-x",
		"messages": []map[string]any{{"role": "system", "content": "be brief"}, {"role": "user", "content": "hi"}},
		"stream":   true,
	})
	out, err := OpenAIToAnthropicRequest(body, "claude-x", mt)
	if err != nil {
		t.Fatal(err)
	}
	var req AnthropicRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	if req.Model != "claude-x" || req.MaxTokens != 512 {
		t.Fatalf("model/max_tokens = %s/%d", req.Model, req.MaxTokens)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Fatalf("messages: %+v", req.Messages)
	}
	if !req.Stream {
		t.Fatal("stream lost")
	}
}

func TestOpenAIToAnthropicRequestToolsAndToolResult(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"model": "gpt-x",
		"messages": []map[string]any{
			{"role": "user", "content": "weather?"},
			{"role": "assistant", "content": "", "tool_calls": []map[string]any{
				{"id": "call_1", "type": "function", "function": map[string]any{"name": "get_weather", "arguments": `{"city":"SF"}`}},
			}},
			{"role": "tool", "tool_call_id": "call_1", "content": "sunny 20C"},
		},
		"tools": []map[string]any{
			{"type": "function", "function": map[string]any{"name": "get_weather", "parameters": map[string]any{"type": "object"}}},
		},
		"tool_choice": "auto",
	})
	out, err := OpenAIToAnthropicRequest(body, "", 1024)
	if err != nil {
		t.Fatal(err)
	}
	var req AnthropicRequest
	if json.Unmarshal(out, &req) != nil {
		t.Fatal("unmarshal")
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "get_weather" || req.Tools[0].InputSchema == nil {
		t.Fatalf("tools: %+v", req.Tools)
	}
	if req.ToolChoice == nil || req.ToolChoice.Type != "auto" {
		t.Fatalf("tool_choice: %+v", req.ToolChoice)
	}
	// 期待 3 条：user / assistant(tool_use) / user(tool_result)
	if len(req.Messages) != 3 {
		t.Fatalf("messages: %+v", req.Messages)
	}
	var blocks []AnthropicBlock
	if json.Unmarshal(req.Messages[1].Content, &blocks) != nil || len(blocks) != 1 || blocks[0].Type != "tool_use" {
		t.Fatalf("assistant blocks: %+v", req.Messages[1].Content)
	}
	if blocks[0].Name != "get_weather" || string(blocks[0].Input) != `{"city":"SF"}` {
		t.Fatalf("tool_use: %+v", blocks[0])
	}
	if json.Unmarshal(req.Messages[2].Content, &blocks) != nil || blocks[0].Type != "tool_result" || blocks[0].ToolUseID != "call_1" {
		t.Fatalf("tool_result: %+v", req.Messages[2].Content)
	}
}

func TestAnthropicToOpenAIRequest(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"model":      "claude-x",
		"max_tokens": 777,
		"system":     "be brief",
		"messages": []map[string]any{
			{"role": "user", "content": "hello"},
		},
	})
	out, req, err := AnthropicToOpenAIRequest(body, "")
	if err != nil {
		t.Fatal(err)
	}
	var cr ChatRequest
	if json.Unmarshal(out, &cr) != nil {
		t.Fatal("unmarshal")
	}
	if cr.Model != "claude-x" || cr.MaxTokens == nil || *cr.MaxTokens != 777 {
		t.Fatalf("chat req: %+v", cr)
	}
	if len(cr.Messages) != 2 || cr.Messages[0].Role != "system" || cr.Messages[1].Role != "user" {
		t.Fatalf("messages: %+v", cr.Messages)
	}
	_ = req
}

// ---------- 流式状态机：Anthropic → OpenAI ----------

func TestAnthropicToOpenAIStream(t *testing.T) {
	s := NewAnthropicToOpenAIStream()
	feed := func(lines ...string) string {
		var sb strings.Builder
		for _, l := range lines {
			for _, out := range s.Feed([]byte(l)) {
				sb.WriteString(out)
				sb.WriteString("\n")
			}
		}
		return sb.String()
	}

	out := feed(
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-x","usage":{"input_tokens":12}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	)
	for _, want := range []string{
		`"role":"assistant"`, `"content":"你好"`, `"finish_reason":"stop"`,
		`"prompt_tokens":12`, `"completion_tokens":7`, `data: [DONE]`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	p, c, ok := s.Usage()
	if !ok || p != 12 || c != 7 {
		t.Fatalf("usage = %d/%d/%v", p, c, ok)
	}
}

func TestAnthropicToOpenAIStreamToolUse(t *testing.T) {
	s := NewAnthropicToOpenAIStream()
	var out strings.Builder
	lines := []string{
		`data: {"type":"message_start","message":{"id":"m","model":"claude-x","usage":{"input_tokens":5}}}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":"f"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"1}"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`,
		`data: {"type":"message_stop"}`,
	}
	for _, l := range lines {
		for _, o := range s.Feed([]byte(l)) {
			out.WriteString(o)
			out.WriteString("\n")
		}
	}
	got := out.String()
	for _, want := range []string{
		`"tool_calls"`, `"name":"f"`, `"arguments":"{\"a\":"`, `"arguments":"1}"`,
		`"finish_reason":"tool_calls"`, `data: [DONE]`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

// ---------- 流式状态机：OpenAI → Anthropic ----------

func TestOpenAIToAnthropicStream(t *testing.T) {
	s := NewOpenAIToAnthropicStream("")
	var out strings.Builder
	lines := []string{
		`data: {"id":"c1","model":"deepseek","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`data: {"id":"c1","model":"deepseek","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`data: {"id":"c1","model":"deepseek","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: {"id":"c1","model":"deepseek","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		`data: [DONE]`,
	}
	for _, l := range lines {
		for _, o := range s.Feed([]byte(l)) {
			out.WriteString(o)
			out.WriteString("\n")
		}
	}
	got := out.String()
	for _, want := range []string{
		`event: message_start`, `event: content_block_start`,
		`event: content_block_delta`, `"text_delta"`, `"text":"hi"`,
		`event: content_block_stop`, `event: message_delta`, `"stop_reason":"end_turn"`,
		`"output_tokens":2`, `event: message_stop`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

// 占位：Usage 返回三元组，避免上面误写；实际断言如下
func TestOpenAIToAnthropicStreamUsage(t *testing.T) {
	s := NewOpenAIToAnthropicStream("")
	for _, l := range []string{
		`data: {"model":"m","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`data: {"model":"m","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":4}}`,
		`data: [DONE]`,
	} {
		s.Feed([]byte(l))
	}
	p, c, ok := s.Usage()
	if !ok || p != 11 || c != 4 {
		t.Fatalf("usage = %d/%d/%v", p, c, ok)
	}
}
