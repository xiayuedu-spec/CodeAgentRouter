package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"codeagentrouter/internal/model"
	"codeagentrouter/internal/router"
)

const (
	StateVersion = 1
	saveDelay    = 2 * time.Second
)

var (
	ErrDailyQuota         = errors.New("daily quota exceeded")
	ErrHourlyQuota        = errors.New("hourly quota exhausted")
	ErrUserNotFound       = errors.New("user not found")
	ErrUsernameTaken      = errors.New("username already exists")
	ErrAccessKeyNotFound  = errors.New("access key not found")
	ErrUpstreamKeyMissing = errors.New("upstream key not found")
)

// State is the persisted in-memory model. Counter values are recorded for
// reporting; in-flight counters are runtime-only.
type State struct {
	Version      int                            `json:"version"`
	Users        map[string]*model.User         `json:"users"`
	AccessKeys   map[string]string              `json:"access_keys"`
	UpstreamKeys map[string]*model.UpstreamKey  `json:"upstream_keys"`
	Counters     map[string]model.UserCounters  `json:"counters"`
	KeyHours     map[string]model.WindowCounter `json:"key_hours"`
	InFlight     map[string]int                 `json:"-"`
}

func freshState() *State {
	return &State{
		Version:      StateVersion,
		Users:        map[string]*model.User{},
		AccessKeys:   map[string]string{},
		UpstreamKeys: map[string]*model.UpstreamKey{},
		Counters:     map[string]model.UserCounters{},
		KeyHours:     map[string]model.WindowCounter{},
		InFlight:     map[string]int{},
	}
}

// Store guards all mutable state with a single mutex. Counter updates are
// short, so the global lock keeps the user -> key lock ordering from the
// design document trivially safe while upstream calls run outside it.
type Store struct {
	mu         sync.RWMutex
	state      *State
	dataPath   string
	encryptKey []byte
	dirty      bool
	timer      *time.Timer
	logf       func(format string, args ...any)
}

func New(dataPath, encryptKey string, logf func(string, ...any)) (*Store, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	sum := sha256.Sum256([]byte(encryptKey))
	s := &Store{dataPath: dataPath, encryptKey: sum[:], logf: logf}
	state, err := s.load()
	if err != nil {
		return nil, err
	}
	if state == nil {
		state = freshState()
	}
	if state.Users == nil {
		state.Users = map[string]*model.User{}
	}
	if state.AccessKeys == nil {
		state.AccessKeys = map[string]string{}
	}
	if state.UpstreamKeys == nil {
		state.UpstreamKeys = map[string]*model.UpstreamKey{}
	}
	if state.Counters == nil {
		state.Counters = map[string]model.UserCounters{}
	}
	if state.KeyHours == nil {
		state.KeyHours = map[string]model.WindowCounter{}
	}
	if state.InFlight == nil {
		state.InFlight = map[string]int{}
	}
	s.state = state
	return s, nil
}

func (s *Store) load() (*State, error) {
	data, err := os.ReadFile(s.dataPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	state.InFlight = map[string]int{}
	return &state, nil
}

// Flush persists state synchronously. It is called on shutdown and by the
// debounce timer.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	if !s.dirty {
		return nil
	}
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.dataPath), 0o755); err != nil {
		return err
	}
	tmp := s.dataPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.dataPath); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

func (s *Store) SaveSoon() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scheduleSaveLocked()
}

func (s *Store) scheduleSaveLocked() {
	s.dirty = true
	if s.timer != nil {
		s.timer.Reset(saveDelay)
	} else {
		s.timer = time.AfterFunc(saveDelay, func() {
			if err := s.Flush(); err != nil {
				s.logf("state flush: %v", err)
			}
		})
	}
}

func (s *Store) markDirtyLocked() {
	s.dirty = true
	s.scheduleSaveLocked()
}

func (s *Store) Encrypt(plain string) ([]byte, error) {
	block, err := aes.NewCipher(s.encryptKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(plain), nil), nil
}

