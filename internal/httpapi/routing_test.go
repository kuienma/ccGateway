package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/llm"
)

// 本文件是唯一在「两个 provider 同时在册」的前提下测路由的地方。
//
// 为什么必须单独有它：其余 internal/httpapi 用例注入的 providers 表**只有 bedrock 一项**，
// 而生产上唯一会出现的形态是两项同时在册（buildProviders 只要有 ARK_API_KEY 就装配 ark，
// configs/models.json 默认就登记了 ark 模型）。也就是说在补这一组用例之前，
// 「provider=ark 的模型真的到达了 ark provider」这个正向属性在 HTTP 层零覆盖 ——
// 已有的两条 ark 用例断言的都是失败路径（登记为 ark 但未注入 → 500）。
//
// 每条断言都是成对的：**既**断言目标 provider 收到了调用（含 modelID 正确），
// **也**断言另一个 provider 一次都没被调用。只断言前者放得过「把请求路由到任意一个
// provider」这类串台变异 —— 那种变异下目标 provider 照样有机会被调到。
// 同时覆盖 providerFor 的全部三个消费点：非流式 /v1/messages、流式 /v1/messages、
// 以及 /v1/messages/count_tokens。

const (
	routingBedrockAlias   = "opus-5"
	routingBedrockModelID = "global.anthropic.claude-opus-5"
	routingArkAlias       = "ds-flash"
	routingArkModelID     = "deepseek-v4-flash-ga-260731"
)

// newRoutingTestServer 装配一台两个 provider 同时在册的 server，并返回两个 stub。
func newRoutingTestServer(t *testing.T) (*Server, *runtimeStub, *runtimeStub) {
	t.Helper()

	bedrockStub := &runtimeStub{
		response:     []byte(`{"id":"msg_bedrock","type":"message","content":[]}`),
		streamChunks: [][]byte{[]byte(`{"type":"message_start"}`), []byte(`{"type":"message_stop"}`)},
		tokenCount:   11,
	}
	arkStub := &runtimeStub{
		response:     []byte(`{"id":"msg_ark","type":"message","content":[]}`),
		streamChunks: [][]byte{[]byte(`{"type":"message_start"}`), []byte(`{"type":"message_stop"}`)},
		tokenCount:   22,
	}
	server := NewServer(
		ServerConfig{APIKey: testGatewayKey, MaxBodyBytes: 1 << 20, StreamHeartbeatInterval: time.Hour,
			MaxConcurrentStreams: 4, StreamQueueTimeout: time.Second},
		providerResolverStub{
			routingBedrockAlias: {ModelID: routingBedrockModelID, Provider: llm.ProviderBedrock},
			routingArkAlias:     {ModelID: routingArkModelID, Provider: llm.ProviderArk},
		},
		map[string]llm.Provider{llm.ProviderBedrock: bedrockStub, llm.ProviderArk: arkStub},
		discardLogger(),
	)
	return server, bedrockStub, arkStub
}

// assertRoutedTo 断言只有 target 被调用过，且拿到的是 expectedModelID；other 必须完全没被碰过。
func assertRoutedTo(t *testing.T, target *runtimeStub, expectedModelID string, other *runtimeStub, otherName string) {
	t.Helper()

	if !target.called() {
		t.Fatalf("目标 provider 没有收到调用")
	}
	if target.modelID != expectedModelID {
		t.Fatalf("目标 provider 拿到的 modelID = %q，期望 %q", target.modelID, expectedModelID)
	}
	if other.called() {
		t.Fatalf("请求被路由到了 %s provider（modelID = %q）：两个 provider 同时在册时发生了串台",
			otherName, other.modelID)
	}
}

