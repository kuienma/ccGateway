package bedrock

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

const contentTypeJSON = "application/json"

type SDK interface {
	InvokeModel(context.Context, *bedrockruntime.InvokeModelInput, ...func(*bedrockruntime.Options)) (*bedrockruntime.InvokeModelOutput, error)
	InvokeModelWithResponseStream(context.Context, *bedrockruntime.InvokeModelWithResponseStreamInput, ...func(*bedrockruntime.Options)) (*bedrockruntime.InvokeModelWithResponseStreamOutput, error)
	CountTokens(context.Context, *bedrockruntime.CountTokensInput, ...func(*bedrockruntime.Options)) (*bedrockruntime.CountTokensOutput, error)
}

type Runtime interface {
	Invoke(context.Context, string, []byte) ([]byte, error)
	Stream(context.Context, string, []byte, func([]byte) error) error
	CountTokens(context.Context, string, []byte) (int32, error)
}

type Client struct {
	sdk SDK
}

func NewClient(sdk SDK) *Client {
	return &Client{sdk: sdk}
}

func (c *Client) Invoke(ctx context.Context, modelID string, body []byte) ([]byte, error) {
	output, err := c.sdk.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		Accept:      aws.String(contentTypeJSON),
		Body:        body,
		ContentType: aws.String(contentTypeJSON),
		ModelId:     aws.String(modelID),
	})
	if err != nil {
		return nil, fmt.Errorf("调用 Bedrock InvokeModel: %w", err)
	}
	return output.Body, nil
}

func (c *Client) Stream(ctx context.Context, modelID string, body []byte, emit func([]byte) error) error {
	output, err := c.sdk.InvokeModelWithResponseStream(ctx, &bedrockruntime.InvokeModelWithResponseStreamInput{
		Accept:      aws.String(contentTypeJSON),
		Body:        body,
		ContentType: aws.String(contentTypeJSON),
		ModelId:     aws.String(modelID),
	})
	if err != nil {
		return fmt.Errorf("调用 Bedrock InvokeModelWithResponseStream: %w", err)
	}

	stream := output.GetStream()
	defer stream.Close()
	for event := range stream.Events() {
		chunk, ok := event.(*brtypes.ResponseStreamMemberChunk)
		if !ok {
			continue
		}
		if err := emit(chunk.Value.Bytes); err != nil {
			return fmt.Errorf("写出流事件: %w", err)
		}
	}
	if err := stream.Err(); err != nil {
		return fmt.Errorf("读取 Bedrock 流事件: %w", err)
	}
	return nil
}

func (c *Client) CountTokens(ctx context.Context, modelID string, body []byte) (int32, error) {
	output, err := c.sdk.CountTokens(ctx, &bedrockruntime.CountTokensInput{
		Input: &brtypes.CountTokensInputMemberInvokeModel{
			Value: brtypes.InvokeModelTokensRequest{Body: body},
		},
		ModelId: aws.String(modelID),
	})
	if err != nil {
		return 0, fmt.Errorf("调用 Bedrock CountTokens: %w", err)
	}
	return aws.ToInt32(output.InputTokens), nil
}
