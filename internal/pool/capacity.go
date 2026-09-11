package pool

import "time"

// Capacity describes configured admission slots, not a provider quota guarantee.
// Unlimited=true means configured/available slots have no finite upper bound.
type Capacity struct {
	Model           string `json:"model,omitempty"`
	PerAccount      int    `json:"max_in_flight_per_account"`
	HealthyAccounts int    `json:"healthy_accounts"`
	ConfiguredSlots int    `json:"configured_slots"`
	AvailableSlots  int    `json:"available_slots"`
	InFlight        int64  `json:"in_flight"`
	Unlimited       bool   `json:"unlimited"`
}

func (p *Pool) CapacityForModel(model string) Capacity {
	model = normalizeModel(model)
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := Capacity{Model: model, PerAccount: p.maxInFlight, Unlimited: p.maxInFlight <= 0}
	now := time.Now()
	for _, e := range p.byUID {
		used := e.inFlight.Load()
		out.InFlight += used // includes requests still finishing on a cooling account
		if !e.healthyForModel(model, now) {
			continue
		}
		out.HealthyAccounts++
		if !out.Unlimited {
			out.ConfiguredSlots += p.maxInFlight
			remaining := p.maxInFlight - int(used)
			if remaining > 0 {
				out.AvailableSlots += remaining
			}
		}
	}
	return out
}