func TestMessagesRoutesToProviderOfResolvedModel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		alias string
		// 正向：应该收到调用的 provider 与它该拿到的 modelID。
		wantModelID string
		// 反向：必须一次都没被调用的那一个。
		otherName string
		pick      func(bedrock, ark *runtimeStub) (target, other *runtimeStub)
	}{
		{
			name: "bedrock 模型走 bedrock", alias: routingBedrockAlias, wantModelID: routingBedrockModelID,
			otherName: "ark",
			pick:      func(bedrock, ark *runtimeStub) (*runtimeStub, *runtimeStub) { return bedrock, ark },
		},
		{
			name: "ark 模型走 ark", alias: routingArkAlias, wantModelID: routingArkModelID,
			otherName: "bedrock",
			pick:      func(bedrock, ark *runtimeStub) (*runtimeStub, *runtimeStub) { return ark, bedrock },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server, bedrockStub, arkStub := newRoutingTestServer(t)
			target, other := tc.pick(bedrockStub, arkStub)
			response := performRequest(server, http.MethodPost, "/v1/messages",
				[]byte(`{"model":"`+tc.alias+`","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`),
				map[string]string{"x-api-key": testGatewayKey})

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			// 响应体必须来自目标 provider：两个 stub 的 response 刻意不同，
			// 这样连「路由对了但写出了另一个 provider 的结果」也会被抓到。
			if response.Body.String() != string(target.response) {
				t.Fatalf("响应体来自错误的 provider: %s", response.Body.String())
			}
			assertRoutedTo(t, target, tc.wantModelID, other, tc.otherName)
		})
	}
}

func TestStreamingMessagesRoutesToProviderOfResolvedModel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		alias       string
		wantModelID string
		otherName   string
		pick        func(bedrock, ark *runtimeStub) (target, other *runtimeStub)
	}{
		{
			name: "bedrock 模型走 bedrock", alias: routingBedrockAlias, wantModelID: routingBedrockModelID,
			otherName: "ark",
			pick:      func(bedrock, ark *runtimeStub) (*runtimeStub, *runtimeStub) { return bedrock, ark },
		},
		{
			name: "ark 模型走 ark", alias: routingArkAlias, wantModelID: routingArkModelID,
			otherName: "bedrock",
			pick:      func(bedrock, ark *runtimeStub) (*runtimeStub, *runtimeStub) { return ark, bedrock },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server, bedrockStub, arkStub := newRoutingTestServer(t)
			target, other := tc.pick(bedrockStub, arkStub)
			response := performRequest(server, http.MethodPost, "/v1/messages",
				[]byte(`{"model":"`+tc.alias+`","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":true}`),
				map[string]string{"x-api-key": testGatewayKey})

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), "event: message_stop\n") {
				t.Fatalf("SSE 未走完: %s", response.Body.String())
			}
			assertRoutedTo(t, target, tc.wantModelID, other, tc.otherName)
		})
	}
}

func TestCountTokensRoutesToProviderOfResolvedModel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		alias       string
		wantModelID string
		wantTokens  int32
		otherName   string
		pick        func(bedrock, ark *runtimeStub) (target, other *runtimeStub)
	}{
		{
			name: "bedrock 模型走 bedrock", alias: routingBedrockAlias, wantModelID: routingBedrockModelID,
			wantTokens: 11, otherName: "ark",
			pick: func(bedrock, ark *runtimeStub) (*runtimeStub, *runtimeStub) { return bedrock, ark },
		},
		{
			name: "ark 模型走 ark", alias: routingArkAlias, wantModelID: routingArkModelID,
			wantTokens: 22, otherName: "bedrock",
			pick: func(bedrock, ark *runtimeStub) (*runtimeStub, *runtimeStub) { return ark, bedrock },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server, bedrockStub, arkStub := newRoutingTestServer(t)
			target, other := tc.pick(bedrockStub, arkStub)
			response := performRequest(server, http.MethodPost, "/v1/messages/count_tokens",
				[]byte(`{"model":"`+tc.alias+`","messages":[{"role":"user","content":"hi"}]}`),
				map[string]string{"x-api-key": testGatewayKey})

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			// 两个 stub 的 tokenCount 刻意不同：计数值本身就能指出结果来自哪一侧。
			var envelope struct {
				InputTokens int32 `json:"input_tokens"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("解析响应失败: %v", err)
			}
			if envelope.InputTokens != tc.wantTokens {
				t.Fatalf("input_tokens = %d，期望 %d（计数来自错误的 provider）", envelope.InputTokens, tc.wantTokens)
			}
			assertRoutedTo(t, target, tc.wantModelID, other, tc.otherName)
		})
	}
}
