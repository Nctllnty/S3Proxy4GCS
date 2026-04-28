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

func TestPutCorsWithAWSSDK(t *testing.T) {
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
		t.Fatalf("unable to load SDK config, %v", err)
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

	input := &s3.PutBucketCorsInput{
		Bucket: aws.String(getTestBucket()),
		CORSConfiguration: &types.CORSConfiguration{
			CORSRules: []types.CORSRule{
				{
					AllowedHeaders: []string{"Authorization"},
					AllowedMethods: []string{"GET", "PUT"},
					AllowedOrigins: []string{"*"},
					ExposeHeaders:  []string{"x-amz-request-id"},
					MaxAgeSeconds:  aws.Int32(3000),
				},
			},
		},
	}

	t.Log("Sending PutBucketCors via standard AWS S3 SDK Go...")
	_, err = client.PutBucketCors(context.TODO(), input)
	if err != nil {
		t.Fatalf("Failed to execute PutBucketCors: %v", err)
	}

	t.Log("Sending GetBucketCors via standard AWS S3 SDK Go...")
	getOut, err := client.GetBucketCors(context.TODO(), &s3.GetBucketCorsInput{
		Bucket: aws.String(getTestBucket()),
	})
	if err != nil {
		t.Fatalf("Failed to execute GetBucketCors: %v", err)
	}
	if len(getOut.CORSRules) == 0 {
		t.Fatalf("GetBucketCors returned 0 rules, expected at least 1")
	}
	t.Logf("GetBucketCors succeeded (Retrieved %d rules)!", len(getOut.CORSRules))

	t.Log("Sending DeleteBucketCors via standard AWS S3 SDK Go...")
	_, err = client.DeleteBucketCors(context.TODO(), &s3.DeleteBucketCorsInput{
		Bucket: aws.String(getTestBucket()),
	})
	if err != nil {
		t.Fatalf("Failed to execute DeleteBucketCors: %v", err)
	}
	t.Log("DeleteBucketCors succeeded!")
}
