package control

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUninstallEdgeScriptSyntax(t *testing.T) {
	command := exec.Command("bash", "-n")
	command.Stdin = strings.NewReader(uninstallEdgeScript)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, output)
	}
}

func TestUninstallEdgeScriptRemovesManagedNginxLayout(t *testing.T) {
	result := runUninstallEdgeScript(t, "2", "")
	if result.err != nil {
		t.Fatalf("script failed: %v\n%s", result.err, result.output)
	}
	for _, path := range []string{
		"opt/cdn-edge", "etc/systemd/system/cdn-edge-agent.service",
		"etc/systemd/system/cdn-edge-updater@.service", "etc/systemd/system/nginx.service",
		"etc/logrotate.d/cdn-edge-platform", "usr/local/lib/sysctl.d/40-simple-cdn-edge.conf",
		"etc/cdn-platform", "var/lib/cdn-platform", "var/log/cdn-platform", "var/cache/cdn-platform",
	} {
		result.requireAbsent(t, path)
	}
	result.requireContents(t, "etc/nginx/operator.conf", "operator owned\n")
	for _, expected := range []string{
		"uninstall/start", "uninstall/complete",
		"systemctl stop cdn-edge-agent.service", "systemctl stop nginx.service",
		"systemctl disable nginx.service", "nft delete table inet simple_cdn",
		"sysctl -q -p", "sysctl --system", "systemctl daemon-reload",
	} {
		if !strings.Contains(result.log, expected) {
			t.Fatalf("mock log does not contain %q:\n%s", expected, result.log)
		}
	}
	if strings.Contains(result.log, "nginx -t") || strings.Contains(result.log, "uninstall/fail") {
		t.Fatalf("managed uninstall touched external Nginx config or reported failure:\n%s", result.log)
	}
	if !strings.Contains(result.output, "managed Nginx were removed") {
		t.Fatalf("unexpected completion output: %s", result.output)
	}
}

func TestUninstallEdgeScriptKeepsExternalNginxForLayoutOne(t *testing.T) {
	result := runUninstallEdgeScript(t, "1", "")
	if result.err != nil {
		t.Fatalf("script failed: %v\n%s", result.err, result.output)
	}
	result.requireAbsent(t, "opt/cdn-edge")
	result.requireAbsent(t, "etc/nginx/conf.d/cdn-platform.conf")
	result.requireAbsent(t, "etc/nginx/modules-enabled/99-cdn-platform-stream.conf")
	result.requireContents(t, "etc/nginx/operator.conf", "operator owned\n")
	result.requireContents(t, "etc/systemd/system/nginx.service", "operator nginx unit\n")
	wantRoot := "worker_processes auto;\nevents {\n    worker_connections 768;\n}\nhttp {}\n"
	result.requireContents(t, "etc/nginx/nginx.conf", wantRoot)
	for _, expected := range []string{"nginx -t", "systemctl reload nginx.service", "uninstall/complete"} {
		if !strings.Contains(result.log, expected) {
			t.Fatalf("compatibility uninstall did not invoke %q:\n%s", expected, result.log)
		}
	}
	if strings.Contains(result.log, "systemctl stop nginx.service") {
		t.Fatalf("layout 1 uninstall stopped external Nginx:\n%s", result.log)
	}
}

func TestUninstallEdgeScriptRestoresLayoutOneConfigOnValidationFailure(t *testing.T) {
	result := runUninstallEdgeScript(t, "1", "nginx-test")
	if result.err == nil {
		t.Fatalf("script unexpectedly succeeded:\n%s", result.output)
	}
	result.requirePath(t, "opt/cdn-edge")
	result.requireContents(t, "etc/nginx/conf.d/cdn-platform.conf", "platform http\n")
	result.requireContents(t, "etc/nginx/modules-enabled/99-cdn-platform-stream.conf", "platform stream\n")
	if root := result.read(t, "etc/nginx/nginx.conf"); !strings.Contains(root, "# simple_cdn nginx capacity main include begin") {
		t.Fatalf("managed root configuration was not restored:\n%s", root)
	}
	for _, expected := range []string{
		"uninstall/start", "uninstall/fail", "systemctl enable cdn-edge-agent.service", "systemctl start cdn-edge-agent.service",
	} {
		if !strings.Contains(result.log, expected) {
			t.Fatalf("rollback log does not contain %q:\n%s", expected, result.log)
		}
	}
	if strings.Contains(result.log, "uninstall/complete") {
		t.Fatalf("failed uninstall reported completion:\n%s", result.log)
	}
}

