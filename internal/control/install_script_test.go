package control

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallEdgeScriptSyntaxAndManagedNginxPolicy(t *testing.T) {
	command := exec.Command("bash", "-n")
	command.Stdin = strings.NewReader(bootstrapEdgeScript)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, output)
	}
	for _, expected := range []string{
		"LAYOUT_VERSION=2",
		"/opt/cdn-edge/nginx/sbin/nginx",
		"libpcre2-8-0 zlib1g libcrypt1",
		"apt-get purge -y",
		"NGINX_BUNDLE_URL_DEFAULT=\"\"",
		"LD_LIBRARY_PATH=\"$candidate_nginx/lib\"",
	} {
		if !strings.Contains(bootstrapEdgeScript, expected) {
			t.Fatalf("installer is missing %q", expected)
		}
	}
	if strings.Contains(bootstrapEdgeScript, "apt-get install -y --no-install-recommends nginx") ||
		strings.Contains(bootstrapEdgeScript, "libnginx-mod-http-lua") {
		t.Fatal("installer still installs a Debian Nginx package")
	}
}

func TestInstallEdgeScriptCreatesManagedOptLayout(t *testing.T) {
	harness := newInstallHarness(t)
	output, err := harness.run(t, "first-token", "edge-binary-v1", "")
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, output)
	}
	for _, path := range []string{
		"opt/cdn-edge/.layout-version",
		"opt/cdn-edge/bin/cdn-edge-agent",
		"opt/cdn-edge/nginx/sbin/nginx",
		"opt/cdn-edge/nginx/conf/nginx.conf",
		"opt/cdn-edge/nginx/VERSION",
		"opt/cdn-edge/nginx/.bundle-sha256",
		"opt/cdn-edge/nginx/tmp/proxy",
		"opt/cdn-edge/config/edge.env",
		"opt/cdn-edge/config/nginx/cdn-platform.conf",
		"opt/cdn-edge/config/nginx/quic-host.key",
		"opt/cdn-edge/static/objects",
		"opt/cdn-edge/systemd/nginx.service",
		"opt/cdn-edge/systemd/cdn-edge-agent.service",
		"opt/cdn-edge/systemd/cdn-edge-updater@.service",
		"opt/cdn-edge/data/edge-client.key",
		"etc/logrotate.d/cdn-edge-platform",
		"usr/local/lib/sysctl.d/40-simple-cdn-edge.conf",
	} {
		harness.requirePath(t, path)
	}
	harness.requireContents(t, "opt/cdn-edge/.layout-version", "2\n")
	harness.requireContents(t, "opt/cdn-edge/nginx/VERSION", "1.30.4\n")
	for path, wanted := range map[string]os.FileMode{
		"opt/cdn-edge/nginx":                 0o755,
		"opt/cdn-edge/nginx/conf/nginx.conf": 0o644,
		"opt/cdn-edge/nginx/sbin/nginx":      0o755,
		"opt/cdn-edge/static":                0o755,
		"opt/cdn-edge/static/objects":        0o755,
	} {
		info, err := os.Stat(filepath.Join(harness.root, path))
		if err != nil || info.Mode().Perm() != wanted {
			t.Fatalf("%s permissions = %v, want %v (err=%v)", path, info.Mode().Perm(), wanted, err)
		}
	}
	harness.requireAbsent(t, "etc/nginx")
	harness.requireLink(t, "etc/systemd/system/nginx.service", "opt/cdn-edge/systemd/nginx.service")
	harness.requireLink(t, "etc/systemd/system/cdn-edge-agent.service", "opt/cdn-edge/systemd/cdn-edge-agent.service")
	harness.requireLink(t, "etc/systemd/system/cdn-edge-updater@.service", "opt/cdn-edge/systemd/cdn-edge-updater@.service")
	harness.requireContents(t, "opt/cdn-edge/bin/cdn-edge-agent", "edge-binary-v1")
	logrotate := harness.read(t, "etc/logrotate.d/cdn-edge-platform")
	for _, expected := range []string{"/opt/cdn-edge/logs/tcp-access.json", "/opt/cdn-edge/logs/tcp-error.log"} {
		if !strings.Contains(logrotate, expected) {
			t.Fatalf("logrotate policy does not contain %q:\n%s", expected, logrotate)
		}
	}
	environment := harness.read(t, "opt/cdn-edge/config/edge.env")
	for _, expected := range []string{
		"CONTROL_URL=https://edge-control.example.test",
		"ENROLLMENT_TOKEN=\n",
		"NGINX_BINARY_PATH=/opt/cdn-edge/nginx/sbin/nginx",
		"NGINX_PID_PATH=/opt/cdn-edge/nginx/run/nginx.pid",
		"NGINX_STATUS_SOCKET_PATH=/opt/cdn-edge/nginx/run/status.sock",
		"NGINX_VERSION_PATH=/opt/cdn-edge/nginx/VERSION",
		"NGINX_SHA256_PATH=/opt/cdn-edge/nginx/.bundle-sha256",
		"EDGE_STATIC_ASSET_DIR=/opt/cdn-edge/static/objects",
		"EDGE_CAPABILITIES=tcp_stream_v1,edge_rate_limit_v1,waf_chain_v1,pow_challenge_v1,static_assets_v1,cache_control_v1,cache_warmup_results_v1,nginx_capacity_v1,nginx_bundle_v1",
	} {
		if !strings.Contains(environment, expected) {
			t.Fatalf("edge.env does not contain %q:\n%s", expected, environment)
		}
	}
	if info, err := os.Stat(filepath.Join(harness.root, "opt/cdn-edge/config/nginx/quic-host.key")); err != nil || info.Mode().Perm() != 0o600 || info.Size() != 32 {
		t.Fatalf("QUIC host key metadata = %#v, err=%v", info, err)
	}
	log := harness.read(t, "mock.log")
	for _, expected := range []string{"systemctl start nginx.service", "systemctl restart cdn-edge-agent.service", "sysctl --system"} {
		if !strings.Contains(log, expected) {
			t.Fatalf("installer did not invoke %q:\n%s", expected, log)
		}
	}
}

