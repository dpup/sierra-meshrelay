package e2e

// Runs scripts/install.sh end to end: a local "GitHub releases" server
// serves a freshly built meshbridge archive and SHA256SUMS, and pairing goes
// to a real relay in process. sudo is stubbed (the test isn't root) and the
// systemd step is skipped (--no-service).

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func releaseArch(t *testing.T) string {
	switch runtime.GOARCH {
	case "amd64", "arm64":
		return runtime.GOARCH
	case "arm":
		return "armv7"
	}
	t.Skipf("no release archive for %s", runtime.GOARCH)
	return ""
}

// fakeRelease builds meshbridge and lays out a release directory serving
// <prefix>/latest/download/<asset> and <prefix>/download/v9.9.9/<asset>.
func fakeRelease(t *testing.T) (dir, asset string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the installer is Linux-only")
	}
	for _, tool := range []string{"sh", "tar", "awk", "sed"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
	asset = "meshbridge_linux_" + releaseArch(t) + ".tar.gz"
	stage := t.TempDir()
	build := exec.Command("go", "build", "-ldflags", "-X main.version=v9.9.9", "-o", filepath.Join(stage, "meshbridge"), "./cmd/meshbridge")
	build.Dir = ".."
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	dir = t.TempDir()
	for _, sub := range []string{"latest/download", "download/v9.9.9"} {
		d := filepath.Join(dir, sub)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("tar", "-C", stage, "-czf", filepath.Join(d, asset), "./meshbridge").CombinedOutput(); err != nil {
			t.Fatalf("tar: %v\n%s", err, out)
		}
		data, _ := os.ReadFile(filepath.Join(d, asset))
		sum := sha256.Sum256(data)
		sums := hex.EncodeToString(sum[:]) + "  " + asset + "\n"
		if err := os.WriteFile(filepath.Join(d, "SHA256SUMS"), []byte(sums), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, asset
}

type installEnv struct {
	t       *testing.T
	baseURL string
	work    string
	path    string
}

func newInstallEnv(t *testing.T, releaseDir string) *installEnv {
	srv := httptest.NewServer(http.FileServer(http.Dir(releaseDir)))
	t.Cleanup(srv.Close)
	stub := t.TempDir()
	// The test isn't root, so the script uses sudo: run commands as-is.
	if err := os.WriteFile(filepath.Join(stub, "sudo"), []byte("#!/bin/sh\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &installEnv{t: t, baseURL: srv.URL, work: t.TempDir(), path: stub + ":" + os.Getenv("PATH")}
}

func (e *installEnv) run(args ...string) (string, error) {
	e.t.Helper()
	all := append([]string{"../scripts/install.sh", "--base-url", e.baseURL,
		"--config", filepath.Join(e.work, "etc", "meshbridge.toml"), "--bin-dir", filepath.Join(e.work, "bin"), "--no-service"}, args...)
	cmd := exec.Command("sh", all...)
	cmd.Env = append(os.Environ(), "PATH="+e.path)
	cmd.Stdin = strings.NewReader("") // like `curl | sh` with nothing more to read
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestInstallScript(t *testing.T) {
	release, asset := fakeRelease(t)
	lr := startRelay(t)
	node := startNode(t, "Pi Node")
	phrase, code := lr.invite(map[string]any{"gateway": "pi-test"})
	if code != 200 {
		t.Fatalf("invite: HTTP %d", code)
	}
	env := newInstallEnv(t, release)

	out, err := env.run("--relay", lr.wsBase(), "--passphrase", strings.ReplaceAll(phrase, "-", " "),
		"--host", "127.0.0.1", "--port", strconv.Itoa(node.Addr().Port))
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	for _, want := range []string{"Installed meshbridge v9.9.9", "Found: Pi Node", `Connected to SIERRA as gateway "pi-test"`,
		"Installing the service…", "Skipped the service"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Start the bridge with") {
		t.Errorf("setup told the user to start the bridge by hand during an install:\n%s", out)
	}
	cfg, err := os.ReadFile(filepath.Join(env.work, "etc", "meshbridge.toml"))
	if err != nil || !strings.Contains(string(cfg), `token = "smb_`) || !strings.Contains(string(cfg), node.PubkeyHex()) {
		t.Fatalf("config: %v\n%s", err, cfg)
	}
	bin := filepath.Join(env.work, "bin", "meshbridge")
	if fi, err := os.Stat(bin); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("binary: %v", err)
	}

	// Running it again is an upgrade: no second pairing.
	out, err = env.run("--version", "v9.9.9")
	if err != nil || !strings.Contains(out, "already paired") {
		t.Fatalf("re-run: %v\n%s", err, out)
	}

	// The real one-liner: the script itself arrives on stdin (`curl | sh -s --`).
	script, _ := os.ReadFile("../scripts/install.sh")
	piped := exec.Command("sh", "-s", "--", "--base-url", env.baseURL, "--config", filepath.Join(env.work, "etc", "meshbridge.toml"),
		"--bin-dir", filepath.Join(env.work, "bin"), "--no-service")
	piped.Env = append(os.Environ(), "PATH="+env.path)
	piped.Stdin = strings.NewReader(string(script))
	if out, err := piped.CombinedOutput(); err != nil || !strings.Contains(string(out), "already paired") {
		t.Fatalf("piped: %v\n%s", err, out)
	}

	// A truncated download runs nothing (main is only called on the last line).
	half := exec.Command("sh", "-s")
	half.Stdin = strings.NewReader(string(script[:len(script)/2]))
	if out, _ := half.CombinedOutput(); strings.Contains(string(out), "Downloading") {
		t.Fatalf("a truncated script ran:\n%s", out)
	}

	// A bad checksum stops before anything is replaced.
	_ = os.WriteFile(bin, []byte("previous binary"), 0o755)
	sums := filepath.Join(release, "latest", "download", "SHA256SUMS")
	_ = os.WriteFile(sums, []byte(strings.Repeat("0", 64)+"  "+asset+"\n"), 0o644)
	out, err = env.run()
	if err == nil || !strings.Contains(out, "checksum mismatch") {
		t.Fatalf("tampered download: %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(bin); string(got) != "previous binary" {
		t.Fatal("binary replaced despite a checksum mismatch")
	}
}

func TestInstallScriptOptions(t *testing.T) {
	env := &installEnv{t: t, baseURL: "http://127.0.0.1:1", work: t.TempDir(), path: os.Getenv("PATH")}
	if out, err := env.run("--bogus"); err == nil || !strings.Contains(out, "unknown option: --bogus") {
		t.Fatalf("unknown option: %v\n%s", err, out)
	}
	if out, err := env.run("--passphrase"); err == nil || !strings.Contains(out, "--passphrase needs a value") {
		t.Fatalf("missing value: %v\n%s", err, out)
	}
}
