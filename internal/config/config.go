// Package config loads exe-hub's JSON config and swaps it atomically on
// reload. Reload is manual, nginx-style: nothing watches the file — the
// daemon re-reads it only on SIGHUP (or `exe-hub -s reload`, which sends
// one). An invalid file keeps the running config.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

type TokenGate struct {
	RPCURL    string `json:"rpc_url"`
	Mint      string `json:"mint"`
	MinAmount string `json:"min_amount"` // raw base units; string, u64s don't survive JSON floats
	Recheck   string `json:"recheck"`
	// RPCUnavailable applies only when there is no cached verdict at all;
	// a stale cached verdict is always preferred over guessing.
	RPCUnavailable string `json:"rpc_unavailable"`

	MinRaw     uint64        `json:"-"`
	RecheckDur time.Duration `json:"-"`
}

type Gate struct {
	Mode  string    `json:"mode"` // "open" | "token"
	Token TokenGate `json:"token"`
}

type Config struct {
	Listen  string `json:"listen"`
	IPFSAPI string `json:"ipfs_api"`
	Gate    Gate   `json:"gate"`
	// Admins are profile ids (pubkey fingerprints). They may issue ban ops
	// and their own writes bypass the token gate.
	Admins           []string `json:"admins"`
	AllowReplication *bool    `json:"allow_replication"` // nil = default true
}

func (c *Config) Replicable() bool {
	return c.AllowReplication == nil || *c.AllowReplication
}

func (c *Config) IsAdmin(profileID string) bool {
	for _, a := range c.Admins {
		if a == profileID {
			return true
		}
	}
	return false
}

// Load reads and validates one config file. It never mutates a running
// config: the caller swaps the result into a Holder only on success.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:7788"
	}
	if c.IPFSAPI == "" {
		c.IPFSAPI = "http://127.0.0.1:5001"
	}
	switch c.Gate.Mode {
	case "", "open":
		c.Gate.Mode = "open"
	case "token":
		t := &c.Gate.Token
		if t.RPCURL == "" || t.Mint == "" {
			return nil, errors.New("gate.token needs rpc_url and mint")
		}
		if t.MinRaw, err = strconv.ParseUint(t.MinAmount, 10, 64); err != nil || t.MinRaw == 0 {
			return nil, errors.New("gate.token.min_amount must be a positive integer string")
		}
		if t.Recheck == "" {
			t.Recheck = "10m"
		}
		if t.RecheckDur, err = time.ParseDuration(t.Recheck); err != nil {
			return nil, fmt.Errorf("gate.token.recheck: %w", err)
		}
		switch t.RPCUnavailable {
		case "":
			t.RPCUnavailable = "deny"
		case "allow", "deny":
		default:
			return nil, errors.New(`gate.token.rpc_unavailable must be "allow" or "deny"`)
		}
	default:
		return nil, fmt.Errorf("gate.mode %q (want open or token)", c.Gate.Mode)
	}
	return c, nil
}

// Holder is the atomically-swappable current config.
type Holder struct{ v atomic.Pointer[Config] }

func NewHolder(c *Config) *Holder {
	h := &Holder{}
	h.v.Store(c)
	return h
}

func (h *Holder) Get() *Config    { return h.v.Load() }
func (h *Holder) Set(c *Config)   { h.v.Store(c) }
