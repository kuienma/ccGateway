package bedrock

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

type streamReaderStub struct {
	events chan brtypes.ResponseStream
	err    error
	closes int
}

func (r *streamReaderStub) Events() <-chan brtypes.ResponseStream { return r.events }
func (r *streamReaderStub) Err() error                            { return r.err }
func (r *streamReaderStub) Close() error                          { r.closes++; return nil }
func testSDKStream(reader *streamReaderStub) *bedrockruntime.InvokeModelWithResponseStreamEventStream {
	return bedrockruntime.NewInvokeModelWithResponseStreamEventStream(func(s *bedrockruntime.InvokeModelWithResponseStreamEventStream) { s.Reader = reader })
}

func TestConsumeStreamClosesOnEveryExit(t *testing.T) {
	t.Parallel()
	readErr, emitErr := errors.New("upstream read failed"), errors.New("client write failed")
	for _, tc := range []struct {
		name             string
		readErr, emitErr error
		want             []string
	}{
		{"normal", nil, nil, []string{"first", "second"}},
		{"upstream_error", readErr, nil, []string{"first", "second"}},
		{"callback_error", nil, emitErr, []string{"first"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &streamReaderStub{events: make(chan brtypes.ResponseStream, 3), err: tc.readErr}
			reader.events <- &brtypes.UnknownUnionMember{Tag: "future"}
			for _, value := range []string{"first", "second"} {
				reader.events <- &brtypes.ResponseStreamMemberChunk{Value: brtypes.PayloadPart{Bytes: []byte(value)}}
			}
			close(reader.events)
			var got []string
			err := consumeStream(context.Background(), testSDKStream(reader), func(chunk []byte) error { got = append(got, string(chunk)); return tc.emitErr })
			wantErr := tc.readErr
			if tc.emitErr != nil {
				wantErr = tc.emitErr
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("error=%v, want %v", err, wantErr)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("events=%v, want %v", got, tc.want)
			}
			if reader.closes != 1 {
				t.Fatalf("Close 调用次数=%d", reader.closes)
			}
		})
	}
}

func TestConsumeStreamCancellationClosesIdleReader(t *testing.T) {
	reader := &streamReaderStub{events: make(chan brtypes.ResponseStream)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- consumeStream(ctx, testSDKStream(reader), func([]byte) error { t.Error("不应有事件"); return nil })
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
		if reader.closes != 1 {
			t.Fatalf("Close 调用次数=%d", reader.closes)
		}
	case <-time.After(time.Second):
		close(reader.events)
		<-done
		t.Fatal("context 取消后仍等待 SDK reader，流未关闭")
	}
}
