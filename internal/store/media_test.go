package store

import (
	"errors"
	"testing"
)

func pinRefs(t *testing.T, s *Store, cid string) int {
	t.Helper()
	var refs int
	if err := s.db.QueryRow(`SELECT refs FROM pins WHERE cid=?`, cid).Scan(&refs); err != nil {
		return -1 // gone
	}
	return refs
}

// TestMediaEmbed: a converted video's post repeats the conversion's facts
// or leaves them out; its poster is refcounted with it, released on
// delete, and restored by a rebuild; the feed serves the facts.
func TestMediaEmbed(t *testing.T) {
	s := openTest(t)
	a := newAuthor(t)
	const vid, poster, plain = "bafybeivideo", "bafybeiposter", "bafybeiplain"
	facts := Facts{Poster: poster, Width: 608, Height: 1080, Duration: 6.02, Loop: false}
	if err := s.AddMediaPin(vid, 3_000_000, "video/mp4", facts); err != nil {
		t.Fatal(err)
	}
	if err := s.AddPin(poster, 90_000, "image/jpeg", false); err != nil {
		t.Fatal(err)
	}
	if err := s.AddPin(plain, 1000, "video/mp4", false); err != nil {
		t.Fatal(err)
	}
	embed := func(mut func(m map[string]any)) map[string]any {
		m := map[string]any{"cid": vid, "mime": "video/mp4", "poster": poster, "width": 608, "height": 1080, "duration": 6.02}
		if mut != nil {
			mut(m)
		}
		return map[string]any{"text": "", "embeds": []any{m}}
	}
	for name, mut := range map[string]func(m map[string]any){
		"another poster": func(m map[string]any) { m["poster"] = "bafybeiplain" },
		"wrong shape":    func(m map[string]any) { m["width"], m["height"] = 1080, 608 },
		"wrong length":   func(m map[string]any) { m["duration"] = 60.0 },
		"a loop":         func(m map[string]any) { m["loop"] = true },
	} {
		raw, sig, e, op := a.msg(t, "post.create", embed(mut))
		if _, _, err := s.Ingest(raw, sig, e, op); !errors.Is(err, ErrFacts) {
			t.Errorf("%s: %v, want ErrFacts", name, err)
		}
		a.seq-- // the rejected message never used its seq
	}
	raw, sig, e, op := a.msg(t, "post.create", map[string]any{"embeds": []any{map[string]any{"cid": plain, "mime": "video/mp4", "poster": "bafybeimissing"}}})
	if _, _, err := s.Ingest(raw, sig, e, op); !errors.Is(err, ErrNoPin) {
		t.Errorf("a poster never uploaded: %v, want ErrNoPin", err)
	}
	a.seq--

	id := ingest(t, s, a, "post.create", embed(nil))
	bare := ingest(t, s, a, "post.create", map[string]any{"embeds": []any{map[string]any{"cid": plain, "mime": "video/mp4", "width": 320, "height": 240}}})
	if pinRefs(t, s, vid) != 1 || pinRefs(t, s, poster) != 1 {
		t.Fatalf("after posting: refs video %d poster %d", pinRefs(t, s, vid), pinRefs(t, s, poster))
	}
	p, err := s.Post(id)
	if err != nil || len(p.Embeds) != 1 {
		t.Fatalf("post: %+v %v", p, err)
	}
	if em := p.Embeds[0]; em.Poster != poster || em.Width != 608 || em.Height != 1080 || em.Duration != 6.02 || em.Loop {
		t.Fatalf("served facts: %+v", em)
	}
	if b, _ := s.Post(bare); b.Embeds[0].Width != 320 || b.Embeds[0].Poster != "" {
		t.Fatalf("an upload declares its own shape: %+v", b.Embeds[0])
	}

	if err := s.Rebuild(); err != nil {
		t.Fatal(err)
	}
	if pinRefs(t, s, vid) != 1 || pinRefs(t, s, poster) != 1 {
		t.Fatalf("after rebuild: refs video %d poster %d", pinRefs(t, s, vid), pinRefs(t, s, poster))
	}
	raw, sig, e, op = a.msg(t, "post.delete", map[string]any{"post": id})
	_, unpin, err := s.Ingest(raw, sig, e, op)
	if err != nil {
		t.Fatal(err)
	}
	if len(unpin) != 2 || pinRefs(t, s, vid) != -1 || pinRefs(t, s, poster) != -1 {
		t.Fatalf("after delete: unpin %v, refs video %d poster %d", unpin, pinRefs(t, s, vid), pinRefs(t, s, poster))
	}
}
