package bedrock

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

type sdkStub struct {
	invokeInput *bedrockruntime.InvokeModelInput
	countInput  *bedrockruntime.CountTokensInput
}

func (s *sdkStub) InvokeModel(
	_ context.Context,
	input *bedrockruntime.InvokeModelInput,
	_ ...func(*bedrockruntime.Options),
) (*bedrockruntime.InvokeModelOutput, error) {
	s.invokeInput = input
	return &bedrockruntime.InvokeModelOutput{Body: []byte(`{"type":"message"}`)}, nil
}

func (s *sdkStub) InvokeModelWithResponseStream(
	context.Context,
	*bedrockruntime.InvokeModelWithResponseStreamInput,
	...func(*bedrockruntime.Options),
) (*bedrockruntime.InvokeModelWithResponseStreamOutput, error) {
	panic("此测试不调用流式接口")
}

func (s *sdkStub) CountTokens(
	_ context.Context,
	input *bedrockruntime.CountTokensInput,
	_ ...func(*bedrockruntime.Options),
) (*bedrockruntime.CountTokensOutput, error) {
	s.countInput = input
	return &bedrockruntime.CountTokensOutput{InputTokens: aws.Int32(42)}, nil
}

func TestClientInvokePassesModelAndBody(t *testing.T) {
	t.Parallel()

	sdk := &sdkStub{}
	client := NewClient(sdk)
	body := []byte(`{"messages":[]}`)

	response, err := client.Invoke(context.Background(), "model-id", body)
	if err != nil {
		t.Fatalf("Invoke 失败: %v", err)
	}
	if string(response) != `{"type":"message"}` {
		t.Fatalf("response = %s", response)
	}
	if sdk.invokeInput == nil || *sdk.invokeInput.ModelId != "model-id" {
		t.Fatalf("ModelId 未透传: %#v", sdk.invokeInput)
	}
	if string(sdk.invokeInput.Body) != string(body) {
		t.Fatalf("Body = %s", sdk.invokeInput.Body)
	}
}

func TestClientCountTokensUsesInvokeModelShape(t *testing.T) {
	t.Parallel()

	sdk := &sdkStub{}
	client := NewClient(sdk)
	body := []byte(`{"messages":[]}`)

	count, err := client.CountTokens(context.Background(), "model-id", body)
	if err != nil {
		t.Fatalf("CountTokens 失败: %v", err)
	}
	if count != 42 {
		t.Fatalf("count = %d", count)
	}
	if sdk.countInput == nil || *sdk.countInput.ModelId != "model-id" {
		t.Fatalf("ModelId 未透传: %#v", sdk.countInput)
	}
	member, ok := sdk.countInput.Input.(*brtypes.CountTokensInputMemberInvokeModel)
	if !ok || string(member.Value.Body) != string(body) {
		t.Fatalf("CountTokens body 未按 InvokeModel 形态透传: %#v", sdk.countInput.Input)
	}
}
