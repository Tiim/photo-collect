// Package config loads application configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tiim/photo-collect/internal/clientip"
)

type Config struct {
	ListenAddr string
	BaseURL    string // public URL, e.g. https://photos.example.com

	DatabasePath string

	StorageBackend string // "filesystem", "s3" or "webdav"
	StoragePath    string
	S3Endpoint     string
	S3Bucket       string
	S3Region       string
	S3AccessKey    string
	S3SecretKey    string
	S3PathStyle    bool

	WebDAVURL      string
	WebDAVUser     string
	WebDAVPassword string
	WebDAVBasePath string

	OIDCIssuerURL    string
	OIDCClientID     string
	OIDCClientSecret string
	OIDCRedirectURL  string
	// OIDCRequireVerifiedEmail rejects logins whose email_verified claim is false.
	OIDCRequireVerifiedEmail bool

	// TrustedProxies are the peers whose X-Forwarded-For header is believed.
	TrustedProxies []netip.Prefix

	SessionSecret string
	SessionTTL    time.Duration

	UploadMaxFileSize        int64
	UploadMaxImagesPerFolder int
	UploadMaxPixels          int64
	UploadLinkDuration       time.Duration
	// UploadMaxConcurrent caps simultaneous upload requests being ingested.
	UploadMaxConcurrent int
	// UploadChunkSize is the largest piece of a file the upload page sends per request.
	UploadChunkSize int64
	// UploadMaxPending caps chunked uploads that are started but not finished.
	UploadMaxPending int

	// Requests per minute; 0 disables the limit.
	RateUploadPerIP   int
	RateUploadPerLink int
	RateAuthPerIP     int
	RateNicknamePerIP int

	ThumbnailSize int
	PreviewSize   int

	WorkerCount int
	ExportDir   string
	ExportTTL   time.Duration
	// ExportMaxConcurrent is the number of ZIP exports built at the same time.
	ExportMaxConcurrent int
	// ExportMaxBytes refuses exports whose originals add up to more; 0 disables the limit.
	ExportMaxBytes int64
}

// Load reads configuration from the environment and validates it.
func Load() (*Config, error) {
	e := &envReader{}
	c := &Config{
		ListenAddr: e.str("LISTEN_ADDR", ":8080"),
		BaseURL:    strings.TrimRight(e.str("BASE_URL", ""), "/"),

		DatabasePath: e.str("DATABASE_PATH", "/data/photos.db"),

		StorageBackend: e.str("STORAGE_BACKEND", "filesystem"),
		StoragePath:    e.str("STORAGE_PATH", "/data/photos"),
		S3Endpoint:     e.str("S3_ENDPOINT", ""),
		S3Bucket:       e.str("S3_BUCKET", ""),
		S3Region:       e.str("S3_REGION", "us-east-1"),
		S3AccessKey:    e.str("S3_ACCESS_KEY", ""),
		S3SecretKey:    e.str("S3_SECRET_KEY", ""),
		S3PathStyle:    e.boolean("S3_PATH_STYLE", true),
		WebDAVURL:      e.str("WEBDAV_URL", ""),
		WebDAVUser:     e.str("WEBDAV_USER", ""),
		WebDAVPassword: e.str("WEBDAV_PASSWORD", ""),
		WebDAVBasePath: e.str("WEBDAV_BASE_PATH", "/"),

		OIDCIssuerURL:    e.str("OIDC_ISSUER_URL", ""),
		OIDCClientID:     e.str("OIDC_CLIENT_ID", ""),
		OIDCClientSecret: e.str("OIDC_CLIENT_SECRET", ""),
		OIDCRedirectURL:  e.str("OIDC_REDIRECT_URL", ""),

		OIDCRequireVerifiedEmail: e.boolean("OIDC_REQUIRE_VERIFIED_EMAIL", true),

		SessionSecret: e.str("SESSION_SECRET", ""),
		SessionTTL:    e.duration("SESSION_TTL", 30*24*time.Hour),

		UploadMaxFileSize:        e.int64("UPLOAD_MAX_FILE_SIZE", 50<<20),
		UploadMaxImagesPerFolder: int(e.int64("UPLOAD_MAX_IMAGES_PER_FOLDER", 5000)),
		UploadMaxPixels:          e.int64("UPLOAD_MAX_PIXELS", 60_000_000),
		UploadLinkDuration:       e.duration("UPLOAD_LINK_DURATION", 7*24*time.Hour),
		UploadChunkSize:          e.int64("UPLOAD_CHUNK_SIZE", 512<<10),
		UploadMaxPending:         int(e.int64("UPLOAD_MAX_PENDING", 64)),

		RateUploadPerIP:   int(e.int64("RATE_UPLOAD_PER_IP", 100)),
		RateUploadPerLink: int(e.int64("RATE_UPLOAD_PER_LINK", 300)),
		RateAuthPerIP:     int(e.int64("RATE_AUTH_PER_IP", 10)),
		RateNicknamePerIP: int(e.int64("RATE_NICKNAME_PER_IP", 5)),

		ThumbnailSize: int(e.int64("THUMBNAIL_SIZE", 400)),
		PreviewSize:   int(e.int64("PREVIEW_SIZE", 1600)),

		WorkerCount: int(e.int64("WORKER_COUNT", 2)),
		ExportDir:   e.str("EXPORT_DIR", "/data/exports"),
		ExportTTL:   e.duration("EXPORT_TTL", 24*time.Hour),

		ExportMaxConcurrent: int(e.int64("EXPORT_MAX_CONCURRENT", 1)),
		ExportMaxBytes:      e.int64("EXPORT_MAX_BYTES", 20<<30),
	}
	c.UploadMaxConcurrent = int(e.int64("UPLOAD_MAX_CONCURRENT", int64(2*c.WorkerCount)))
	if v := e.str("TRUSTED_PROXIES", ""); v != "" {
		var err error
		if c.TrustedProxies, err = clientip.ParseTrusted(v); err != nil {
			e.errs = append(e.errs, fmt.Errorf("TRUSTED_PROXIES: %w", err))
		}
	}
	if err := c.validate(); err != nil {
		e.errs = append(e.errs, err)
	}
	return c, errors.Join(e.errs...)
}

