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
)

// ForwardResult 转发结果，供结算与日志使用
type ForwardResult struct {
	PromptTokens     int
	CompletionTokens int
	UsageEstimated   bool // 上游没给 usage，按输出字符数估算
	FirstByteMs      int
	UpstreamStatus   int
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

// Forward 单渠道转发（P1）：按 RelayPipe 做协议转换，流式逐行 flush，
// 客户端断连立即关闭上游（01 文档 3.1）。
// 调用方保证 openai 格式 body 已注入 include_usage（如启用且上游为 OpenAI 系）。
func Forward(
	ctx context.Context,
	w http.ResponseWriter,
	client *http.Client,
	pipe *RelayPipe,
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
		return relayNonStream(w, resp.Body, pipe, result)
	}
	return relayStream(w, resp.Body, pipe, result)
}

// relayNonStream 非流式：整包读出 → 管道转换 → 提取 usage → 回写
func relayNonStream(w http.ResponseWriter, body io.Reader, pipe *RelayPipe, result *ForwardResult) (*ForwardResult, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return result, fmt.Errorf("read upstream response: %w", err)
	}
	out, usage, usageOK, cerr := pipe.NonStream(raw)
	if cerr != nil {
		return result, fmt.Errorf("convert upstream response: %w", cerr)
	}
	if usageOK && usage != nil {
		result.PromptTokens = usage.PromptTokens
		result.CompletionTokens = usage.CompletionTokens
	} else {
		result.CompletionTokens = EstimateTokens(raw)
		result.UsageEstimated = true
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
	return result, nil
}

// relayStream 流式核心：上游行 → 管道转换 → 逐行写出 + flush；断连即关闭上游
func relayStream(w http.ResponseWriter, upstream io.ReadCloser, pipe *RelayPipe, result *ForwardResult) (*ForwardResult, error) {
	flusher, canFlush := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // nginx 不缓冲，关键！
	if canFlush {
		flusher.Flush()
	}

	scanner := bufio.NewScanner(upstream)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024) // 大 input_json_delta 单行可能超 64KB

	writeLines := func(lines []string) bool {
		for _, l := range lines {
			if _, err := w.Write([]byte(l + "\n")); err != nil {
				return false
			}
		}
		if canFlush {
			flusher.Flush()
		}
		return true
	}
	// 第二段转换：openai 行 → 客户端行（nil = 客户端就是 openai 格式）
	passthrough := func(lines []string) ([]string, bool) {
		if pipe.ToClient == nil {
			return lines, true
		}
		var out []string
		for _, l := range lines {
			out = append(out, pipe.ToClient.Feed([]byte(l))...)
		}
		return out, true
	}

	for scanner.Scan() {
		lines := pipe.FromUpstream.Feed(scanner.Bytes())
		if len(lines) == 0 {
			continue
		}
		lines, _ = passthrough(lines)
		if !writeLines(lines) {
			// 客户端断开：必须关闭上游连接，否则继续烧钱；
			// 已收到的部分照常提取 usage 走结算
			upstream.Close()
			result.PromptTokens, result.CompletionTokens, result.UsageEstimated = extractUsageResult(pipe)
			return result, nil
		}
	}
	if scanner.Err() != nil {
		// 上游中途断流：已输出的部分没法撤回，按已有数据估算 usage 走结算
		result.UsageEstimated = true
	}

	// 收尾：第一段的 Finish 行（如补发 [DONE]）过一遍第二段，再补第二段自己的收尾
	lines := pipe.FromUpstream.Finish()
	lines, _ = passthrough(lines)
	if pipe.ToClient != nil {
		lines = append(lines, pipe.ToClient.Finish()...)
	}
	if len(lines) > 0 {
		if !writeLines(lines) {
			upstream.Close()
			result.PromptTokens, result.CompletionTokens, result.UsageEstimated = extractUsageResult(pipe)
			return result, nil
		}
	}

	result.PromptTokens, result.CompletionTokens, result.UsageEstimated = extractUsageResult(pipe)
	return result, nil
}

// extractUsageResult usage 优先级：上游真实 usage → 输出字符数估算
func extractUsageResult(pipe *RelayPipe) (int, int, bool) {
	if p, c, ok := pipe.FromUpstream.Usage(); ok {
		return p, c, false
	}
	if pipe.ToClient != nil {
		if p, c, ok := pipe.ToClient.Usage(); ok {
			return p, c, false
		}
	}
	chars := pipe.FromUpstream.ContentChars()
	if chars <= 0 && pipe.ToClient != nil {
		chars = pipe.ToClient.ContentChars()
	}
	if chars <= 0 {
		return 0, 0, true
	}
	// 中文≈1 token/字，英文≈4字符/token，取 2 字符/token 折中
	c := chars / 2
	if c == 0 {
		c = 1
	}
	return 0, c, true
}

func msSince(t time.Time) int { return int(time.Since(t).Milliseconds()) }
