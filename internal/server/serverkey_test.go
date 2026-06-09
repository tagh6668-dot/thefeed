package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreateServerKeyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	k1, err := LoadOrCreateServerKey(dir)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(k1) != ed25519.PrivateKeySize {
		t.Fatalf("key size = %d, want %d", len(k1), ed25519.PrivateKeySize)
	}
	if _, err := os.Stat(filepath.Join(dir, serverKeyFile)); err != nil {
		t.Fatalf("key file not persisted: %v", err)
	}
	// A second call must return the SAME key, not generate a new one.
	k2, err := LoadOrCreateServerKey(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !k1.Equal(k2) {
		t.Error("reloaded key differs from the generated key")
	}
}

func TestLoadServerKeyAcceptsRawSeed(t *testing.T) {
	dir := t.TempDir()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// Operator dropped in a raw 32-byte seed.
	if err := os.WriteFile(filepath.Join(dir, serverKeyFile), priv.Seed(), 0o600); err != nil {
		t.Fatal(err)
	}
	k, err := LoadOrCreateServerKey(dir)
	if err != nil {
		t.Fatalf("load raw seed: %v", err)
	}
	if !k.Equal(priv) {
		t.Error("key from raw seed does not match")
	}
}

func TestDecodeSeedRejectsBad(t *testing.T) {
	if _, err := decodeSeed([]byte("not valid base64 !!!")); err == nil {
		t.Error("expected error for non-base64 junk")
	}
	short := base64.StdEncoding.EncodeToString([]byte("too short"))
	if _, err := decodeSeed([]byte(short)); err == nil {
		t.Error("expected error for wrong-length seed")
	}
}

func TestServerPublicKeyString(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s := ServerPublicKeyString(priv)
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("public key not base64url: %v", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		t.Errorf("public key = %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
}

func TestConfigURI(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri := ConfigURI("t.example.com", "pass phrase", priv)
	if !strings.HasPrefix(uri, "thefeed://t.example.com/") {
		t.Errorf("bad prefix: %s", uri)
	}
	if !strings.Contains(uri, "sk="+ServerPublicKeyString(priv)) {
		t.Errorf("missing sk=: %s", uri)
	}
	if !strings.Contains(uri, "1.1.1.1") || !strings.Contains(uri, "8.8.8.8") {
		t.Errorf("missing bootstrap resolvers: %s", uri)
	}
	// Resolvers (r=) must come LAST so a truncated URI only loses resolvers.
	if iSK, iR := strings.Index(uri, "sk="), strings.Index(uri, "r="); iSK < 0 || iR < iSK {
		t.Errorf("r= must come after sk=: %s", uri)
	}
	// The space in the passphrase must be percent-escaped.
	if strings.Contains(uri, "pass phrase") {
		t.Errorf("passphrase not URL-escaped: %s", uri)
	}
}