func TestInstallEdgeScriptAdvertisesHTTP3FromManagedBinary(t *testing.T) {
	harness := newInstallHarness(t)
	harness.nginxFlags = "--with-http_v2_module --with-http_v3_module"
	output, err := harness.run(t, "first-token", "edge-binary-v1", "")
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, output)
	}
	if environment := harness.read(t, "opt/cdn-edge/config/edge.env"); !strings.Contains(environment, "nginx_bundle_v1,http3_v1") {
		t.Fatalf("HTTP/3-capable managed Nginx was not advertised:\n%s", environment)
	}
}

func TestInstallEdgeScriptAdvertisesCompressionModulesFromManagedBinary(t *testing.T) {
	harness := newInstallHarness(t)
	harness.nginxFlags = "--add-module=/build/ngx_brotli-commit --add-module=/build/zstd-nginx-module-commit"
	output, err := harness.run(t, "first-token", "edge-binary-v1", "")
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, output)
	}
	if environment := harness.read(t, "opt/cdn-edge/config/edge.env"); !strings.Contains(environment, "nginx_bundle_v1,compression_v1") {
		t.Fatalf("compression-capable managed Nginx was not advertised:\n%s", environment)
	}
}

func TestInstallEdgeScriptRequiresTokenForFreshHost(t *testing.T) {
	harness := newInstallHarness(t)
	output, err := harness.run(t, "", "edge-binary-v1", "")
	if err == nil || !strings.Contains(output, "an enrollment token is required") {
		t.Fatalf("fresh install without a token was not rejected: %v\n%s", err, output)
	}
	harness.requireAbsent(t, "opt/cdn-edge")
}

func TestInstallEdgeScriptMigratesLegacyStateAndRemovesDebianIntegration(t *testing.T) {
	harness := newInstallHarness(t)
	harness.seedLegacy()
	output, err := harness.run(t, "", "edge-binary-v2", "")
	if err != nil {
		t.Fatalf("migration failed: %v\n%s", err, output)
	}
	for _, path := range []string{
		"usr/local/bin/cdn-edge-agent", "etc/cdn-platform", "var/lib/cdn-platform",
		"var/log/cdn-platform", "var/cache/cdn-platform", "etc/nginx",
	} {
		harness.requireAbsent(t, path)
	}
	for path, contents := range map[string]string{
		"opt/cdn-edge/data/edge-client.key":         "legacy-key\n",
		"opt/cdn-edge/data/access-log-queue.ndjson": "queued\n",
		"opt/cdn-edge/config/certs/site.crt":        "site-cert\n",
		"opt/cdn-edge/logs/access.json":             "access event\n",
	} {
		harness.requireContents(t, path, contents)
	}
	configuration := harness.read(t, "opt/cdn-edge/config/nginx/cdn-platform.conf")
	for _, expected := range []string{"/opt/cdn-edge/cache", "/opt/cdn-edge/config/certs/site.crt", "/opt/cdn-edge/logs/access.json"} {
		if !strings.Contains(configuration, expected) {
			t.Fatalf("migrated Nginx configuration does not contain %q:\n%s", expected, configuration)
		}
	}
	if environment := harness.read(t, "opt/cdn-edge/config/edge.env"); !strings.Contains(environment, "EDGE_POLL_SECONDS=45") {
		t.Fatalf("migration did not retain poll interval:\n%s", environment)
	}
}

