package integration

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go/middleware"
	smithy_http "github.com/aws/smithy-go/transport/http"
)

func TestPutLoggingWithAWSSDK(t *testing.T) {
	dialer := &net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if strings.HasPrefix(addr, "storage.googleapis.com") {
				return dialer.DialContext(ctx, network, "localhost:8081")
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}

	creds := aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
		return aws.Credentials{
			AccessKeyID:     getAWSAccessKey(),
			SecretAccessKey: getAWSSecretKey(),
			Source:          "test-env",
		}, nil
	})

	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithCredentialsProvider(creds),
		config.WithRegion("us-east-1"),
		config.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		config.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	)
	if err != nil {
		t.Fatalf("Failed to load AWS config: %v", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
		o.HTTPClient = &http.Client{Transport: transport}
		o.BaseEndpoint = aws.String("http://storage.googleapis.com")
		o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
			_, _ = stack.Finalize.Remove("DisableAcceptEncodingGzip")

			return stack.Build.Add(middleware.BuildMiddlewareFunc("GCSParamFix", func(
				ctx context.Context, in middleware.BuildInput, next middleware.BuildHandler,
			) (out middleware.BuildOutput, metadata middleware.Metadata, err error) {
				req, ok := in.Request.(*smithy_http.Request)
				if ok {
					q := req.URL.Query()
					q.Del("x-id")
					req.URL.RawQuery = q.Encode()

					req.Header.Del("User-Agent")
				}
				return next.HandleBuild(ctx, in)
			}), middleware.After)
		})
	})

	// 2. Prepare Bucket Logging Configuration
	input := &s3.PutBucketLoggingInput{
		Bucket: aws.String(getTestBucket()),
		BucketLoggingStatus: &types.BucketLoggingStatus{
			LoggingEnabled: &types.LoggingEnabled{
				TargetBucket: aws.String(integrationLogTargetBucket),
				TargetPrefix: aws.String("logs/"),
			},
		},
	}

	t.Logf("Sending PutBucketLogging via standard AWS S3 SDK Go...")
	_, err = client.PutBucketLogging(context.TODO(), input)
	if err != nil {
		t.Fatalf("Failed to execute PutBucketLogging: %v", err)
	}

	t.Logf("Sending GetBucketLogging via standard AWS S3 SDK Go...")
	getOut, err := client.GetBucketLogging(context.TODO(), &s3.GetBucketLoggingInput{
		Bucket: aws.String(getTestBucket()),
	})
	if err != nil {
		t.Fatalf("Failed to execute GetBucketLogging: %v", err)
	}
	if getOut.LoggingEnabled == nil {
		t.Fatalf("GetBucketLogging returned nil LoggingEnabled, expected non-nil")
	}
	t.Logf("GetBucketLogging succeeded (TargetBucket: %s)!", *getOut.LoggingEnabled.TargetBucket)
}
