package hostwatch

import "time"

// sample is a value observed at a time.
type sample[T any] struct {
	at time.Time
	v  T
}

// window keeps about an hour of samples to compute rates over any span.
type window[T any] struct {
	keep    time.Duration
	samples []sample[T]
}

func newWindow[T any](keep time.Duration) *window[T] { return &window[T]{keep: keep} }

func (w *window[T]) add(at time.Time, v T) {
	w.samples = append(w.samples, sample[T]{at, v})
	cut := 0
	for cut < len(w.samples) && at.Sub(w.samples[cut].at) > w.keep {
		cut++
	}
	w.samples = w.samples[cut:]
}

// span returns the first and last samples covering at least ~d: first is
// the newest sample that is still old enough (90% of d, to tolerate jitter),
// so the span is d rounded up to the next sample, not down. ok is false
// while the history is shorter.
func (w *window[T]) span(d time.Duration) (first, last sample[T], ok bool) {
	if len(w.samples) == 0 {
		return first, last, false
	}
	last = w.samples[len(w.samples)-1]
	for _, s := range w.samples {
		if last.at.Sub(s.at) < d*9/10 {
			break
		}
		first, ok = s, true
	}
	return first, last, ok
}
