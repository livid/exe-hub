package envelope

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEmbedFacts(t *testing.T) {
	op := func(embed string) error {
		body := `{"text":"","embeds":[` + embed + `]}`
		e := &Envelope{Type: "post.create", Body: json.RawMessage(body)}
		_, err := e.Op()
		return err
	}
	for _, ok := range []string{
		`{"cid":"bafybeivideo","mime":"video/mp4"}`,
		`{"cid":"bafybeivideo","mime":"video/mp4","poster":"bafybeiposter","width":1920,"height":1080,"duration":12.5}`,
		`{"cid":"bafybeivideo","mime":"video/mp4","width":480,"height":270,"loop":true}`,
		`{"cid":"bafybeivideo","mime":"video/mp4","later":"a field this hub does not know yet"}`,
	} {
		if err := op(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{
		`{"cid":"bafybeivideo","mime":"video/mp4","poster":"not a cid"}`,
		`{"cid":"bafybeivideo","mime":"video/mp4","width":1920}`,
		`{"cid":"bafybeivideo","mime":"video/mp4","width":-1,"height":-1}`,
		`{"cid":"bafybeivideo","mime":"video/mp4","width":20000,"height":10}`,
		`{"cid":"bafybeivideo","mime":"video/mp4","duration":-3}`,
		`{"cid":"bafybeivideo","mime":"video/mp4","duration":1e9}`,
	} {
		if err := op(bad); err == nil || !strings.Contains(err.Error(), "embed") {
			t.Errorf("%s: accepted (%v)", bad, err)
		}
	}
}

// a post.mark names a post by id and a box by its count, from nought
// and under MaxBoxes, and says the state it wants
func TestPostMark(t *testing.T) {
	post := strings.Repeat("ab", 32)
	for _, c := range []struct {
		body string
		ok   bool
	}{
		{`{"post":"` + post + `","box":0,"done":true}`, true},
		{`{"post":"` + post + `","box":999,"done":false}`, true},
		{`{"post":"` + post + `","box":1000,"done":true}`, false},
		{`{"post":"` + post + `","box":-1,"done":true}`, false},
		{`{"post":"nope","box":0,"done":true}`, false},
		{`{"post":"` + post + `","box":0}`, true},
	} {
		e := &Envelope{Type: "post.mark", Body: json.RawMessage(c.body)}
		op, err := e.Op()
		if (err == nil) != c.ok {
			t.Errorf("%s: err %v, want ok %v", c.body, err, c.ok)
			continue
		}
		if m, _ := op.(*PostMark); c.ok && (m == nil || m.Post != post) {
			t.Errorf("%s: op %#v", c.body, op)
		}
	}
}
