package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type archiveEntry struct {
	hdr  tar.Header
	body []byte
}

func readReleaseArchive(t *testing.T, archivePath string) []archiveEntry {
	t.Helper()
	f, err := os.Open(archivePath)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var entries []archiveEntry
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read tar entry %q: %v", hdr.Name, err)
		}
		entries = append(entries, archiveEntry{hdr: *hdr, body: data})
	}
	return entries
}

func runReleaseAsset(t *testing.T, version, arch, outDir, keyPath, keyID, epoch string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command("scripts/release-asset.sh", version, arch, outDir)
	env := append([]string(nil), os.Environ()...)
	env = append(env,
		"SIGNING_KEY="+keyPath,
		"SIGNING_KEY_ID="+keyID,
	)
	if epoch != "" {
		env = append(env, "SOURCE_DATE_EPOCH="+epoch)
	}
	cmd.Env = env
	return cmd.CombinedOutput()
}

func cleanupStageDir(t *testing.T, arch string) {
	t.Helper()
	stageDir := filepath.Join("dist", "stage-"+arch)
	t.Cleanup(func() {
		_ = os.RemoveAll(stageDir)
		// Remove dist/ only if empty so we don't disturb unrelated files.
		_ = os.Remove("dist")
	})
}

func TestReleaseAsset_アーカイブ構造とマニフェストと署名と再現性を検証する(t *testing.T) {
	openssl := findOpenSSL3(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found")
	}

	cleanupStageDir(t, "amd64")

	keyDir := t.TempDir()
	privPath, wantPubText := generateTestKey(t, openssl, keyDir)

	const (
		version   = "0.2.3"
		arch      = "amd64"
		keyID     = "test-release-2026"
		epochStr  = "1700000000"
		epochInt  = int64(1700000000)
		installTo = "/usr/lib/db-branch/runtimes/clickhouse"
		binName   = "db-branch-rt-clickhouse"
	)

	out1 := t.TempDir()
	if out, err := runReleaseAsset(t, version, arch, out1, privPath, keyID, epochStr); err != nil {
		t.Fatalf("release-asset.sh (run 1) failed: %v\n%s", err, out)
	}

	archiveName := binName + "_" + version + "_linux_" + arch + ".tar.gz"
	archivePath1 := filepath.Join(out1, archiveName)
	entries := readReleaseArchive(t, archivePath1)

	// (a) Flat regular files only, allowlisted names, no leading ./, exact modes & metadata.
	allowed := map[string]int64{
		binName:             0o755,
		"manifest.json":     0o644,
		"manifest.json.sig": 0o644,
		"runtimes.json":     0o644,
		keyID + ".pub":      0o644,
	}
	byName := make(map[string]archiveEntry, len(entries))
	for _, e := range entries {
		name := e.hdr.Name
		if name == "" || strings.HasPrefix(name, ".") || strings.Contains(name, "..") || strings.ContainsAny(name, `/\`) {
			t.Fatalf("unsafe tar entry name: %q", name)
		}
		if e.hdr.Typeflag != tar.TypeReg {
			t.Fatalf("entry %q: typeflag = %q, want regular file", name, e.hdr.Typeflag)
		}
		if e.hdr.Mode&0o7000 != 0 {
			t.Fatalf("entry %q: special mode bits set: %04o", name, e.hdr.Mode)
		}
		wantMode, ok := allowed[name]
		if !ok {
			t.Fatalf("unexpected entry %q in archive", name)
		}
		if gotMode := e.hdr.Mode & 0o777; gotMode != wantMode {
			t.Fatalf("entry %q: mode = %04o, want %04o", name, gotMode, wantMode)
		}
		if e.hdr.Uid != 0 || e.hdr.Gid != 0 || e.hdr.Uname != "" || e.hdr.Gname != "" {
			t.Fatalf("entry %q: unexpected owner uid=%d gid=%d uname=%q gname=%q",
				name, e.hdr.Uid, e.hdr.Gid, e.hdr.Uname, e.hdr.Gname)
		}
		if e.hdr.ModTime.Unix() != epochInt {
			t.Fatalf("entry %q: mtime = %d, want %d", name, e.hdr.ModTime.Unix(), epochInt)
		}
		if _, dup := byName[name]; dup {
			t.Fatalf("duplicate entry %q", name)
		}
		byName[name] = e
	}
	if len(byName) != len(allowed) {
		t.Fatalf("archive entry count = %d, want %d", len(byName), len(allowed))
	}

	// (b) manifest.json paths, SHA-256 digests, and module_version.
	var manifest struct {
		Engine        string `json:"engine"`
		ModuleVersion string `json:"module_version"`
		Binary        string `json:"binary"`
		BinarySHA256  string `json:"binary_sha256"`
		Catalog       string `json:"catalog"`
		CatalogSHA256 string `json:"catalog_sha256"`
		RunAsUser     string `json:"run_as_user"`
	}
	if err := json.Unmarshal(byName["manifest.json"].body, &manifest); err != nil {
		t.Fatalf("unmarshal manifest.json: %v", err)
	}
	if manifest.Engine != "clickhouse" {
		t.Fatalf("engine = %q, want %q", manifest.Engine, "clickhouse")
	}
	if manifest.ModuleVersion != version {
		t.Fatalf("module_version = %q, want %q", manifest.ModuleVersion, version)
	}
	if manifest.Binary != installTo+"/"+binName {
		t.Fatalf("binary = %q, want %q", manifest.Binary, installTo+"/"+binName)
	}
	if manifest.Catalog != installTo+"/runtimes.json" {
		t.Fatalf("catalog = %q, want %q", manifest.Catalog, installTo+"/runtimes.json")
	}
	if manifest.RunAsUser != "db-branch-rt-clickhouse" {
		t.Fatalf("run_as_user = %q, want %q", manifest.RunAsUser, "db-branch-rt-clickhouse")
	}

	binBytes := byName[binName].body
	if !bytes.HasPrefix(binBytes, []byte{0x7f, 'E', 'L', 'F'}) {
		t.Fatalf("binary does not start with ELF magic")
	}
	binSum := sha256.Sum256(binBytes)
	if manifest.BinarySHA256 != hex.EncodeToString(binSum[:]) {
		t.Fatalf("binary_sha256 = %q, want %q", manifest.BinarySHA256, hex.EncodeToString(binSum[:]))
	}
	catSum := sha256.Sum256(byName["runtimes.json"].body)
	if manifest.CatalogSHA256 != hex.EncodeToString(catSum[:]) {
		t.Fatalf("catalog_sha256 = %q, want %q", manifest.CatalogSHA256, hex.EncodeToString(catSum[:]))
	}

	// (c) manifest.json.sig verifies with crypto/ed25519 using <key_id>.pub from the archive.
	pubData := byName[keyID+".pub"].body
	if string(pubData) != wantPubText {
		t.Fatalf("bundled %s.pub = %q, want %q", keyID, string(pubData), wantPubText)
	}
	pubRaw, err := base64.StdEncoding.DecodeString(strings.TrimRight(string(pubData), "\r\n"))
	if err != nil || len(pubRaw) != ed25519.PublicKeySize {
		t.Fatalf("decode bundled public key: err=%v len=%d", err, len(pubRaw))
	}

	verifyDir := t.TempDir()
	keysDir := filepath.Join(verifyDir, "keys")
	if err := os.MkdirAll(keysDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manPath := filepath.Join(verifyDir, "manifest.json")
	sigPath := filepath.Join(verifyDir, "manifest.json.sig")
	if err := os.WriteFile(manPath, byName["manifest.json"].body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sigPath, byName["manifest.json.sig"].body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keysDir, keyID+".pub"), pubData, 0o644); err != nil {
		t.Fatal(err)
	}
	gotKeyID := verifyLikeLoader(t, manPath, sigPath, keysDir)
	if gotKeyID != keyID {
		t.Fatalf("verified key_id = %q, want %q", gotKeyID, keyID)
	}

	// (d) Reproducible output: a second run with the same inputs produces identical bytes.
	out2 := t.TempDir()
	if out, err := runReleaseAsset(t, version, arch, out2, privPath, keyID, epochStr); err != nil {
		t.Fatalf("release-asset.sh (run 2) failed: %v\n%s", err, out)
	}
	raw1, err := os.ReadFile(archivePath1)
	if err != nil {
		t.Fatal(err)
	}
	raw2, err := os.ReadFile(filepath.Join(out2, archiveName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw1, raw2) {
		h1 := sha256.Sum256(raw1)
		h2 := sha256.Sum256(raw2)
		t.Fatalf("archive is not reproducible: run1=%x (%d bytes) != run2=%x (%d bytes)",
			h1, len(raw1), h2, len(raw2))
	}
}

func TestReleaseAsset_不正なversionやarchやkey_idを拒否する(t *testing.T) {
	openssl := findOpenSSL3(t)
	keyDir := t.TempDir()
	privPath, _ := generateTestKey(t, openssl, keyDir)

	badVersions := []string{
		"",
		"v0.1.0",
		"0.1",
		"01.0.0",
		"0.1.0.0",
		"abc",
		"0.1.0-01",
		"0.1.0/bad",
		"0.1.0 bad",
	}
	for _, ver := range badVersions {
		outDir := t.TempDir()
		if out, err := runReleaseAsset(t, ver, "amd64", outDir, privPath, "test-key", "0"); err == nil {
			t.Fatalf("version %q: expected failure, got success: %s", ver, out)
		}
		matches, _ := filepath.Glob(filepath.Join(outDir, "*.tar.gz"))
		if len(matches) != 0 {
			t.Fatalf("version %q: archive must not be created on failure: %v", ver, matches)
		}
	}

	badArches := []string{
		"",
		"386",
		"x86_64",
		"aarch64",
		"arm",
		"darwin",
	}
	for _, arch := range badArches {
		outDir := t.TempDir()
		if out, err := runReleaseAsset(t, "0.1.0", arch, outDir, privPath, "test-key", "0"); err == nil {
			t.Fatalf("arch %q: expected failure, got success: %s", arch, out)
		}
		matches, _ := filepath.Glob(filepath.Join(outDir, "*.tar.gz"))
		if len(matches) != 0 {
			t.Fatalf("arch %q: archive must not be created on failure: %v", arch, matches)
		}
	}

	badKeyIDs := []string{
		"",
		".leading-dot",
		"bad..dots",
		"bad/id",
	}
	for _, id := range badKeyIDs {
		outDir := t.TempDir()
		if out, err := runReleaseAsset(t, "0.1.0", "amd64", outDir, privPath, id, "0"); err == nil {
			t.Fatalf("key_id %q: expected failure, got success: %s", id, out)
		}
		matches, _ := filepath.Glob(filepath.Join(outDir, "*.tar.gz"))
		if len(matches) != 0 {
			t.Fatalf("key_id %q: archive must not be created on failure: %v", id, matches)
		}
	}
}
