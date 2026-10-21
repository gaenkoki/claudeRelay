package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadValidatesUpstreamProxy(t *testing.T) {
	t.Setenv("CLAUDE_RELAY_API_KEY", "")
	t.Setenv("CLAUDE_RELAY_ADMIN_API_KEY", "")
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"listen":"127.0.0.1:8317","relay_api_key":"relay-key","admin_api_key":"admin-key","credentials_file":"credentials.json","upstream_base_url":"https://api.anthropic.com","upstream_proxy":"127.0.0.1:7890"}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load() accepted a proxy URL without a scheme")
	}
}

func TestAutoRefreshDefaultsOnAndCanBeDisabled(t *testing.T) {
	t.Setenv("CLAUDE_RELAY_AUTO_REFRESH_ENABLED", "")
	path := filepath.Join(t.TempDir(), "config.json")
	base := `{"listen":"127.0.0.1:8567","relay_api_key":"relay-key","admin_api_key":"admin-key","database_file":"relay.db","upstream_base_url":"https://api.anthropic.com"}`
	if err := os.WriteFile(path, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AutoRefresh {
		t.Fatal("automatic refresh did not default to enabled")
	}
	disabled := `{"listen":"127.0.0.1:8567","relay_api_key":"relay-key","admin_api_key":"admin-key","database_file":"relay.db","upstream_base_url":"https://api.anthropic.com","auto_refresh_enabled":false}`
	if err := os.WriteFile(path, []byte(disabled), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AutoRefresh {
		t.Fatal("explicit automatic refresh disable was ignored")
	}
}

func TestContainerEnvironmentOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"listen":"127.0.0.1:8567","relay_api_key":"file-relay-key","admin_api_key":"file-admin-key","database_file":"relay.db","upstream_base_url":"https://api.anthropic.com"}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_RELAY_LISTEN", "0.0.0.0:8567")
	t.Setenv("CLAUDE_RELAY_API_KEY", "environment-key")
	t.Setenv("CLAUDE_RELAY_EXPERIMENTAL_API_KEY", "environment-experimental-key")
	t.Setenv("CLAUDE_RELAY_OFFICIAL_API_KEY", "environment-official-key")
	t.Setenv("CLAUDE_RELAY_ADMIN_API_KEY", "environment-admin-key")
	t.Setenv("CLAUDE_RELAY_AVAILABILITY_API_KEY", "environment-availability-key")
	t.Setenv("CLAUDE_RELAY_DATABASE_FILE", "/data/claude-relay.db")
	t.Setenv("CLAUDE_RELAY_UPSTREAM_PROXY", "http://proxy:7890")
	t.Setenv("CLAUDE_RELAY_MAX_REQUEST_BYTES", "1048576")
	t.Setenv("CLAUDE_RELAY_MAX_INFLIGHT_PER_ACCOUNT", "6")
	t.Setenv("CLAUDE_RELAY_MAX_COUNT_TOKENS_INFLIGHT_PER_ACCOUNT", "24")
	t.Setenv("CLAUDE_RELAY_MAX_ACTIVE_SESSIONS_PER_ACCOUNT", "4")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "0.0.0.0:8567" || cfg.RelayAPIKey != "environment-key" ||
		cfg.ExperimentalAPIKey != "environment-experimental-key" || cfg.OfficialAPIKey != "environment-official-key" || cfg.AdminAPIKey != "environment-admin-key" ||
		cfg.AvailabilityAPIKey != "environment-availability-key" {
		t.Fatalf("server environment overrides = listen %q compatible key %q experimental key %q official key %q admin key %q availability key %q",
			cfg.Listen, cfg.RelayAPIKey, cfg.ExperimentalAPIKey, cfg.OfficialAPIKey, cfg.AdminAPIKey, cfg.AvailabilityAPIKey)
	}
	if cfg.DatabaseFile != "/data/claude-relay.db" || cfg.UpstreamProxy != "http://proxy:7890" {
		t.Fatalf("runtime environment overrides = database %q proxy %q", cfg.DatabaseFile, cfg.UpstreamProxy)
	}
	if cfg.MaxRequestBytes != 1048576 {
		t.Fatalf("max request bytes = %d", cfg.MaxRequestBytes)
	}
	if cfg.MaxInflightPerAccount != 6 {
		t.Fatalf("max inflight per account = %d", cfg.MaxInflightPerAccount)
	}
	if cfg.MaxCountTokensInflightPerAccount != 24 {
		t.Fatalf("max count_tokens inflight per account = %d", cfg.MaxCountTokensInflightPerAccount)
	}
	if cfg.MaxActiveSessionsPerAccount != 4 {
		t.Fatalf("max active sessions per account = %d", cfg.MaxActiveSessionsPerAccount)
	}
}

func TestMaxInflightPerAccountDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"listen":"127.0.0.1:8567","relay_api_key":"relay-key","admin_api_key":"admin-key","database_file":"relay.db","upstream_base_url":"https://api.anthropic.com"}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_RELAY_MAX_INFLIGHT_PER_ACCOUNT", "")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxInflightPerAccount != DefaultMaxInflightPerAccount {
		t.Fatalf("max inflight per account = %d, want %d", cfg.MaxInflightPerAccount, DefaultMaxInflightPerAccount)
	}
}

func TestMaxCountTokensInflightPerAccountDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"listen":"127.0.0.1:8567","relay_api_key":"relay-key","admin_api_key":"admin-key","database_file":"relay.db","upstream_base_url":"https://api.anthropic.com"}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_RELAY_MAX_COUNT_TOKENS_INFLIGHT_PER_ACCOUNT", "")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxCountTokensInflightPerAccount != DefaultMaxCountTokensInflightPerAccount {
		t.Fatalf("max count_tokens inflight per account = %d, want %d", cfg.MaxCountTokensInflightPerAccount, DefaultMaxCountTokensInflightPerAccount)
	}
}

func TestOfficialKeyIsOptionalAndMustBeDistinct(t *testing.T) {
	t.Setenv("CLAUDE_RELAY_OFFICIAL_API_KEY", "")
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"listen":"127.0.0.1:8567","relay_api_key":"relay-key","official_api_key":"relay-key","admin_api_key":"admin-key","database_file":"relay.db","upstream_base_url":"https://api.anthropic.com"}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load() accepted an official key matching the compatible key")
	}
	raw = `{"listen":"127.0.0.1:8567","relay_api_key":"relay-key","admin_api_key":"admin-key","database_file":"relay.db","upstream_base_url":"https://api.anthropic.com"}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load() rejected an omitted official key: %v", err)
	}
}

func TestExperimentalKeyIsOptionalAndMustBeDistinct(t *testing.T) {
	t.Setenv("CLAUDE_RELAY_EXPERIMENTAL_API_KEY", "")
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"listen":"127.0.0.1:8567","relay_api_key":"relay-key","experimental_api_key":"relay-key","admin_api_key":"admin-key","database_file":"relay.db","upstream_base_url":"https://api.anthropic.com"}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load() accepted an experimental key matching the compatible key")
	}
	raw = `{"listen":"127.0.0.1:8567","relay_api_key":"relay-key","admin_api_key":"admin-key","database_file":"relay.db","upstream_base_url":"https://api.anthropic.com"}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load() rejected an omitted experimental key: %v", err)
	}
}

func TestInvalidEnvironmentRequestLimitFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"listen":"127.0.0.1:8567","relay_api_key":"relay-key","admin_api_key":"admin-key","database_file":"relay.db","upstream_base_url":"https://api.anthropic.com"}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_RELAY_MAX_REQUEST_BYTES", "not-a-number")
	if _, err := Load(path); err == nil {
		t.Fatal("Load() accepted an invalid environment request limit")
	}
}

func TestRelayAndAdminKeysMustDiffer(t *testing.T) {
	t.Setenv("CLAUDE_RELAY_API_KEY", "same-key")
	t.Setenv("CLAUDE_RELAY_ADMIN_API_KEY", "same-key")
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"listen":"127.0.0.1:8567","database_file":"relay.db","upstream_base_url":"https://api.anthropic.com"}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load() accepted identical relay and admin API keys")
	}
}

func TestUpstreamCompressionEnvironment(t *testing.T) {
	for _, value := range []string{"true", "false", "invalid"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("CLAUDE_RELAY_DISABLE_UPSTREAM_COMPRESSION", value)
			var cfg Config
			err := applyEnvironment(&cfg)
			if value == "invalid" {
				if err == nil {
					t.Fatal("invalid boolean accepted")
				}
				return
			}
			if err != nil || cfg.DisableUpstreamCompression != (value == "true") {
				t.Fatalf("disabled=%v err=%v", cfg.DisableUpstreamCompression, err)
			}
		})
	}
}
