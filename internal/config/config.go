package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config mirrors the documented config.yaml schema.
type Config struct {
	Server   ServerConfig
	Quota    QuotaConfig
	Security SecurityConfig
	Logging  LoggingConfig
}

type ServerConfig struct {
	Addr         string
	Timezone     string
	WorkingHours []WorkingHour
}

type WorkingHour struct {
	Start int
	End   int
}

type QuotaConfig struct {
	DefaultHourlyTokens int64
	DefaultDailyTokens  int64
	PerMinuteRequests   int
}

type SecurityConfig struct {
	EncryptKeyEnv    string
	EncryptKey       string
	AdminUsername    string
	AdminPasswordEnv string
	AdminPassword    string
}

type LoggingConfig struct {
	Dir string
}

func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Addr:         "0.0.0.0:8080",
			Timezone:     "Asia/Shanghai",
			WorkingHours: []WorkingHour{{Start: 9, End: 12}, {Start: 14, End: 18}},
		},
		Quota: QuotaConfig{
			DefaultHourlyTokens: 10_000_000,
			DefaultDailyTokens:  400_000_000,
			PerMinuteRequests:   10,
		},
		Security: SecurityConfig{
			EncryptKeyEnv:    "RELAY_ENCRYPT_KEY",
			AdminUsername:    "admin",
			AdminPasswordEnv: "RELAY_ADMIN_PASSWORD",
			AdminPassword:    "admin123",
		},
		Logging: LoggingConfig{Dir: "logs"},
	}
}

// Load reads a config.yaml; a missing file falls back to defaults.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Default(), nil
		}
		return nil, err
	}
	return Parse(data)
}

// Parse parses the YAML subset used by the documented config file.
func Parse(data []byte) (*Config, error) {
	root, err := parseYAML(data)
	if err != nil {
		return nil, err
	}
	cfg := Default()
	apply(root, cfg)
	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func apply(root *yNode, cfg *Config) {
	if n := root.get("server", "addr"); n != nil {
		cfg.Server.Addr = asString(n, cfg.Server.Addr)
	}
	if n := root.get("server", "timezone"); n != nil {
		cfg.Server.Timezone = asString(n, cfg.Server.Timezone)
	}
	if n := root.get("server", "working_hours"); n != nil && n.kind == yList {
		var hours []WorkingHour
		for _, item := range n.l {
			if item.kind != yMap {
				continue
			}
			hours = append(hours, WorkingHour{
				Start: asInt(item.get("start"), 0),
				End:   asInt(item.get("end"), 0),
			})
		}
		if len(hours) > 0 {
			cfg.Server.WorkingHours = hours
		}
	}
	if n := root.get("quota", "default_hourly_tokens"); n != nil {
		cfg.Quota.DefaultHourlyTokens = asInt64(n, cfg.Quota.DefaultHourlyTokens)
	}
	if n := root.get("quota", "default_daily_tokens"); n != nil {
		cfg.Quota.DefaultDailyTokens = asInt64(n, cfg.Quota.DefaultDailyTokens)
	}
	if n := root.get("quota", "per_minute_requests"); n != nil {
		cfg.Quota.PerMinuteRequests = asInt(n, cfg.Quota.PerMinuteRequests)
	}
	if n := root.get("security", "encrypt_key_env"); n != nil {
		cfg.Security.EncryptKeyEnv = asString(n, cfg.Security.EncryptKeyEnv)
	}
	if n := root.get("security", "encrypt_key"); n != nil {
		cfg.Security.EncryptKey = asString(n, cfg.Security.EncryptKey)
	}
	if n := root.get("security", "admin_username"); n != nil {
		cfg.Security.AdminUsername = asString(n, cfg.Security.AdminUsername)
	}
	if n := root.get("security", "admin_password_env"); n != nil {
		cfg.Security.AdminPasswordEnv = asString(n, cfg.Security.AdminPasswordEnv)
	}
	if n := root.get("security", "admin_password"); n != nil {
		cfg.Security.AdminPassword = asString(n, cfg.Security.AdminPassword)
	}
	if n := root.get("logging", "dir"); n != nil {
		cfg.Logging.Dir = asString(n, cfg.Logging.Dir)
	}
}

func validate(cfg *Config) error {
	if strings.TrimSpace(cfg.Server.Addr) == "" {
		return errors.New("server.addr must not be empty")
	}
	for i, wh := range cfg.Server.WorkingHours {
		if wh.Start < 0 || wh.End > 24 || wh.Start >= wh.End {
			return fmt.Errorf("server.working_hours[%d]: invalid range %d-%d", i, wh.Start, wh.End)
		}
	}
	if cfg.Quota.DefaultHourlyTokens <= 0 || cfg.Quota.DefaultDailyTokens <= 0 {
		return errors.New("quota limits must be positive")
	}
	if cfg.Quota.PerMinuteRequests <= 0 {
		return errors.New("quota.per_minute_requests must be positive")
	}
	if strings.TrimSpace(cfg.Security.AdminUsername) == "" {
		return errors.New("security.admin_username must not be empty")
	}
	return nil
}

func asString(n *yNode, fallback string) string {
	if n == nil || n.kind != yScalar {
		return fallback
	}
	return n.s
}

func asInt(n *yNode, fallback int) int {
	if n == nil || n.kind != yScalar {
		return fallback
	}
	v, err := strconv.Atoi(n.s)
	if err != nil {
		return fallback
	}
	return v
}

func asInt64(n *yNode, fallback int64) int64 {
	if n == nil || n.kind != yScalar {
		return fallback
	}
	v, err := strconv.ParseInt(n.s, 10, 64)
	if err != nil {
		return fallback
	}
	return v
}
