package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// media.max_out_mb defaults to the most a peer mirrors, and a config that
// asks for more is refused rather than writing files no peer would take.
func TestMediaMaxOut(t *testing.T) {
	load := func(media string) (*Config, error) {
		p := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(p, []byte(`{"listen":"127.0.0.1:0","media":`+media+`}`), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(p)
	}
	c, err := load(`{}`)
	if err != nil || c.Media.MaxOutMB != 64 {
		t.Fatalf("default: %+v, %v", c, err)
	}
	if c, err = load(`{"max_out_mb":32}`); err != nil || c.Media.MaxOutMB != 32 {
		t.Fatalf("32: %+v, %v", c, err)
	}
	if _, err = load(`{"max_out_mb":65}`); err == nil || !strings.Contains(err.Error(), "at most 64") {
		t.Fatalf("65: %v", err)
	}
}
