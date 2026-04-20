package integration

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestMain(m *testing.M) {
	// 1. Build the proxy binary from the root.
	// Resolve `go` from PATH so the harness works on dev machines where
	// the toolchain lives outside /usr/local/go (e.g. Homebrew on macOS).
	goBin, err := exec.LookPath("go")
	if err != nil {
		log.Fatalf("Failed to locate `go` in PATH: %v", err)
	}
	buildCmd := exec.Command(goBin, "build", "-o", "s3proxy4gcs_test_bin", ".")
	buildCmd.Dir = "../"
	if err := buildCmd.Run(); err != nil {
		log.Fatalf("Failed to build proxy binary: %v", err)
	}

	// Disable AWS SDK automatic checksum calculation for performance and GCS compatibility
	os.Setenv("AWS_REQUEST_CHECKSUM_CALCULATION", "WHEN_REQUIRED")
	os.Setenv("AWS_RESPONSE_CHECKSUM_VALIDATION", "WHEN_REQUIRED")

	// 2. Start the proxy server using the built binary
	cmd := exec.Command("./s3proxy4gcs_test_bin")
	cmd.Dir = "../" // Critical to read the root .env file!
	cmd.Env = append(os.Environ(), "PORT=8081")
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		log.Fatalf("Failed to start proxy server: %v", err)
	}

	// 2. Poll port 8081 until the proxy begins listening. The previous
	// fixed 2-second sleep was brittle: when GCS warmup hits its 10s
	// deadline (common on flaky networks) the binary only starts the
	// HTTP listener ~10s after launch, long after 2s has elapsed.
	// We give ourselves up to 20s so warmup has time to fail gracefully.
	waitForListen("127.0.0.1:8081", 20*time.Second)

	// 3. Run tests
	code := m.Run()

	// 4. Cleanup
	if err := cmd.Process.Kill(); err != nil {
		log.Printf("Failed to kill proxy server: %v", err)
	}
	os.Remove("../s3proxy4gcs_test_bin") // Clean up binary!

	os.Exit(code)
}

// waitForListen blocks until addr accepts a TCP connection or the overall
// deadline elapses. Used in TestMain to bridge the gap between `cmd.Start()`
// and the proxy's main listener becoming ready, which can take >10s when
// the GCS warmup call times out. Keeps integration tests deterministic
// without introducing arbitrary sleeps into individual cases.
func waitForListen(addr string, max time.Duration) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	log.Fatalf("proxy did not begin listening on %s within %s", addr, max)
}

func TestPutLifecycleWithAWSSDK(t *testing.T) {
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
	)
	if err != nil {
		t.Fatalf("unable to load SDK config, %v", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
		o.HTTPClient = &http.Client{Transport: transport}
		o.BaseEndpoint = aws.String("http://storage.googleapis.com")
	})

	// Construct request
	input := &s3.PutBucketLifecycleConfigurationInput{
		Bucket: aws.String("test-bucket"),
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{
			Rules: []types.LifecycleRule{
				{
					ID:     aws.String("Rule1"),
					Status: types.ExpirationStatusEnabled,
					Transitions: []types.Transition{
						{
							Days:         aws.Int32(30),
							StorageClass: types.TransitionStorageClassGlacier,
						},
					},
					Expiration: &types.LifecycleExpiration{
						Days: aws.Int32(365),
					},
				},
			},
		},
	}

	t.Log("Sending PutBucketLifecycleConfiguration via standard AWS S3 SDK Go...")
	_, err = client.PutBucketLifecycleConfiguration(context.TODO(), input)
	if err != nil {
		t.Fatalf("Failed to execute PutBucketLifecycleConfiguration: %v", err)
	}

	t.Log("PutBucketLifecycleConfiguration succeeded or responded correctly (according to proxy stub)!")
}

func TestPutLifecycleMultipleTransitionsAWSSDK(t *testing.T) {
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
	)
	if err != nil {
		t.Fatalf("unable to load SDK config, %v", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
		o.HTTPClient = &http.Client{Transport: transport}
		o.BaseEndpoint = aws.String("http://storage.googleapis.com")
	})

	// Construct request with multiple transitions (Full Function)
	input := &s3.PutBucketLifecycleConfigurationInput{
		Bucket: aws.String("test-bucket"),
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{
			Rules: []types.LifecycleRule{
				{
					ID:     aws.String("MultiTransitionRule"),
					Status: types.ExpirationStatusEnabled,
					Transitions: []types.Transition{
						{
							Days:         aws.Int32(30),
							StorageClass: types.TransitionStorageClassGlacier,
						},
						{
							Days:         aws.Int32(90),
							StorageClass: types.TransitionStorageClassDeepArchive,
						},
					},
					Expiration: &types.LifecycleExpiration{
						Days: aws.Int32(365),
					},
				},
			},
		},
	}

	t.Log("Sending PutBucketLifecycleConfiguration with multiple transitions via standard AWS S3 SDK Go...")
	_, err = client.PutBucketLifecycleConfiguration(context.TODO(), input)
	if err != nil {
		t.Fatalf("Failed to execute PutBucketLifecycleConfiguration with multiple transitions: %v", err)
	}

	t.Logf("PutBucketLifecycleConfiguration with multiple transitions succeeded!")
}
