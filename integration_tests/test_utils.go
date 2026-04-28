package integration

import (
	"bufio"
	"os"
	"strings"
)

// integrationBucket is the real GCS bucket used by all integration tests
// that mutate bucket-level configuration (lifecycle / CORS / logging / website)
// or exercise object-level data-plane (tagging / restore / storage_class / CRUD).
// It is provisioned once in project `cbs-poctest` (US-EAST1) and referenced
// directly from the tests — no TEST_BUCKET env indirection.
const integrationBucket = "s3proxy-integration"

// integrationLogTargetBucket is the dedicated target bucket for PutBucketLogging
// tests. GCS requires the target bucket to be different from the source bucket
// and to grant roles/storage.objectCreator to group:cloud-storage-analytics@google.com
// (granted at provisioning time).
const integrationLogTargetBucket = "s3proxy-integration-log-target"

// getTestBucket returns the integration-tests bucket name. Kept as a function
// (instead of inlining the constant) so individual tests can be retargeted in
// the future with a single edit here.
func getTestBucket() string {
	return integrationBucket
}

// getTestPrefix resolves an optional per-run key prefix from GCS_PREFIX so
// that parallel CI runs do not collide on object keys. Returns empty string
// when unset.
func getTestPrefix() string {
	return os.Getenv("GCS_PREFIX")
}

// getAWSAccessKey resolves the AWS_ACCESS_KEY_ID from environment or parent .env.
func getAWSAccessKey() string {
	if k := os.Getenv("AWS_ACCESS_KEY_ID"); k != "" {
		return k
	}
	return readEnvFileValue("AWS_ACCESS_KEY_ID")
}

// getAWSSecretKey resolves the AWS_SECRET_ACCESS_KEY from environment or parent .env.
func getAWSSecretKey() string {
	if k := os.Getenv("AWS_SECRET_ACCESS_KEY"); k != "" {
		return k
	}
	return readEnvFileValue("AWS_SECRET_ACCESS_KEY")
}

// readEnvFileValue reads a single KEY=VALUE entry from ../.env when the
// matching environment variable is not present in os.Environ(). Returns "" if
// the file is missing or the key is not found.
func readEnvFileValue(key string) string {
	file, err := os.Open("../.env")
	if err != nil {
		return ""
	}
	defer file.Close()

	prefix := key + "="
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, prefix) {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				return strings.Trim(parts[1], " \"'")
			}
		}
	}
	return ""
}
