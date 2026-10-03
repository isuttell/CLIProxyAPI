package auth

import (
	"context"
	"sync"
)

type streamProducerTracker struct {
	mu     sync.Mutex
	active int
	sealed bool
	done   chan struct{}
}

func (t *streamProducerTracker) begin() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sealed {
		return false
	}
	if t.active == 0 {
		t.done = make(chan struct{})
	}
	t.active++
	return true
}

func (t *streamProducerTracker) fork() {
	t.mu.Lock()
	defer t.mu.Unlock()
	// A child drain starts while its tracked parent is still active.
	if t.active > 0 {
		t.active++
	}
}

func (t *streamProducerTracker) end() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active == 0 {
		return
	}
	t.active--
	if t.active == 0 {
		close(t.done)
	}
}

// SealAndWaitStreamProducers joins streaming forwarders and detached drains after request handlers stop.
func (m *Manager) SealAndWaitStreamProducers(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	t := &m.streamProducers
	t.mu.Lock()
	t.sealed = true
	if t.active == 0 {
		t.mu.Unlock()
		return nil
	}
	done := t.done
	t.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
