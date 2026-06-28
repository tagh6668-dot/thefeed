package web

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	var key [32]byte
	copy(key[:], bytes.Repeat([]byte{7}, 32))
	pt := []byte("hello saved messages")
	sealed := sealBytes(key, pt)
	if bytes.Equal(sealed, pt) {
		t.Fatal("sealed bytes equal plaintext")
	}
	got, err := openBytes(key, sealed)
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("openBytes = %q, %v", got, err)
	}
}

func TestOpenWrongKeyFailsClosed(t *testing.T) {
	var k1, k2 [32]byte
	copy(k1[:], bytes.Repeat([]byte{1}, 32))
	copy(k2[:], bytes.Repeat([]byte{2}, 32))
	sealed := sealBytes(k1, []byte("secret"))
	if _, err := openBytes(k2, sealed); err == nil {
		t.Fatal("expected auth failure with wrong key")
	}
}

func TestOpenTooShortFailsClosed(t *testing.T) {
	var key [32]byte
	if _, err := openBytes(key, []byte{1, 2, 3}); err == nil {
		t.Fatal("expected error on too-short input")
	}
}

func TestSavedCryptoPassphraseRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sc, _ := loadSavedCrypto(dir)
	sealed := sealBytes(sc.dek, []byte("payload")) // seal under the original DEK

	if err := sc.setPassphrase("correct horse"); err != nil {
		t.Fatal(err)
	}
	// A fresh load is now locked (passphrase mode, no in-memory DEK).
	locked, err := loadSavedCrypto(dir)
	if err != nil || !locked.locked {
		t.Fatalf("expected locked instance after setPassphrase: %+v %v", locked, err)
	}
	// Wrong passphrase fails closed.
	if _, err := unlockSavedCrypto(dir, "wrong"); err != errBadPassphrase {
		t.Fatalf("wrong passphrase err = %v, want errBadPassphrase", err)
	}
	// Correct passphrase recovers the SAME DEK (existing data still opens).
	un, err := unlockSavedCrypto(dir, "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := openBytes(un.dek, sealed); err != nil || string(got) != "payload" {
		t.Fatalf("DEK changed across passphrase wrap: %q %v", got, err)
	}
}

func TestSavedCryptoRemovePassphraseKeepsData(t *testing.T) {
	dir := t.TempDir()
	sc, _ := loadSavedCrypto(dir)
	sealed := sealBytes(sc.dek, []byte("payload"))
	if err := sc.setPassphrase("pw"); err != nil {
		t.Fatal(err)
	}
	un, err := unlockSavedCrypto(dir, "pw")
	if err != nil {
		t.Fatal(err)
	}
	if err := un.removePassphrase(); err != nil {
		t.Fatal(err)
	}
	// Back to transparent device mode, DEK preserved, data still opens.
	reloaded, err := loadSavedCrypto(dir)
	if err != nil || reloaded.locked || reloaded.mode != "device" {
		t.Fatalf("expected unlocked device mode: %+v %v", reloaded, err)
	}
	if got, err := openBytes(reloaded.dek, sealed); err != nil || string(got) != "payload" {
		t.Fatalf("data lost across removePassphrase: %q %v", got, err)
	}
}

// If the keyring is lost but a SEALED saved.json remains (e.g. an inconsistent
// OS backup-restore), loadSavedCrypto must LOCK and preserve the bytes — never
// regenerate a fresh DEK, which would orphan the user's data forever.
func TestSavedCryptoLostKeyringWithSealedStoreLocks(t *testing.T) {
	dir := t.TempDir()
	// Establish a device-mode store and seal some data with its DEK.
	sc1, err := loadSavedCrypto(dir)
	if err != nil {
		t.Fatal(err)
	}
	sealed := sealBytes(sc1.dek, []byte("{\"items\":[]}"))
	if err := os.WriteFile(filepath.Join(dir, "saved.json"), sealed, 0o600); err != nil {
		t.Fatal(err)
	}
	// Simulate the keyring + device key vanishing (only the sealed store is left).
	os.Remove(filepath.Join(dir, "saved", "keyring.json"))
	os.Remove(filepath.Join(dir, "saved", "devicekey"))

	sc2, err := loadSavedCrypto(dir)
	if err != nil {
		t.Fatalf("loadSavedCrypto err = %v", err)
	}
	if !sc2.locked {
		t.Fatal("expected LOCKED instance when keyring is lost but a sealed store exists")
	}
	// The sealed store must be untouched (no fresh keyring written over it).
	if _, err := os.Stat(filepath.Join(dir, "saved", "keyring.json")); err == nil {
		t.Fatal("a new keyring was generated — sealed data would be orphaned")
	}
	got, err := os.ReadFile(filepath.Join(dir, "saved.json"))
	if err != nil || !bytes.Equal(got, sealed) {
		t.Fatal("sealed saved.json was modified/lost")
	}
}

// Lost keyring with a legacy PLAINTEXT saved.json is a safe migration: init a
// device key and re-seal (no data loss).
func TestSavedCryptoLostKeyringWithPlaintextMigrates(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "saved.json"), []byte("{\"items\":[]}"), 0o600); err != nil {
		t.Fatal(err)
	}
	sc, err := loadSavedCrypto(dir)
	if err != nil {
		t.Fatalf("loadSavedCrypto err = %v", err)
	}
	if sc.locked || sc.mode != "device" {
		t.Fatalf("expected unlocked device mode for legacy plaintext, got %+v", sc)
	}
}

func TestSavedCryptoDeviceKeyPersists(t *testing.T) {
	dir := t.TempDir()
	sc1, err := loadSavedCrypto(dir)
	if err != nil {
		t.Fatal(err)
	}
	sealed := sealBytes(sc1.dek, []byte("x"))

	// Reload from disk — same DEK recovered, so the blob still opens.
	sc2, err := loadSavedCrypto(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := openBytes(sc2.dek, sealed); err != nil || string(got) != "x" {
		t.Fatalf("DEK not stable across reload: %q %v", got, err)
	}
	if sc2.locked {
		t.Fatal("device-mode crypto should never be locked")
	}
}
