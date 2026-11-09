// Package accesslog records private, bounded JSON access logs independently
// of page-view analytics. It never reads or stores request bodies itself.
package accesslog

import (
	"fmt"
	"net/netip"
)

type Config struct {
	Enabled        bool     `json:"enabled"`
	Path           string   `json:"path,omitempty"`
	MaxSizeMB      int      `json:"max_size_mb,omitempty"`
	MaxBackups     int      `json:"max_backups,omitempty"`
	MaxAgeDays     int      `json:"max_age_days,omitempty"`
	TrustedProxies []string `json:"trusted_proxies,omitempty"`
}

func (c *Config) Normalize() error {
	if c.Path == "" {
		c.Path = "access.jsonl"
	}
	if c.MaxSizeMB < 0 || c.MaxBackups < 0 || c.MaxAgeDays < 0 {
		return fmt.Errorf("access_log: rotation limits must not be negative")
	}
	if c.MaxSizeMB == 0 {
		c.MaxSizeMB = 50
	}
	if c.MaxBackups == 0 {
		c.MaxBackups = 7
	}
	if c.MaxAgeDays == 0 {
		c.MaxAgeDays = 7
	}
	for _, raw := range c.TrustedProxies {
		if _, err := netip.ParsePrefix(raw); err != nil {
			return fmt.Errorf("access_log.trusted_proxies: invalid CIDR %q", raw)
		}
	}
	return nil
}
