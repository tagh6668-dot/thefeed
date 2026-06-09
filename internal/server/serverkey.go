package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// serverKeyFile is the filename, under the server data-dir, holding the
// ed25519 signing seed (base64 std of the 32-byte seed). This one key both
// signs feed/response data and (via ed25519->curve25519 conversion on the
// client) lets clients encrypt routing metadata to the server.
const serverKeyFile = "server_ed25519.key"

// LoadOrCreateServerKey returns the server's ed25519 private key, reading it
// from <dataDir>/server_ed25519.key or generating and persisting a new one
// if the file is absent. The accompanying public key (base64url) is what the
// operator pins in configs — print it with ServerPublicKeyString.
//
// The file format is base64-std of the 32-byte seed. A raw 32-byte seed is
// also accepted, so an operator can drop in a key produced elsewhere.
func LoadOrCreateServerKey(dataDir string) (ed25519.PrivateKey, error) {
	path := filepath.Join(dataDir, serverKeyFile)

	if raw, err := os.ReadFile(path); err == nil {
		seed, derr := decodeSeed(raw)
		if derr != nil {
			return nil, fmt.Errorf("server key %s: %w", path, derr)
		}
		return ed25519.NewKeyFromSeed(seed), nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read server key %s: %w", path, err)
	}

	// Absent: generate and persist.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate server key: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	enc := base64.StdEncoding.EncodeToString(priv.Seed())
	if err := os.WriteFile(path, []byte(enc+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write server key %s: %w", path, err)
	}
	return priv, nil
}

// decodeSeed parses a 32-byte ed25519 seed from base64-std text or raw bytes.
func decodeSeed(raw []byte) ([]byte, error) {
	if len(raw) == ed25519.SeedSize {
		return raw, nil
	}
	s := strings.TrimSpace(string(raw))
	seed, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("not base64 and not a %d-byte raw seed: %w", ed25519.SeedSize, err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("seed is %d bytes, want %d", len(seed), ed25519.SeedSize)
	}
	return seed, nil
}

// ServerPublicKeyString returns the base64url (no padding) encoding of the
// server public key — the value pinned in configs and bundled defaults.
func ServerPublicKeyString(priv ed25519.PrivateKey) string {
	pub := priv.Public().(ed25519.PublicKey)
	return base64.RawURLEncoding.EncodeToString(pub)
}

// ConfigURI builds the thefeed:// import URI advertising this server's
// domain, passphrase, pinned signing public key (sk=), and two bootstrap
// resolvers so a freshly-imported client can reach DNS immediately.
func ConfigURI(domain, passphrase string, priv ed25519.PrivateKey) string {
	// Resolvers (r=) go LAST: if the URI is truncated (long resolver list,
	// lost message tail), only trailing resolvers are dropped — domain, key,
	// and sk= survive.
	return fmt.Sprintf("thefeed://%s/%s?sk=%s&r=%s",
		uriComponent(domain),
		uriComponent(passphrase),
		ServerPublicKeyString(priv),
		uriComponent("1.1.1.1,8.8.8.8"),
	)
}

// uriComponent escapes s like JavaScript's encodeURIComponent, so the
// client's URI parser (which decodeURIComponent's each field) round-trips
// it exactly. Non-ASCII bytes are percent-escaped per UTF-8 byte.
func uriComponent(s string) string {
	const safe = "-_.!~*'()"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || strings.IndexByte(safe, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
