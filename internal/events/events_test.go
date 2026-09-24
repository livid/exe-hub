package events

import (
	"encoding/json"
	"testing"
)

// a post.mark event carries its box and state; every other event has
// no mark field at all
func TestMarkEvent(t *testing.T) {
	b, _ := json.Marshal(Event{Type: "post.mark", ID: "p", Author: "a", Mark: &Mark{Box: 2, Done: true}})
	if got, want := string(b), `{"type":"post.mark","id":"p","author":"a","mark":{"box":2,"done":true}}`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	b, _ = json.Marshal(Event{Type: "post.delete", ID: "p", Author: "a"})
	if got, want := string(b), `{"type":"post.delete","id":"p","author":"a"}`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}
