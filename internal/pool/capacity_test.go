package pool

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestAtomicReservationsUseAllSlotsWithoutOversubscription(t *testing.T) {
	p := New("")
	p.SetMaxInFlight(3)
	for i := 0; i < 8; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprint(i)})
	}
	var wg sync.WaitGroup
	gate := make(chan struct{})
	reserved := make(chan string, 200)
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			if a := p.PickAndAcquireForModel("model", nil); a != nil {
				reserved <- a.UID
			}
		}()
	}
	close(gate)
	wg.Wait()
	close(reserved)
	if len(reserved) != 24 {
		t.Fatalf("reserved=%d want 24", len(reserved))
	}
	cap := p.CapacityForModel("model")
	if cap.InFlight != 24 || cap.AvailableSlots != 0 {
		t.Fatalf("capacity=%+v", cap)
	}
	for uid := range reserved {
		p.Release(uid)
	}
	if cap := p.CapacityForModel("model"); cap.InFlight != 0 || cap.AvailableSlots != 24 {
		t.Fatalf("released capacity=%+v", cap)
	}
}

func TestCapacityHonorsStickyReservationsAndModelCooldown(t *testing.T) {
	p := New("")
	p.SetMaxInFlight(1)
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	if p.PickAndAcquireByUIDForModel("a", "m") == nil {
		t.Fatal("missing first lease")
	}
	if p.PickAndAcquireByUIDForModel("a", "m") != nil {
		t.Fatal("oversubscribed sticky account")
	}
	p.CooldownModel("b", "m", time.Minute, "test")
	if p.PickAndAcquireForModel("m", nil) != nil {
		t.Fatal("selected full or model-cooled account")
	}
	cap := p.CapacityForModel("m")
	if cap.ConfiguredSlots != 1 || cap.AvailableSlots != 0 || cap.InFlight != 1 {
		t.Fatalf("capacity=%+v", cap)
	}
	if p.PickAndAcquireForModel("other", map[string]bool{"b": true}) != nil {
		t.Fatal("ignored exclusion")
	}
	if p.PickAndAcquireForModel("other", nil) == nil {
		t.Fatal("model cooldown leaked")
	}
	p.Release("a")
	p.Release("b")
}
