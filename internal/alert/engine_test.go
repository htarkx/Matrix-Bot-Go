package alert

import (
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// summary renders events compactly: "firing:cpu@0s reminder:cpu@6h0m0s".
func summary(evs []Event) string {
	var parts []string
	for _, e := range evs {
		parts = append(parts, fmt.Sprintf("%s:%s@%s", e.Kind, e.Key, e.Age))
	}
	return strings.Join(parts, " ")
}

func TestLifecycle(t *testing.T) {
	// synctest gives the test a fake clock: time.Sleep advances it instantly,
	// so six hours of a 30-second loop run in microseconds.
	synctest.Test(t, func(t *testing.T) {
		e := New(2, 6*time.Hour)
		tick := 30 * time.Second
		step := func(bad map[string]string) string {
			time.Sleep(tick)
			return summary(e.Update(bad))
		}
		cpu := map[string]string{"cpu": "busy"}

		if got := step(cpu); got != "" {
			t.Fatalf("first bad sample must not fire: %q", got)
		}
		if got := step(cpu); got != "firing:cpu@0s" {
			t.Fatalf("second bad sample: %q", got)
		}
		// keeps firing quietly until the reminder is due
		for range int(6*time.Hour/tick) - 1 {
			if got := step(cpu); got != "" {
				t.Fatalf("quiet period: %q", got)
			}
		}
		if got := step(map[string]string{"cpu": "busier"}); got != "reminder:cpu@6h0m0s" {
			t.Fatalf("reminder: %q", got)
		}
		if a := e.Active(); len(a) != 1 || a[0].Msg != "busier" {
			t.Fatalf("active = %+v", a)
		}
		if got := step(nil); got != "resolved:cpu@6h0m30s" {
			t.Fatalf("resolve: %q", got)
		}
		if len(e.Active()) != 0 {
			t.Fatal("still active after resolve")
		}
	})
}

func TestFlapDoesNotFire(t *testing.T) {
	e := New(3, time.Hour)
	for range 10 {
		if evs := e.Update(map[string]string{"x": "bad"}); len(evs) != 0 {
			t.Fatalf("fired: %v", evs)
		}
		e.Update(nil) // a good sample resets the count
		if evs := e.Update(map[string]string{"x": "bad"}); len(evs) != 0 {
			t.Fatalf("fired: %v", evs)
		}
		e.Update(nil)
	}
}

func TestKeyOrderAndResolveMessage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := New(1, time.Hour)
		got := summary(e.Update(map[string]string{"b": "1", "a": "1", "c": "1"}))
		if got != "firing:a@0s firing:b@0s firing:c@0s" {
			t.Fatalf("order: %q", got)
		}
		time.Sleep(time.Minute)
		evs := e.Update(map[string]string{"b": "still"})
		if summary(evs) != "resolved:a@1m0s resolved:c@1m0s" || evs[0].Msg != "1" {
			t.Fatalf("resolve: %+v", evs)
		}
	})
}
