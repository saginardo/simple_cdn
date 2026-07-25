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

func TestUninstallEdgeScriptRemovesOnlyPlatformFiles(t *testing.T) {
	root, log, output, err := runUninstallEdgeScript(t, "")
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, output)
	}
	for _, path := range []string{
		"etc/nginx/conf.d/cdn-platform.conf",
		"etc/nginx/modules-enabled/99-cdn-platform-stream.conf",
		"etc/logrotate.d/cdn-edge-platform",
		"etc/systemd/system/cdn-edge-agent.service",
		"etc/systemd/system/cdn-edge-updater@.service",
		"usr/local/bin/cdn-edge-agent",
		"usr/local/lib/sysctl.d/40-simple-cdn-edge.conf",
		"opt/cdn-edge",
		"etc/cdn-platform",
		"var/lib/cdn-platform",
		"var/log/cdn-platform",
		"var/cache/cdn-platform",
	} {
		if _, statErr := os.Stat(filepath.Join(root, path)); !os.IsNotExist(statErr) {
			t.Fatalf("platform path %s was retained: %v", path, statErr)
		}
	}
	if _, statErr := os.Stat(filepath.Join(root, "etc/nginx/nginx.conf")); statErr != nil {
		t.Fatalf("unrelated Nginx configuration was removed: %v", statErr)
	}
	operatorSysctl, readErr := os.ReadFile(filepath.Join(root, "etc/sysctl.d/90-operator.conf"))
	if readErr != nil || string(operatorSysctl) != "net.ipv4.tcp_congestion_control = cubic\n" {
		t.Fatalf("operator sysctl configuration was changed: %q, %v", operatorSysctl, readErr)
	}
	for _, expected := range []string{"uninstall/start", "uninstall/complete", "nginx -t", "systemctl reload nginx", "systemctl daemon-reload", "nft delete table inet simple_cdn", "nft delete table inet cdn_platform", "sysctl -q -p", "sysctl --system"} {
		if !strings.Contains(log, expected) {
			t.Fatalf("mock log does not contain %q:\n%s", expected, log)
		}
	}
	if baselineIndex, systemIndex := strings.Index(log, "sysctl -q -p"), strings.Index(log, "sysctl --system"); baselineIndex < 0 || systemIndex < 0 || baselineIndex > systemIndex {
		t.Fatalf("uninstall did not restore the baseline before loading remaining sysctl files:\n%s", log)
	}
	if strings.Contains(log, "uninstall/fail") {
		t.Fatalf("successful uninstall reported failure:\n%s", log)
	}
}

