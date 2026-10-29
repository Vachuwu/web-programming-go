// Package events provides best-effort in-memory notifications for playlist changes.
package events

import "sync"

const subscriberBufferSize = 16

// Event is a lightweight SSE notification. Clients fetch the current playlist
// through REST after receiving an event.
type Event struct {
	Name string
	Data any
}

// Hub fans events out to subscribers of the single server instance. A full
// subscriber buffer means that client is too slow, so it is disconnected
// instead of delaying a REST request.
type Hub struct {
	mu          sync.Mutex
	subscribers map[chan Event]struct{}
	closed      bool
}

func NewHub() *Hub {
	return &Hub{subscribers: make(map[chan Event]struct{})}
}

// Subscribe returns a channel and an idempotent function that releases it.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	channel := make(chan Event, subscriberBufferSize)

	h.mu.Lock()
	if h.closed {
		close(channel)
	} else {
		h.subscribers[channel] = struct{}{}
	}
	h.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			h.remove(channel)
		})
	}
	return channel, unsubscribe
}

// Publish never waits for a subscriber. It must be called only after the
// mutation transaction has committed.
func (h *Hub) Publish(event Event) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		return
	}
	for channel := range h.subscribers {
		select {
		case channel <- event:
		default:
			delete(h.subscribers, channel)
			close(channel)
		}
	}
}

// Close disconnects all current subscribers. It is intended for graceful
// shutdown and is safe to call more than once.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for channel := range h.subscribers {
		close(channel)
	}
	h.subscribers = make(map[chan Event]struct{})
}

func (h *Hub) remove(channel chan Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subscribers[channel]; ok {
		delete(h.subscribers, channel)
		close(channel)
	}
}
