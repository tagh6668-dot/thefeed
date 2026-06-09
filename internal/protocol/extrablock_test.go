package protocol

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"
)

func newTestSigningKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	return pub, priv
}

// TestExtraBlockRoundTrip: encode -> parse -> verify signature -> verify
// content, all for the same channel and content, must succeed.
func TestExtraBlockRoundTrip(t *testing.T) {
	pub, priv := newTestSigningKey(t)
	const ch uint16 = 7
	content := []byte("canonical channel 7 content")
	digest := ContentDigest(content)
	ts := time.Now().Unix()

	raw, err := EncodeExtraBlock(priv, ch, digest, ts)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(raw) < MinBlockPayload || len(raw) > MaxBlockPayload {
		t.Errorf("block size %d outside [%d,%d]", len(raw), MinBlockPayload, MaxBlockPayload)
	}

	eb, err := ParseExtraBlock(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if eb.Timestamp != ts {
		t.Errorf("timestamp = %d, want %d", eb.Timestamp, ts)
	}
	if err := VerifyExtraBlock(pub, ch, eb); err != nil {
		t.Fatalf("verify sig: %v", err)
	}
	if err := eb.VerifyChannelContent(content); err != nil {
		t.Fatalf("verify content: %v", err)
	}
}

// TestExtraBlockWrongChannel: a block signed for one channel must not
// verify when presented as another channel's (resolver substitution).
func TestExtraBlockWrongChannel(t *testing.T) {
	pub, priv := newTestSigningKey(t)
	digest := ContentDigest([]byte("x"))
	raw, err := EncodeExtraBlock(priv, 1, digest, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	eb, err := ParseExtraBlock(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyExtraBlock(pub, 2, eb); err == nil {
		t.Fatal("expected verify to fail for wrong channel id")
	}
	if err := VerifyExtraBlock(pub, 1, eb); err != nil {
		t.Fatalf("expected verify to pass for correct channel id: %v", err)
	}
}

// TestExtraBlockTamperedContent: signature still valid, but the content
// the client reassembled does not match the signed digest.
func TestExtraBlockTamperedContent(t *testing.T) {
	pub, priv := newTestSigningKey(t)
	const ch uint16 = 3
	digest := ContentDigest([]byte("real content"))
	raw, err := EncodeExtraBlock(priv, ch, digest, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	eb, err := ParseExtraBlock(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyExtraBlock(pub, ch, eb); err != nil {
		t.Fatalf("sig should verify: %v", err)
	}
	if err := eb.VerifyChannelContent([]byte("FAKE content")); err == nil {
		t.Fatal("expected content digest mismatch")
	}
}

// TestExtraBlockWrongKey: a different server key must not verify.
func TestExtraBlockWrongKey(t *testing.T) {
	_, priv := newTestSigningKey(t)
	otherPub, _ := newTestSigningKey(t)
	digest := ContentDigest([]byte("y"))
	raw, err := EncodeExtraBlock(priv, 5, digest, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	eb, err := ParseExtraBlock(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyExtraBlock(otherPub, 5, eb); err == nil {
		t.Fatal("expected verify to fail with wrong public key")
	}
}

// TestParseExtraBlockRejectsGarbage: a bad/short/old-server response must
// be rejected without panicking.
func TestParseExtraBlockRejectsGarbage(t *testing.T) {
	cases := [][]byte{
		nil,
		{},
		{0x00},
		{0x58, 0x42},                   // magic only, too short
		{0x00, 0x00, 0x01, 0x01, 0x00}, // bad magic
		{0x58, 0x42, 0x09, 0x01, 0x00}, // unsupported version
		make([]byte, 300),              // all-zero garbage
	}
	for i, c := range cases {
		if _, err := ParseExtraBlock(c); err == nil {
			t.Errorf("case %d: expected error for invalid block", i)
		}
	}
}

// TestExtraBlockTimestampSurvives: a forged later timestamp would change
// the signed data and so must break verification (anti-rollback relies on
// the timestamp being signed).
func TestExtraBlockTimestampSigned(t *testing.T) {
	pub, priv := newTestSigningKey(t)
	const ch uint16 = 11
	digest := ContentDigest([]byte("z"))
	raw, err := EncodeExtraBlock(priv, ch, digest, 1000)
	if err != nil {
		t.Fatal(err)
	}
	eb, err := ParseExtraBlock(raw)
	if err != nil {
		t.Fatal(err)
	}
	// Tamper with the parsed timestamp, as a forging resolver might.
	eb.Timestamp = 9999
	if err := VerifyExtraBlock(pub, ch, eb); err == nil {
		t.Fatal("expected verify to fail after timestamp tamper")
	}
}
