package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// ForwardResult 转发结果，供结算与日志使用
type ForwardResult struct {
	PromptTokens     int
	CompletionTokens int
	UsageEstimated   bool // 上游没给 usage，按字符数/4 估算
	FirstByteMs      int
	UpstreamStatus   int
}

// 上游 OpenAI 兼容响应里只关心 usage 字段，其余透传不做结构化解析
type usagePayload struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type usageCarrier struct {
	Usage *usagePayload `json:"usage"`
}

// streamChunk 流式 chunk 里只关心 usage 和 delta.content（估算兜底用）
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *usagePayload `json:"usage"`
}

// OpenAIError 对外错误统一 OpenAI 风格（03-API设计.md 1.1）
type OpenAIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

func ErrorBody(message, errType, code string) []byte {
	b, _ := json.Marshal(map[string]any{
		"error": OpenAIError{Message: message, Type: errType, Code: code},
	})
	return b
}

// InjectStreamUsage 请求注入 stream_options.include_usage（01 文档 3.4⑤）。
// 用 map 解析保留全部未知字段，不丢 SDK 新增属性。
func InjectStreamUsage(body []byte) ([]byte, bool, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, false, err
	}
	stream, _ := m["stream"].(bool)
	if !stream {
		return body, false, nil
	}
	opts, ok := m["stream_options"].(map[string]any)
	if !ok {
		opts = map[string]any{}
		m["stream_options"] = opts
	}
	opts["include_usage"] = true
	out, err := json.Marshal(m)
	return out, true, err
}

// Forward 单渠道转发（P0）：OpenAI 兼容 → OpenAI 兼容，body 原样透传。
// 流式：逐 chunk flush + 客户端断连关闭上游（01 文档 3.1）。
// 调用方保证 body 已注入 include_usage（如启用）。
func Forward(
	ctx context.Context,
	w http.ResponseWriter,
	client *http.Client,
	baseURL, path, upstreamKey string,
	body []byte,
) (*ForwardResult, error) {
	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+upstreamKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := client.Do(req)
	if err != nil {
		// 客户端主动断开不算上游失败
		if ctx.Err() != nil {
			return &ForwardResult{FirstByteMs: msSince(start)}, ctx.Err()
		}
		return nil, fmt.Errorf("upstream request failed: %w", err)
	}
	defer resp.Body.Close()

	result := &ForwardResult{UpstreamStatus: resp.StatusCode}
	if resp.StatusCode != http.StatusOK {
		// 上游错误：读一小段错误体返回给调用方记日志（不透传给客户端，统一 502）
		errSnippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return result, fmt.Errorf("upstream status %d: %s", resp.StatusCode, string(errSnippet))
	}
	result.FirstByteMs = msSince(start)

	isStream := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
	if !isStream {
		return relayNonStream(w, resp.Body, result)
	}
	return relayStream(w, resp.Body, result, start)
}

// relayNonStream 非流式：整包读出 → 提取 usage → 原样回写
func relayNonStream(w http.ResponseWriter, body io.Reader, result *ForwardResult) (*ForwardResult, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return result, fmt.Errorf("read upstream response: %w", err)
	}
	var carrier usageCarrier
	if json.Unmarshal(raw, &carrier) == nil && carrier.Usage != nil {
		result.PromptTokens = carrier.Usage.PromptTokens
		result.CompletionTokens = carrier.Usage.CompletionTokens
	} else {
		result.CompletionTokens = EstimateTokens(raw)
		result.UsageEstimated = true
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
	return result, nil
}

// relayStream 流式透传核心：逐行转发、每行 flush、断连即关闭上游
func relayStream(w http.ResponseWriter, upstream io.ReadCloser, result *ForwardResult, start time.Time) (*ForwardResult, error) {
	flusher, canFlush := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // nginx 不缓冲，关键！
	if canFlush {
		flusher.Flush()
	}

	scanner := bufio.NewScanner(upstream)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024) // 大 input_json_delta 单行可能超 64KB

	var contentChars int
	for scanner.Scan() {
		line := scanner.Bytes()

		// usage 在最后一个 chunk（include_usage 已注入）——解析但不修改转发内容；
		// 同时累计 delta.content 字符数，供上游不给 usage 时的估算兜底
		// （不能按整行字节估，JSON 结构开销会高估数十倍）
		if data, ok := bytes.CutPrefix(line, []byte("data: ")); ok && !bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			var carrier streamChunk
			if json.Unmarshal(data, &carrier) == nil {
				if carrier.Usage != nil {
					result.PromptTokens = carrier.Usage.PromptTokens
					result.CompletionTokens = carrier.Usage.CompletionTokens
				}
				if len(carrier.Choices) > 0 {
					contentChars += utf8.RuneCountInString(carrier.Choices[0].Delta.Content)
				}
			}
		}

		if _, err := w.Write(append(line[:len(line):len(line)], '\n')); err != nil {
			upstream.Close() // 客户端断开：必须关闭上游连接，否则继续烧钱
			return result, nil
		}
		if canFlush {
			flusher.Flush()
		}
	}
	if err := scanner.Err(); err != nil {
		// 上游中途断流：已输出的部分没法撤回，按已有数据估算 usage 走结算
		result.UsageEstimated = true
	}
	// 上游没给 usage：按输出内容字符数估算（中文≈1 token/字，英文≈4字符/token，
	// 取 2 字符/token 折中）；prompt 侧由调用方在预扣时估算
	if result.CompletionTokens == 0 && contentChars > 0 {
		result.CompletionTokens = contentChars / 2
		if result.CompletionTokens == 0 {
			result.CompletionTokens = 1
		}
		result.UsageEstimated = true
	}
	return result, nil
}

func msSince(t time.Time) int { return int(time.Since(t).Milliseconds()) }