func (c *Config) validate() error {
	var errs []error
	req := func(name, v string) {
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		}
	}
	req("BASE_URL", c.BaseURL)
	req("OIDC_ISSUER_URL", c.OIDCIssuerURL)
	req("OIDC_CLIENT_ID", c.OIDCClientID)
	req("OIDC_CLIENT_SECRET", c.OIDCClientSecret)
	if c.OIDCRedirectURL == "" && c.BaseURL != "" {
		c.OIDCRedirectURL = c.BaseURL + "/auth/callback"
	}
	if len(c.SessionSecret) < 32 {
		errs = append(errs, errors.New("SESSION_SECRET must be at least 32 characters"))
	}
	switch c.StorageBackend {
	case "filesystem":
		req("STORAGE_PATH", c.StoragePath)
	case "s3":
		req("S3_BUCKET", c.S3Bucket)
	case "webdav":
		req("WEBDAV_URL", c.WebDAVURL)
		req("WEBDAV_USER", c.WebDAVUser)
		req("WEBDAV_PASSWORD", c.WebDAVPassword)
	default:
		errs = append(errs, fmt.Errorf("STORAGE_BACKEND must be \"filesystem\", \"s3\" or \"webdav\", got %q", c.StorageBackend))
	}
	if c.UploadMaxPixels < 1 {
		errs = append(errs, errors.New("UPLOAD_MAX_PIXELS must be >= 1"))
	}
	if c.WorkerCount < 1 {
		errs = append(errs, errors.New("WORKER_COUNT must be >= 1"))
	}
	for name, v := range map[string]int{
		"RATE_UPLOAD_PER_IP": c.RateUploadPerIP, "RATE_UPLOAD_PER_LINK": c.RateUploadPerLink,
		"RATE_AUTH_PER_IP": c.RateAuthPerIP, "RATE_NICKNAME_PER_IP": c.RateNicknamePerIP,
	} {
		if v < 0 {
			errs = append(errs, fmt.Errorf("%s must be >= 0 (0 disables the limit)", name))
		}
	}
	if c.UploadMaxConcurrent < 1 {
		errs = append(errs, errors.New("UPLOAD_MAX_CONCURRENT must be >= 1"))
	}
	if c.UploadChunkSize < 64<<10 {
		errs = append(errs, errors.New("UPLOAD_CHUNK_SIZE must be >= 65536"))
	}
	if c.UploadMaxPending < 1 {
		errs = append(errs, errors.New("UPLOAD_MAX_PENDING must be >= 1"))
	}
	if c.ExportMaxConcurrent < 1 {
		errs = append(errs, errors.New("EXPORT_MAX_CONCURRENT must be >= 1"))
	}
	if c.ExportMaxBytes < 0 {
		errs = append(errs, errors.New("EXPORT_MAX_BYTES must be >= 0 (0 disables the limit)"))
	}
	return errors.Join(errs...)
}

// Warnings returns non-fatal configuration problems worth logging at startup.
func (c *Config) Warnings() []string {
	var w []string
	if strings.HasPrefix(c.BaseURL, "http://") && !loopbackListener(c.ListenAddr) {
		w = append(w, "BASE_URL is http:// but the server listens on a non-loopback address; "+
			"sessions and upload links travel unencrypted unless a TLS proxy terminates in front")
	}
	return w
}

// loopbackListener reports whether addr only accepts local connections.
func loopbackListener(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

type envReader struct{ errs []error }

func (e *envReader) str(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func (e *envReader) int64(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return n
}

func (e *envReader) boolean(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return b
}

func (e *envReader) duration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return d
}
