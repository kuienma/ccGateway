package ark

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/ark/translate"
)

// 取自 2026-09-22 对 Ark 的真实流式响应（见设计文档 §2）。
//
// 参数分片是**完整的八片**，与设计文档 §2 记的真机分帧逐片一致：
// `{"city": "` → `北京` → `"` → `, "unit": "` → `c` → `elsius` → `"` → `}`。
// 早先这里只留了第一片就直接跳到 finish_reason，那等于让 fixture 断言
// 「拼起来是半截 JSON 的 arguments 也算正常流」—— 真机从来不是这样，而流式
// arguments 对象守卫（events.go validateOpenToolArgs）补上后，那份删减恰好会
// 把守卫顶掉。fixture 自称取自真机，就必须把真机那几片都留着。
const arkToolStream = `data: {"choices":[{"delta":{"content":"","reasoning_content":"The","role":"assistant"},"index":0}],"id":"s1","model":"m","object":"chat.completion.chunk","usage":null}

data: {"choices":[{"delta":{"content":"","role":"assistant","tool_calls":[{"function":{"arguments":"","name":"get_weather"},"id":"call_1","index":0,"type":"function"}]},"index":0}],"id":"s1","model":"m","object":"chat.completion.chunk","usage":null}

data: {"choices":[{"delta":{"content":"","role":"assistant","tool_calls":[{"function":{"arguments":"{\"city\": \""},"index":0}]},"index":0}],"id":"s1","model":"m","object":"chat.completion.chunk","usage":null}

data: {"choices":[{"delta":{"content":"","role":"assistant","tool_calls":[{"function":{"arguments":"北京"},"index":0}]},"index":0}],"id":"s1","model":"m","object":"chat.completion.chunk","usage":null}

data: {"choices":[{"delta":{"content":"","role":"assistant","tool_calls":[{"function":{"arguments":"\""},"index":0}]},"index":0}],"id":"s1","model":"m","object":"chat.completion.chunk","usage":null}

data: {"choices":[{"delta":{"content":"","role":"assistant","tool_calls":[{"function":{"arguments":", \"unit\": \""},"index":0}]},"index":0}],"id":"s1","model":"m","object":"chat.completion.chunk","usage":null}

data: {"choices":[{"delta":{"content":"","role":"assistant","tool_calls":[{"function":{"arguments":"c"},"index":0}]},"index":0}],"id":"s1","model":"m","object":"chat.completion.chunk","usage":null}

data: {"choices":[{"delta":{"content":"","role":"assistant","tool_calls":[{"function":{"arguments":"elsius"},"index":0}]},"index":0}],"id":"s1","model":"m","object":"chat.completion.chunk","usage":null}

data: {"choices":[{"delta":{"content":"","role":"assistant","tool_calls":[{"function":{"arguments":"\""},"index":0}]},"index":0}],"id":"s1","model":"m","object":"chat.completion.chunk","usage":null}

data: {"choices":[{"delta":{"content":"","role":"assistant","tool_calls":[{"function":{"arguments":"}"},"index":0}]},"index":0}],"id":"s1","model":"m","object":"chat.completion.chunk","usage":null}

data: {"choices":[{"delta":{"content":"","role":"assistant"},"finish_reason":"tool_calls","index":0}],"id":"s1","model":"m","object":"chat.completion.chunk","usage":{"completion_tokens":81,"prompt_tokens":397}}

data: {"choices":[],"id":"s1","model":"m","object":"chat.completion.chunk","usage":{"completion_tokens":81,"prompt_tokens":397}}

data: [DONE]
`

