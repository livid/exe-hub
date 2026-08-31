// Package events fans post activity out to live subscribers — the SSE
// endpoint's backing bus. Emit never blocks ingest: a subscriber that
// can't keep up has its events dropped (the client refetches on
// reconnect, so a drop costs a refresh, never correctness).
package events

import "sync"

// Event is one feed-visible change. Type is the envelope op that caused
// it; ID is the post id it concerns (for post.delete, the deleted post,
// not the delete message's own id).
type Event struct {
	Type    string `json:"type"`               // "post.create" | "post.delete"
	ID      string `json:"id"`                 // post id
	ReplyTo string `json:"reply_to,omitempty"` // parent post id when the post is a reply
	Author  string `json:"author"`             // author profile id
}

type Broadcaster struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

func New() *Broadcaster {
	return &Broadcaster{subs: make(map[chan Event]struct{})}
}

// Subscribe returns a channel that receives every future event. The
// buffer absorbs bursts; overflow drops (see Emit).
func (b *Broadcaster) Subscribe() chan Event {
	ch := make(chan Event, 32)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *Broadcaster) Unsubscribe(ch chan Event) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
}

func (b *Broadcaster) Emit(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		default: // slow subscriber: drop rather than stall ingest
		}
	}
}
