package quota

import (
	"time"

	"codeagentrouter/internal/config"
	"codeagentrouter/internal/model"
	"codeagentrouter/internal/store"
)

// Engine applies the working-hour rule and drives pre-reserve/settle on the
// store counters.
type Engine struct {
	cfg   *config.Config
	store *store.Store
	loc   *time.Location
}

func New(cfg *config.Config, st *store.Store) *Engine {
	loc, err := time.LoadLocation(cfg.Server.Timezone)
	if err != nil {
		loc = time.Local
	}
	return &Engine{cfg: cfg, store: st, loc: loc}
}

func (e *Engine) Location() *time.Location {
	return e.loc
}

func (e *Engine) WorkingHours() []config.WorkingHour {
	return append([]config.WorkingHour(nil), e.cfg.Server.WorkingHours...)
}

func (e *Engine) IsWorkingHour(t time.Time) bool {
	lt := t.In(e.loc)
	for _, wh := range e.cfg.Server.WorkingHours {
		if lt.Hour() >= wh.Start && lt.Hour() < wh.End {
			return true
		}
	}
	return false
}

func (e *Engine) CheckAndReserve(userID string, est int64, now time.Time) error {
	user := e.store.GetUser(userID)
	if user == nil {
		return store.ErrUserNotFound
	}
	return e.store.ReserveUser(userID, est, e.HourlyLimit(user), e.DailyLimit(user), e.IsWorkingHour(now), now)
}

func (e *Engine) HourlyLimit(u *model.User) int64 {
	if u.HourlyTokens > 0 {
		return u.HourlyTokens
	}
	return e.cfg.Quota.DefaultHourlyTokens
}

func (e *Engine) DailyLimit(u *model.User) int64 {
	if u.DailyTokens > 0 {
		return u.DailyTokens
	}
	return e.cfg.Quota.DefaultDailyTokens
}

func (e *Engine) Release(userID, keyID string, est int64) {
	e.store.ReleaseUser(userID, est)
	if keyID != "" {
		e.store.ReleaseKey(keyID, est)
	}
}

func (e *Engine) Settle(userID, keyID string, est, actual int64) {
	e.store.SettleUser(userID, est, actual)
	if keyID != "" {
		e.store.SettleKey(keyID, est, actual)
	}
}
