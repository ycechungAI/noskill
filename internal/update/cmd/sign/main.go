// Command sign signs a file with the ed25519 release key (scripts/sign-checksums.sh) or creates a
// new key pair (-gen). The private key is read from NOSKILL_RELEASE_KEY (base64 seed) and never
// printed; the signature goes to standard output as base64.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "-gen" {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("public key (embed with -X noskill/internal/update.PublicKey=…; keep it in the repo):")
		fmt.Println(base64.StdEncoding.EncodeToString(pub))
		fmt.Println("private seed (store ONLY as the NOSKILL_RELEASE_KEY secret; never commit it):")
		fmt.Println(base64.StdEncoding.EncodeToString(priv.Seed()))
		return
	}
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: sign <file>   (NOSKILL_RELEASE_KEY = base64 ed25519 seed)")
		os.Exit(2)
	}
	seed, err := base64.StdEncoding.DecodeString(os.Getenv("NOSKILL_RELEASE_KEY"))
	if err != nil || len(seed) != ed25519.SeedSize {
		fmt.Fprintln(os.Stderr, "NOSKILL_RELEASE_KEY must be a base64 ed25519 seed (see: sign -gen)")
		os.Exit(2)
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(base64.StdEncoding.EncodeToString(ed25519.Sign(ed25519.NewKeyFromSeed(seed), data)))
}
