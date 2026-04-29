package config

import (
	"log"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"

	"s3proxy4gcs/pkg/credstore"
)

// FeatureFlags gates every externally-visible operation exposed by the proxy.
//
// Every flag defaults to true so a zero-config upgrade from previous releases
// continues to work. Disabling a control-plane or data-plane flag rejects the
// matching requests with `501 NotImplemented` at handler dispatch (before any
// body parsing or GCS call), and increments
// `s3proxy_feature_disabled_rejections_total{feature=...}`.
//
// Health / Readyz / Metrics endpoints are registered only when their flag is
// on; when disabled the proxy logs a startup WARN so operators are not
// surprised by K8s probe or Prometheus scrape failures.
type FeatureFlags struct {
	// Control plane (XML-translating handlers)
	Lifecycle     bool
	CORS          bool
	Logging       bool
	Website       bool
	Tagging       bool
	RestoreObject bool

	// Data plane — composite / high-cost operations only. Basic object CRUD
	// (Get/Put/Head/Delete/List/ListBuckets) has no switch by design: turning
	// them off would break the proxy as a whole and is better handled at the
	// network or IAM layer.
	CopyObject      bool
	MultipartUpload bool
	DeleteObjects   bool

	// Ops plane (operational endpoints)
	HealthEndpoint  bool
	ReadyzEndpoint  bool
	MetricsEndpoint bool
}

// Settings contains all the configuration for the proxy
type Settings struct {
	Port                  string
	GCPProjectID          string
	TargetBucket          string
	StorageBaseURL        string // For testing or custom setups
	GCSPrefix             string // Subfolder prefix for testing or namespacing
	DryRun                bool   // DryRun mode disables real GCS API hits
	DebugLogging          bool   // DebugLogging enables verbose output
	MaxIdleConns          int
	MaxIdleConnsPerHost   int
	MaxConcurrentRequests int           // Throttle middleware limit; 0 = no limit
	GCSCallTimeout        time.Duration // Timeout for individual GCS SDK calls; 0 = no limit
	IdleConnTimeout       time.Duration // How long idle connections stay in pool
	ResponseHeaderTimeout time.Duration // Max wait for response headers from GCS
	ReadBufferSize        int           // TCP read buffer size for read-path Transport
	WriteBufferSize       int           // TCP write buffer size for write-path Transport
	ProxyBufferSize       int           // Application-layer io.CopyBuffer size for ReverseProxy.BufferPool; 0 = Go default 32KB
	FlushIntervalMS       int           // ReverseProxy FlushInterval for read proxy in ms; -1 = immediate, 0 = no flush, >0 = periodic
	ProxyAccessKey        string        // Legacy single-key fallback (AK)
	ProxySecretKey        string        // Legacy single-key fallback (SK)
	JSONKey               string        // Path to GCS Service Account JSON key
	ProxyBaseDomain       string        // Base domain for virtual-hosted style support

	// DisableHeaderStrip disables the Director's removal of SDK-diagnostic
	// headers (Accept-Encoding, Amz-Sdk-Invocation-Id, Amz-Sdk-Request,
	// X-Amz-Decoded-Content-Length, X-Amz-Trailer, Content-Encoding:
	// aws-chunked) before re-signing. Default true since v1.8: the proxy
	// is a transparent pass-through and clients are responsible for not
	// sending headers GCS cannot verify. See docs/sdk-client-config.md for
	// the per-SDK workarounds (Go V2 / Go V1 / boto3 need explicit header
	// scrubbing; Java V1/V2 and C++ work out of the box). Set to false
	// only when re-enabling the legacy server-side strip behaviour.
	DisableHeaderStrip bool

	// HMACCredentials is the in-memory AK→SK mapping used by the Director
	// to re-sign each inbound request with the client's own credentials.
	// Populated from HMAC_CREDENTIALS (inline JSON), HMAC_CREDENTIALS_FILE
	// (volume-mounted K8s Secret), or synthesised from the legacy single-
	// key env vars for zero-config upgrades. See
	// docs/hmac-credential-mapping-design.md for the full specification.
	HMACCredentials map[string]string

	// CredentialsFile is the path to the JSON credential map watched for
	// hot reload via fsnotify. Empty when credentials were loaded from an
	// inline env var or the legacy single-key fallback.
	CredentialsFile string

	// HMACStrict forces the Director to reject requests whose access key
	// is not in HMACCredentials with `403 InvalidAccessKeyId`. When false
	// (legacy behaviour, used as the fallback when only the single-key
	// vars are configured), the proxy falls back to the single-key path
	// and a migration WARN is logged at startup. Controlled by
	// HMAC_STRICT — defaults to true once HMAC_CREDENTIALS* is set and
	// false otherwise.
	HMACStrict bool

	// PPROFAddr controls the optional pprof profiling endpoint. Empty (default)
	// keeps it disabled; set e.g. "127.0.0.1:6060" to expose runtime profiles
	// on an independent port. MUST NOT be exposed publicly.
	PPROFAddr string

	// RestoreSkipExistenceCheck toggles the object HEAD probe inside the
	// synthetic RestoreObject handler. Default false (probe enabled) so the
	// proxy returns 404 NoSuchKey for missing keys, matching AWS S3 semantics.
	// Set to true to short-circuit the probe when you are confident callers
	// will follow the restore with a GET that can surface 404 on its own;
	// useful for latency-sensitive workloads where the extra GCS HEAD matters.
	RestoreSkipExistenceCheck bool

	// Request data logging (SOH-delimited CSV file via ymlog)
	ReqLogEnabled   bool   // REQUEST_LOG_ENABLED,      default true
	ReqLogPath      string // REQUEST_LOG_PATH,          default "./logs/req_%Y%M%D.csv"
	ReqLogMaxSizeMB int    // REQUEST_LOG_MAX_SIZE_MB,   default 512
	ReqLogMaxBackup int    // REQUEST_LOG_MAX_BACKUP,    default 5
	ReqLogChanBuf   int    // REQUEST_LOG_CHAN_BUF,       default 10240
	ReqLogKeepDays  int    // REQUEST_LOG_KEEP_DAYS,     default 7

	// Error-only structured log file (logfmt via ymlog). Mirrors every
	// slog.Error/Critical record to a daily-rotated local file independent
	// of the stderr JSON stream so operators can grep `level=error` without
	// scraping container logs. Uses the same async / rotation contract as
	// the request log above.
	ErrLogEnabled   bool   // ERROR_LOG_ENABLED,        default true
	ErrLogPath      string // ERROR_LOG_PATH,            default "./logs/error_%Y%M%D.log"
	ErrLogMaxSizeMB int    // ERROR_LOG_MAX_SIZE_MB,     default 256
	ErrLogMaxBackup int    // ERROR_LOG_MAX_BACKUP,      default 5
	ErrLogChanBuf   int    // ERROR_LOG_CHAN_BUF,        default 4096
	ErrLogKeepDays  int    // ERROR_LOG_KEEP_DAYS,       default 14

	// Features toggles every plane's operations on/off. See FeatureFlags doc.
	Features FeatureFlags
}

