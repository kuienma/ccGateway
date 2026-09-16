package bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
)

type httpClientFunc func(*http.Request) (*http.Response, error)

func (f httpClientFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

type observedBody struct {
	io.ReadCloser
	closed              chan struct{}
	reading             chan struct{}
	closeOnce, readOnce sync.Once
}

func (b *observedBody) Close() error {
	b.closeOnce.Do(func() { _ = b.ReadCloser.Close(); close(b.closed) })
	return nil
}
func (b *observedBody) Read(p []byte) (int, error) {
	b.readOnce.Do(func() { close(b.reading) })
	return b.ReadCloser.Read(p)
}

func encodedEvent(t *testing.T, messageType, eventType string, payload []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	var headers eventstream.Headers
	headers.Set(":message-type", eventstream.StringValue(messageType))
	if messageType == "exception" {
		headers.Set(":exception-type", eventstream.StringValue(eventType))
	} else {
		headers.Set(":event-type", eventstream.StringValue(eventType))
	}
	headers.Set(":content-type", eventstream.StringValue("application/json"))
	if err := eventstream.NewEncoder().Encode(&buffer, eventstream.Message{Headers: headers, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestClientStreamThroughSDK(t *testing.T) {
	t.Parallel()
	emitErr := errors.New("write failed")
	for _, mode := range []string{"normal", "exception", "callback_error", "cancel_idle"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			var encoded []byte
			for _, chunk := range []string{`{"type":"message_start"}`, `{"type":"message_stop"}`} {
				payload, err := json.Marshal(map[string][]byte{"bytes": []byte(chunk)})
				if err != nil {
					t.Fatal(err)
				}
				encoded = append(encoded, encodedEvent(t, "event", "chunk", payload)...)
			}
			if mode == "exception" {
				encoded = append(encoded, encodedEvent(t, "exception", "modelStreamErrorException", []byte(`{"message":"stream failed"}`))...)
			}
			var source io.ReadCloser = io.NopCloser(bytes.NewReader(encoded))
			if mode == "cancel_idle" {
				reader, writer := io.Pipe()
				source = reader
				defer writer.Close()
			}
			body := &observedBody{ReadCloser: source, closed: make(chan struct{}), reading: make(chan struct{})}
			defer body.Close()
			calls := 0
			sdk := bedrockruntime.New(bedrockruntime.Options{
				Region: "us-east-1", Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
					return aws.Credentials{AccessKeyID: "test-only", SecretAccessKey: "test-only"}, nil
				}), BaseEndpoint: aws.String("http://bedrock.invalid"),
				HTTPClient: httpClientFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					data, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					if r.Method != "POST" || r.URL.Path != "/model/model-id/invoke-with-response-stream" || string(data) != `{"messages":[]}` || r.Header.Get("Content-Type") != contentTypeJSON || r.Header.Get("X-Amzn-Bedrock-Accept") != contentTypeJSON {
						t.Errorf("SDK 请求方法、路径、body 或内容类型未正确透传")
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}}, Body: body, Request: r}, nil
				}),
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var chunks []string
			done := make(chan error, 1)
			go func() {
				done <- NewClient(sdk).Stream(ctx, "model-id", []byte(`{"messages":[]}`), func(chunk []byte) error {
					chunks = append(chunks, string(chunk))
					if mode == "callback_error" {
						return emitErr
					}
					return nil
				})
			}()
			if mode == "cancel_idle" {
				select {
				case <-body.reading:
					cancel()
				case <-time.After(time.Second):
					t.Fatal("SDK 未开始读取流")
				}
			}
			var err error
			select {
			case err = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("流没有退出")
			}
			switch mode {
			case "normal":
				if err != nil {
					t.Fatal(err)
				}
			case "exception":
				if err == nil || !strings.Contains(err.Error(), "stream failed") {
					t.Fatalf("SDK 异常未传播: %v", err)
				}
			case "callback_error":
				if !errors.Is(err, emitErr) {
					t.Fatal(err)
				}
			case "cancel_idle":
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			}
			want := []string{`{"type":"message_start"}`, `{"type":"message_stop"}`}
			if mode == "callback_error" {
				want = want[:1]
			}
			if mode == "cancel_idle" {
				want = nil
			}
			if !reflect.DeepEqual(chunks, want) {
				t.Fatalf("chunks=%v, want %v", chunks, want)
			}
			if calls != 1 {
				t.Fatalf("流被重放，调用次数=%d", calls)
			}
			select {
			case <-body.closed:
			case <-time.After(time.Second):
				t.Fatal("SDK 响应 Body 未关闭")
			}
		})
	}
}
