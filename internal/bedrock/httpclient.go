package bedrock

import (
	"net"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
)

func NewHTTPClient() *awshttp.BuildableClient {
	return awshttp.NewBuildableClient().
		WithDialerOptions(func(dialer *net.Dialer) {
			dialer.Timeout = 10 * time.Second
			dialer.KeepAlive = 30 * time.Second
		}).
		WithTransportOptions(func(transport *http.Transport) {
			transport.TLSHandshakeTimeout = 10 * time.Second
			transport.ResponseHeaderTimeout = 120 * time.Second
			transport.IdleConnTimeout = 90 * time.Second
			transport.MaxIdleConns = 100
			transport.MaxIdleConnsPerHost = 20
		})
}

func NewRetryer() aws.Retryer {
	standard := retry.NewStandard(func(options *retry.StandardOptions) {
		options.MaxAttempts = 3
	})
	return retry.AddWithMaxBackoffDelay(standard, 5*time.Second)
}
