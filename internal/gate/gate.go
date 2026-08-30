// Package gate decides who may post. Open mode admits any valid signature.
// Token mode requires the author's Solana address — the raw 32-byte ed25519
// pubkey in base58, so a signed message already proves control of the
// checked address — to hold a minimum of the configured SPL token. Holding
// needs no Solana signatures; this is a read-only RPC balance check.
package gate

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"sync"
	"time"

	"exehub/internal/config"
)

var ErrDenied = errors.New("token gate: balance below threshold")
var ErrUnavailable = errors.New("token gate: RPC unavailable and no cached verdict")

type verdict struct {
	ok bool
	at time.Time
}

type Gate struct {
	cfg    *config.Holder
	client *http.Client

	mu       sync.Mutex
	cache    map[string]verdict // solana address -> last verdict
	decimals map[string]int     // mint -> decimals; immutable on-chain, so cached forever
}

func New(cfg *config.Holder) *Gate {
	return &Gate{cfg: cfg, client: &http.Client{Timeout: 15 * time.Second},
		cache: map[string]verdict{}, decimals: map[string]int{}}
}

// Decimals returns the configured mint's on-chain decimal places (via
// getTokenSupply, cached forever — a mint's decimals are immutable). ok is
// false in open mode or while the RPC is unreachable with no cached value;
// callers fall back to raw base units.
func (g *Gate) Decimals() (dec int, ok bool) {
	c := g.cfg.Get()
	if c.Gate.Mode != "token" {
		return 0, false
	}
	t := &c.Gate.Token
	g.mu.Lock()
	dec, cached := g.decimals[t.Mint]
	g.mu.Unlock()
	if cached {
		return dec, true
	}
	req, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getTokenSupply", "params": []any{t.Mint},
	})
	resp, err := g.client.Post(t.RPCURL, "application/json", bytes.NewReader(req))
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	var out struct {
		Result struct {
			Value struct {
				Decimals *int `json:"decimals"`
			} `json:"value"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Result.Value.Decimals == nil {
		return 0, false
	}
	g.mu.Lock()
	g.decimals[t.Mint] = *out.Result.Value.Decimals
	g.mu.Unlock()
	return *out.Result.Value.Decimals, true
}

// Check returns nil if pub may write under the current gate config.
func (g *Gate) Check(pub ed25519.PublicKey) error {
	c := g.cfg.Get()
	if c.Gate.Mode != "token" {
		return nil
	}
	t := &c.Gate.Token
	addr := Base58(pub)

	g.mu.Lock()
	v, cached := g.cache[addr]
	g.mu.Unlock()
	if cached && time.Since(v.at) < t.RecheckDur {
		return v.errIfDenied()
	}

	held, err := g.balance(t, addr)
	if err != nil {
		// A stale verdict beats guessing; only with no cache at all does
		// rpc_unavailable apply.
		if cached {
			return v.errIfDenied()
		}
		if t.RPCUnavailable == "allow" {
			return nil
		}
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	v = verdict{ok: held >= t.MinRaw, at: time.Now()}
	g.mu.Lock()
	g.cache[addr] = v
	g.mu.Unlock()
	return v.errIfDenied()
}

func (v verdict) errIfDenied() error {
	if v.ok {
		return nil
	}
	return ErrDenied
}

// balance sums the address's token accounts for the mint, in raw base
// units. The RPC's mint filter is program-agnostic — it covers both the
// classic SPL Token and Token-2022 programs in one call.
func (g *Gate) balance(t *config.TokenGate, addr string) (uint64, error) {
	req, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getTokenAccountsByOwner",
		"params": []any{addr,
			map[string]string{"mint": t.Mint},
			map[string]string{"encoding": "jsonParsed"}},
	})
	resp, err := g.client.Post(t.RPCURL, "application/json", bytes.NewReader(req))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var out struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Result struct {
			Value []struct {
				Account struct {
					Data struct {
						Parsed struct {
							Info struct {
								TokenAmount struct {
									Amount string `json:"amount"`
								} `json:"tokenAmount"`
							} `json:"info"`
						} `json:"parsed"`
					} `json:"data"`
				} `json:"account"`
			} `json:"value"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	if out.Error != nil {
		return 0, errors.New(out.Error.Message)
	}
	var total uint64
	for _, v := range out.Result.Value {
		n, err := strconv.ParseUint(v.Account.Data.Parsed.Info.TokenAmount.Amount, 10, 64)
		if err != nil {
			continue
		}
		total += n
	}
	return total, nil
}

const b58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// Base58 renders a public key as a Solana address.
func Base58(b []byte) string {
	n := new(big.Int).SetBytes(b)
	radix := big.NewInt(58)
	mod := new(big.Int)
	var out []byte
	for n.Sign() > 0 {
		n.DivMod(n, radix, mod)
		out = append([]byte{b58Alphabet[mod.Int64()]}, out...)
	}
	for _, c := range b {
		if c != 0 {
			break
		}
		out = append([]byte{'1'}, out...)
	}
	return string(out)
}
