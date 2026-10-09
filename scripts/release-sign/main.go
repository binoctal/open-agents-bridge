// Command release-sign manages the Ed25519 key that signs release
// checksums.txt files (bridge-abuse-hardening 5e.2).
//
//	release-sign keygen [-key PATH]            create a keypair; prints ONLY the public key
//	release-sign pubkey [-key PATH]            print the public key of an existing key file
//	release-sign sign   [-key PATH] -in checksums.txt [-out checksums.txt.sig]
//
// The private key lives outside the repository (default
// ~/.config/open-agents/release-signing/ed25519.key, mode 0600) and is never
// printed. The signature file is the base64 of the raw 64-byte Ed25519
// signature over the exact bytes of checksums.txt.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func defaultKeyPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		fatalf("cannot resolve home directory: %v", err)
	}
	return filepath.Join(home, ".config", "open-agents", "release-signing", "ed25519.key")
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "release-sign: "+format+"\n", a...)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: release-sign keygen|pubkey|sign [flags]")
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	keyPath := fs.String("key", defaultKeyPath(), "private key file")
	in := fs.String("in", "", "sign: file to sign (checksums.txt)")
	out := fs.String("out", "", "sign: signature output (default <in>.sig)")
	_ = fs.Parse(args)

	switch cmd {
	case "keygen":
		keygen(*keyPath)
	case "pubkey":
		fmt.Println(base64.StdEncoding.EncodeToString(load(*keyPath).Public().(ed25519.PublicKey)))
	case "sign":
		if *in == "" {
			fatalf("sign: -in is required")
		}
		if *out == "" {
			*out = *in + ".sig"
		}
		sign(*keyPath, *in, *out)
	default:
		fatalf("unknown command %q", cmd)
	}
}

func keygen(path string) {
	if _, err := os.Stat(path); err == nil {
		fatalf("%s already exists; refusing to overwrite a signing key", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		fatalf("create key directory: %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fatalf("generate key: %v", err)
	}
	enc := base64.StdEncoding.EncodeToString(priv.Seed()) + "\n"
	// O_EXCL: never clobber a key that appeared between the Stat and here.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		fatalf("write key: %v", err)
	}
	if _, err := f.WriteString(enc); err != nil {
		f.Close()
		fatalf("write key: %v", err)
	}
	if err := f.Close(); err != nil {
		fatalf("write key: %v", err)
	}
	fmt.Fprintf(os.Stderr, "private key written to %s (mode 0600); back it up offline\n", path)
	fmt.Println(base64.StdEncoding.EncodeToString(pub))
}

func load(path string) ed25519.PrivateKey {
	raw, err := os.ReadFile(path)
	if err != nil {
		fatalf("read key: %v", err)
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		fatalf("%s is not a valid release signing key", path)
	}
	return ed25519.NewKeyFromSeed(seed)
}

func sign(keyPath, in, out string) {
	priv := load(keyPath)
	data, err := os.ReadFile(in)
	if err != nil {
		fatalf("read %s: %v", in, err)
	}
	sig := ed25519.Sign(priv, data)
	if !ed25519.Verify(priv.Public().(ed25519.PublicKey), data, sig) {
		fatalf("self-check failed")
	}
	if err := os.WriteFile(out, []byte(base64.StdEncoding.EncodeToString(sig)+"\n"), 0o644); err != nil {
		fatalf("write %s: %v", out, err)
	}
	fmt.Fprintf(os.Stderr, "signed %s -> %s\n", in, out)
}
