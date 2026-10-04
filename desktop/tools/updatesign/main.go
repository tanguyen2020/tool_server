// Command updatesign manages the signed update manifest of a release.
//
//	updatesign genkey -out <file>                      create a key pair: the private key goes to <file>,
//	                                                   the public key is printed (put it in updater.PublicKey)
//	updatesign manifest -version 1.2.3 -dir dist       write dist/latest.json from the release files
//	updatesign sign -in dist/latest.json               sign with $UPDATE_SIGNING_KEY → latest.json.sig
//	updatesign verify -in dist/latest.json -pub <key>  check a signature
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Release files used by the in-app updater, per platform key (see updater.PlatformKey).
var platformFiles = map[string]string{
	"windows-amd64":    "ServerDashboard-windows-amd64.exe",
	"darwin-universal": "ServerDashboard-macos-universal",
	"linux-amd64":      "ServerDashboard-linux-amd64",
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	out := fs.String("out", "", "output file")
	in := fs.String("in", "", "input file")
	dir := fs.String("dir", "", "directory with the release files")
	version := fs.String("version", "", "release version (x.y.z)")
	notes := fs.String("notes", "", "release notes")
	pub := fs.String("pub", "", "public key (base64)")
	_ = fs.Parse(os.Args[2:])

	switch os.Args[1] {
	case "genkey":
		if *out == "" {
			fail("-out is required")
		}
		pk, sk, err := ed25519.GenerateKey(rand.Reader)
		check(err)
		check(os.WriteFile(*out, []byte(base64.StdEncoding.EncodeToString(sk)+"\n"), 0o600))
		fmt.Println(base64.StdEncoding.EncodeToString(pk))
	case "manifest":
		if *version == "" || *dir == "" {
			fail("-version and -dir are required")
		}
		m := map[string]any{"version": strings.TrimPrefix(*version, "v"), "notes": *notes, "published": time.Now().UTC().Format(time.RFC3339)}
		assets := map[string]any{}
		for key, name := range platformFiles {
			path := filepath.Join(*dir, name)
			f, err := os.Open(path)
			if os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "warning: %s missing, %s will not be updated\n", name, key)
				continue
			}
			check(err)
			h := sha256.New()
			n, err := io.Copy(h, f)
			f.Close()
			check(err)
			assets[key] = map[string]any{"name": name, "sha256": hex.EncodeToString(h.Sum(nil)), "size": n}
		}
		if len(assets) == 0 {
			fail("no release files found in " + *dir)
		}
		m["assets"] = assets
		b, _ := json.MarshalIndent(m, "", "  ")
		target := *out
		if target == "" {
			target = filepath.Join(*dir, "latest.json")
		}
		check(os.WriteFile(target, b, 0o644))
		fmt.Println("wrote", target)
	case "sign":
		raw := strings.TrimSpace(os.Getenv("UPDATE_SIGNING_KEY"))
		if raw == "" {
			fail("UPDATE_SIGNING_KEY is not set")
		}
		sk, err := base64.StdEncoding.DecodeString(raw)
		if err != nil || len(sk) != ed25519.PrivateKeySize {
			fail("UPDATE_SIGNING_KEY is not a base64 Ed25519 private key")
		}
		body, err := os.ReadFile(*in)
		check(err)
		sig := ed25519.Sign(ed25519.PrivateKey(sk), body)
		target := *out
		if target == "" {
			target = *in + ".sig"
		}
		check(os.WriteFile(target, []byte(base64.StdEncoding.EncodeToString(sig)+"\n"), 0o644))
		fmt.Println("signed", *in)
	case "verify":
		pk, err := base64.StdEncoding.DecodeString(*pub)
		if err != nil || len(pk) != ed25519.PublicKeySize {
			fail("-pub must be a base64 Ed25519 public key")
		}
		body, err := os.ReadFile(*in)
		check(err)
		sigRaw, err := os.ReadFile(*in + ".sig")
		check(err)
		sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigRaw)))
		if err != nil || !ed25519.Verify(pk, body, sig) {
			fail("signature does NOT match")
		}
		fmt.Println("signature OK")
	default:
		usage()
	}
}

func usage() {
	fail("usage: updatesign genkey|manifest|sign|verify [flags]")
}

func check(err error) {
	if err != nil {
		fail(err.Error())
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