func (s *Store) Decrypt(data []byte) (string, error) {
	block, err := aes.NewCipher(s.encryptKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(data) < gcm.NonceSize() {
		return "", errors.New("invalid ciphertext")
	}
	nonce, ct := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt upstream key: %w", err)
	}
	return string(plain), nil
}

// ---- users ----

func (s *Store) CreateUser(username, displayName, passwordHash string, enabled bool) (*model.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.state.Users {
		if strings.EqualFold(u.Username, username) {
			return nil, ErrUsernameTaken
		}
	}
	u := &model.User{
		ID:           randomID("u_"),
		Username:     username,
		DisplayName:  displayName,
		PasswordHash: passwordHash,
		CreatedAt:    time.Now(),
		Enabled:      enabled,
	}
	s.state.Users[u.ID] = u
	s.markDirtyLocked()
	return u, nil
}

func (s *Store) GetUser(id string) *model.User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u := s.state.Users[id]
	if u == nil {
		return nil
	}
	cp := *u
	return &cp
}

func (s *Store) GetUserByUsername(username string) *model.User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.state.Users {
		if strings.EqualFold(u.Username, username) {
			cp := *u
			return &cp
		}
	}
	return nil
}

func (s *Store) ListUsers() []*model.User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.User, 0, len(s.state.Users))
	for _, u := range s.state.Users {
		cp := *u
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (s *Store) UpdateUserQuota(id string, hourly, daily int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.state.Users[id]
	if u == nil {
		return ErrUserNotFound
	}
	u.HourlyTokens = hourly
	u.DailyTokens = daily
	s.markDirtyLocked()
	return nil
}

func (s *Store) UpdateUserPassword(id, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.state.Users[id]
	if u == nil {
		return ErrUserNotFound
	}
	u.PasswordHash = hash
	s.markDirtyLocked()
	return nil
}

func (s *Store) SetUserEnabled(id string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.state.Users[id]
	if u == nil {
		return ErrUserNotFound
	}
	u.Enabled = enabled
	s.markDirtyLocked()
	return nil
}

func (s *Store) DeleteUser(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Users[id] == nil {
		return ErrUserNotFound
	}
	delete(s.state.Users, id)
	for k, uid := range s.state.AccessKeys {
		if uid == id {
			delete(s.state.AccessKeys, k)
		}
	}
	for kid, k := range s.state.UpstreamKeys {
		if k.UserID == id {
			delete(s.state.UpstreamKeys, kid)
			delete(s.state.KeyHours, kid)
			delete(s.state.InFlight, kid)
		}
	}
	delete(s.state.Counters, id)
	s.markDirtyLocked()
	return nil
}

// ---- access keys ----

func (s *Store) AddAccessKey(userID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Users[userID] == nil {
		return "", ErrUserNotFound
	}
	key := "sk-relay-" + randomHex(18)
	s.state.AccessKeys[key] = userID
	s.markDirtyLocked()
	return key, nil
}

func (s *Store) RemoveAccessKey(userID, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.AccessKeys[key] != userID {
		return ErrAccessKeyNotFound
	}
	delete(s.state.AccessKeys, key)
	s.markDirtyLocked()
	return nil
}

func (s *Store) UserByAccessKey(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	uid, ok := s.state.AccessKeys[key]
	return uid, ok
}

func (s *Store) ListAccessKeys(userID string) []model.AccessKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []model.AccessKey
	for k, uid := range s.state.AccessKeys {
		if uid == userID {
			out = append(out, model.AccessKey{Key: k, UserID: uid})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// ---- upstream keys ----

func (s *Store) CreateUpstreamKey(userID, name, baseURL, apiKey string, models []string, enabled bool, hourlyLimit int64) (*model.UpstreamKey, error) {
	enc, err := s.Encrypt(apiKey)
	if err != nil {
		return nil, err
	}
	k := &model.UpstreamKey{
		ID:          randomID("k_"),
		UserID:      userID,
		Name:        name,
		BaseURL:     baseURL,
		APIKeyEnc:   enc,
		Models:      normalizeModels(models),
		Enabled:     enabled,
		CreatedAt:   time.Now(),
		HourlyLimit: hourlyLimit,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Users[userID] == nil {
		return nil, ErrUserNotFound
	}
	s.state.UpstreamKeys[k.ID] = k
	s.markDirtyLocked()
	return k, nil
}

func (s *Store) GetUpstreamKey(id string) *model.UpstreamKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k := s.state.UpstreamKeys[id]
	if k == nil {
		return nil
	}
	cp := *k
	cp.APIKeyEnc = append([]byte(nil), k.APIKeyEnc...)
	cp.Models = append([]string(nil), k.Models...)
	return &cp
}

func (s *Store) ListUpstreamKeys(userID string) []*model.UpstreamKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*model.UpstreamKey
	for _, k := range s.state.UpstreamKeys {
		if k.UserID == userID {
			cp := *k
			cp.APIKeyEnc = append([]byte(nil), k.APIKeyEnc...)
			cp.Models = append([]string(nil), k.Models...)
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (s *Store) AllUpstreamKeys() []*model.UpstreamKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.UpstreamKey, 0, len(s.state.UpstreamKeys))
	for _, k := range s.state.UpstreamKeys {
		cp := *k
		cp.APIKeyEnc = append([]byte(nil), k.APIKeyEnc...)
		cp.Models = append([]string(nil), k.Models...)
		out = append(out, &cp)
	}
	return out
}

func (s *Store) ReplaceUpstreamKey(k *model.UpstreamKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.UpstreamKeys[k.ID] == nil {
		return ErrUpstreamKeyMissing
	}
	old := s.state.UpstreamKeys[k.ID]
	k.UserID = old.UserID
	k.CreatedAt = old.CreatedAt
	k.Models = normalizeModels(k.Models)
	s.state.UpstreamKeys[k.ID] = k
	s.markDirtyLocked()
	return nil
}

func (s *Store) DeleteUpstreamKey(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.UpstreamKeys[id] == nil {
		return ErrUpstreamKeyMissing
	}
	delete(s.state.UpstreamKeys, id)
	delete(s.state.KeyHours, id)
	delete(s.state.InFlight, id)
	s.markDirtyLocked()
	return nil
}

func (s *Store) DecryptAPIKey(k *model.UpstreamKey) (string, error) {
	return s.Decrypt(k.APIKeyEnc)
}

func normalizeModels(models []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range models {
		m = strings.ToLower(strings.TrimSpace(m))
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// ---- counters and routing ----

func (s *Store) UserCounters(userID string) model.UserCounters {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.countersLocked(userID)
}

func (s *Store) countersLocked(userID string) model.UserCounters {
	now := time.Now()
	hc := s.state.Counters[userID]
	hc.Hour = ensureWindow(hc.Hour, hourStart(now), now)
	hc.Day = ensureWindow(hc.Day, dayStart(now), now)
	s.state.Counters[userID] = hc
	return hc
}

func (s *Store) KeyHourCounter(keyID string) model.WindowCounter {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keyHourLocked(keyID)
}

func (s *Store) keyHourLocked(keyID string) model.WindowCounter {
	now := time.Now()
	c := s.state.KeyHours[keyID]
	c = ensureWindow(c, hourStart(now), now)
	s.state.KeyHours[keyID] = c
	return c
}

func (s *Store) InFlight(keyID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.InFlight[keyID]
}

// ReserveUser checks the daily/hourly gates and atomically reserves the
// estimate. Hourly usage is recorded at all times but only enforced during
// working hours.
func (s *Store) ReserveUser(userID string, est, hourlyLimit, dailyLimit int64, working bool, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.state.Users[userID]
	if u == nil || !u.Enabled {
		return ErrUserNotFound
	}
	hc := s.state.Counters[userID]
	hc.Hour = ensureWindow(hc.Hour, hourStart(now), now)
	hc.Day = ensureWindow(hc.Day, dayStart(now), now)
	if dailyLimit > 0 && hc.Day.Value+est > dailyLimit {
		return ErrDailyQuota
	}
	if working && hourlyLimit > 0 && hc.Hour.Value+est > hourlyLimit {
		return ErrHourlyQuota
	}
	hc.Hour.Value += est
	hc.Day.Value += est
	s.state.Counters[userID] = hc
	s.markDirtyLocked()
	return nil
}

func (s *Store) ReleaseUser(userID string, est int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hc := s.countersLocked(userID)
	hc.Hour.Value = max64(0, hc.Hour.Value-est)
	hc.Day.Value = max64(0, hc.Day.Value-est)
	s.state.Counters[userID] = hc
	s.markDirtyLocked()
}

// SettleUser adjusts the reservation by (actual - estimate); the result is
// clamped at zero to keep counters monotonic.
func (s *Store) SettleUser(userID string, est, actual int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hc := s.countersLocked(userID)
	delta := actual - est
	hc.Hour.Value = max64(0, hc.Hour.Value+delta)
	hc.Day.Value = max64(0, hc.Day.Value+delta)
	s.state.Counters[userID] = hc
	s.markDirtyLocked()
}

func (s *Store) ReserveKey(keyID string, est, limit int64, working bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.state.UpstreamKeys[keyID]
	if k == nil || !k.Enabled {
		return ErrUpstreamKeyMissing
	}
	c := s.keyHourLocked(keyID)
	if working && limit > 0 && c.Value+est > limit {
		return ErrHourlyQuota
	}
	c.Value += est
	s.state.KeyHours[keyID] = c
	s.state.InFlight[keyID]++
	s.markDirtyLocked()
	return nil
}

func (s *Store) ReleaseKey(keyID string, est int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.keyHourLocked(keyID)
	c.Value = max64(0, c.Value-est)
	s.state.KeyHours[keyID] = c
	if s.state.InFlight[keyID] > 0 {
		s.state.InFlight[keyID]--
	}
	s.markDirtyLocked()
}

func (s *Store) SettleKey(keyID string, est, actual int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.keyHourLocked(keyID)
	c.Value = max64(0, c.Value+actual-est)
	s.state.KeyHours[keyID] = c
	if s.state.InFlight[keyID] > 0 {
		s.state.InFlight[keyID]--
	}
	s.markDirtyLocked()
}

// SelectKey routes the request under the store lock so the chosen key is
// reserved immediately afterwards without racing other requests.
func (s *Store) SelectKey(userID, modelName string, working bool, defaultHourly int64, exclude map[string]bool) (*model.UpstreamKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]*model.UpstreamKey, 0, len(s.state.UpstreamKeys))
	for _, k := range s.state.UpstreamKeys {
		keys = append(keys, k)
	}
	now := time.Now()
	return router.Select(
		keys,
		userID, modelName, working, defaultHourly, exclude,
		func(id string) int64 {
			c := s.state.KeyHours[id]
			return windowValue(c, now)
		},
		func(id string) int { return s.state.InFlight[id] },
	)
}

func hourStart(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, t.Hour(), 0, 0, 0, t.Location())
}

func dayStart(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

func ensureWindow(c model.WindowCounter, start time.Time, now time.Time) model.WindowCounter {
	if !c.WindowStart.Equal(start) || c.WindowStart.IsZero() {
		c.WindowStart = start
		c.Value = 0
	}
	return c
}

func windowValue(c model.WindowCounter, now time.Time) int64 {
	if !c.WindowStart.IsZero() && !c.WindowStart.Equal(hourStart(now)) {
		return 0
	}
	return c.Value
}

func randomID(prefix string) string {
	return prefix + randomHex(12)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
