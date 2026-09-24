// Package events fans post activity out to live subscribers — the SSE
// endpoint's backing bus. Emit never blocks ingest: a subscriber that
// can't keep up has its events dropped (the client refetches on
// reconnect, so a drop costs a refresh, never correctness).
package events

import "sync"

// Event is one feed-visible change. Type is the envelope op that caused
// it; ID is the post id it concerns (for post.delete, the deleted post,
// not the delete message's own id; for post.mark, the post whose box
// changed, Mark saying which and how; for profile.set, the message
// itself — Author names the profile that changed). post.card is the one type with
// no envelope behind it: something the hub derived for the post landed —
// its link card, the card's archived copy, or a linked picture (none of
// them signed) — so live pages refetch the post and draw it in.
type Event struct {
	Type    string `json:"type"`               // "post.create" | "post.delete" | "post.mark" | "profile.set" | "post.card" | "post.translation"
	ID      string `json:"id"`                 // post id
	ReplyTo string `json:"reply_to,omitempty"` // parent post id when the post is a reply
	Root    string `json:"root,omitempty"`     // the thread's root, for post.create of a reply, post.delete and post.summary: a thread page filters by it
	Author  string `json:"author"`             // author profile id
	Mark    *Mark  `json:"mark,omitempty"`     // post.mark: the box and its new state
}

// Mark is one to-do box's change: which box, counted as a page reads
// them, and whether it is done now.
type Mark struct {
	Box  int  `json:"box"`
	Done bool `json:"done"`
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
