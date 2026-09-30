package convert

import "bytes"

// trimSpace 去掉行首尾空白（SSE 行常见 \r 结尾，Windows 上游可能 \r\n）
func trimSpace(b []byte) []byte {
	return bytes.TrimSpace(b)
}

// cutPrefix 剥掉 SSE "data:" 前缀
func cutPrefix(line []byte, prefix string) ([]byte, bool) {
	if bytes.HasPrefix(line, []byte(prefix)) {
		return line[len(prefix):], true
	}
	return nil, false
}
