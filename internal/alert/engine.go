// Package alert turns "what is bad right now" into alert events: a condition
// fires after it held for a number of consecutive evaluations, reminds while
// it lasts and resolves when it is gone.
package alert

import (
	"cmp"
	"maps"
	"slices"
	"sync"
	"time"
)

// Kind of an Event.
type Kind int

const (
	Firing   Kind = iota // the condition just crossed the sustain threshold
	Reminder             // still firing, remind interval elapsed
	Resolved             // the condition is gone
)

func (k Kind) String() string {
	return [...]string{"firing", "reminder", "resolved"}[k]
}

// Event is something worth telling a human.
type Event struct {
	Kind  Kind
	Key   string
	Msg   string        // latest text of the condition
	Since time.Time     // when it started firing
	Age   time.Duration // how long it has been firing (Reminder, Resolved)
}

// Active is a firing alert, for /alerts and status headers.
type Active struct {
	Key   string
	Msg   string
	Since time.Time
}

type firing struct {
	since, notified time.Time
	msg             string
}

// Engine keeps alert state between evaluations. Safe for concurrent use:
// the watch loop updates it while commands read it.
type Engine struct {
	sustain int
	remind  time.Duration

	mu      sync.Mutex
	pending map[string]int // key -> consecutive bad evaluations
	firing  map[string]*firing
}

// New returns an engine that fires after sustain consecutive bad
// evaluations (at least 1) and reminds every remind.
func New(sustain int, remind time.Duration) *Engine {
	return &Engine{
		sustain: max(1, sustain),
		remind:  remind,
		pending: map[string]int{},
		firing:  map[string]*firing{},
	}
}

// Update takes every currently bad condition (key -> message) and returns
// the events, in key order.
func (e *Engine) Update(bad map[string]string) []Event {
	now := time.Now()
	e.mu.Lock()
	defer e.mu.Unlock()

	var out []Event
	for _, key := range slices.Sorted(maps.Keys(bad)) {
		msg := bad[key]
		e.pending[key]++
		if f, ok := e.firing[key]; ok {
			f.msg = msg
			if now.Sub(f.notified) >= e.remind {
				out = append(out, Event{Reminder, key, msg, f.since, now.Sub(f.since)})
				f.notified = now
			}
		} else if e.pending[key] >= e.sustain {
			e.firing[key] = &firing{since: now, notified: now, msg: msg}
			out = append(out, Event{Firing, key, msg, now, 0})
		}
	}
	for key := range e.pending {
		if _, ok := bad[key]; !ok {
			delete(e.pending, key)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(e.firing)) {
		if _, ok := bad[key]; !ok {
			f := e.firing[key]
			delete(e.firing, key)
			out = append(out, Event{Resolved, key, f.msg, f.since, now.Sub(f.since)})
		}
	}
	return out
}

// Active lists the firing alerts, oldest first.
func (e *Engine) Active() []Active {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Active, 0, len(e.firing))
	for k, f := range e.firing {
		out = append(out, Active{k, f.msg, f.since})
	}
	slices.SortFunc(out, func(a, b Active) int {
		if c := a.Since.Compare(b.Since); c != 0 {
			return c
		}
		return cmp.Compare(a.Key, b.Key)
	})
	return out
}
