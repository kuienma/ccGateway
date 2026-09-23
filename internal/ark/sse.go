package ark

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/ark/translate"
)

const (
	dataPrefix = "data:"
	doneMarker = "[DONE]"
	// 工具参数可能很长，单行上限放宽到 4MiB。
	maxSSELineBytes = 4 << 20
	initialSSEBuf   = 64 << 10
)

// scanSSE 逐行解析 OpenAI 兼容的 SSE 流，对每个 data chunk 调用 onChunk。
// 返回是否见到 [DONE]，供调用方判断流是否完整结束。
func scanSSE(r io.Reader, onChunk func(translate.StreamChunk) error) (bool, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, initialSSEBuf), maxSSELineBytes)

	sawDone := false
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		// 空行是事件分隔符，以 ':' 开头的是注释（含心跳），都跳过。
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, dataPrefix) {
			// 忽略 event: / id: 等其他字段，Ark 当前不使用。
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(line, dataPrefix))
		if payload == "" {
			continue
		}
		if payload == doneMarker {
			sawDone = true
			continue
		}

		var chunk translate.StreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return sawDone, fmt.Errorf("解析 Ark 流事件: %w", err)
		}
		if err := onChunk(chunk); err != nil {
			return sawDone, err
		}
	}
	if err := scanner.Err(); err != nil {
		return sawDone, fmt.Errorf("读取 Ark 流: %w", err)
	}
	return sawDone, nil
}
