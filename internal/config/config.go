package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const (
	defaultMaxRequestBytes                  int64 = 32 << 20
	DefaultMaxInflightPerAccount                  = 6
	DefaultMaxCountTokensInflightPerAccount       = 32
	DefaultMaxActiveSessionsPerAccount            = 5
	defaultRequestLogSize                         = 500
	maxRequestLogSize                             = 10000
)

type Config struct {
	DisableUpstreamCompression       bool   `json:"disable_upstream_compression"`
	Listen                           string `json:"listen"`
	RelayAPIKey                      string `json:"relay_api_key"`
	ExperimentalAPIKey               string `json:"experimental_api_key"`
	OfficialAPIKey                   string `json:"official_api_key"`
	AdminAPIKey                      string `json:"admin_api_key"`
	AvailabilityAPIKey               string `json:"availability_api_key"`
	DatabaseFile                     string `json:"database_file"`
	CredentialsFile                  string `json:"credentials_file"`
	UpstreamBaseURL                  string `json:"upstream_base_url"`
	UpstreamProxy                    string `json:"upstream_proxy"`
	MaxRequestBytes                  int64  `json:"max_request_bytes"`
	MaxInflightPerAccount            int    `json:"max_inflight_per_account"`
	MaxCountTokensInflightPerAccount int    `json:"max_count_tokens_inflight_per_account"`
	MaxActiveSessionsPerAccount      int    `json:"max_active_sessions_per_account"`
	AutoRefresh                      bool   `json:"auto_refresh_enabled"`
	RequestLogSize                   int    `json:"request_log_size"`
}

func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	cfg := Config{
		AutoRefresh:                      true,
		MaxInflightPerAccount:            DefaultMaxInflightPerAccount,
		MaxCountTokensInflightPerAccount: DefaultMaxCountTokensInflightPerAccount,
		MaxActiveSessionsPerAccount:      DefaultMaxActiveSessionsPerAccount,
		RequestLogSize:                   defaultRequestLogSize,
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := applyEnvironment(&cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyEnvironment(cfg *Config) error {
	if value := strings.TrimSpace(os.Getenv("CLAUDE_RELAY_DISABLE_UPSTREAM_COMPRESSION")); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("parse CLAUDE_RELAY_DISABLE_UPSTREAM_COMPRESSION: %w", err)
		}
		cfg.DisableUpstreamCompression = parsed
	}
	if value := strings.TrimSpace(os.Getenv("CLAUDE_RELAY_LISTEN")); value != "" {
		cfg.Listen = value
	}
	if value := strings.TrimSpace(os.Getenv("CLAUDE_RELAY_API_KEY")); value != "" {
		cfg.RelayAPIKey = value
	}
	if value := strings.TrimSpace(os.Getenv("CLAUDE_RELAY_EXPERIMENTAL_API_KEY")); value != "" {
		cfg.ExperimentalAPIKey = value
	}
	if value := strings.TrimSpace(os.Getenv("CLAUDE_RELAY_OFFICIAL_API_KEY")); value != "" {
		cfg.OfficialAPIKey = value
	}
	if value := strings.TrimSpace(os.Getenv("CLAUDE_RELAY_ADMIN_API_KEY")); value != "" {
		cfg.AdminAPIKey = value
	}
	if value := strings.TrimSpace(os.Getenv("CLAUDE_RELAY_AVAILABILITY_API_KEY")); value != "" {
		cfg.AvailabilityAPIKey = value
	}
	if value := strings.TrimSpace(os.Getenv("CLAUDE_RELAY_CREDENTIALS_FILE")); value != "" {
		cfg.CredentialsFile = value
	}
	if value := strings.TrimSpace(os.Getenv("CLAUDE_RELAY_DATABASE_FILE")); value != "" {
		cfg.DatabaseFile = value
	}
	if value := strings.TrimSpace(os.Getenv("CLAUDE_RELAY_UPSTREAM_BASE_URL")); value != "" {
		cfg.UpstreamBaseURL = value
	}
	if value := strings.TrimSpace(os.Getenv("CLAUDE_RELAY_UPSTREAM_PROXY")); value != "" {
		cfg.UpstreamProxy = value
	}
	if value := strings.TrimSpace(os.Getenv("CLAUDE_RELAY_MAX_REQUEST_BYTES")); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return fmt.Errorf("parse CLAUDE_RELAY_MAX_REQUEST_BYTES: %w", err)
		}
		cfg.MaxRequestBytes = parsed
	}
	if value := strings.TrimSpace(os.Getenv("CLAUDE_RELAY_MAX_INFLIGHT_PER_ACCOUNT")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("parse CLAUDE_RELAY_MAX_INFLIGHT_PER_ACCOUNT: %w", err)
		}
		cfg.MaxInflightPerAccount = parsed
	}
	if value := strings.TrimSpace(os.Getenv("CLAUDE_RELAY_MAX_COUNT_TOKENS_INFLIGHT_PER_ACCOUNT")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("parse CLAUDE_RELAY_MAX_COUNT_TOKENS_INFLIGHT_PER_ACCOUNT: %w", err)
		}
		cfg.MaxCountTokensInflightPerAccount = parsed
	}
	if value := strings.TrimSpace(os.Getenv("CLAUDE_RELAY_MAX_ACTIVE_SESSIONS_PER_ACCOUNT")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("parse CLAUDE_RELAY_MAX_ACTIVE_SESSIONS_PER_ACCOUNT: %w", err)
		}
		cfg.MaxActiveSessionsPerAccount = parsed
	}
	if value := strings.TrimSpace(os.Getenv("CLAUDE_RELAY_REQUEST_LOG_SIZE")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("parse CLAUDE_RELAY_REQUEST_LOG_SIZE: %w", err)
		}
		cfg.RequestLogSize = parsed
	}
	if value := strings.TrimSpace(os.Getenv("CLAUDE_RELAY_AUTO_REFRESH_ENABLED")); value != "" {
		cfg.AutoRefresh = strings.EqualFold(value, "true") || value == "1"
	}
	return nil
}