func TestInstallEdgeScriptMigratesLayoutOneToManagedNginx(t *testing.T) {
	harness := newInstallHarness(t)
	harness.seedLayoutOne()
	output, err := harness.run(t, "", "edge-binary-v2", "")
	if err != nil {
		t.Fatalf("layout migration failed: %v\n%s", err, output)
	}
	harness.requireContents(t, "opt/cdn-edge/.layout-version", "2\n")
	harness.requireContents(t, "opt/cdn-edge/data/preserved", "keep\n")
	harness.requirePath(t, "opt/cdn-edge/nginx/sbin/nginx")
	harness.requireAbsent(t, "etc/nginx")
	streamConfiguration := harness.read(t, "opt/cdn-edge/config/nginx/fragments/stream-v111-24db564cb146/site-mail.conf")
	for _, expected := range []string{"/opt/cdn-edge/logs/tcp-access.json", "/opt/cdn-edge/logs/tcp-error.log"} {
		if !strings.Contains(streamConfiguration, expected) {
			t.Fatalf("migrated stream configuration does not contain %q:\n%s", expected, streamConfiguration)
		}
	}
	if strings.Contains(streamConfiguration, "/var/log/nginx") {
		t.Fatalf("migrated stream configuration retained a Debian log path:\n%s", streamConfiguration)
	}
}

func TestInstallEdgeScriptRestoresLayoutOneStreamConfigOnFailure(t *testing.T) {
	harness := newInstallHarness(t)
	harness.seedLayoutOne()
	output, err := harness.run(t, "", "edge-binary-v2", "nginx-test")
	if err == nil {
		t.Fatalf("layout migration unexpectedly succeeded:\n%s", output)
	}
	harness.requireContents(t, "opt/cdn-edge/.layout-version", "1\n")
	harness.requireContents(t, "opt/cdn-edge/config/nginx/cdn-platform-stream.conf", layoutOneStreamConfiguration)
	harness.requireContents(t, "opt/cdn-edge/config/nginx/fragments/stream-v111-24db564cb146/site-mail.conf", layoutOneStreamFragmentConfiguration)
}

func TestInstallEdgeScriptRejectsUnsafeLayoutOneConfigEntry(t *testing.T) {
	harness := newInstallHarness(t)
	harness.seedLayoutOne()
	link := filepath.Join(harness.root, "opt/cdn-edge/config/nginx/fragments/unsafe.conf")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", link); err != nil {
		t.Fatal(err)
	}
	output, err := harness.run(t, "", "edge-binary-v2", "")
	if err == nil || !strings.Contains(output, "configuration contains an unsafe entry") {
		t.Fatalf("unsafe config entry was not rejected: %v\n%s", err, output)
	}
	harness.requireContents(t, "opt/cdn-edge/.layout-version", "1\n")
	harness.requireContents(t, "opt/cdn-edge/config/nginx/fragments/stream-v111-24db564cb146/site-mail.conf", layoutOneStreamFragmentConfiguration)
	if target, err := os.Readlink(link); err != nil || target != "/etc/passwd" {
		t.Fatalf("unsafe link was not restored: target=%q err=%v", target, err)
	}
}

func TestInstallEdgeScriptOnlineUpgradeAndReadinessRollback(t *testing.T) {
	harness := newInstallHarness(t)
	if output, err := harness.run(t, "first-token", "edge-binary-v1", ""); err != nil {
		t.Fatalf("first install failed: %v\n%s", err, output)
	}
	oldDigest := harness.read(t, "opt/cdn-edge/nginx/.bundle-sha256")
	oldQUICHostKey := harness.read(t, "opt/cdn-edge/config/nginx/quic-host.key")
	harness.write("opt/cdn-edge/data/preserved", "keep\n")
	harness.write("opt/cdn-edge/cache/cache-object", "cache\n")
	harness.setNginxVersion(t, "1.30.5")

	output, err := harness.runOnline(t, "edge-binary-v2", "readiness")
	if err == nil || !strings.Contains(output, "did not confirm a control-plane heartbeat") {
		t.Fatalf("online upgrade without readiness was not rejected: %v\n%s", err, output)
	}
	harness.requireContents(t, "opt/cdn-edge/bin/cdn-edge-agent", "edge-binary-v1")
	harness.requireContents(t, "opt/cdn-edge/nginx/VERSION", "1.30.4\n")
	harness.requireContents(t, "opt/cdn-edge/nginx/.bundle-sha256", oldDigest)
	harness.requireContents(t, "opt/cdn-edge/data/preserved", "keep\n")
	harness.requireContents(t, "opt/cdn-edge/cache/cache-object", "cache\n")
	harness.requireContents(t, "opt/cdn-edge/config/nginx/quic-host.key", oldQUICHostKey)

	output, err = harness.runOnline(t, "edge-binary-v2", "")
	if err != nil {
		t.Fatalf("online upgrade failed: %v\n%s", err, output)
	}
	harness.requireContents(t, "opt/cdn-edge/bin/cdn-edge-agent", "edge-binary-v2")
	harness.requireContents(t, "opt/cdn-edge/nginx/VERSION", "1.30.5\n")
	harness.requireContents(t, "opt/cdn-edge/data/preserved", "keep\n")
	harness.requireContents(t, "opt/cdn-edge/config/nginx/quic-host.key", oldQUICHostKey)
}

