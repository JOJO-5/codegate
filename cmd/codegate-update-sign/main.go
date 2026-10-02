// codegate-update-sign signs already built platform artifacts with an offline
// Ed25519 key. The private key must never be copied into the runtime image.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/updatefile"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "signing failed:", err)
		os.Exit(1)
	}
}
func run() error {
	keyFile := flag.String("key", "", "base64 Ed25519 private key file")
	dir := flag.String("dir", "", "platform artifact directory (os/arch)")
	generate := flag.String("generate-key", "", "create a new private key file; prints only its public key")
	flag.Parse()
	if *generate != "" {
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(*generate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err = f.WriteString(base64.StdEncoding.EncodeToString(private) + "\n"); err != nil {
			return err
		}
		fmt.Println(base64.StdEncoding.EncodeToString(public))
		return nil
	}
	if *keyFile == "" || *dir == "" {
		return fmt.Errorf("-key and -dir are required")
	}
	info, err := os.Stat(*keyFile)
	if err != nil || info.Size() > 1024 {
		return fmt.Errorf("invalid key file")
	}
	raw, err := os.ReadFile(*keyFile)
	if err != nil {
		return err
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) != ed25519.PrivateKeySize {
		return fmt.Errorf("invalid Ed25519 private key")
	}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, arch := range []string{"amd64", "arm64"} {
			platform := filepath.Join(*dir, goos, arch)
			versionBytes, err := os.ReadFile(filepath.Join(platform, "version.txt"))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			version := strings.TrimSpace(string(versionBytes))
			if !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(version) {
				return fmt.Errorf("invalid artifact version")
			}
			name := "codegate-agent"
			if goos == "windows" {
				name += ".exe"
			}
			f, err := os.Open(filepath.Join(platform, name))
			if err != nil {
				return err
			}
			sum := sha256.New()
			size, err := io.Copy(sum, io.LimitReader(f, (100<<20)+1))
			f.Close()
			if err != nil {
				return err
			}
			if size <= 0 || size > 100<<20 {
				return fmt.Errorf("invalid artifact size")
			}
			signature := ed25519.Sign(key, protocol.ReleaseSigningPayload(version, goos, arch, hex.EncodeToString(sum.Sum(nil)), size))
			target := filepath.Join(platform, "signature.txt")
			tmp, err := os.CreateTemp(platform, ".signature-*")
			if err != nil {
				return err
			}
			_, err = tmp.WriteString(base64.StdEncoding.EncodeToString(signature) + "\n")
			if err == nil {
				err = tmp.Sync()
			}
			closeErr := tmp.Close()
			if err == nil {
				err = closeErr
			}
			if err == nil {
				err = updatefile.Replace(tmp.Name(), target)
			}
			_ = os.Remove(tmp.Name())
			if err != nil {
				return err
			}
			fmt.Printf("signed %s/%s %s\n", goos, arch, version)
		}
	}
	return nil
}
