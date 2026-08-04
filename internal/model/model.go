package model

import "time"

// User is an account that can call the relay and own upstream keys.
type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"password_hash"`
	DisplayName  string    `json:"display_name,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	Enabled      bool      `json:"enabled"`
	HourlyTokens int64     `json:"hourly_tokens,omitempty"`
	DailyTokens  int64     `json:"daily_tokens,omitempty"`
}

// AccessKey is a relay-issued bearer token bound to a user.
type AccessKey struct {
	Key       string    `json:"key"`
	UserID    string    `json:"user_id"`
	CreatedAt time.Time `json:"created_at"`
	Enabled   bool      `json:"enabled"`
}

// UpstreamKey is a real provider API key. The plaintext is encrypted before
// it is persisted.
type UpstreamKey struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	Name        string    `json:"name"`
	BaseURL     string    `json:"base_url"`
	APIKeyEnc   []byte    `json:"api_key_enc"`
	Models      []string  `json:"models,omitempty"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	HourlyLimit int64     `json:"hourly_limit,omitempty"`
}

// WindowCounter is a lazily reset sliding-window counter.
type WindowCounter struct {
	WindowStart time.Time `json:"window_start"`
	Value       int64     `json:"value"`
}

// UserCounters holds the hourly and daily counters for one user.
type UserCounters struct {
	Hour WindowCounter `json:"hour"`
	Day  WindowCounter `json:"day"`
}
