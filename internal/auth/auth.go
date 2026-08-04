package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"codeagentrouter/internal/store"
)

var (
	ErrUnauthorized       = errors.New("unauthorized")
	ErrInvalidCredentials = errors.New("invalid username or password")
)

const sessionTTL = 24 * time.Hour

// Session is a web console login session.
type Session struct {
	Role   string
	UserID string
	Expiry time.Time
}

// Manager validates relay access keys and web console sessions.
type Manager struct {
	store     *store.Store
	adminUser string
	adminPass string
	sessions  sync.Map // token -> *Session
}

func New(st *store.Store, adminUser, adminPass string) *Manager {
	return &Manager{store: st, adminUser: adminUser, adminPass: adminPass}
}

func (m *Manager) LoginAdmin(username, password string) (string, error) {
	if subtle.ConstantTimeCompare([]byte(username), []byte(m.adminUser)) != 1 ||
		subtle.ConstantTimeCompare([]byte(password), []byte(m.adminPass)) != 1 {
		return "", ErrInvalidCredentials
	}
	return m.newSession("admin", "")
}

func (m *Manager) LoginUser(username, password string) (string, error) {
	u := m.store.GetUserByUsername(username)
	if u == nil || !u.Enabled || u.PasswordHash == "" {
		return "", ErrInvalidCredentials
	}
	if !VerifyPassword(u.PasswordHash, password) {
		return "", ErrInvalidCredentials
	}
	return m.newSession("user", u.ID)
}

func (m *Manager) Check(token string) (*Session, error) {
	if token == "" {
		return nil, ErrUnauthorized
	}
	if v, ok := m.sessions.Load(token); ok {
		s := v.(*Session)
		if time.Now().Before(s.Expiry) {
			return s, nil
		}
		m.sessions.Delete(token)
	}
	return nil, ErrUnauthorized
}

func (m *Manager) RequireAdmin(token string) error {
	s, err := m.Check(token)
	if err != nil {
		return err
	}
	if s.Role != "admin" {
		return ErrUnauthorized
	}
	return nil
}

func (m *Manager) RequireUser(token string) (*Session, error) {
	s, err := m.Check(token)
	if err != nil {
		return nil, err
	}
	if s.Role != "user" {
		return nil, ErrUnauthorized
	}
	return s, nil
}

func (m *Manager) newSession(role, userID string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	m.sessions.Store(token, &Session{Role: role, UserID: userID, Expiry: time.Now().Add(sessionTTL)})
	return token, nil
}

// HashPassword stores a PBKDF2-HMAC-SHA256 hash with a random salt.
func HashPassword(password string) string {
	salt := randomBytes(16)
	dk := pbkdf2SHA256([]byte(password), salt, 120_000, 32)
	return fmt.Sprintf("pbkdf2$120000$%x$%x", salt, dk)
}

func VerifyPassword(hash, password string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	expect, err := hex.DecodeString(parts[3])
	if err != nil || len(expect) == 0 {
		return false
	}
	got := pbkdf2SHA256([]byte(password), salt, iter, len(expect))
	return subtle.ConstantTimeCompare(got, expect) == 1
}

func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	u := hmacSHA256(password, salt, []byte{0, 0, 0, 1})
	t := append([]byte(nil), u...)
	for i := 1; i < iter; i++ {
		u = hmacSHA256(password, u)
		for j := range t {
			t[j] ^= u[j]
		}
	}
	return t[:keyLen]
}

func hmacSHA256(key []byte, data ...[]byte) []byte {
	h := hmac.New(sha256.New, key)
	for _, d := range data {
		h.Write(d)
	}
	return h.Sum(nil)
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}