func TestInstallEdgeScriptRejectsUnsafeNginxBundleBeforeMutation(t *testing.T) {
	harness := newInstallHarness(t)
	harness.writeBundle(t, "1.30.4", true)
	output, err := harness.run(t, "first-token", "edge-binary-v1", "")
	if err == nil || !strings.Contains(output, "unsafe path") {
		t.Fatalf("unsafe bundle was not rejected: %v\n%s", err, output)
	}
	harness.requireAbsent(t, "opt/cdn-edge")
}

func TestInstallEdgeScriptRejectsInvalidNginxServiceBeforeMutation(t *testing.T) {
	harness := newInstallHarness(t)
	if err := os.WriteFile(harness.nginxServicePath, []byte("[Service]\nExecStart=/usr/sbin/nginx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output, err := harness.run(t, "first-token", "edge-binary-v1", "")
	if err == nil || !strings.Contains(output, "Nginx service does not match") {
		t.Fatalf("invalid Nginx service was not rejected: %v\n%s", err, output)
	}
	harness.requireAbsent(t, "opt/cdn-edge")
}

func TestInstallEdgeScriptSysctlDegradesAndUsesMemorySizedBuffers(t *testing.T) {
	harness := newInstallHarness(t)
	harness.write("proc/meminfo", "MemTotal:        8388608 kB\n")
	output, err := harness.run(t, "first-token", "edge-binary-v1", "sysctl-bbr")
	if err != nil {
		t.Fatalf("install failed instead of degrading: %v\n%s", err, output)
	}
	if !strings.Contains(output, "BBR is unavailable") {
		t.Fatalf("installer did not report unavailable BBR:\n%s", output)
	}
	profile := harness.read(t, "usr/local/lib/sysctl.d/40-simple-cdn-edge.conf")
	if strings.Contains(profile, "tcp_congestion_control") || !strings.Contains(profile, "33554432") {
		t.Fatalf("unexpected sysctl profile:\n%s", profile)
	}
}

func TestInstallEdgeScriptRollsBackLegacyMigrationOnNginxFailure(t *testing.T) {
	harness := newInstallHarness(t)
	harness.seedLegacy()
	output, err := harness.run(t, "", "edge-binary-v2", "nginx-test")
	if err == nil {
		t.Fatalf("migration unexpectedly succeeded:\n%s", output)
	}
	harness.requireAbsent(t, "opt/cdn-edge")
	harness.requireContents(t, "etc/nginx/conf.d/cdn-platform.conf", legacyNginxConfiguration)
	harness.requireContents(t, "usr/local/bin/cdn-edge-agent", "legacy-binary\n")
	harness.requireAbsent(t, "usr/local/lib/sysctl.d/40-simple-cdn-edge.conf")
	harness.requireContents(t, "run/mock-sysctl/net.ipv4.tcp_congestion_control", "cubic\n")
	transactions, globErr := filepath.Glob(filepath.Join(harness.root, "opt/.cdn-edge-install.*"))
	if globErr != nil || len(transactions) != 0 {
		t.Fatalf("installer left transaction directories: %#v, %v", transactions, globErr)
	}
}

func TestInstallEdgeScriptRejectsMixedLayouts(t *testing.T) {
	harness := newInstallHarness(t)
	harness.seedLayoutOne()
	harness.write("var/lib/cdn-platform/edge-client.key", "legacy key\n")
	output, err := harness.run(t, "token", "edge-binary-v2", "")
	if err == nil || !strings.Contains(output, "both /opt/cdn-edge and legacy CDN paths exist") {
		t.Fatalf("mixed layout was not rejected: %v\n%s", err, output)
	}
}

func TestInstallEdgeScriptCleansStaleLegacyPathsFromLayoutTwo(t *testing.T) {
	harness := newInstallHarness(t)
	if output, err := harness.run(t, "first-token", "edge-binary-v1", ""); err != nil {
		t.Fatalf("first install failed: %v\n%s", err, output)
	}
	harness.write("opt/cdn-edge/data/preserved", "keep\n")
	harness.write("usr/local/bin/cdn-edge-agent", "stale binary\n")
	harness.write("var/lib/cdn-platform/stale", "stale state\n")

	output, err := harness.run(t, "", "edge-binary-v2", "")
	if err != nil {
		t.Fatalf("reinstall with stale legacy paths failed: %v\n%s", err, output)
	}
	if !strings.Contains(output, "stale legacy CDN paths found") {
		t.Fatalf("stale legacy paths were not reported:\n%s", output)
	}
	harness.requireContents(t, "opt/cdn-edge/data/preserved", "keep\n")
	harness.requireContents(t, "opt/cdn-edge/bin/cdn-edge-agent", "edge-binary-v2")
	harness.requireAbsent(t, "usr/local/bin/cdn-edge-agent")
	harness.requireAbsent(t, "var/lib/cdn-platform")
}

const legacyNginxConfiguration = `# Generated by cdn-edge-agent. Do not edit.
proxy_cache_path /var/cache/cdn-platform levels=1:2 keys_zone=cdn_cache:100m;
server {
    listen 80 default_server;
    location = /__cdn_health { return 200 "ok\n"; }
    ssl_certificate /etc/cdn-platform/certs/site.crt;
    access_log /var/log/cdn-platform/access.json;
}
`

const layoutOneStreamConfiguration = `# Generated by cdn-edge-agent. Do not edit.
include /opt/cdn-edge/config/nginx/fragments/stream-v111-24db564cb146/*.conf;
`

const layoutOneStreamFragmentConfiguration = `# CDN stream site fragment mail port 9465 begin
server {
    listen 9465;
    access_log /var/log/nginx/cdn-platform-tcp-access.log cdn_tcp_json;
    error_log /var/log/nginx/cdn-platform-tcp-error.log warn;
}
# CDN stream site fragment mail port 9465 end
`

type installHarness struct {
	root               string
	mockBin            string
	logPath            string
	binaryPath         string
	servicePath        string
	updaterServicePath string
	nginxBundlePath    string
	nginxServicePath   string
	nginxVersion       string
	nginxFlags         string
}

func newInstallHarness(t *testing.T) *installHarness {
	t.Helper()
	root := t.TempDir()
	harness := &installHarness{
		root: root, mockBin: filepath.Join(root, "mock-bin"), logPath: filepath.Join(root, "mock.log"),
		binaryPath: filepath.Join(root, "download-binary"), servicePath: filepath.Join(root, "download-service"),
		updaterServicePath: filepath.Join(root, "download-updater-service"),
		nginxBundlePath:    filepath.Join(root, "cdn-nginx.tar.gz"), nginxServicePath: filepath.Join(root, "download-nginx-service"),
		nginxVersion: "1.30.4", nginxFlags: "--with-http_v2_module",
	}
	for _, directory := range []string{
		"tmp", "run", "mock-bin", "proc", "opt", "etc/nginx/conf.d", "etc/nginx/sites-enabled",
		"etc/nginx/modules-enabled", "etc/logrotate.d", "etc/systemd/system", "etc/sysctl.d", "usr/local/lib/sysctl.d",
	} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	harness.write("etc/nginx/nginx.conf", "worker_processes auto;\nevents { worker_connections 768; }\nhttp { include /etc/nginx/conf.d/*.conf; }\n")
	harness.write("etc/nginx/sites-enabled/default", "default site\n")
	harness.write("proc/meminfo", "MemTotal:        4194304 kB\n")
	for key, value := range map[string]string{
		"net.core.default_qdisc": "pfifo_fast\n", "net.ipv4.tcp_congestion_control": "cubic\n",
		"net.ipv4.tcp_mtu_probing": "0\n", "net.core.rmem_max": "4194304\n",
		"net.core.wmem_max": "4194304\n", "net.ipv4.tcp_rmem": "4096 131072 6291456\n",
		"net.ipv4.tcp_wmem": "4096 16384 4194304\n",
	} {
		harness.write(filepath.Join("run/mock-sysctl", key), value)
	}
	if err := os.WriteFile(harness.servicePath, []byte(bootstrapEdgeService), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(harness.updaterServicePath, []byte(bootstrapEdgeUpdaterService), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(harness.nginxServicePath, []byte(bootstrapEdgeNginxService), 0o644); err != nil {
		t.Fatal(err)
	}
	harness.writeBundle(t, harness.nginxVersion, false)
	harness.installMocks(t)
	return harness
}

func (h *installHarness) installMocks(t *testing.T) {
	h.writeMock(t, "curl", `#!/usr/bin/env bash
set -euo pipefail
printf 'curl %s\n' "$*" >>"$MOCK_LOG"
output=""
url=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --output) output="$2"; shift 2 ;;
    http*) url="$1"; shift ;;
    *) shift ;;
  esac
done
if [[ -z "$output" ]]; then exit 0; fi
case "$url" in
  https://downloads.example.test/edge) cp "$MOCK_BINARY" "$output" ;;
  https://edge-control.example.test/install-edge.service) cp "$MOCK_SERVICE" "$output" ;;
  https://edge-control.example.test/install-edge-updater.service) cp "$MOCK_UPDATER_SERVICE" "$output" ;;
  *) echo "unexpected URL: $url" >&2; exit 1 ;;
esac
`)
	h.writeMock(t, "chown", "#!/usr/bin/env bash\nprintf 'chown %s\\n' \"$*\" >>\"$MOCK_LOG\"\n")
	h.writeMock(t, "sleep", "#!/usr/bin/env bash\nexit 0\n")
	h.writeMock(t, "modprobe", "#!/usr/bin/env bash\nprintf 'modprobe %s\\n' \"$*\" >>\"$MOCK_LOG\"\n")
	h.writeMock(t, "sysctl", `#!/usr/bin/env bash
set -euo pipefail
printf 'sysctl %s\n' "$*" >>"$MOCK_LOG"
state_dir="$CDN_EDGE_INSTALL_ROOT/run/mock-sysctl"
trim() { awk '{$1=$1; print}' <<<"$1"; }
write_setting() {
  local key="$1" value
  value=$(trim "$2")
  if [[ "${MOCK_FAILURE:-}" == "sysctl-bbr" && "$key" == "net.ipv4.tcp_congestion_control" && "$value" == "bbr" ]]; then return 1; fi
  mkdir -p "$state_dir"
  printf '%s\n' "$value" >"$state_dir/$key"
}
apply_file() {
  local file="$1" line key value
  [[ -f "$file" ]] || return 0
  while IFS= read -r line || [[ -n "$line" ]]; do
    line=$(trim "$line")
    [[ -z "$line" || "$line" == \#* || "$line" == \;* ]] && continue
    line="${line#-}"
    key=$(trim "${line%%=*}")
    value=$(trim "${line#*=}")
    write_setting "$key" "$value"
  done <"$file"
}
while [[ "${1:-}" == "-q" ]]; do shift; done
case "${1:-}" in
  -n) cat "$state_dir/$2" ;;
  -w) assignment="$2"; write_setting "${assignment%%=*}" "${assignment#*=}" ;;
  -p) apply_file "$2" ;;
  --system)
    for file in "$CDN_EDGE_INSTALL_ROOT/usr/local/lib/sysctl.d/"*.conf "$CDN_EDGE_INSTALL_ROOT/etc/sysctl.d/"*.conf; do
      [[ -e "$file" ]] && apply_file "$file"
    done
    ;;
  *) echo "unexpected sysctl arguments: $*" >&2; exit 1 ;;
esac
`)
	h.writeMock(t, "systemctl", `#!/usr/bin/env bash
set -euo pipefail
printf 'systemctl %s\n' "$*" >>"$MOCK_LOG"
root="$CDN_EDGE_INSTALL_ROOT"
command="${1:-}"
service="${*: -1}"
state_name=agent
if [[ "$service" == "nginx" || "$service" == "nginx.service" ]]; then state_name=nginx; fi
active="$root/run/mock-$state_name-active"
enabled="$root/run/mock-$state_name-enabled"
case "$command" in
  is-active) [[ -f "$active" ]] ;;
  is-enabled) [[ -f "$enabled" ]] ;;
  stop) rm -f "$active" ;;
  disable) rm -f "$enabled" ;;
  enable) touch "$enabled" ;;
  start|restart)
    if [[ "$state_name" == "nginx" && "${MOCK_FAILURE:-}" == "nginx-start" ]]; then exit 1; fi
    if [[ "$state_name" == "agent" && "${MOCK_FAILURE:-}" == "agent" ]]; then exit 1; fi
    touch "$active"
    if [[ "$state_name" == "agent" ]]; then
      unit="$root/etc/systemd/system/cdn-edge-agent.service"
      if [[ -L "$unit" && "$(readlink "$unit")" == "$root/opt/cdn-edge/systemd/cdn-edge-agent.service" ]]; then
        mkdir -p "$root/opt/cdn-edge/data"
        for file in edge-client.key edge-client.crt edge-ca.crt; do
          [[ -s "$root/opt/cdn-edge/data/$file" ]] || printf '%s\n' "$file" >"$root/opt/cdn-edge/data/$file"
        done
        if [[ -n "${MOCK_READINESS_FILE:-}" && "${MOCK_FAILURE:-}" != "readiness" ]]; then
          mkdir -p "$(dirname "$MOCK_READINESS_FILE")"
          sha256sum "$MOCK_BINARY" | awk '{print $1}' >"$MOCK_READINESS_FILE"
        fi
      fi
    fi
    ;;
  reload|daemon-reload|reset-failed) ;;
esac
`)
}

func (h *installHarness) writeBundle(t *testing.T, version string, unsafe bool) {
	t.Helper()
	file, err := os.Create(h.nginxBundlePath)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	entries := []struct {
		name     string
		mode     int64
		contents string
		typeflag byte
	}{
		{"nginx/", 0o755, "", tar.TypeDir},
		{"nginx/sbin/", 0o755, "", tar.TypeDir},
		{"nginx/conf/", 0o755, "", tar.TypeDir},
		{"nginx/lib/", 0o755, "", tar.TypeDir},
		{"nginx/licenses/", 0o755, "", tar.TypeDir},
		{"nginx/sbin/nginx", 0o755, fmt.Sprintf(`#!/usr/bin/env bash
printf 'managed-nginx %%s\n' "$*" >>"$MOCK_LOG"
if [[ "${1:-}" == "-V" ]]; then
  printf 'nginx version: nginx/%s configure arguments: %%s\n' "${MOCK_NGINX_FLAGS:-}" >&2
  exit 0
fi
if [[ "${MOCK_FAILURE:-}" == "nginx-test" && "${1:-}" == "-t" ]]; then exit 1; fi
exit 0
`, version), tar.TypeReg},
		{"nginx/conf/nginx.conf", 0o644, "# managed test config\n", tar.TypeReg},
		{"nginx/conf/mime.types", 0o644, "types {}\n", tar.TypeReg},
		{"nginx/licenses/nginx.txt", 0o644, "nginx license\n", tar.TypeReg},
		{"nginx/licenses/ngx_devel_kit.txt", 0o644, "ndk license\n", tar.TypeReg},
		{"nginx/licenses/openresty-luajit.txt", 0o644, "luajit license\n", tar.TypeReg},
		{"nginx/licenses/lua-nginx-module.txt", 0o644, "lua nginx license\n", tar.TypeReg},
		{"nginx/licenses/lua-resty-core.txt", 0o644, "resty core license\n", tar.TypeReg},
		{"nginx/licenses/lua-resty-lrucache.txt", 0o644, "lrucache license\n", tar.TypeReg},
		{"nginx/licenses/ngx_brotli.txt", 0o644, "ngx brotli license\n", tar.TypeReg},
		{"nginx/licenses/brotli.txt", 0o644, "brotli license\n", tar.TypeReg},
		{"nginx/licenses/zstd-nginx-module.txt", 0o644, "zstd nginx license\n", tar.TypeReg},
		{"nginx/licenses/zstd-library.txt", 0o644, "zstd license\n", tar.TypeReg},
		{"nginx/VERSION", 0o644, version + "\n", tar.TypeReg},
		{"nginx/BUILD.json", 0o644, testNginxBuildJSON(version, "amd64") + "\n", tar.TypeReg},
	}
	if unsafe {
		entries = append(entries, struct {
			name     string
			mode     int64
			contents string
			typeflag byte
		}{"../escape", 0o644, "unsafe\n", tar.TypeReg})
	}
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Mode: entry.mode, Typeflag: entry.typeflag, Size: int64(len(entry.contents))}
		if entry.typeflag == tar.TypeDir {
			header.Size = 0
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if entry.contents != "" {
			if _, err := tarWriter.Write([]byte(entry.contents)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func (h *installHarness) setNginxVersion(t *testing.T, version string) {
	t.Helper()
	h.nginxVersion = version
	h.writeBundle(t, version, false)
}

func (h *installHarness) seedLegacy() {
	for path, contents := range map[string]string{
		"usr/local/bin/cdn-edge-agent":    "legacy-binary\n",
		"etc/cdn-platform/edge.env":       "CONTROL_URL=https://old.example.test\nENROLLMENT_TOKEN=old-token\nEDGE_POLL_SECONDS=45\n",
		"etc/cdn-platform/certs/site.crt": "site-cert\n", "etc/cdn-platform/certs/site.key": "site-key\n",
		"var/lib/cdn-platform/edge-client.key": "legacy-key\n", "var/lib/cdn-platform/edge-client.crt": "legacy-cert\n",
		"var/lib/cdn-platform/edge-ca.crt": "legacy-ca\n", "var/lib/cdn-platform/access-log-queue.ndjson": "queued\n",
		"var/log/cdn-platform/access.json": "access event\n", "var/cache/cdn-platform/cache-object": "discard\n",
		"etc/nginx/conf.d/cdn-platform.conf":        legacyNginxConfiguration,
		"etc/systemd/system/cdn-edge-agent.service": "legacy service\n",
	} {
		h.write(path, contents)
	}
	h.write("run/mock-agent-active", "")
	h.write("run/mock-agent-enabled", "")
	h.write("run/mock-nginx-active", "")
}

func (h *installHarness) seedLayoutOne() {
	for path, contents := range map[string]string{
		"opt/cdn-edge/.layout-version": "1\n", "opt/cdn-edge/bin/cdn-edge-agent": "old agent\n",
		"opt/cdn-edge/config/edge.env":                                                "CONTROL_URL=https://old.example.test\nEDGE_POLL_SECONDS=60\n",
		"opt/cdn-edge/config/nginx/cdn-platform.conf":                                 "# existing\n",
		"opt/cdn-edge/config/nginx/cdn-platform-stream.conf":                          layoutOneStreamConfiguration,
		"opt/cdn-edge/config/nginx/fragments/stream-v111-24db564cb146/site-mail.conf": layoutOneStreamFragmentConfiguration,
		"opt/cdn-edge/config/nginx/cdn-platform-main.conf":                            "worker_processes auto;\n",
		"opt/cdn-edge/config/nginx/cdn-platform-events.conf":                          "worker_connections 4096;\n",
		"opt/cdn-edge/data/edge-client.key":                                           "key\n", "opt/cdn-edge/data/edge-client.crt": "cert\n",
		"opt/cdn-edge/data/edge-ca.crt": "ca\n", "opt/cdn-edge/data/preserved": "keep\n",
	} {
		h.write(path, contents)
	}
}

func (h *installHarness) run(t *testing.T, token, binary, failure string) (string, error) {
	t.Helper()
	if err := os.WriteFile(h.binaryPath, []byte(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	arguments := []string{"-s", "--", "--control-url", "https://edge-control.example.test"}
	if token != "" {
		arguments = append(arguments, "--enrollment-token", token)
	}
	arguments = append(arguments,
		"--binary-url", "https://downloads.example.test/edge", "--binary-sha256", fileDigest(t, h.binaryPath),
		"--service-sha256", fileDigest(t, h.servicePath),
		"--updater-service-sha256", fileDigest(t, h.updaterServicePath),
		"--nginx-bundle-file", h.nginxBundlePath, "--nginx-bundle-sha256", fileDigest(t, h.nginxBundlePath),
		"--nginx-service-file", h.nginxServicePath, "--nginx-service-sha256", fileDigest(t, h.nginxServicePath),
	)
	return h.execute(arguments, failure, "")
}

func (h *installHarness) runOnline(t *testing.T, binary, failure string) (string, error) {
	t.Helper()
	if err := os.WriteFile(h.binaryPath, []byte(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	readinessPath := filepath.Join(h.root, "opt/cdn-edge/data/upgrades/online-test/ready")
	if err := os.MkdirAll(filepath.Dir(readinessPath), 0o700); err != nil {
		t.Fatal(err)
	}
	arguments := []string{
		"-s", "--", "--control-url", "https://edge-control.example.test",
		"--binary-file", h.binaryPath, "--binary-sha256", fileDigest(t, h.binaryPath),
		"--service-file", h.servicePath, "--service-sha256", fileDigest(t, h.servicePath),
		"--updater-service-file", h.updaterServicePath, "--updater-service-sha256", fileDigest(t, h.updaterServicePath),
		"--nginx-bundle-file", h.nginxBundlePath, "--nginx-bundle-sha256", fileDigest(t, h.nginxBundlePath),
		"--nginx-service-file", h.nginxServicePath, "--nginx-service-sha256", fileDigest(t, h.nginxServicePath),
		"--readiness-file", readinessPath,
	}
	return h.execute(arguments, failure, readinessPath)
}

func (h *installHarness) execute(arguments []string, failure, readinessPath string) (string, error) {
	command := exec.Command("bash", arguments...)
	command.Stdin = strings.NewReader(bootstrapEdgeScript)
	command.Env = []string{
		"PATH=" + h.mockBin + ":/usr/bin:/bin", "CDN_EDGE_INSTALL_ROOT=" + h.root,
		"MOCK_LOG=" + h.logPath, "MOCK_BINARY=" + h.binaryPath,
		"MOCK_SERVICE=" + h.servicePath, "MOCK_UPDATER_SERVICE=" + h.updaterServicePath,
		"MOCK_READINESS_FILE=" + readinessPath, "MOCK_FAILURE=" + failure,
		"MOCK_NGINX_FLAGS=" + h.nginxFlags,
	}
	output, err := command.CombinedOutput()
	return string(output), err
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(contents))
}

func (h *installHarness) writeMock(t *testing.T, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.mockBin, name), []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (h *installHarness) write(path, contents string) {
	fullPath := filepath.Join(h.root, path)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(fullPath, []byte(contents), 0o644); err != nil {
		panic(err)
	}
}

func (h *installHarness) read(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(h.root, path))
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func (h *installHarness) requireContents(t *testing.T, path, expected string) {
	t.Helper()
	if actual := h.read(t, path); actual != expected {
		t.Fatalf("%s = %q, want %q", path, actual, expected)
	}
}

func (h *installHarness) requirePath(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(h.root, path)); err != nil {
		t.Fatalf("expected %s: %v", path, err)
	}
}

func (h *installHarness) requireAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(h.root, path)); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be absent: %v", path, err)
	}
}

func (h *installHarness) requireLink(t *testing.T, path, target string) {
	t.Helper()
	actual, err := os.Readlink(filepath.Join(h.root, path))
	if err != nil {
		t.Fatalf("read %s symlink: %v", path, err)
	}
	expected := filepath.Join(h.root, target)
	if actual != expected {
		t.Fatalf("%s -> %s, want %s", path, actual, expected)
	}
}