var Config *Settings

// LoadConfig initialize the settings from a .env file or environment variables
func LoadConfig() {
	// Load from .env if it exists
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, reading from environment variables directly.")
	}

	dryRunStr := getEnv("DRY_RUN", "true") // Default to true if not set (Safe for laptop testing)
	dryRun := dryRunStr == "true"
	debugLogging := getEnv("DEBUG_LOGGING", "false") == "true"

	maxIdleConns := 1000
	if v := getEnv("MAX_IDLE_CONNS", "1000"); v != "" {
		if n, err := strconv.Atoi(v); err != nil {
			log.Printf("WARNING: invalid MAX_IDLE_CONNS value %q, using default 1000", v)
		} else {
			maxIdleConns = n
		}
	}

	maxIdleConnsPerHost := 1000
	if v := getEnv("MAX_IDLE_CONNS_PER_HOST", "1000"); v != "" {
		if n, err := strconv.Atoi(v); err != nil {
			log.Printf("WARNING: invalid MAX_IDLE_CONNS_PER_HOST value %q, using default 1000", v)
		} else {
			maxIdleConnsPerHost = n
		}
	}

	maxConcurrentRequests := 1000
	if v := getEnv("MAX_CONCURRENT_REQUESTS", "1000"); v != "" {
		if n, err := strconv.Atoi(v); err != nil {
			log.Printf("WARNING: invalid MAX_CONCURRENT_REQUESTS value %q, using default 1000", v)
		} else if n > 0 {
			maxConcurrentRequests = n
		} else {
			maxConcurrentRequests = 0 // 0 means disabled
		}
	}

	gcsCallTimeoutSec := 30
	if v := getEnv("GCS_CALL_TIMEOUT_SEC", "30"); v != "" {
		if n, err := strconv.Atoi(v); err != nil {
			log.Printf("WARNING: invalid GCS_CALL_TIMEOUT_SEC value %q, using default 30", v)
		} else if n > 0 {
			gcsCallTimeoutSec = n
		} else {
			gcsCallTimeoutSec = 0 // 0 means disabled
		}
	}

	idleConnTimeoutSec := 120
	if v := getEnv("IDLE_CONN_TIMEOUT_SEC", "120"); v != "" {
		if n, err := strconv.Atoi(v); err != nil {
			log.Printf("WARNING: invalid IDLE_CONN_TIMEOUT_SEC value %q, using default 120", v)
		} else if n > 0 {
			idleConnTimeoutSec = n
		}
	}

	responseHeaderTimeoutSec := 30
	if v := getEnv("RESPONSE_HEADER_TIMEOUT_SEC", "30"); v != "" {
		if n, err := strconv.Atoi(v); err != nil {
			log.Printf("WARNING: invalid RESPONSE_HEADER_TIMEOUT_SEC value %q, using default 30", v)
		} else if n > 0 {
			responseHeaderTimeoutSec = n
		}
	}

	readBufferSize := 65536 // 64KB
	if v := getEnv("READ_BUFFER_SIZE", "65536"); v != "" {
		if n, err := strconv.Atoi(v); err != nil {
			log.Printf("WARNING: invalid READ_BUFFER_SIZE value %q, using default 65536", v)
		} else if n > 0 {
			readBufferSize = n
		}
	}

	writeBufferSize := 65536 // 64KB
	if v := getEnv("WRITE_BUFFER_SIZE", "65536"); v != "" {
		if n, err := strconv.Atoi(v); err != nil {
			log.Printf("WARNING: invalid WRITE_BUFFER_SIZE value %q, using default 65536", v)
		} else if n > 0 {
			writeBufferSize = n
		}
	}

	proxyBufferSize := 32768 // 32KB — matches Go's default io.Copy buffer
	if v := getEnv("PROXY_BUFFER_SIZE", "32768"); v != "" {
		if n, err := strconv.Atoi(v); err != nil {
			log.Printf("WARNING: invalid PROXY_BUFFER_SIZE value %q, using default 32768", v)
		} else if n > 0 {
			proxyBufferSize = n
		}
	}

	flushIntervalMS := -1 // immediate flush by default (best TTFB)
	if v, ok := os.LookupEnv("FLUSH_INTERVAL_MS"); ok && v != "" {
		if n, err := strconv.Atoi(v); err != nil {
			log.Printf("WARNING: invalid FLUSH_INTERVAL_MS value %q, using default -1", v)
		} else {
			flushIntervalMS = n // -1, 0, or positive ms are all valid
		}
	}

	reqLogEnabled := getEnv("REQUEST_LOG_ENABLED", "true") == "true"
	reqLogMaxSizeMB := 512
	if v := getEnv("REQUEST_LOG_MAX_SIZE_MB", "512"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			reqLogMaxSizeMB = n
		}
	}
	reqLogMaxBackup := 5
	if v := getEnv("REQUEST_LOG_MAX_BACKUP", "5"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			reqLogMaxBackup = n
		}
	}
	reqLogChanBuf := 10240
	if v := getEnv("REQUEST_LOG_CHAN_BUF", "10240"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			reqLogChanBuf = n
		}
	}
	reqLogKeepDays := 7
	if v := getEnv("REQUEST_LOG_KEEP_DAYS", "7"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			reqLogKeepDays = n
		}
	}

	errLogEnabled := getEnv("ERROR_LOG_ENABLED", "true") == "true"
	errLogMaxSizeMB := 256
	if v := getEnv("ERROR_LOG_MAX_SIZE_MB", "256"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			errLogMaxSizeMB = n
		}
	}
	errLogMaxBackup := 5
	if v := getEnv("ERROR_LOG_MAX_BACKUP", "5"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			errLogMaxBackup = n
		}
	}
	errLogChanBuf := 4096
	if v := getEnv("ERROR_LOG_CHAN_BUF", "4096"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			errLogChanBuf = n
		}
	}
	errLogKeepDays := 14
	if v := getEnv("ERROR_LOG_KEEP_DAYS", "14"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			errLogKeepDays = n
		}
	}

	features := FeatureFlags{
		Lifecycle:       getEnvBool("ENABLE_LIFECYCLE", true),
		CORS:            getEnvBool("ENABLE_CORS", true),
		Logging:         getEnvBool("ENABLE_LOGGING", true),
		Website:         getEnvBool("ENABLE_WEBSITE", true),
		Tagging:         getEnvBool("ENABLE_TAGGING", true),
		RestoreObject:   getEnvBool("ENABLE_RESTORE_OBJECT", true),
		CopyObject:      getEnvBool("ENABLE_COPY_OBJECT", true),
		MultipartUpload: getEnvBool("ENABLE_MULTIPART_UPLOAD", true),
		DeleteObjects:   getEnvBool("ENABLE_DELETE_OBJECTS", true),
		HealthEndpoint:  getEnvBool("ENABLE_HEALTH_ENDPOINT", true),
		ReadyzEndpoint:  getEnvBool("ENABLE_READYZ_ENDPOINT", true),
		MetricsEndpoint: getEnvBool("ENABLE_METRICS_ENDPOINT", true),
	}

	proxyAK := getEnv("PROXY_AWS_ACCESS_KEY_ID", getEnv("AWS_ACCESS_KEY_ID", ""))
	proxySK := getEnv("PROXY_AWS_SECRET_ACCESS_KEY", getEnv("AWS_SECRET_ACCESS_KEY", ""))
	credsMap, credsFile, credsSource := loadHMACCredentials(proxyAK, proxySK)
	hmacStrict := getEnvBool("HMAC_STRICT", credsSource != "legacy")

	Config = &Settings{
		Port:                      getEnv("PORT", "8080"),
		GCPProjectID:              getEnv("GCP_PROJECT_ID", ""),
		TargetBucket:              getEnv("TARGET_BUCKET", ""),
		StorageBaseURL:            getEnv("STORAGE_BASE_URL", "https://storage.googleapis.com"),
		GCSPrefix:                 getEnv("GCS_PREFIX", ""),
		DryRun:                    dryRun,
		DebugLogging:              debugLogging,
		MaxIdleConns:              maxIdleConns,
		MaxIdleConnsPerHost:       maxIdleConnsPerHost,
		MaxConcurrentRequests:     maxConcurrentRequests,
		GCSCallTimeout:            time.Duration(gcsCallTimeoutSec) * time.Second,
		IdleConnTimeout:           time.Duration(idleConnTimeoutSec) * time.Second,
		ResponseHeaderTimeout:     time.Duration(responseHeaderTimeoutSec) * time.Second,
		ReadBufferSize:            readBufferSize,
		WriteBufferSize:           writeBufferSize,
		ProxyBufferSize:           proxyBufferSize,
		FlushIntervalMS:           flushIntervalMS,
		ProxyAccessKey:            proxyAK,
		ProxySecretKey:            proxySK,
		HMACCredentials:           credsMap,
		CredentialsFile:           credsFile,
		HMACStrict:                hmacStrict,
		JSONKey:                   getEnv("JSON_KEY", ""),
		ProxyBaseDomain:           getEnv("PROXY_BASE_DOMAIN", ""),
		DisableHeaderStrip:        getEnvBool("DISABLE_HEADER_STRIP", true),
		PPROFAddr:                 getEnv("PPROF_ADDR", ""),
		RestoreSkipExistenceCheck: getEnv("RESTORE_SKIP_EXISTENCE_CHECK", "false") == "true",
		ReqLogEnabled:             reqLogEnabled,
		ReqLogPath:                getEnv("REQUEST_LOG_PATH", "./logs/req_%Y%M%D.csv"),
		ReqLogMaxSizeMB:           reqLogMaxSizeMB,
		ReqLogMaxBackup:           reqLogMaxBackup,
		ReqLogChanBuf:             reqLogChanBuf,
		ReqLogKeepDays:            reqLogKeepDays,
		ErrLogEnabled:             errLogEnabled,
		ErrLogPath:                getEnv("ERROR_LOG_PATH", "./logs/error_%Y%M%D.log"),
		ErrLogMaxSizeMB:           errLogMaxSizeMB,
		ErrLogMaxBackup:           errLogMaxBackup,
		ErrLogChanBuf:             errLogChanBuf,
		ErrLogKeepDays:            errLogKeepDays,
		Features:                  features,
	}

	// Validate required fields for non-DryRun mode
	if !dryRun {
		if Config.TargetBucket == "" {
			slog.Info("TARGET_BUCKET not set; control-plane handlers will parse bucket from request URL (multi-tenant mode). Warmup and /readyz active probing are disabled.")
		} else {
			slog.Info("TARGET_BUCKET configured as warmup / readyz probe hint", "bucket", Config.TargetBucket)
		}
		if Config.GCPProjectID == "" {
			log.Println("WARNING: GCP_PROJECT_ID is empty, some GCS operations may fail")
		}
		if len(credsMap) == 0 && credsFile == "" {
			log.Fatal("FATAL: no HMAC credentials configured; set HMAC_CREDENTIALS, HMAC_CREDENTIALS_FILE, or PROXY_AWS_ACCESS_KEY_ID/PROXY_AWS_SECRET_ACCESS_KEY when DRY_RUN=false")
		}
	}

	logHMACCredentialsSummary(credsSource, credsFile, len(credsMap), hmacStrict)
	logFeatureFlags(features)
}

