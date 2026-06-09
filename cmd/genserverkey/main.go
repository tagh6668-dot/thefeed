// Command genserverkey generates a server signing keypair for thefeed.
//
// The ed25519 keypair serves two roles: signing server responses/feed
// metadata, and (via ed25519->curve25519 conversion, done in the client)
// encrypting routing metadata to the server. One key, both jobs.
//
//	go run ./cmd/genserverkey
//
// Store the printed private seed on the server (secret). Paste the public
// key into the config / the bundled default configs.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
)

func main() {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintln(os.Stderr, "keygen:", err)
		os.Exit(1)
	}

	// Store the 32-byte seed, not the 64-byte expanded key — the full
	// private key and public key both derive from it.
	seed := priv.Seed()

	fmt.Println("server private seed (KEEP SECRET — store on the server):")
	fmt.Println("  " + base64.StdEncoding.EncodeToString(seed))
	fmt.Println()
	fmt.Println("server public key (paste into config + bundled default configs):")
	fmt.Println("  " + base64.RawURLEncoding.EncodeToString(pub))
}
