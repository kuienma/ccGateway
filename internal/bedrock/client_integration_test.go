//go:build integration

package bedrock

import (
	"context"
	"os"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
)

const integrationTimeout = 30 * time.Second

func TestAWSCountTokensIntegration(t *testing.T) {
	modelID := os.Getenv("BEDROCK_INTEGRATION_MODEL")
	if modelID == "" {
		t.Skip("未设置 BEDROCK_INTEGRATION_MODEL")
	}

	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		t.Fatalf("加载 AWS 默认配置失败: %v", err)
	}

	client := NewClient(bedrockruntime.NewFromConfig(cfg))
	body := []byte(`{
		"anthropic_version":"bedrock-2023-05-31",
		"messages":[{"role":"user","content":"hello"}]
	}`)
	count, err := client.CountTokens(ctx, modelID, body)
	if err != nil {
		t.Fatalf("调用真实 Bedrock CountTokens 失败: %v", err)
	}
	if count <= 0 {
		t.Fatalf("input_tokens = %d", count)
	}
}