// loadHMACCredentials resolves the AK→SK mapping using, in order:
//
//  1. HMAC_CREDENTIALS_FILE — path to a JSON map, loaded once at startup
//     and watched for hot reload by main.go (fsnotify in pkg/credstore).
//  2. HMAC_CREDENTIALS       — inline JSON map (legacy / simple deployments).
//  3. PROXY_AWS_ACCESS_KEY_ID + PROXY_AWS_SECRET_ACCESS_KEY or the plain
//     AWS_* variants — wrapped into a single-entry map so clients that
//     still share the one proxy identity keep working unchanged.
//
// The third return value is a source tag used only in the startup log
// ("file" | "inline" | "legacy" | "none") so operators can see where
// their credentials came from.
func loadHMACCredentials(legacyAK, legacySK string) (map[string]string, string, string) {
	if path := os.Getenv("HMAC_CREDENTIALS_FILE"); path != "" {
		m, err := credstore.LoadFile(path)
		if err != nil {
			log.Fatalf("FATAL: HMAC_CREDENTIALS_FILE=%q invalid: %v", path, err)
		}
		return m, path, "file"
	}
	if raw := os.Getenv("HMAC_CREDENTIALS"); raw != "" {
		m, err := credstore.ParseJSON(raw)
		if err != nil {
			log.Fatalf("FATAL: HMAC_CREDENTIALS is not a valid JSON map[AK]SK: %v", err)
		}
		return m, "", "inline"
	}
	if legacyAK != "" && legacySK != "" {
		return map[string]string{legacyAK: legacySK}, "", "legacy"
	}
	return map[string]string{}, "", "none"
}