func TestScanSSEParsesRealStream(t *testing.T) {
	t.Parallel()

	var chunks []translate.StreamChunk
	sawDone, err := scanSSE(strings.NewReader(arkToolStream), func(chunk translate.StreamChunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !sawDone {
		t.Fatal("应识别到 [DONE]")
	}
	if len(chunks) != 12 {
		t.Fatalf("chunk 数 = %d，期望 12（[DONE] 不产出 chunk）", len(chunks))
	}

	if chunks[0].Choices[0].Delta.ReasoningContent != "The" {
		t.Fatalf("首个 chunk 的 reasoning_content = %q", chunks[0].Choices[0].Delta.ReasoningContent)
	}

	first := chunks[1].Choices[0].Delta.ToolCalls[0]
	if first.ID != "call_1" || first.Function.Name != "get_weather" || first.Index != 0 {
		t.Fatalf("工具首发 chunk = %#v", first)
	}
	if first.Function.Arguments != "" {
		t.Fatalf("首发 chunk 的 arguments 应为空，实际 %q", first.Function.Arguments)
	}

	fragment := chunks[2].Choices[0].Delta.ToolCalls[0]
	if fragment.Function.Arguments != `{"city": "` {
		t.Fatalf("参数分片应原样保留转义后的文本，实际 %q", fragment.Function.Arguments)
	}
	if fragment.ID != "" {
		t.Fatal("后续分片不带 id，解析器不应凭空补值")
	}

	// 八片拼完必须是一个合法的 JSON 对象 —— 这是真机的事实，也是流式 arguments
	// 守卫（validateOpenToolArgs）所依赖的前提。在 fixture 层面锁住它，后人再删减
	// 分片时会在这里先失败，而不是把那个守卫悄悄顶掉。
	var joined string
	for _, chunk := range chunks {
		if len(chunk.Choices) == 0 || len(chunk.Choices[0].Delta.ToolCalls) == 0 {
			continue
		}
		joined += chunk.Choices[0].Delta.ToolCalls[0].Function.Arguments
	}
	if joined != `{"city": "北京", "unit": "celsius"}` {
		t.Fatalf("参数分片拼接结果 = %q，与设计文档 §2 记的真机分帧不一致", joined)
	}

	if chunks[10].Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q", chunks[10].Choices[0].FinishReason)
	}
	if chunks[11].Usage == nil || chunks[11].Usage.PromptTokens != 397 {
		t.Fatalf("末尾 usage chunk = %#v", chunks[11].Usage)
	}
	if len(chunks[11].Choices) != 0 {
		t.Fatal("末尾 usage chunk 的 choices 应为空")
	}
}

func TestScanSSEIgnoresCommentsAndBlankLines(t *testing.T) {
	t.Parallel()

	stream := ": ping\n\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"index\":0}]}\n\ndata: [DONE]\n"
	var count int
	sawDone, err := scanSSE(strings.NewReader(stream), func(translate.StreamChunk) error {
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !sawDone || count != 1 {
		t.Fatalf("sawDone=%v count=%d，期望 true/1", sawDone, count)
	}
}

func TestScanSSEReportsMissingDone(t *testing.T) {
	t.Parallel()

	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"index\":0}]}\n\n"
	sawDone, err := scanSSE(strings.NewReader(stream), func(translate.StreamChunk) error { return nil })
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if sawDone {
		t.Fatal("流里没有 [DONE] 时 sawDone 必须为 false")
	}
}

func TestScanSSERejectsMalformedChunk(t *testing.T) {
	t.Parallel()

	stream := "data: {不是JSON\n\n"
	if _, err := scanSSE(strings.NewReader(stream), func(translate.StreamChunk) error { return nil }); err == nil {
		t.Fatal("畸形 chunk 必须报错，不能静默跳过")
	}
}

func TestScanSSEPropagatesCallbackError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("下游中止")
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"index\":0}]}\n\n"
	_, err := scanSSE(strings.NewReader(stream), func(translate.StreamChunk) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("回调错误应透传，实际 %v", err)
	}
}

func TestScanSSEHandlesLongLine(t *testing.T) {
	t.Parallel()

	// 工具参数可能很长，单行远超 bufio.Scanner 的 64KiB 默认上限。
	long := strings.Repeat("a", 200*1024)
	stream := fmt.Sprintf(
		"data: {\"choices\":[{\"delta\":{\"content\":\"\",\"tool_calls\":[{\"function\":{\"arguments\":%q},\"index\":0}]},\"index\":0}]}\n\ndata: [DONE]\n",
		long)

	var got string
	if _, err := scanSSE(strings.NewReader(stream), func(chunk translate.StreamChunk) error {
		if len(chunk.Choices) > 0 && len(chunk.Choices[0].Delta.ToolCalls) > 0 {
			got = chunk.Choices[0].Delta.ToolCalls[0].Function.Arguments
		}
		return nil
	}); err != nil {
		t.Fatalf("超长行解析失败: %v", err)
	}
	if len(got) != len(long) {
		t.Fatalf("超长参数被截断：得到 %d 字节，期望 %d", len(got), len(long))
	}
}
