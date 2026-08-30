// Package identity manages the hub's own ed25519 keypair — generated on
// first start, distinct from any exe node's peer key. Unused by the v1 API
// beyond the hub-info endpoint; it exists so future trust aggregation can
// let hubs identify and sign exchanges with each other.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
)

type Identity struct {
	ID   string // 16 hex chars of sha256(pub), same scheme as exe node ids
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

// Load reads <stateDir>/hub_ed25519, generating it on first use.
func Load(stateDir string) (*Identity, error) {
	p := filepath.Join(stateDir, "hub_ed25519")
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		var pub ed25519.PublicKey
		var priv ed25519.PrivateKey
		if pub, priv, err = ed25519.GenerateKey(rand.Reader); err != nil {
			return nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(priv)
		if err != nil {
			return nil, err
		}
		b = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		if err := os.WriteFile(p, b, 0o600); err != nil {
			return nil, err
		}
		return &Identity{ID: Fingerprint(pub), priv: priv, pub: pub}, nil
	} else if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("hub_ed25519: not PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("hub_ed25519: not an ed25519 key")
	}
	pub := priv.Public().(ed25519.PublicKey)
	return &Identity{ID: Fingerprint(pub), priv: priv, pub: pub}, nil
}

// Fingerprint is the canonical id for a public key: 16 hex chars of its
// sha256 — profiles and hubs share the scheme.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

func (id *Identity) PubKey() string {
	return base64.StdEncoding.EncodeToString(id.pub)
}

// Sign signs with the hub's own key. Callers add their domain prefix.
func (id *Identity) Sign(msg []byte) []byte {
	return ed25519.Sign(id.priv, msg)
}
