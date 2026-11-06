//go:build windows

package e2e

// Runs scripts/install.ps1 end to end on Windows: a local "GitHub releases"
// server serves a freshly built meshbridge.exe zip and SHA256SUMS, and
// pairing goes to a real relay in process. The script needs an administrator.
//
// TestWindowsService also installs the real Windows service, which runs from
// %ProgramFiles%; it runs only with MESHBRIDGE_WINDOWS_SERVICE_TEST=1 (CI sets
// it on a throwaway runner), so `go test` on a workstation never touches the
// machine's services.

import (
	"archive/zip"
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
	"time"

	"golang.org/x/sys/windows"
)

func requireAdmin(t *testing.T) {
	t.Helper()
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("install.ps1 needs an administrator")
	}
}

// fakeWindowsRelease builds meshbridge.exe and lays out a release directory
// serving <prefix>/latest/download/<asset>, plus install.ps1 at the root.
func fakeWindowsRelease(t *testing.T) (dir, asset string) {
	t.Helper()
	asset = "meshbridge_windows_" + runtime.GOARCH + ".zip"
	stage := t.TempDir()
	exe := filepath.Join(stage, "meshbridge.exe")
	build := exec.Command("go", "build", "-ldflags", "-X main.version=v9.9.9", "-o", exe, "./cmd/meshbridge")
	build.Dir = ".."
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	dir = t.TempDir()
	d := filepath.Join(dir, "latest", "download")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	zf, err := os.Create(filepath.Join(d, asset))
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	w, _ := zw.Create("meshbridge.exe")
	data, _ := os.ReadFile(exe)
	_, _ = w.Write(data)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zf.Close()
	archive, _ := os.ReadFile(filepath.Join(d, asset))
	sum := sha256.Sum256(archive)
	if err := os.WriteFile(filepath.Join(d, "SHA256SUMS"), []byte(hex.EncodeToString(sum[:])+"  "+asset+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script, _ := os.ReadFile("../scripts/install.ps1")
	if err := os.WriteFile(filepath.Join(dir, "install.ps1"), script, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, asset
}

type winEnv struct {
	t       *testing.T
	baseURL string
	work    string
}

func newWinEnv(t *testing.T) (*winEnv, string) {
	release, _ := fakeWindowsRelease(t)
	srv := httptest.NewServer(http.FileServer(http.Dir(release)))
	t.Cleanup(srv.Close)
	return &winEnv{t: t, baseURL: srv.URL, work: t.TempDir()}, filepath.Join(release, "latest", "download", "SHA256SUMS")
}

func (e *winEnv) config() string     { return filepath.Join(e.work, "config", "meshbridge.toml") }
func (e *winEnv) installDir() string { return filepath.Join(e.work, "bin") }

// powershell runs a script with Windows PowerShell 5.1, the one every
// Windows machine has. Stdin is empty, like a non-interactive session.
func powershell(t *testing.T, script string) (string, error) {
	t.Helper()
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.Stdin = strings.NewReader("")
	// Started from PowerShell 7 (as CI's steps are), PSModulePath points at
	// 7's modules, which Windows PowerShell can't load. A user's session
	// doesn't have that.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "PSMODULEPATH=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// run invokes the script the way the documented one-liner with options does:
// & ([scriptblock]::Create((irm URL))) -Option value …
func (e *winEnv) run(args ...string) (string, error) {
	e.t.Helper()
	all := append([]string{"-BaseUrl", psQuote(e.baseURL), "-Config", psQuote(e.config()), "-NoPath"}, args...)
	return powershell(e.t, "& ([scriptblock]::Create((irm "+psQuote(e.baseURL+"/install.ps1")+"))) "+strings.Join(all, " "))
}

func TestWindowsInstallScript(t *testing.T) {
	requireAdmin(t)
	env, sums := newWinEnv(t)
	lr := startRelay(t)
	node := startNode(t, "Win Node")
	phrase, code := lr.invite(map[string]any{"gateway": "win-test"})
	if code != 200 {
		t.Fatalf("invite: HTTP %d", code)
	}

	out, err := env.run("-NoService", "-InstallDir", psQuote(env.installDir()), "-Relay", psQuote(lr.wsBase()),
		"-Passphrase", psQuote(strings.ReplaceAll(phrase, "-", " ")), "-NodeHost", "127.0.0.1", "-Port", strconv.Itoa(node.Addr().Port))
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	for _, want := range []string{"Installed meshbridge v9.9.9", "Found: Win Node", `Connected to SIERRA as gateway "win-test"`,
		"Installing the service", "Skipped the service"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	cfg, err := os.ReadFile(env.config())
	if err != nil || !strings.Contains(string(cfg), `token = "smb_`) || !strings.Contains(string(cfg), node.PubkeyHex()) {
		t.Fatalf("config: %v\n%s", err, cfg)
	}
	assertLockedDown(t, filepath.Dir(env.config()), false)

	// Running it again is an upgrade: no second pairing.
	if out, err := env.run("-NoService", "-InstallDir", psQuote(env.installDir())); err != nil || !strings.Contains(out, "already paired") {
		t.Fatalf("re-run: %v\n%s", err, out)
	}

	// A bad checksum stops before anything is replaced.
	bin := filepath.Join(env.installDir(), "meshbridge.exe")
	_ = os.WriteFile(bin, []byte("previous binary"), 0o755)
	asset := "meshbridge_windows_" + runtime.GOARCH + ".zip"
	_ = os.WriteFile(sums, []byte(strings.Repeat("0", 64)+"  "+asset+"\n"), 0o644)
	out, err = env.run("-NoService", "-InstallDir", psQuote(env.installDir()))
	if err == nil || !strings.Contains(out, "checksum mismatch") {
		t.Fatalf("tampered download: %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(bin); string(got) != "previous binary" {
		t.Fatal("binary replaced despite a checksum mismatch")
	}
}

// TestWindowsInstallScriptPiped runs the plain one-liner, `irm URL | iex`,
// which can't take options. It stops at the CPU check (forced to fail), which
// shows the script's param block and function run under iex, without
// downloading anything.
func TestWindowsInstallScriptPiped(t *testing.T) {
	requireAdmin(t)
	env, _ := newWinEnv(t)
	out, err := powershell(t, "$env:PROCESSOR_ARCHITEW6432 = 'bogus'; irm "+psQuote(env.baseURL+"/install.ps1")+" | iex")
	if err == nil || !strings.Contains(out, "unsupported CPU: bogus") {
		t.Fatalf("piped: %v\n%s", err, out)
	}
	if strings.Contains(out, "Downloading") {
		t.Fatalf("got past the CPU check:\n%s", out)
	}
}

// assertLockedDown checks the config folder's ACL: no access for ordinary
// users (it holds the bridge token).
func assertLockedDown(t *testing.T, dir string, withService bool) {
	t.Helper()
	out, err := exec.Command("icacls", filepath.Join(dir, "meshbridge.toml")).CombinedOutput()
	if err != nil {
		t.Fatalf("icacls: %v\n%s", err, out)
	}
	acl := string(out)
	for _, bad := range []string{`BUILTIN\Users`, "Everyone", `NT AUTHORITY\Authenticated Users`, "CREATOR OWNER"} {
		if strings.Contains(acl, bad) {
			t.Errorf("config is readable by %s:\n%s", bad, acl)
		}
	}
	for _, want := range []string{`NT AUTHORITY\SYSTEM`, `BUILTIN\Administrators`} {
		if !strings.Contains(acl, want) {
			t.Errorf("config ACL lacks %s:\n%s", want, acl)
		}
	}
	if withService != strings.Contains(acl, `NT SERVICE\meshbridge`) {
		t.Errorf("service account in ACL = %v, want %v:\n%s", !withService, withService, acl)
	}
}

func TestWindowsService(t *testing.T) {
	requireAdmin(t)
	if os.Getenv("MESHBRIDGE_WINDOWS_SERVICE_TEST") != "1" {
		t.Skip("installs a real Windows service; set MESHBRIDGE_WINDOWS_SERVICE_TEST=1 on a throwaway machine")
	}
	env, _ := newWinEnv(t)
	lr := startRelay(t)
	node := startNode(t, "Service Node")
	phrase, _ := lr.invite(map[string]any{"gateway": "win-svc"})
	installed := filepath.Join(os.Getenv("ProgramFiles"), "SIERRA", "meshbridge", "meshbridge.exe")
	t.Cleanup(func() { _ = exec.Command(installed, "uninstall").Run() })

	t.Cleanup(func() {
		if t.Failed() {
			logData, err := os.ReadFile(filepath.Join(filepath.Dir(env.config()), "meshbridge.log"))
			t.Logf("service log (%v):\n%s", err, logData)
			ev, _ := powershell(t, "Get-WinEvent -LogName System -MaxEvents 15 | Where-Object ProviderName -eq 'Service Control Manager' | Format-List TimeCreated,Id,Message")
			t.Logf("service manager events:\n%s", ev)
			acl, _ := exec.Command("icacls", filepath.Dir(env.config())).CombinedOutput()
			files, _ := exec.Command("icacls", filepath.Join(filepath.Dir(env.config()), "*")).CombinedOutput()
			t.Logf("config folder ACL:\n%s\n%s", acl, files)
		}
	})

	out, err := env.run("-Relay", psQuote(lr.wsBase()), "-Passphrase", psQuote(strings.ReplaceAll(phrase, "-", " ")),
		"-NodeHost", "127.0.0.1", "-Port", strconv.Itoa(node.Addr().Port))
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Installed and started the meshbridge service") {
		t.Fatalf("output:\n%s", out)
	}
	waitConnected(t, lr, "win-svc", true, 60*time.Second)
	assertLockedDown(t, filepath.Dir(env.config()), true)

	qc, _ := exec.Command("sc.exe", "qc", "meshbridge").CombinedOutput()
	for _, want := range []string{`NT SERVICE\meshbridge`, "AUTO_START", installed} {
		if !strings.Contains(string(qc), want) {
			t.Errorf("sc qc lacks %q:\n%s", want, qc)
		}
	}
	status, err := exec.Command(installed, "status", "-config", env.config()).CombinedOutput()
	if err != nil || !strings.Contains(string(status), "win-svc") || !strings.Contains(string(status), "Service Node") {
		t.Fatalf("status: %v\n%s", err, status)
	}
	logData, err := os.ReadFile(filepath.Join(filepath.Dir(env.config()), "meshbridge.log"))
	if err != nil || !strings.Contains(string(logData), "meshbridge starting") {
		t.Fatalf("service log: %v\n%s", err, logData)
	}
	if !strings.Contains(string(logData), "keeping this computer awake") {
		t.Errorf("service didn't take the keep-awake request:\n%s", logData)
	}

	// A crash is restarted by the service's recovery settings.
	if out, err := exec.Command("taskkill", "/F", "/IM", "meshbridge.exe").CombinedOutput(); err != nil {
		t.Fatalf("taskkill: %v\n%s", err, out)
	}
	waitConnected(t, lr, "win-svc", false, 30*time.Second)
	waitConnected(t, lr, "win-svc", true, 60*time.Second)

	// Re-running the installer upgrades in place: stop, replace, start.
	if out, err := env.run(); err != nil || !strings.Contains(out, "already paired") || !strings.Contains(out, "Installed and started") {
		t.Fatalf("upgrade: %v\n%s", err, out)
	}
	waitConnected(t, lr, "win-svc", true, 60*time.Second)

	if out, err := exec.Command(installed, "uninstall").CombinedOutput(); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	waitConnected(t, lr, "win-svc", false, 30*time.Second)
}

func waitConnected(t *testing.T, lr *localRelay, gateway string, want bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		d := lr.gateway(gateway)
		if connected(d) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("gateway %s connected=%v after %s, want %v: %+v", gateway, !want, timeout, want, d)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
