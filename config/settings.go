package config

import (
	"log"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

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
	ProxyAccessKey        string        // For re-signing
	ProxySecretKey        string        // For re-signing
	JSONKey               string        // Path to GCS Service Account JSON key
	ProxyBaseDomain       string        // Base domain for virtual-hosted style support

	// PPROFAddr controls the optional pprof profiling endpoint. Empty (default)
	// keeps it disabled; set e.g. "127.0.0.1:6060" to expose runtime profiles
	// on an independent port. MUST NOT be exposed publicly.
	PPROFAddr string

	// Request data logging (SOH-delimited CSV file via ymlog)
	ReqLogEnabled   bool   // REQUEST_LOG_ENABLED,      default true
	ReqLogPath      string // REQUEST_LOG_PATH,          default "./logs/req_%Y%M%D.csv"
	ReqLogMaxSizeMB int    // REQUEST_LOG_MAX_SIZE_MB,   default 512
	ReqLogMaxBackup int    // REQUEST_LOG_MAX_BACKUP,    default 5
	ReqLogChanBuf   int    // REQUEST_LOG_CHAN_BUF,       default 10240
	ReqLogKeepDays  int    // REQUEST_LOG_KEEP_DAYS,     default 7
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

	Config = &Settings{
		Port:                  getEnv("PORT", "8080"),
		GCPProjectID:          getEnv("GCP_PROJECT_ID", ""),
		TargetBucket:          getEnv("TARGET_BUCKET", ""),
		StorageBaseURL:        getEnv("STORAGE_BASE_URL", "https://storage.googleapis.com"),
		GCSPrefix:             getEnv("GCS_PREFIX", ""),
		DryRun:                dryRun,
		DebugLogging:          debugLogging,
		MaxIdleConns:          maxIdleConns,
		MaxIdleConnsPerHost:   maxIdleConnsPerHost,
		MaxConcurrentRequests: maxConcurrentRequests,
		GCSCallTimeout:        time.Duration(gcsCallTimeoutSec) * time.Second,
		IdleConnTimeout:       time.Duration(idleConnTimeoutSec) * time.Second,
		ResponseHeaderTimeout: time.Duration(responseHeaderTimeoutSec) * time.Second,
		ReadBufferSize:        readBufferSize,
		WriteBufferSize:       writeBufferSize,
		ProxyAccessKey:        getEnv("PROXY_AWS_ACCESS_KEY_ID", getEnv("AWS_ACCESS_KEY_ID", "")),
		ProxySecretKey:        getEnv("PROXY_AWS_SECRET_ACCESS_KEY", getEnv("AWS_SECRET_ACCESS_KEY", "")),
		JSONKey:               getEnv("JSON_KEY", ""),
		ProxyBaseDomain:       getEnv("PROXY_BASE_DOMAIN", ""),
		PPROFAddr:             getEnv("PPROF_ADDR", ""),
		ReqLogEnabled:         reqLogEnabled,
		ReqLogPath:            getEnv("REQUEST_LOG_PATH", "./logs/req_%Y%M%D.csv"),
		ReqLogMaxSizeMB:       reqLogMaxSizeMB,
		ReqLogMaxBackup:       reqLogMaxBackup,
		ReqLogChanBuf:         reqLogChanBuf,
		ReqLogKeepDays:        reqLogKeepDays,
	}

	// Validate required fields for non-DryRun mode
	if !dryRun {
		if Config.TargetBucket == "" {
			log.Fatal("FATAL: TARGET_BUCKET is required when DRY_RUN=false")
		}
		if Config.GCPProjectID == "" {
			log.Println("WARNING: GCP_PROJECT_ID is empty, some GCS operations may fail")
		}
	}
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}
