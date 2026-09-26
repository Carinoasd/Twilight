package api

import (
	"strings"
	"sync"
)

// The hub is only a local wake-up optimization. PostgreSQL owns every challenge.
type bindStatusHub struct {
	mu       sync.Mutex
	watchers map[string]map[chan struct{}]struct{}
}

func newBindStatusHub() *bindStatusHub {
	return &bindStatusHub{watchers: map[string]map[chan struct{}]struct{}{}}
}
func (h *bindStatusHub) subscribe(code string) (<-chan struct{}, func()) {
	code = normalizeBindStatusCode(code)
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.watchers[code] == nil {
		h.watchers[code] = map[chan struct{}]struct{}{}
	}
	h.watchers[code][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if watchers := h.watchers[code]; watchers != nil {
			delete(watchers, ch)
			if len(watchers) == 0 {
				delete(h.watchers, code)
			}
		}
		h.mu.Unlock()
	}
}

func (h *bindStatusHub) notify(code string) {
	code = normalizeBindStatusCode(code)
	if code == "" {
		return
	}
	h.mu.Lock()
	h.notifyLocked(code)
	h.mu.Unlock()
}

func (h *bindStatusHub) notifyLocked(code string) {
	for ch := range h.watchers[code] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func normalizeBindStatusCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}
