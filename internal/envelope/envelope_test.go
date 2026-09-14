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