func TestUninstallEdgeScriptDoesNotDeleteWhileAgentStopFails(t *testing.T) {
	result := runUninstallEdgeScript(t, "2", "agent-stop")
	if result.err == nil {
		t.Fatalf("script unexpectedly succeeded:\n%s", result.output)
	}
	result.requirePath(t, "opt/cdn-edge/nginx/sbin/nginx")
	result.requirePath(t, "etc/systemd/system/nginx.service")
	for _, expected := range []string{
		"systemctl stop cdn-edge-agent.service", "systemctl enable cdn-edge-agent.service",
		"systemctl start cdn-edge-agent.service", "uninstall/fail",
	} {
		if !strings.Contains(result.log, expected) {
			t.Fatalf("stop rollback log does not contain %q:\n%s", expected, result.log)
		}
	}
	if strings.Contains(result.log, "systemctl stop nginx.service") || strings.Contains(result.log, "uninstall/complete") {
		t.Fatalf("cleanup advanced after agent stop failure:\n%s", result.log)
	}
}

func TestUninstallEdgeScriptReportsCallbackFailureAfterLocalCleanup(t *testing.T) {
	result := runUninstallEdgeScript(t, "2", "complete-callback")
	if result.err == nil || !strings.Contains(result.output, "local cleanup completed") {
		t.Fatalf("callback failure result = %v\n%s", result.err, result.output)
	}
	result.requireAbsent(t, "opt/cdn-edge")
	if strings.Contains(result.log, "uninstall/fail") {
		t.Fatalf("post-cleanup callback failure was incorrectly rolled back:\n%s", result.log)
	}
}

func TestUninstallEdgeScriptRejectsUnknownLayout(t *testing.T) {
	result := runUninstallEdgeScript(t, "99", "")
	if result.err == nil || !strings.Contains(result.output, "unsupported /opt/cdn-edge layout version") {
		t.Fatalf("unknown layout result = %v\n%s", result.err, result.output)
	}
	result.requirePath(t, "opt/cdn-edge")
	if strings.Contains(result.log, "uninstall/start") {
		t.Fatalf("unknown layout contacted the control plane:\n%s", result.log)
	}
}

type uninstallResult struct {
	root   string
	log    string
	output string
	err    error
}

