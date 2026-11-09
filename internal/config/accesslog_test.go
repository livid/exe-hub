package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAccessLogConfig(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		fail       bool
	}{
		{"absent", `{}`, false},
		{"enabled", `{"access_log":{"enabled":true,"trusted_proxies":["172.30.0.1/32"]}}`, false},
		{"bad proxy", `{"access_log":{"enabled":true,"trusted_proxies":["trust everybody"]}}`, true},
		{"bad size", `{"access_log":{"max_size_mb":-1}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if (err != nil) != tc.fail {
				t.Fatalf("Load: %v", err)
			}
			if tc.name == "absent" && cfg.AccessLog != nil {
				t.Fatal("access logging enabled by default")
			}
			if tc.name == "enabled" && (!cfg.AccessLog.Enabled || cfg.AccessLog.MaxSizeMB != 50) {
				t.Fatalf("access log defaults: %+v", cfg.AccessLog)
			}
		})
	}
}
