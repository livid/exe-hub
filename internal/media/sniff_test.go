package media

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func ftyp(major string, compat ...string) []byte {
	b := []byte{0, 0, 0, 0, 'f', 't', 'y', 'p'}
	b = append(b, major...)
	b = append(b, 0, 0, 2, 0)
	for _, c := range compat {
		b = append(b, c...)
	}
	b[3] = byte(len(b))
	return append(b, make([]byte, 64)...)
}

func TestSniffBrands(t *testing.T) {
	for _, c := range []struct {
		head []byte
		want string
	}{
		{ftyp("isom", "isom", "iso2", "avc1", "mp41"), "video/mp4"}, // ffmpeg's mp4
		{ftyp("isom", "isom"), "video/mp4"},
		{ftyp("mp42", "mp42", "isom"), "video/mp4"},
		{ftyp("qt  ", "qt  "), "video/quicktime"},           // an iPhone movie
		{ftyp("M4A ", "M4A ", "mp42", "isom"), "audio/mp4"}, // a voice memo
		{ftyp("3gp4", "3gp4", "isom"), "video/3gpp"},
		{ftyp("heic", "mif1", "heic"), "application/octet-stream"}, // a HEIF picture is not a video
		{append([]byte("fLaC"), make([]byte, 60)...), "audio/flac"},
		{append([]byte("\x1aE\xdf\xa3"), make([]byte, 60)...), "video/webm"},
		{append([]byte("ID3"), make([]byte, 60)...), "audio/mpeg"},
		{append([]byte("GIF89a"), make([]byte, 60)...), "image/gif"},
		{[]byte("<!doctype html><title>x</title>"), "text/html"},
	} {
		if got := Sniff(c.head); got != c.want {
			t.Errorf("Sniff(%q…) = %q, want %q", c.head[:12], got, c.want)
		}
	}
}

// What ffmpeg itself writes, sniffed the way an upload is.
func TestSniffFFmpegOutput(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg on this host")
	}
	dir := t.TempDir()
	for name, want := range map[string]string{
		"a.mp4": "video/mp4", "a.mov": "video/quicktime", "a.m4a": "audio/mp4", "a.3gp": "video/3gpp",
	} {
		p := filepath.Join(dir, name)
		args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "sine=duration=0.2"}
		if name != "a.m4a" {
			args = append(args, "-f", "lavfi", "-i", "testsrc2=size=176x144:duration=0.2", "-c:v", "mpeg4")
		}
		args = append(args, "-c:a", "aac", p)
		if out, err := exec.Command(ff, args...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %s: %v\n%s", name, err, out)
		}
		b, _ := os.ReadFile(p)
		if got := Sniff(b[:min(len(b), 512)]); got != want {
			t.Errorf("%s sniffs as %q, want %q", name, got, want)
		}
	}
}