func TestUninstallEdgeScriptRestoresManagedNginxRootConfig(t *testing.T) {
	root, _, output, err := runUninstallEdgeScript(t, "capacity")
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, output)
	}
	rootConfig, readErr := os.ReadFile(filepath.Join(root, "etc/nginx/nginx.conf"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	expected := "worker_processes auto;\nevents {\n    worker_connections 768;\n}\nhttp {\n    include /etc/nginx/conf.d/*.conf;\n}\n"
	if string(rootConfig) != expected {
		t.Fatalf("Nginx root config was not restored:\n%s", rootConfig)
	}
	if _, statErr := os.Stat(filepath.Join(root, "etc/logrotate.d/cdn-edge-platform")); !os.IsNotExist(statErr) {
		t.Fatalf("platform logrotate policy was retained: %v", statErr)
	}
}

func TestUninstallEdgeScriptRestoresManagedNginxRootWhenValidationFails(t *testing.T) {
	root, _, output, err := runUninstallEdgeScript(t, "capacity-nginx")
	if err == nil {
		t.Fatalf("script unexpectedly succeeded:\n%s", output)
	}
	rootConfig, readErr := os.ReadFile(filepath.Join(root, "etc/nginx/nginx.conf"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(rootConfig), "# simple_cdn nginx capacity main include begin") ||
		!strings.Contains(string(rootConfig), "# simple_cdn nginx capacity managed worker_connections: worker_connections 768;") {
		t.Fatalf("managed Nginx root config was not restored after validation failure:\n%s", rootConfig)
	}
	if _, statErr := os.Stat(filepath.Join(root, "etc/logrotate.d/cdn-edge-platform")); statErr != nil {
		t.Fatalf("failed uninstall removed logrotate policy: %v", statErr)
	}
}

func TestUninstallEdgeScriptRemovesStreamEntryWhenHTTPEntryIsAlreadyMissing(t *testing.T) {
	root, _, output, err := runUninstallEdgeScript(t, "stream-only")
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, output)
	}
	if _, statErr := os.Stat(filepath.Join(root, "etc/nginx/modules-enabled/99-cdn-platform-stream.conf")); !os.IsNotExist(statErr) {
		t.Fatalf("stream entry was retained: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, "etc/nginx/nginx.conf")); statErr != nil {
		t.Fatalf("unrelated Nginx configuration was removed: %v", statErr)
	}
}

func TestUninstallEdgeScriptRestoresConfigurationWhenNginxValidationFails(t *testing.T) {
	root, log, output, err := runUninstallEdgeScript(t, "nginx")
	if err == nil {
		t.Fatalf("script unexpectedly succeeded:\n%s", output)
	}
	config, readErr := os.ReadFile(filepath.Join(root, "etc/nginx/conf.d/cdn-platform.conf"))
	if readErr != nil || string(config) != "platform config\n" {
		t.Fatalf("Nginx configuration was not restored: %q, %v", config, readErr)
	}
	streamConfig, readErr := os.ReadFile(filepath.Join(root, "etc/nginx/modules-enabled/99-cdn-platform-stream.conf"))
	if readErr != nil || string(streamConfig) != "stream config\n" {
		t.Fatalf("Nginx stream configuration was not restored: %q, %v", streamConfig, readErr)
	}
	for _, path := range []string{
		"etc/systemd/system/cdn-edge-agent.service",
		"usr/local/bin/cdn-edge-agent",
		"usr/local/lib/sysctl.d/40-simple-cdn-edge.conf",
		"etc/cdn-platform/state",
		"var/lib/cdn-platform/state",
		"var/log/cdn-platform/state",
		"var/cache/cdn-platform/state",
	} {
		if _, statErr := os.Stat(filepath.Join(root, path)); statErr != nil {
			t.Fatalf("rollback removed %s: %v", path, statErr)
		}
	}
	for _, expected := range []string{"uninstall/start", "uninstall/fail", "systemctl enable cdn-edge-agent.service", "systemctl start cdn-edge-agent.service"} {
		if !strings.Contains(log, expected) {
			t.Fatalf("rollback log does not contain %q:\n%s", expected, log)
		}
	}
	if strings.Contains(log, "uninstall/complete") || strings.Contains(log, "systemctl daemon-reload") {
		t.Fatalf("failed uninstall committed cleanup:\n%s", log)
	}
	matches, globErr := filepath.Glob(filepath.Join(root, "tmp/cdn-platform-nginx.*"))
	if globErr != nil || len(matches) != 0 {
		t.Fatalf("rollback left Nginx backup files: %#v, %v", matches, globErr)
	}
}

func TestUninstallEdgeScriptRestoresConfigurationWhenNginxReloadFails(t *testing.T) {
	root, log, output, err := runUninstallEdgeScript(t, "reload")
	if err == nil {
		t.Fatalf("script unexpectedly succeeded:\n%s", output)
	}
	config, readErr := os.ReadFile(filepath.Join(root, "etc/nginx/conf.d/cdn-platform.conf"))
	if readErr != nil || string(config) != "platform config\n" {
		t.Fatalf("Nginx configuration was not restored: %q, %v", config, readErr)
	}
	streamConfig, readErr := os.ReadFile(filepath.Join(root, "etc/nginx/modules-enabled/99-cdn-platform-stream.conf"))
	if readErr != nil || string(streamConfig) != "stream config\n" {
		t.Fatalf("Nginx stream configuration was not restored: %q, %v", streamConfig, readErr)
	}
	for _, expected := range []string{"uninstall/start", "uninstall/fail", "systemctl reload nginx", "systemctl enable cdn-edge-agent.service", "systemctl start cdn-edge-agent.service"} {
		if !strings.Contains(log, expected) {
			t.Fatalf("reload rollback log does not contain %q:\n%s", expected, log)
		}
	}
	if strings.Contains(log, "uninstall/complete") || strings.Contains(log, "systemctl daemon-reload") {
		t.Fatalf("reload failure committed cleanup:\n%s", log)
	}
}

func TestUninstallEdgeScriptRestoresServiceWhenBackupCreationFails(t *testing.T) {
	root, log, output, err := runUninstallEdgeScript(t, "mktemp")
	if err == nil {
		t.Fatalf("script unexpectedly succeeded:\n%s", output)
	}
	for _, path := range []string{
		"etc/nginx/conf.d/cdn-platform.conf",
		"etc/nginx/modules-enabled/99-cdn-platform-stream.conf",
		"etc/systemd/system/cdn-edge-agent.service",
		"usr/local/bin/cdn-edge-agent",
		"var/lib/cdn-platform/state",
	} {
		if _, statErr := os.Stat(filepath.Join(root, path)); statErr != nil {
			t.Fatalf("generic rollback removed %s: %v", path, statErr)
		}
	}
	for _, expected := range []string{"uninstall/start", "uninstall/fail", "systemctl enable cdn-edge-agent.service", "systemctl start cdn-edge-agent.service"} {
		if !strings.Contains(log, expected) {
			t.Fatalf("generic rollback log does not contain %q:\n%s", expected, log)
		}
	}
	if strings.Contains(log, "uninstall/complete") || strings.Contains(log, "systemctl daemon-reload") {
		t.Fatalf("generic failure committed cleanup:\n%s", log)
	}
}

func TestUninstallEdgeScriptRestoresServiceWhenStopFails(t *testing.T) {
	root, log, output, err := runUninstallEdgeScript(t, "stop")
	if err == nil {
		t.Fatalf("script unexpectedly succeeded:\n%s", output)
	}
	for _, path := range []string{
		"etc/nginx/conf.d/cdn-platform.conf",
		"etc/nginx/modules-enabled/99-cdn-platform-stream.conf",
		"etc/systemd/system/cdn-edge-agent.service",
		"usr/local/bin/cdn-edge-agent",
		"var/lib/cdn-platform/state",
	} {
		if _, statErr := os.Stat(filepath.Join(root, path)); statErr != nil {
			t.Fatalf("stop rollback removed %s: %v", path, statErr)
		}
	}
	for _, expected := range []string{"uninstall/start", "uninstall/fail", "systemctl stop cdn-edge-agent.service", "systemctl enable cdn-edge-agent.service", "systemctl start cdn-edge-agent.service"} {
		if !strings.Contains(log, expected) {
			t.Fatalf("stop rollback log does not contain %q:\n%s", expected, log)
		}
	}
	if strings.Contains(log, "uninstall/complete") || strings.Contains(log, "systemctl daemon-reload") {
		t.Fatalf("stop failure committed cleanup:\n%s", log)
	}
}

func runUninstallEdgeScript(t *testing.T, failureMode string) (string, string, string, error) {
	t.Helper()
	root := t.TempDir()
	for _, directory := range []string{
		"run", "tmp", "mock-bin", "etc/nginx/conf.d", "etc/nginx/modules-enabled", "etc/logrotate.d", "etc/systemd/system", "usr/local/bin",
		"etc/sysctl.d", "usr/local/lib/sysctl.d",
		"opt/cdn-edge/bin", "opt/cdn-edge/config", "opt/cdn-edge/data", "opt/cdn-edge/logs", "opt/cdn-edge/cache",
		"etc/cdn-platform", "var/lib/cdn-platform", "var/log/cdn-platform", "var/cache/cdn-platform",
	} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, contents := range map[string]string{
		"etc/nginx/conf.d/cdn-platform.conf":                    "platform config\n",
		"etc/nginx/modules-enabled/99-cdn-platform-stream.conf": "stream config\n",
		"etc/nginx/nginx.conf":                                  "unrelated config\n",
		"etc/logrotate.d/cdn-edge-platform":                     "platform logrotate\n",
		"etc/systemd/system/cdn-edge-agent.service":             "service\n",
		"etc/systemd/system/cdn-edge-updater@.service":          "updater service\n",
		"usr/local/bin/cdn-edge-agent":                          "binary\n",
		"usr/local/lib/sysctl.d/40-simple-cdn-edge.conf":        "-net.ipv4.tcp_congestion_control = bbr\n",
		"etc/sysctl.d/90-operator.conf":                         "net.ipv4.tcp_congestion_control = cubic\n",
		"opt/cdn-edge/.layout-version":                          "1\n",
		"opt/cdn-edge/bin/cdn-edge-agent":                       "new binary\n",
		"opt/cdn-edge/data/state":                               "new state\n",
		"opt/cdn-edge/data/sysctl-baseline.conf":                "net.ipv4.tcp_congestion_control = cubic\n",
		"etc/cdn-platform/state":                                "state\n",
		"var/lib/cdn-platform/state":                            "state\n",
		"var/log/cdn-platform/state":                            "state\n",
		"var/cache/cdn-platform/state":                          "state\n",
	} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if failureMode == "capacity" || failureMode == "capacity-nginx" {
		managedRoot := "# simple_cdn nginx capacity managed worker_processes: worker_processes auto;\n# simple_cdn nginx capacity main include begin\ninclude /opt/cdn-edge/config/nginx/cdn-platform-main.conf;\n# simple_cdn nginx capacity main include end\nevents {\n    # simple_cdn nginx capacity events include begin\n    include /opt/cdn-edge/config/nginx/cdn-platform-events.conf;\n    # simple_cdn nginx capacity events include end\n    # simple_cdn nginx capacity managed worker_connections: worker_connections 768;\n}\nhttp {\n    include /etc/nginx/conf.d/*.conf;\n}\n"
		if err := os.WriteFile(filepath.Join(root, "etc/nginx/nginx.conf"), []byte(managedRoot), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if failureMode == "stream-only" {
		if err := os.Remove(filepath.Join(root, "etc/nginx/conf.d/cdn-platform.conf")); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(root, "mock.log")
	mocks := map[string]string{
		"curl": `#!/usr/bin/env bash
printf 'curl %s\n' "$*" >> "$MOCK_LOG"
exit 0
`,
		"nginx": `#!/usr/bin/env bash
printf 'nginx %s\n' "$*" >> "$MOCK_LOG"
if [[ "$1" == "-t" && "${MOCK_NGINX_FAIL:-0}" == "1" ]]; then exit 1; fi
exit 0
`,
		"mktemp": `#!/usr/bin/env bash
printf 'mktemp %s\n' "$*" >> "$MOCK_LOG"
if [[ "${MOCK_MKTEMP_FAIL:-0}" == "1" ]]; then exit 1; fi
exec /usr/bin/mktemp "$@"
`,
		"systemctl": `#!/usr/bin/env bash
printf 'systemctl %s\n' "$*" >> "$MOCK_LOG"
case "$1" in
  is-enabled|is-active) exit 0 ;;
esac
if [[ "$1" == "reload" && "$2" == "nginx" && "${MOCK_RELOAD_FAIL:-0}" == "1" ]]; then exit 1; fi
if [[ "$1" == "stop" && "${MOCK_STOP_FAIL:-0}" == "1" ]]; then exit 1; fi
exit 0
`,
		"nft": `#!/usr/bin/env bash
printf 'nft %s\n' "$*" >> "$MOCK_LOG"
exit 0
`,
		"sysctl": `#!/usr/bin/env bash
printf 'sysctl %s\n' "$*" >> "$MOCK_LOG"
exit 0
`,
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
		"MOCK_LOG=" + logPath,
		"MOCK_NGINX_FAIL=" + boolEnvironment(failureMode == "nginx" || failureMode == "capacity-nginx"),
		"MOCK_MKTEMP_FAIL=" + boolEnvironment(failureMode == "mktemp"),
		"MOCK_RELOAD_FAIL=" + boolEnvironment(failureMode == "reload"),
		"MOCK_STOP_FAIL=" + boolEnvironment(failureMode == "stop"),
		"SIMPLE_CDN_UNINSTALL_ROOT=" + root,
	}
	output, err := command.CombinedOutput()
	logContents, readErr := os.ReadFile(logPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	return root, string(logContents), string(output), err
}

func boolEnvironment(value bool) string {
	if value {
		return "1"
	}
	return "0"
}
