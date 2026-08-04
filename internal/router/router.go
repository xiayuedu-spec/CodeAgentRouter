package router

import (
	"errors"
	"sort"
	"strings"

	"codeagentrouter/internal/model"
)

var (
	// ErrNoKey means no enabled upstream key matches the request.
	ErrNoKey = errors.New("no usable upstream key")
	// ErrHourlyExhausted means matching keys exist but are all at their
	// hourly limit during working hours.
	ErrHourlyExhausted = errors.New("hourly quota exhausted")
)

// Select implements the documented routing rules: own key first, then the
// shared pool, choosing the least-used key and breaking ties by in-flight
// concurrency.
func Select(
	keys []*model.UpstreamKey,
	userID, modelName string,
	working bool,
	defaultHourly int64,
	exclude map[string]bool,
	usage func(keyID string) int64,
	inFlight func(keyID string) int,
) (*model.UpstreamKey, error) {
	modelKey := strings.ToLower(strings.TrimSpace(modelName))
	var own, pool []*model.UpstreamKey
	for _, k := range keys {
		if k == nil || !k.Enabled || exclude[k.ID] {
			continue
		}
		if !servesModel(k, modelKey) {
			continue
		}
		if k.UserID == userID {
			own = append(own, k)
		} else {
			pool = append(pool, k)
		}
	}

	qualified := func(list []*model.UpstreamKey) []*model.UpstreamKey {
		if !working {
			return list
		}
		var out []*model.UpstreamKey
		for _, k := range list {
			if usage(k.ID) < hourlyLimit(k, defaultHourly) {
				out = append(out, k)
			}
		}
		return out
	}

	if own := qualified(own); len(own) > 0 {
		return best(own, usage, inFlight), nil
	}
	if pool := qualified(pool); len(pool) > 0 {
		return best(pool, usage, inFlight), nil
	}
	if len(own) > 0 || len(pool) > 0 {
		return nil, ErrHourlyExhausted
	}
	return nil, ErrNoKey
}

func servesModel(k *model.UpstreamKey, model string) bool {
	if len(k.Models) == 0 {
		return true
	}
	for _, m := range k.Models {
		if strings.ToLower(strings.TrimSpace(m)) == strings.ToLower(strings.TrimSpace(model)) {
			return true
		}
	}
	return false
}

func hourlyLimit(k *model.UpstreamKey, fallback int64) int64 {
	if k.HourlyLimit > 0 {
		return k.HourlyLimit
	}
	return fallback
}

func best(list []*model.UpstreamKey, usage func(string) int64, inFlight func(string) int) *model.UpstreamKey {
	sort.SliceStable(list, func(i, j int) bool {
		ui, uj := usage(list[i].ID), usage(list[j].ID)
		if ui != uj {
			return ui < uj
		}
		fi, fj := inFlight(list[i].ID), inFlight(list[j].ID)
		if fi != fj {
			return fi < fj
		}
		return list[i].ID < list[j].ID
	})
	return list[0]
}
