package media

import (
	"context"
	"encoding/json"
	"errors"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These run the real ffmpeg on generated clips, and skip without it.

func testConverter(t *testing.T) *Converter {
	t.Helper()
	c, err := New("", "", "auto")
	if err != nil {
		t.Skipf("no ffmpeg here: %v", err)
	}
	c.Log = t.Logf
	return c
}

func ffmpeg(t *testing.T, c *Converter, args ...string) {
	t.Helper()
	args = append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)
	if out, err := exec.Command(c.FFmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
	}
}

// probeOut is what ffprobe reports of an output: the tags a conversion
// must drop and the facts it must set.
func probeOut(t *testing.T, c *Converter, path string) map[string]any {
	t.Helper()
	out, err := exec.Command(c.FFprobe, "-v", "error", "-of", "json", "-show_format", "-show_streams", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.Unmarshal(out, &doc)
	return doc
}

// gray is a tiny grayscale frame from the middle of a video, to compare
// two conversions' pictures.
func gray(t *testing.T, c *Converter, path string) []byte {
	t.Helper()
	out, err := exec.Command(c.FFmpeg, "-hide_banner", "-loglevel", "error", "-i", path,
		"-frames:v", "1", "-vf", "select=eq(n\\,10),scale=24:24", "-pix_fmt", "gray", "-f", "rawvideo", "-").Output()
	if err != nil || len(out) != 576 {
		t.Fatalf("frame of %s: %v (%d bytes)", path, err, len(out))
	}
	return out
}

// edges are a video's first and last rows and columns in gray at full
// size, where a GPU turn once left a green row.
func edges(t *testing.T, c *Converter, path string) []byte {
	t.Helper()
	var all []byte
	for _, crop := range []string{"iw:1:0:0", "iw:1:0:ih-1", "1:ih:0:0", "1:ih:iw-1:0"} {
		out, err := exec.Command(c.FFmpeg, "-hide_banner", "-loglevel", "error", "-i", path,
			"-frames:v", "1", "-vf", "select=eq(n\\,10),format=gray,crop="+crop, "-pix_fmt", "gray", "-f", "rawvideo", "-").Output()
		if err != nil || len(out) == 0 {
			t.Fatalf("edge %s of %s: %v", crop, path, err)
		}
		all = append(all, out...)
	}
	return all
}

func meanDiff(a, b []byte) float64 {
	var sum int
	for i := range a {
		d := int(a[i]) - int(b[i])
		if d < 0 {
			d = -d
		}
		sum += d
	}
	return float64(sum) / float64(len(a))
}

// A phone's portrait HDR movie: HEVC 10-bit HLG stored landscape with a
// display rotation, GPS in its metadata, and sound.
func TestConvertPhoneVideo(t *testing.T) {
	c := testConverter(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "phone.mov")
	tmp := filepath.Join(dir, "tmp.mov")
	ffmpeg(t, c, "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30", "-f", "lavfi", "-i", "sine=frequency=440",
		"-t", "2", "-vf", "format=yuv420p10le", "-c:v", "libx265", "-x265-params", "log-level=error",
		"-color_primaries", "bt2020", "-color_trc", "arib-std-b67", "-colorspace", "bt2020nc", "-tag:v", "hvc1",
		"-c:a", "aac", "-metadata", "location=+37.7749-122.4194/", "-metadata", "com.apple.quicktime.location.ISO6709=+37.7749-122.4194/",
		"-metadata:s:v", "handler_name=Core Media Video", "-movflags", "+use_metadata_tags", tmp)
	ffmpeg(t, c, "-display_rotation:v:0", "90", "-i", tmp, "-c", "copy", "-movflags", "+use_metadata_tags", in)

	var last float64
	res, err := c.Convert(context.Background(), in, dir, func(f float64) { last = f })
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != Video || res.MIME != "video/mp4" || res.Width != 720 || res.Height != 1280 ||
		res.Duration < 1.9 || res.Duration > 2.1 || res.PosterMIME != "image/jpeg" {
		t.Fatalf("result %+v", res)
	}
	if last < 0.5 {
		t.Errorf("progress only reached %.2f", last)
	}
	doc := probeOut(t, c, res.File)
	js, _ := json.Marshal(doc)
	for _, gone := range []string{"37.7749", "ISO6709", "Core Media", "rotation", "arib-std-b67"} {
		if strings.Contains(string(js), gone) {
			t.Errorf("output still carries %q: %s", gone, js)
		}
	}
	if !strings.Contains(string(js), `"codec_name":"h264"`) || !strings.Contains(string(js), `"codec_name":"aac"`) ||
		!strings.Contains(string(js), `"width":720`) || !strings.Contains(string(js), `"color_transfer":"bt709"`) {
		t.Errorf("output streams: %s", js)
	}
	if st, _ := os.Stat(res.File); st.Size() > c.Cap {
		t.Errorf("output %d bytes over the %d cap", st.Size(), c.Cap)
	}
	pf, err := os.Open(res.Poster)
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	b := make([]byte, 3)
	pf.Read(b)
	if b[0] != 0xFF || b[1] != 0xD8 {
		t.Errorf("poster is not a JPEG: % x", b)
	}
}

// The GPU chain turns a rotated video the same way the CPU does, both
// ways round.
func TestConvertRotationGPUMatchesCPU(t *testing.T) {
	c := testConverter(t)
	if !c.Vulkan {
		t.Skip("no Vulkan filters here")
	}
	for _, tc := range []struct{ rot, codec string }{{"90", "libx264"}, {"-90", "libx264"}, {"180", "libx264"}, {"-90", "libx265"}} {
		rot := tc.rot + " " + tc.codec
		dir := t.TempDir()
		tmp, in := filepath.Join(dir, "tmp.mp4"), filepath.Join(dir, "in.mp4")
		// still bars: a moving test pattern differs a frame apart
		ffmpeg(t, c, "-f", "lavfi", "-i", "smptehdbars=size=640x360:rate=30", "-t", "1", "-c:v", tc.codec, "-pix_fmt", "yuv420p", "-tag:v", map[string]string{"libx264": "avc1", "libx265": "hvc1"}[tc.codec], tmp)
		ffmpeg(t, c, "-display_rotation:v:0", tc.rot, "-i", tmp, "-c", "copy", in)
		gpuDir, cpuDir := filepath.Join(dir, "gpu"), filepath.Join(dir, "cpu")
		os.Mkdir(gpuDir, 0o700)
		os.Mkdir(cpuDir, 0o700)
		g, err := c.Convert(context.Background(), in, gpuDir, nil)
		if err != nil {
			t.Fatal(err)
		}
		cpu := *c
		cpu.Vulkan = false
		k, err := cpu.Convert(context.Background(), in, cpuDir, nil)
		if err != nil {
			t.Fatal(err)
		}
		if g.Width != k.Width || g.Height != k.Height {
			t.Fatalf("rotation %s: GPU %dx%d, CPU %dx%d", rot, g.Width, g.Height, k.Width, k.Height)
		}
		if d := meanDiff(gray(t, c, g.File), gray(t, c, k.File)); d > 6 {
			t.Errorf("rotation %s: GPU and CPU pictures differ by %.1f levels", rot, d)
		}
		if d := meanDiff(edges(t, c, g.File), edges(t, c, k.File)); d > 12 {
			t.Errorf("rotation %s: GPU and CPU edges differ by %.1f levels", rot, d)
		}
	}
}

func TestConvertAudio(t *testing.T) {
	c := testConverter(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "memo.mp3")
	ffmpeg(t, c, "-f", "lavfi", "-i", "sine=frequency=220:duration=3", "-metadata", "artist=Someone", "-c:a", "libmp3lame", in)
	res, err := c.Convert(context.Background(), in, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != Audio || res.MIME != "audio/mp4" || res.PosterMIME != "image/png" || res.Duration < 2.9 || res.Duration > 3.2 {
		t.Fatalf("result %+v", res)
	}
	b, _ := os.ReadFile(res.File)
	if Sniff(b[:64]) != "audio/mp4" {
		t.Errorf("the m4a sniffs as %q", Sniff(b[:64]))
	}
	if js, _ := json.Marshal(probeOut(t, c, res.File)); strings.Contains(string(js), "Someone") {
		t.Errorf("tags kept: %s", js)
	}
	f, _ := os.Open(res.Poster)
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 1200 || img.Bounds().Dy() != 96 {
		t.Errorf("waveform %v", img.Bounds())
	}
	if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
		t.Errorf("waveform corner is not clear: alpha %d", a)
	}
}

func TestConvertGIF(t *testing.T) {
	c := testConverter(t)
	dir := t.TempDir()
	anim, still := filepath.Join(dir, "anim.gif"), filepath.Join(dir, "still.gif")
	ffmpeg(t, c, "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=10", "-t", "2", anim)
	ffmpeg(t, c, "-f", "lavfi", "-i", "testsrc2=size=320x240", "-frames:v", "1", still)
	res, err := c.Convert(context.Background(), anim, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != Loop || res.MIME != "video/mp4" || res.Width != 320 || res.Height != 240 || res.Poster == "" {
		t.Fatalf("animated: %+v", res)
	}
	if js, _ := json.Marshal(probeOut(t, c, res.File)); strings.Contains(string(js), `"codec_type":"audio"`) {
		t.Errorf("a loop has sound: %s", js)
	}
	res, err = c.Convert(context.Background(), still, t.TempDir(), nil)
	if err != nil || res.Kind != Picture || res.File != still || res.MIME != "image/gif" {
		t.Fatalf("still: %+v %v", res, err)
	}
}

// Playlists dressed as media are refused before anything reads what they
// point at; so is what is too long or not media at all.
func TestConvertRefuses(t *testing.T) {
	c := testConverter(t)
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.txt")
	os.WriteFile(secret, []byte("do not read"), 0o600)
	for name, body := range map[string]string{
		"hls.mp4":    "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\nfile://" + secret + "\n#EXT-X-ENDLIST\n",
		"concat.mov": "ffconcat version 1.0\nfile '" + secret + "'\n",
		"text.mp4":   "just some words",
	} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o600)
		if _, err := c.Convert(context.Background(), p, dir, nil); !errors.Is(err, ErrUnreadable) {
			t.Errorf("%s: %v, want ErrUnreadable", name, err)
		}
	}
	long := filepath.Join(dir, "long.mp4")
	ffmpeg(t, c, "-f", "lavfi", "-i", "testsrc2=size=160x120:rate=10", "-t", "3", "-c:v", "libx264", long)
	short := *c
	short.MaxVideo = 2 * time.Second
	if _, err := short.Convert(context.Background(), long, dir, nil); err == nil || !strings.Contains(err.Error(), "capped at 0:02") {
		t.Errorf("a 3 s video under a 2 s cap: %v", err)
	}
}

