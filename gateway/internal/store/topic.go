package store

import "sync"

// topic fans hints out to independent subscribers. Slow subscribers may miss
// hints; consumers must reconcile from state (SSE is not a durable log).
// Its own mutex avoids coupling subscription lifetime to the store lock.
type topic[T any] struct {
	mu   sync.Mutex
	subs map[chan T]struct{}
}

func (t *topic[T]) subscribe() (<-chan T, func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.subs == nil {
		t.subs = make(map[chan T]struct{})
	}
	ch := make(chan T, 64)
	t.subs[ch] = struct{}{}
	return ch, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if _, ok := t.subs[ch]; ok {
			delete(t.subs, ch)
			close(ch)
		}
	}
}

func (t *topic[T]) publish(v T) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for ch := range t.subs {
		select {
		case ch <- v:
		default:
		}
	}
}
