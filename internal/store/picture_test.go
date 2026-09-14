package store

import (
	"testing"
	"time"
)

// TestPictures: a linked picture joins its post in every read, a failed
// try is counted and never served, the sweep lists what has tries left,
// deleting the post releases the copies' pins, and Rebuild keeps them
// honest (pictures are not in the log).
func TestPictures(t *testing.T) {
	s := openTest(t)
	a := newAuthor(t)
	const l1 = "https://ipfs.filebase.io/ipfs/QmQCqET4hLUqyMPQRVZ9X2VmaN44PC7iHovmdxG4WoLQJZ"
	const l2 = "https://ipfs.io/ipfs/bafybeiatkuft2dk5jwyrati4vmqsks4hiiqukhfxazv3yfyb5scimclnki/shot%20one.png"
	id := ingest(t, s, a, "post.create", map[string]any{"text": "shots " + l1 + " " + l2})

	todo, err := s.PostsWithoutPictures(10)
	if err != nil || len(todo) != 1 || todo[0].ID != id {
		t.Fatalf("PostsWithoutPictures = %+v, %v", todo, err)
	}
	if tried, _ := s.PictureTries(id); len(tried) != 0 {
		t.Fatalf("tries before any = %+v", tried)
	}
	if _, err := s.SetPicture(id, 0, l1, "bafypic1", 100, "image/png", 1, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPicture(id, 1, l2, "", 0, "", 1, false); err != nil {
		t.Fatal(err)
	}
	posts, err := s.Feed("", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 1 || len(posts[0].Pictures) != 1 || posts[0].Pictures[0] != (Picture{URL: l1, CID: "bafypic1", MIME: "image/png"}) {
		t.Fatalf("feed pictures = %+v", posts[0].Pictures)
	}
	if (Picture{URL: l2}).Name() != "shot one.png" || (Picture{URL: l1}).Name() != "Picture" {
		t.Errorf("Name = %q, %q", Picture{URL: l2}.Name(), Picture{URL: l1}.Name())
	}
	var refs int
	if err := s.db.QueryRow(`SELECT refs FROM pins WHERE cid='bafypic1'`).Scan(&refs); err != nil || refs != 1 {
		t.Fatalf("copy pin refs = %d (%v), want 1", refs, err)
	}
	tried, _ := s.PictureTries(id)
	if tried[l1] != (PictureTry{OK: true, Tries: 1}) || tried[l2] != (PictureTry{OK: false, Tries: 1}) {
		t.Fatalf("tries = %+v", tried)
	}
	if left, _ := s.PostsWithoutPictures(10); len(left) != 0 {
		t.Fatalf("PostsWithoutPictures after tries = %+v", left)
	}

	// the sweep: the failed link has tries left until the cap, and only
	// once its last try is old enough
	far := time.Now().Add(time.Hour).UnixMilli()
	if todo, _ := s.PicturesToRetry(3, far, 10); len(todo) != 1 || todo[0].ID != id {
		t.Fatalf("PicturesToRetry = %+v", todo)
	}
	if todo, _ := s.PicturesToRetry(3, time.Now().Add(-time.Hour).UnixMilli(), 10); len(todo) != 0 {
		t.Fatalf("PicturesToRetry inside the hour = %+v", todo)
	}
	if _, err := s.SetPicture(id, 1, l2, "", 0, "", 3, false); err != nil {
		t.Fatal(err)
	}
	if tried, _ := s.PictureTries(id); tried[l2].Tries != 3 {
		t.Fatalf("tries after the cap = %+v", tried[l2])
	}
	if todo, _ := s.PicturesToRetry(3, far, 10); len(todo) != 0 {
		t.Fatalf("PicturesToRetry past the cap = %+v", todo)
	}
	// a later success lands the second picture, in link order
	if _, err := s.SetPicture(id, 1, l2, "bafypic2", 200, "image/jpeg", 3, true); err != nil {
		t.Fatal(err)
	}
	p, err := s.Post(id)
	if err != nil || len(p.Pictures) != 2 || p.Pictures[1].CID != "bafypic2" {
		t.Fatalf("post pictures = %+v, %v", p.Pictures, err)
	}

	// Rebuild replays the log; the pictures, absent from it, survive with
	// their pins' refcounts intact
	if err := s.Rebuild(); err != nil {
		t.Fatal(err)
	}
	for _, cid := range []string{"bafypic1", "bafypic2"} {
		if err := s.db.QueryRow(`SELECT refs FROM pins WHERE cid=?`, cid).Scan(&refs); err != nil || refs != 1 {
			t.Fatalf("after rebuild, %s refs = %d (%v), want 1", cid, refs, err)
		}
	}
	if p, _ := s.Post(id); len(p.Pictures) != 2 {
		t.Fatal("pictures gone after rebuild")
	}

	// deleting the post takes the pictures and queues the copies' unpins
	raw, sig, e, op := a.msg(t, "post.delete", map[string]any{"post": id})
	_, unpin, err := s.Ingest(raw, sig, e, op)
	if err != nil {
		t.Fatal(err)
	}
	if len(unpin) != 2 {
		t.Fatalf("unpin = %v, want both copies", unpin)
	}
	if tried, _ := s.PictureTries(id); len(tried) != 0 {
		t.Fatal("pictures outlived their post")
	}
}
