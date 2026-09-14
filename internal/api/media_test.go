package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"exehub/internal/config"
	"exehub/internal/envelope"
	"exehub/internal/identity"
	"exehub/internal/ipfs"
	"exehub/internal/media"
	"exehub/internal/store"
)

// fakeAdd is a kubo that adds: the CID is a base32 digest of the bytes,
// so the same file always gets the same one.
func fakeAdd(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v0/version":
			w.Write([]byte(`{"Version":"fake"}`))
		case "/api/v0/add":
			mr, err := r.MultipartReader()
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			var part *multipart.Part
			if part, err = mr.NextPart(); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			sum := sha256.New()
			io.Copy(sum, part)
			cid := "bafkrei" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum.Sum(nil)))[:52]
			w.Write([]byte(`{"Name":"f","Hash":"` + cid + `"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func signedUpload(t *testing.T, h http.Handler, priv ed25519.PrivateKey, pub ed25519.PublicKey, path string, body []byte, sigBody []byte) *httptest.ResponseRecorder {
	t.Helper()
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	sum := sha256.Sum256(sigBody)
	req := httptest.NewRequest("POST", path, bytes.NewReader(body))
	req.Header.Set("X-Hub-Author", base64.StdEncoding.EncodeToString(pub))
	req.Header.Set("X-Hub-Ts", ts)
	req.Header.Set("X-Hub-Sig", base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(envelope.UploadPrefix+ts+"\n"+hex.EncodeToString(sum[:])))))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestMediaJob: a signed clip goes in, a job converts it, the result
// posts with its facts, and a post that bends them is refused.
func TestMediaJob(t *testing.T) {
	conv, err := media.New("", "", "auto")
	if err != nil {
		t.Skipf("no ffmpeg here: %v", err)
	}
	dir := t.TempDir()
	clip := filepath.Join(dir, "clip.mov")
	if out, err := exec.Command(conv.FFmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=30",
		"-f", "lavfi", "-i", "sine", "-t", "1.5", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", clip).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	body, _ := os.ReadFile(clip)

	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	s.IPFS = ipfs.New(fakeAdd(t).URL)
	if s.Hub, err = identity.Load(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	pub, priv, _ := ed25519.GenerateKey(nil)

	if w := signedUpload(t, h, priv, pub, "/v1/media", body, body); w.Code != 404 {
		t.Fatalf("a hub without media: %d %s", w.Code, w.Body)
	}
	if s.Media, err = NewMedia(conv, filepath.Join(dir, "jobs"), 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, info := get(t, h, "/v1/hub"); !strings.Contains(info, `"media":{"encoder":`) {
		t.Errorf("/v1/hub does not offer media: %s", info)
	}
	if w := signedUpload(t, h, priv, pub, "/v1/media", body, []byte("other bytes")); w.Code != 401 {
		t.Errorf("signature over other bytes: %d %s", w.Code, w.Body)
	}
	if w := signedUpload(t, h, priv, pub, "/v1/media", make([]byte, 2<<20), make([]byte, 2<<20)); w.Code != 413 || !strings.Contains(w.Body.String(), "capped at 1 MB") {
		t.Errorf("over the size limit: %d %s", w.Code, w.Body)
	}

	w := signedUpload(t, h, priv, pub, "/v1/media", body, body)
	if w.Code != 202 {
		t.Fatalf("POST /v1/media: %d %s", w.Code, w.Body)
	}
	var job struct {
		Job, Status, Error string
		Result             *struct {
			Kind, CID, MIME, Poster string
			Size                    int64
			Width, Height           int
			Duration                float64
		}
	}
	json.Unmarshal(w.Body.Bytes(), &job)
	if job.Status != "queued" || len(job.Job) != 24 {
		t.Fatalf("job %+v", job)
	}
	if w := signedUpload(t, h, priv, pub, "/v1/media", body, body); w.Code != 429 {
		t.Errorf("a second job while one waits: %d %s", w.Code, w.Body)
	}
	go s.RunMedia()
	deadline := time.Now().Add(60 * time.Second)
	for job.Status != "done" && job.Status != "failed" {
		if time.Now().After(deadline) {
			t.Fatalf("job stuck: %+v", job)
		}
		time.Sleep(200 * time.Millisecond)
		code, out := get(t, h, "/v1/media/"+job.Job)
		if code != 200 {
			t.Fatalf("GET job: %d %s", code, out)
		}
		json.Unmarshal([]byte(out), &job)
	}
	r := job.Result
	if job.Status != "done" || r == nil || r.Kind != "video" || r.MIME != "video/mp4" || r.Width != 640 || r.Height != 360 || r.Poster == "" {
		t.Fatalf("finished job %+v %+v", job, r)
	}
	if code, _ := get(t, h, "/v1/media/000000000000000000000000"); code != 404 {
		t.Errorf("an unknown job: %d", code)
	}
	for _, cid := range []string{r.CID, r.Poster} {
		if _, err := s.St.PinInfo(cid); err != nil {
			t.Errorf("pin %s: %v", cid, err)
		}
	}

	embed := map[string]any{"cid": r.CID, "mime": r.MIME, "poster": r.Poster, "width": r.Width, "height": r.Height, "duration": r.Duration}
	bent := map[string]any{"cid": r.CID, "mime": r.MIME, "poster": r.Poster, "width": 360, "height": 640, "duration": r.Duration}
	raw, _ := json.Marshal(map[string]any{"type": "post.create", "author": base64.StdEncoding.EncodeToString(pub), "seq": 1, "ts": 1,
		"body": map[string]any{"embeds": []any{bent}}})
	e, _ := envelope.Parse(raw)
	op, _ := e.Op()
	if _, _, err := s.St.Ingest(raw, ed25519.Sign(priv, raw), e, op); !errors.Is(err, store.ErrFacts) {
		t.Errorf("a post that turns the video: %v", err)
	}
	id := ingest(t, s, priv, pub, 1, "post.create", map[string]any{"embeds": []any{embed}})
	p, err := s.St.Post(id)
	if err != nil || p.Embeds[0].Poster != r.Poster || p.Embeds[0].Width != 640 {
		t.Fatalf("posted: %+v %v", p, err)
	}
}
