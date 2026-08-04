package config

import "testing"

func TestParseDocumentedConfig(t *testing.T) {
	data := []byte(`
server:
  addr: "0.0.0.0:9090"
  timezone: "Asia/Shanghai"
  working_hours:
    - {start: 9, end: 12}
    - {start: 14, end: 18}

quota:
  default_hourly_tokens: 5000000
  default_daily_tokens: 100000000
  per_minute_requests: 5

security:
  encrypt_key_env: "RELAY_ENCRYPT_KEY"
  encrypt_key: "dev-key"
  admin_username: "root"
  admin_password_env: "RELAY_ADMIN_PASSWORD"
  admin_password: "secret"

logging:
  dir: "logs"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != "0.0.0.0:9090" {
		t.Fatalf("addr = %q", cfg.Server.Addr)
	}
	if len(cfg.Server.WorkingHours) != 2 || cfg.Server.WorkingHours[1].End != 18 {
		t.Fatalf("working hours = %+v", cfg.Server.WorkingHours)
	}
	if cfg.Quota.DefaultHourlyTokens != 5_000_000 || cfg.Quota.PerMinuteRequests != 5 {
		t.Fatalf("quota = %+v", cfg.Quota)
	}
	if cfg.Security.AdminUsername != "root" || cfg.Security.AdminPassword != "secret" {
		t.Fatalf("security = %+v", cfg.Security)
	}
}
