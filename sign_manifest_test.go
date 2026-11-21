package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var (
	testKeyIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	openSSLVerRe     = regexp.MustCompile(`^OpenSSL ([0-9]+)\.`)
)

// findOpenSSL3 matches sign-manifest.sh's OpenSSL selection (Homebrew first,
// refusing LibreSSL and OpenSSL < 3). Skips the test when unavailable.
func findOpenSSL3(t *testing.T) string {
	t.Helper()
	var bin string
	if fi, err := os.Stat("/opt/homebrew/bin/openssl"); err == nil && !fi.IsDir() && fi.Mode().Perm()&0o111 != 0 {
		bin = "/opt/homebrew/bin/openssl"
	} else if p, err := exec.LookPath("openssl"); err == nil {
		bin = p
	} else {
		t.Skip("openssl not found")
	}
	out, err := exec.Command(bin, "version").CombinedOutput()
	if err != nil {
		t.Skipf("openssl version failed: %v", err)
	}
	ver := strings.TrimSpace(string(out))
	if strings.Contains(ver, "LibreSSL") {
		t.Skipf("LibreSSL is not supported: %s", ver)
	}
	m := openSSLVerRe.FindStringSubmatch(ver)
	if len(m) < 2 {
		t.Skipf("unsupported openssl version: %s", ver)
	}
	major, err := strconv.Atoi(m[1])
	if err != nil || major < 3 {
		t.Skipf("OpenSSL 3+ is required: %s", ver)
	}
	return bin
}

// generateTestKey creates a disposable Ed25519 key pair in dir and returns the
// private key path (mode 0600) and the base64-encoded 32-byte raw public key
// (the exact format expected in /etc/db-branch/trusted-keys/<key_id>.pub).
func generateTestKey(t *testing.T, openssl, dir string) (string, string) {
	t.Helper()
	privPath := filepath.Join(dir, "signing.pem")
	if out, err := exec.Command(openssl, "genpkey", "-algorithm", "ED25519", "-out", privPath).CombinedOutput(); err != nil {
		t.Fatalf("openssl genpkey failed: %v: %s", err, out)
	}
	if err := os.Chmod(privPath, 0o600); err != nil {
		t.Fatal(err)
	}
	der, err := exec.Command(openssl, "pkey", "-in", privPath, "-pubout", "-outform", "DER").Output()
	if err != nil {
		t.Fatalf("openssl pkey -pubout failed: %v", err)
	}
	if len(der) < ed25519.PublicKeySize {
		t.Fatalf("unexpected DER public key length: %d", len(der))
	}
	rawPub := der[len(der)-ed25519.PublicKeySize:]
	return privPath, base64.StdEncoding.EncodeToString(rawPub) + "\n"
}

// verifyLikeLoader reproduces db-branch's runtimemodule.Loader.verifySignature
// logic using crypto/ed25519.
func verifyLikeLoader(t *testing.T, manifestPath, sigPath, keysDir string) string {
	t.Helper()
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	sigData, err := os.ReadFile(sigPath)
	if err != nil {
		t.Fatalf("read signature: %v", err)
	}
	var sf struct {
		KeyID     string `json:"key_id"`
		Signature string `json:"signature"`
	}
	dec := json.NewDecoder(bytes.NewReader(sigData))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sf); err != nil {
		t.Fatalf("decode signature json: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		t.Fatalf("trailing data in signature file: %v", err)
	}
	if !testKeyIDPattern.MatchString(sf.KeyID) {
		t.Fatalf("invalid key_id: %q", sf.KeyID)
	}
	sigBytes, err := base64.StdEncoding.DecodeString(sf.Signature)
	if err != nil || len(sigBytes) != ed25519.SignatureSize {
		t.Fatalf("invalid signature: err=%v len=%d", err, len(sigBytes))
	}
	keyData, err := os.ReadFile(filepath.Join(keysDir, sf.KeyID+".pub"))
	if err != nil {
		t.Fatalf("read public key: %v", err)
	}
	pubBytes, err := base64.StdEncoding.DecodeString(strings.TrimRight(string(keyData), "\r\n"))
	if err != nil || len(pubBytes) != ed25519.PublicKeySize {
		t.Fatalf("invalid public key: err=%v len=%d", err, len(pubBytes))
	}
	if !ed25519.Verify(ed25519.PublicKey(pubBytes), manifestData, sigBytes) {
		t.Fatalf("ed25519.Verify failed for key_id %q", sf.KeyID)
	}
	// Modifying a single byte of manifest.json must invalidate the signature.
	tampered := append([]byte(nil), manifestData...)
	tampered[0] ^= 0xff
	if ed25519.Verify(ed25519.PublicKey(pubBytes), tampered, sigBytes) {
		t.Fatal("ed25519.Verify unexpectedly succeeded on tampered manifest")
	}
	return sf.KeyID
}