func logHMACCredentialsSummary(source, path string, count int, strict bool) {
	switch source {
	case "file":
		slog.Info("HMAC credentials loaded from file", "path", path, "count", count, "strict", strict)
	case "inline":
		slog.Info("HMAC credentials loaded from HMAC_CREDENTIALS env var", "count", count, "strict", strict)
	case "legacy":
		slog.Warn("HMAC credentials loaded from legacy PROXY_AWS_ACCESS_KEY_ID/SECRET — single identity for all clients; migrate to HMAC_CREDENTIALS_FILE for per-client mapping",
			"count", count, "strict", strict)
	case "none":
		slog.Warn("HMAC credentials mapping is empty — re-signing will be skipped; acceptable only for DRY_RUN=true tests")
	}
}

// logFeatureFlags emits a single Info summary of every feature flag and an
// extra Warn for each operational endpoint disabled, since those break K8s
// probes and Prometheus scraping and are the most common mis-configuration.
func logFeatureFlags(f FeatureFlags) {
	slog.Info("Feature flags",
		"lifecycle", f.Lifecycle,
		"cors", f.CORS,
		"logging", f.Logging,
		"website", f.Website,
		"tagging", f.Tagging,
		"restore_object", f.RestoreObject,
		"copy_object", f.CopyObject,
		"multipart_upload", f.MultipartUpload,
		"delete_objects", f.DeleteObjects,
		"health_endpoint", f.HealthEndpoint,
		"readyz_endpoint", f.ReadyzEndpoint,
		"metrics_endpoint", f.MetricsEndpoint,
	)
	if !f.HealthEndpoint {
		slog.Warn("/health endpoint DISABLED — Kubernetes liveness probes will fail")
	}
	if !f.ReadyzEndpoint {
		slog.Warn("/readyz endpoint DISABLED — Kubernetes readiness probes will fail")
	}
	if !f.MetricsEndpoint {
		slog.Warn("/metrics endpoint DISABLED — Prometheus scraping will fail")
	}
}

// getEnvBool parses boolean env vars with a configurable default. Only the
// literal string "false" (case-insensitive) flips a default-true flag off;
// any other non-empty value keeps it true to protect against typos like
// "0" / "no" / " false " that would silently disable critical features.
// For default-false flags the inverse applies.
func getEnvBool(key string, fallback bool) bool {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	switch raw {
	case "true", "TRUE", "True", "1":
		return true
	case "false", "FALSE", "False", "0":
		return false
	default:
		log.Printf("WARNING: invalid boolean for %s=%q, using default %v", key, raw, fallback)
		return fallback
	}
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}