func runUninstallEdgeScript(t *testing.T, layout, failure string) uninstallResult {
	t.Helper()
	root := t.TempDir()
	for _, directory := range []string{
		"run", "tmp", "mock-bin", "etc/nginx/conf.d", "etc/nginx/modules-enabled",
		"etc/logrotate.d", "etc/systemd/system", "etc/sysctl.d", "usr/local/lib/sysctl.d",
		"opt/cdn-edge/bin", "opt/cdn-edge/nginx/sbin", "opt/cdn-edge/systemd", "opt/cdn-edge/data",
		"etc/cdn-platform", "var/lib/cdn-platform", "var/log/cdn-platform", "var/cache/cdn-platform",
	} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, root, "opt/cdn-edge/.layout-version", layout+"\n")
	writeTestFile(t, root, "opt/cdn-edge/bin/cdn-edge-agent", "agent\n")
	writeTestFile(t, root, "opt/cdn-edge/nginx/sbin/nginx", "managed nginx\n")
	writeTestFile(t, root, "opt/cdn-edge/systemd/cdn-edge-agent.service", "agent unit\n")
	writeTestFile(t, root, "opt/cdn-edge/systemd/cdn-edge-updater@.service", "updater unit\n")
	writeTestFile(t, root, "opt/cdn-edge/systemd/nginx.service", "nginx unit\n")
	writeTestFile(t, root, "opt/cdn-edge/data/sysctl-baseline.conf", "net.ipv4.tcp_congestion_control = cubic\n")
	writeTestFile(t, root, "etc/logrotate.d/cdn-edge-platform", "logrotate\n")
	writeTestFile(t, root, "usr/local/lib/sysctl.d/40-simple-cdn-edge.conf", "-net.ipv4.tcp_congestion_control = bbr\n")
	writeTestFile(t, root, "etc/sysctl.d/90-operator.conf", "net.ipv4.tcp_congestion_control = cubic\n")
	writeTestFile(t, root, "etc/nginx/operator.conf", "operator owned\n")
	for _, path := range []string{"etc/cdn-platform/state", "var/lib/cdn-platform/state", "var/log/cdn-platform/state", "var/cache/cdn-platform/state"} {
		writeTestFile(t, root, path, "legacy state\n")
	}
	writeTestFile(t, root, "run/mock-agent-active", "")
	writeTestFile(t, root, "run/mock-agent-enabled", "")
	writeTestFile(t, root, "run/mock-nginx-active", "")
	writeTestFile(t, root, "run/mock-nginx-enabled", "")
	for link, target := range map[string]string{
		"etc/systemd/system/cdn-edge-agent.service":    "opt/cdn-edge/systemd/cdn-edge-agent.service",
		"etc/systemd/system/cdn-edge-updater@.service": "opt/cdn-edge/systemd/cdn-edge-updater@.service",
	} {
		if err := os.Symlink(filepath.Join(root, target), filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	if layout == "2" || layout == "99" {
		if err := os.Symlink(filepath.Join(root, "opt/cdn-edge/systemd/nginx.service"), filepath.Join(root, "etc/systemd/system/nginx.service")); err != nil {
			t.Fatal(err)
		}
	} else {
		writeTestFile(t, root, "etc/systemd/system/nginx.service", "operator nginx unit\n")
		writeTestFile(t, root, "etc/nginx/conf.d/cdn-platform.conf", "platform http\n")
		writeTestFile(t, root, "etc/nginx/modules-enabled/99-cdn-platform-stream.conf", "platform stream\n")
		writeTestFile(t, root, "etc/nginx/nginx.conf", managedLayoutOneNginxRoot)
	}

	logPath := filepath.Join(root, "mock.log")
	mocks := map[string]string{
		"curl": `#!/usr/bin/env bash
printf 'curl %s\n' "$*" >>"$MOCK_LOG"
url="${*: -1}"
if [[ "${MOCK_FAILURE:-}" == "complete-callback" && "$url" == */complete ]]; then exit 1; fi
exit 0
`,
		"nginx": `#!/usr/bin/env bash
printf 'nginx %s\n' "$*" >>"$MOCK_LOG"
if [[ "${MOCK_FAILURE:-}" == "nginx-test" && "${1:-}" == "-t" ]]; then exit 1; fi
exit 0
`,
		"systemctl": `#!/usr/bin/env bash
set -euo pipefail
printf 'systemctl %s\n' "$*" >>"$MOCK_LOG"
root="$SIMPLE_CDN_UNINSTALL_ROOT"
command="${1:-}"
service="${*: -1}"
name=agent
if [[ "$service" == "nginx" || "$service" == "nginx.service" ]]; then name=nginx; fi
active="$root/run/mock-$name-active"
enabled="$root/run/mock-$name-enabled"
case "$command" in
  is-active) [[ -f "$active" ]] ;;
  is-enabled) [[ -f "$enabled" ]] ;;
  stop)
    if [[ "${MOCK_FAILURE:-}" == "agent-stop" && "$name" == "agent" ]]; then exit 1; fi
    rm -f "$active"
    ;;
  disable) rm -f "$enabled" ;;
  start) touch "$active" ;;
  enable) touch "$enabled" ;;
  reload)
    if [[ "${MOCK_FAILURE:-}" == "nginx-reload" ]]; then exit 1; fi
    ;;
  daemon-reload|reset-failed) ;;
esac
`,
		"nft":    "#!/usr/bin/env bash\nprintf 'nft %s\\n' \"$*\" >>\"$MOCK_LOG\"\nexit 0\n",
		"sysctl": "#!/usr/bin/env bash\nprintf 'sysctl %s\\n' \"$*\" >>\"$MOCK_LOG\"\nexit 0\n",
	}
	for name, contents := range mocks {
		if err := os.WriteFile(filepath.Join(root, "mock-bin", name), []byte(contents), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command("bash", "-s", "--", "--control-url", "https://control.example.test", "--token", "test-token")
	command.Stdin = strings.NewReader(uninstallEdgeScript)
	command.Env = []string{
		"PATH=" + filepath.Join(root, "mock-bin") + ":/usr/bin:/bin",
		"MOCK_LOG=" + logPath, "MOCK_FAILURE=" + failure, "SIMPLE_CDN_UNINSTALL_ROOT=" + root,
	}
	output, err := command.CombinedOutput()
	logContents, readErr := os.ReadFile(logPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	return uninstallResult{root: root, log: string(logContents), output: string(output), err: err}
}

const managedLayoutOneNginxRoot = `# simple_cdn nginx capacity managed worker_processes: worker_processes auto;
# simple_cdn nginx capacity main include begin
include /opt/cdn-edge/config/nginx/cdn-platform-main.conf;
# simple_cdn nginx capacity main include end
events {
    # simple_cdn nginx capacity events include begin
    include /opt/cdn-edge/config/nginx/cdn-platform-events.conf;
    # simple_cdn nginx capacity events include end
    # simple_cdn nginx capacity managed worker_connections: worker_connections 768;
}
http {}
`

func writeTestFile(t *testing.T, root, path, contents string) {
	t.Helper()
	fullPath := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (r uninstallResult) read(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(r.root, path))
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func (r uninstallResult) requireContents(t *testing.T, path, expected string) {
	t.Helper()
	if actual := r.read(t, path); actual != expected {
		t.Fatalf("%s = %q, want %q", path, actual, expected)
	}
}

func (r uninstallResult) requirePath(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(r.root, path)); err != nil {
		t.Fatalf("expected %s: %v", path, err)
	}
}

func (r uninstallResult) requireAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(r.root, path)); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be absent: %v", path, err)
	}
}