func TestSignManifest_署名と検証がloaderの手順と一致する(t *testing.T) {
	openssl := findOpenSSL3(t)
	dir := t.TempDir()
	privPath, pubText := generateTestKey(t, openssl, dir)

	const keyID = "test-key_2026.1"
	keysDir := filepath.Join(dir, "trusted-keys")
	if err := os.MkdirAll(keysDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keysDir, keyID+".pub"), []byte(pubText), 0o644); err != nil {
		t.Fatal(err)
	}

	manifestPath := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(`{"schema_version":1,"engine":"clickhouse"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("scripts/sign-manifest.sh", manifestPath, privPath, keyID)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sign-manifest.sh failed: %v: %s", err, out)
	}

	gotKeyID := verifyLikeLoader(t, manifestPath, manifestPath+".sig", keysDir)
	if gotKeyID != keyID {
		t.Fatalf("key_id = %q, want %q", gotKeyID, keyID)
	}
}

func TestSignManifest_権限が緩い鍵は拒否する(t *testing.T) {
	openssl := findOpenSSL3(t)
	dir := t.TempDir()
	privPath, _ := generateTestKey(t, openssl, dir)

	manifestPath := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(`{"schema_version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, mode := range []os.FileMode{0o640, 0o644, 0o604, 0o660, 0o700} {
		if err := os.Chmod(privPath, mode); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("scripts/sign-manifest.sh", manifestPath, privPath, "test-key").CombinedOutput()
		if err == nil {
			t.Fatalf("mode %04o: expected failure, got success: %s", mode, out)
		}
		if _, statErr := os.Stat(manifestPath + ".sig"); statErr == nil {
			t.Fatalf("mode %04o: .sig file must not be created on failure", mode)
		}
	}
}

func TestSignManifest_不正なkey_idは拒否する(t *testing.T) {
	openssl := findOpenSSL3(t)
	dir := t.TempDir()
	privPath, _ := generateTestKey(t, openssl, dir)

	manifestPath := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(`{"schema_version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, badID := range []string{"", "bad/id", "bad id", "../escape", strings.Repeat("a", 65)} {
		out, err := exec.Command("scripts/sign-manifest.sh", manifestPath, privPath, badID).CombinedOutput()
		if err == nil {
			t.Fatalf("key_id %q: expected failure, got success: %s", badID, out)
		}
		if _, statErr := os.Stat(manifestPath + ".sig"); statErr == nil {
			t.Fatalf("key_id %q: .sig file must not be created on failure", badID)
		}
	}
}

func TestMkmanifest_ステージングからSHA256を計算しインストール先パスを書く(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found")
	}
	stage := t.TempDir()
	binContent := []byte("#!/bin/sh\necho ok\n")
	if err := os.WriteFile(filepath.Join(stage, "db-branch-rt-clickhouse"), binContent, 0o755); err != nil {
		t.Fatal(err)
	}

	const installDir = "/usr/lib/db-branch/runtimes/clickhouse"
	if out, err := exec.Command("python3", "scripts/mkmanifest.py", installDir, stage, "0.2.3").CombinedOutput(); err != nil {
		t.Fatalf("mkmanifest.py failed: %v: %s", err, out)
	}

	manData, err := os.ReadFile(filepath.Join(stage, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		ModuleVersion string `json:"module_version"`
		Binary        string `json:"binary"`
		BinarySHA256  string `json:"binary_sha256"`
		Catalog       string `json:"catalog"`
		CatalogSHA256 string `json:"catalog_sha256"`
	}
	if err := json.Unmarshal(manData, &m); err != nil {
		t.Fatal(err)
	}

	wantBinSum := sha256.Sum256(binContent)
	catData, err := os.ReadFile(filepath.Join(stage, "runtimes.json"))
	if err != nil {
		t.Fatal(err)
	}
	wantCatSum := sha256.Sum256(catData)

	if m.ModuleVersion != "0.2.3" ||
		m.Binary != installDir+"/db-branch-rt-clickhouse" ||
		m.BinarySHA256 != hex.EncodeToString(wantBinSum[:]) ||
		m.Catalog != installDir+"/runtimes.json" ||
		m.CatalogSHA256 != hex.EncodeToString(wantCatSum[:]) {
		t.Fatalf("unexpected manifest: %+v", m)
	}
}