func (cfg *Config) validate() error {
	cfg.Listen = strings.TrimSpace(cfg.Listen)
	cfg.RelayAPIKey = strings.TrimSpace(cfg.RelayAPIKey)
	cfg.ExperimentalAPIKey = strings.TrimSpace(cfg.ExperimentalAPIKey)
	cfg.OfficialAPIKey = strings.TrimSpace(cfg.OfficialAPIKey)
	cfg.AdminAPIKey = strings.TrimSpace(cfg.AdminAPIKey)
	cfg.AvailabilityAPIKey = strings.TrimSpace(cfg.AvailabilityAPIKey)
	cfg.DatabaseFile = strings.TrimSpace(cfg.DatabaseFile)
	cfg.CredentialsFile = strings.TrimSpace(cfg.CredentialsFile)
	cfg.UpstreamBaseURL = strings.TrimRight(strings.TrimSpace(cfg.UpstreamBaseURL), "/")
	cfg.UpstreamProxy = strings.TrimSpace(cfg.UpstreamProxy)
	if cfg.MaxRequestBytes == 0 {
		cfg.MaxRequestBytes = defaultMaxRequestBytes
	}
	if cfg.MaxInflightPerAccount == 0 {
		cfg.MaxInflightPerAccount = DefaultMaxInflightPerAccount
	}
	if cfg.MaxCountTokensInflightPerAccount == 0 {
		cfg.MaxCountTokensInflightPerAccount = DefaultMaxCountTokensInflightPerAccount
	}

	if cfg.Listen == "" {
		return fmt.Errorf("config listen is required")
	}
	if cfg.RelayAPIKey == "" {
		return fmt.Errorf("config relay_api_key or CLAUDE_RELAY_API_KEY is required")
	}
	if cfg.AdminAPIKey == "" {
		return fmt.Errorf("config admin_api_key or CLAUDE_RELAY_ADMIN_API_KEY is required")
	}
	if cfg.RelayAPIKey == cfg.AdminAPIKey {
		return fmt.Errorf("relay and admin API keys must be different")
	}
	if cfg.OfficialAPIKey != "" && (cfg.OfficialAPIKey == cfg.RelayAPIKey || cfg.OfficialAPIKey == cfg.AdminAPIKey) {
		return fmt.Errorf("official, compatible, and admin API keys must be different")
	}
	if cfg.ExperimentalAPIKey != "" && (cfg.ExperimentalAPIKey == cfg.RelayAPIKey || cfg.ExperimentalAPIKey == cfg.OfficialAPIKey || cfg.ExperimentalAPIKey == cfg.AdminAPIKey) {
		return fmt.Errorf("experimental, official, compatible, and admin API keys must be different")
	}
	if cfg.AvailabilityAPIKey != "" && (cfg.AvailabilityAPIKey == cfg.RelayAPIKey || cfg.AvailabilityAPIKey == cfg.ExperimentalAPIKey || cfg.AvailabilityAPIKey == cfg.OfficialAPIKey || cfg.AvailabilityAPIKey == cfg.AdminAPIKey) {
		return fmt.Errorf("availability, experimental, official, compatible, and admin API keys must be different")
	}
	if cfg.DatabaseFile == "" {
		cfg.DatabaseFile = "data/claude-relay.db"
	}
	if cfg.MaxRequestBytes < 1 {
		return fmt.Errorf("config max_request_bytes must be positive")
	}
	if cfg.MaxInflightPerAccount < 1 {
		return fmt.Errorf("config max_inflight_per_account must be positive")
	}
	if cfg.MaxCountTokensInflightPerAccount < 1 {
		return fmt.Errorf("config max_count_tokens_inflight_per_account must be positive")
	}
	if cfg.MaxActiveSessionsPerAccount < 1 {
		return fmt.Errorf("config max_active_sessions_per_account must be positive")
	}
	if cfg.RequestLogSize < 0 || cfg.RequestLogSize > maxRequestLogSize {
		return fmt.Errorf("config request_log_size must be between 0 and %d", maxRequestLogSize)
	}
	parsed, err := url.Parse(cfg.UpstreamBaseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("config upstream_base_url must be an absolute HTTP(S) URL")
	}
	if cfg.UpstreamProxy != "" {
		proxyURL, proxyErr := url.Parse(cfg.UpstreamProxy)
		if proxyErr != nil || proxyURL.Host == "" || (proxyURL.Scheme != "http" && proxyURL.Scheme != "https") {
			return fmt.Errorf("config upstream_proxy must be an absolute HTTP(S) URL")
		}
	}
	return nil
}