// A clip that overshoots a small cap is converted again, lower, until it
// fits.
func TestConvertFitsTheCap(t *testing.T) {
	c := testConverter(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "noise.mp4")
	ffmpeg(t, c, "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=30,noise=alls=60:allf=t", "-t", "6", "-c:v", "libx264", "-crf", "10", in)
	for _, cpu := range []bool{false, true} {
		cc := *c
		cc.Cap = 400_000
		if cpu {
			cc.NVENC, cc.Vulkan = false, false
		}
		out := filepath.Join(dir, map[bool]string{false: "gpu", true: "cpu"}[cpu])
		os.Mkdir(out, 0o700)
		res, err := cc.Convert(context.Background(), in, out, nil)
		if err != nil {
			t.Fatalf("cpu=%v: %v", cpu, err)
		}
		if st, _ := os.Stat(res.File); st.Size() > cc.Cap {
			t.Errorf("cpu=%v: %d bytes over a %d cap", cpu, st.Size(), cc.Cap)
		}
	}
}

func TestTierFit(t *testing.T) {
	for _, c := range []struct {
		w, h   int
		d      float64
		ww, wh int
	}{
		{3840, 2160, 20, 1920, 1080},
		{2160, 3840, 20, 1080, 1920},
		{1920, 1080, 45, 1280, 720},
		{1080, 1920, 90, 480, 854},
		{640, 480, 10, 640, 480}, // never enlarged
		{3000, 1000, 10, 1920, 640},
		{721, 405, 50, 722, 406}, // even sides
	} {
		long, short, _ := Tier(c.d)
		if w, h := Fit(c.w, c.h, long, short); w != c.ww || h != c.wh {
			t.Errorf("%dx%d at %gs: %dx%d, want %dx%d", c.w, c.h, c.d, w, h, c.ww, c.wh)
		}
	}
}

// MEDIA_TEST_FILE=/path/to/clip.mov go test -run ConvertFile -v converts a
// real file and reports what came out, for trying one from a phone.
func TestConvertFile(t *testing.T) {
	in := os.Getenv("MEDIA_TEST_FILE")
	if in == "" {
		t.Skip("MEDIA_TEST_FILE not set")
	}
	c := testConverter(t)
	dir := os.Getenv("MEDIA_TEST_OUT")
	if dir == "" {
		dir = t.TempDir()
	}
	start := time.Now()
	res, err := c.Convert(context.Background(), in, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(res.File)
	t.Logf("%s with %s in %s: %+v, %d bytes", in, c.Encoder(), time.Since(start).Round(time.Millisecond), *res, st.Size())
}
