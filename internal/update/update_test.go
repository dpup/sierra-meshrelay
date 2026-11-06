package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v0.7.0", "v0.6.1", true}, {"v0.6.1", "v0.7.0", false}, {"v0.7.0", "v0.7.0", false},
		{"v1.0.0", "v0.99.99", true}, {"v0.10.0", "v0.9.0", true}, {"v0.7.0", "dev", false}, {"dev", "v0.1.0", false},
		{"v0.7.1-rc1", "v0.7.0", true},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

func TestAssetName(t *testing.T) {
	for _, c := range [][4]string{
		{"linux", "amd64", "", "meshbridge_linux_amd64.tar.gz"}, {"linux", "arm", "7", "meshbridge_linux_armv7.tar.gz"},
		{"linux", "arm", "6", "meshbridge_linux_armv6.tar.gz"}, {"windows", "arm64", "", "meshbridge_windows_arm64.zip"},
	} {
		if got := assetName(c[0], c[1], c[2]); got != c[3] {
			t.Errorf("%v: %s", c, got)
		}
	}
}

func TestBuiltInKey(t *testing.T) {
	if len(TrustedKeys()) == 0 {
		t.Fatal("no release signing key built in")
	}
}

// release serves a fake release whose meshbridge is a shell script that
// prints its version.
func release(t *testing.T, key ed25519.PrivateKey, version string) (*httptest.Server, map[string][]byte) {
	return releaseOf(t, key, version, version)
}

// releaseOf signs the release as version, with a binary that says it is binVersion.
func releaseOf(t *testing.T, key ed25519.PrivateKey, version, binVersion string) (*httptest.Server, map[string][]byte) {
	t.Helper()
	var tgz bytes.Buffer
	gz := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gz)
	script := []byte("#!/bin/sh\necho \"meshbridge " + binVersion + " (test)\"\n")
	_ = tw.WriteHeader(&tar.Header{Name: "./meshbridge", Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(script)
	tw.Close()
	gz.Close()
	asset := AssetName()
	sum := sha256.Sum256(tgz.Bytes())
	sums := []byte(hex.EncodeToString(sum[:]) + "  " + asset + "\n")
	files := map[string][]byte{asset: tgz.Bytes(), SumsFile: sums, SigFile: []byte(Sign(key, version, sums) + "\n")}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[filepath.Base(r.URL.Path)]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv, files
}

func TestStageAndApply(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as the new binary")
	}
	pub, priv, _ := ed25519.GenerateKey(nil)
	keys := []ed25519.PublicKey{pub}
	srv, files := release(t, priv, "v9.9.9")
	dir := t.TempDir()
	bin := filepath.Join(dir, "meshbridge")
	_ = os.WriteFile(bin, []byte("old"), 0o755)

	s := &Stager{Dir: dir, Keys: keys}
	if err := s.Stage(context.Background(), srv.URL+"/download/v9.9.9", "v9.9.9"); err != nil {
		t.Fatal(err)
	}
	if v, ready := Staged(dir); v != "v9.9.9" || ready {
		t.Fatalf("staged %q ready=%v", v, ready)
	}
	_ = MarkReady(dir)
	if _, ready := Staged(dir); !ready {
		t.Fatal("not ready")
	}

	// Not newer than what's running: discarded, nothing installed.
	if v, err := Apply(dir, bin, "v9.9.9", keys); err != nil || v != "" {
		t.Fatalf("same version: %q %v", v, err)
	}
	if v, _ := Staged(dir); v != "" {
		t.Fatal("not discarded")
	}

	if err := s.Stage(context.Background(), srv.URL+"/download/v9.9.9", "v9.9.9"); err != nil {
		t.Fatal(err)
	}
	v, err := Apply(dir, bin, "v0.7.0", keys)
	if err != nil || v != "v9.9.9" {
		t.Fatalf("apply: %q %v", v, err)
	}
	if b, _ := os.ReadFile(bin); !strings.Contains(string(b), "v9.9.9") {
		t.Fatalf("binary not replaced: %s", b)
	}

	// A staged archive tampered with after staging is refused at apply.
	if err := s.Stage(context.Background(), srv.URL+"/download/v9.9.9", "v9.9.9"); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, StageDir, AssetName()), []byte("evil"), 0o600)
	if _, err := Apply(dir, bin, "v0.7.0", keys); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("tampered: %v", err)
	}

	// A signature for another version, or by another key, is refused.
	if err := s.Stage(context.Background(), srv.URL+"/download/v9.9.9", "v9.9.8"); err == nil {
		t.Fatal("staged with a signature for another version")
	}
	other, _, _ := ed25519.GenerateKey(nil)
	if err := (&Stager{Dir: dir, Keys: []ed25519.PublicKey{other}}).Stage(context.Background(), srv.URL+"/x", "v9.9.9"); err == nil {
		t.Fatal("staged with an untrusted key")
	}
	// A bad archive in the release itself is refused at staging.
	files[AssetName()] = []byte("not the archive")
	if err := s.Stage(context.Background(), srv.URL+"/x", "v9.9.9"); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("bad archive: %v", err)
	}
}

func TestApplyRefusesBinaryThatSaysAnotherVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as the new binary")
	}
	pub, priv, _ := ed25519.GenerateKey(nil)
	srv, _ := releaseOf(t, priv, "v9.9.10", "v9.9.9")
	dir := t.TempDir()
	bin := filepath.Join(dir, "meshbridge")
	_ = os.WriteFile(bin, []byte("old"), 0o755)
	keys := []ed25519.PublicKey{pub}
	if err := (&Stager{Dir: dir, Keys: keys}).Stage(context.Background(), srv.URL+"/x", "v9.9.10"); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(dir, bin, "v0.7.0", keys); err == nil || !strings.Contains(err.Error(), "didn't run") {
		t.Fatalf("apply: %v", err)
	}
	if b, _ := os.ReadFile(bin); string(b) != "old" {
		t.Fatal("replaced anyway")
	}
	if _, err := os.Stat(bin + ".new"); !os.IsNotExist(err) {
		t.Fatal("left the new binary behind")
	}
}
