package control

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallEdgeScriptSyntax(t *testing.T) {
	command := exec.Command("bash", "-n")
	command.Stdin = strings.NewReader(bootstrapEdgeScript)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, output)
	}
	if !strings.Contains(bootstrapEdgeScript, "iproute2 nftables") {
		t.Fatal("edge installer does not install nftables")
	}
	if !strings.Contains(bootstrapEdgeScript, "lz4 procps kmod") {
		t.Fatal("edge installer does not install sysctl and kernel module tooling")
	}
}

func TestInstallEdgeScriptCreatesOptLayout(t *testing.T) {
	harness := newInstallHarness(t)
	harness.write("etc/nginx/sites-enabled/default", "default site\n")

	output, err := harness.run(t, "first-token", "edge-binary-v1", "")
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, output)
	}
	for _, path := range []string{
		"opt/cdn-edge/.layout-version",
		"opt/cdn-edge/bin/cdn-edge-agent",
		"opt/cdn-edge/config/edge.env",
		"opt/cdn-edge/config/nginx/cdn-platform-stream.conf",
		"opt/cdn-edge/config/nginx/cdn-platform-main.conf",
		"opt/cdn-edge/config/nginx/cdn-platform-events.conf",
		"etc/logrotate.d/cdn-edge-platform",
		"opt/cdn-edge/data/edge-client.key",
		"opt/cdn-edge/data/edge-client.crt",
		"opt/cdn-edge/data/edge-ca.crt",
		"opt/cdn-edge/systemd/cdn-edge-agent.service",
		"opt/cdn-edge/systemd/cdn-edge-updater@.service",
		"opt/cdn-edge/data/sysctl-baseline.conf",
		"usr/local/lib/sysctl.d/40-simple-cdn-edge.conf",
	} {
		harness.requirePath(t, path)
	}
	harness.requireAbsent(t, "etc/nginx/sites-enabled/default")
	harness.requireLink(t, "etc/nginx/conf.d/cdn-platform.conf", "opt/cdn-edge/config/nginx/cdn-platform.conf")
	harness.requirePath(t, "etc/nginx/modules-enabled/99-cdn-platform-stream.conf")
	nginxRoot := harness.read(t, "etc/nginx/nginx.conf")
	for _, expected := range []string{"include /opt/cdn-edge/config/nginx/cdn-platform-main.conf;", "include /opt/cdn-edge/config/nginx/cdn-platform-events.conf;"} {
		if !strings.Contains(nginxRoot, expected) {
			t.Fatalf("nginx.conf does not include managed capacity config %q:\n%s", expected, nginxRoot)
		}
	}
	logrotate := harness.read(t, "etc/logrotate.d/cdn-edge-platform")
	for _, expected := range []string{"size 32M", "rotate 16", "compresscmd /usr/bin/lz4", "compressext .lz4", "copytruncate"} {
		if !strings.Contains(logrotate, expected) {
			t.Fatalf("logrotate policy does not contain %q:\n%s", expected, logrotate)
		}
	}
	harness.requireLink(t, "etc/systemd/system/cdn-edge-agent.service", "opt/cdn-edge/systemd/cdn-edge-agent.service")
	harness.requireLink(t, "etc/systemd/system/cdn-edge-updater@.service", "opt/cdn-edge/systemd/cdn-edge-updater@.service")
	harness.requireContents(t, "opt/cdn-edge/bin/cdn-edge-agent", "edge-binary-v1")
	if configuration := harness.read(t, "opt/cdn-edge/config/nginx/cdn-platform.conf"); !strings.Contains(configuration, "location = /__cdn_health") {
		t.Fatalf("fresh install did not create the health-only Nginx configuration:\n%s", configuration)
	}
	environment := harness.read(t, "opt/cdn-edge/config/edge.env")
	for _, expected := range []string{
		"CONTROL_URL=https://edge-control.example.test",
		"ENROLLMENT_TOKEN=first-token",
		"EDGE_STATE_DIR=/opt/cdn-edge/data",
		"NGINX_CONFIG_PATH=/opt/cdn-edge/config/nginx/cdn-platform.conf",
		"NGINX_STREAM_CONFIG_PATH=/opt/cdn-edge/config/nginx/cdn-platform-stream.conf",
		"NGINX_MAIN_CONFIG_PATH=/opt/cdn-edge/config/nginx/cdn-platform-main.conf",
		"NGINX_EVENTS_CONFIG_PATH=/opt/cdn-edge/config/nginx/cdn-platform-events.conf",
		"EDGE_CERT_DIR=/opt/cdn-edge/config/certs",
		"EDGE_ACCESS_LOG=/opt/cdn-edge/logs/access.json",
		"EDGE_SECURITY_LOG=/opt/cdn-edge/logs/security.json",
		"EDGE_CAPABILITIES=tcp_stream_v1,edge_rate_limit_v1,nginx_capacity_v1",
	} {
		if !strings.Contains(environment, expected) {
			t.Fatalf("edge.env does not contain %q:\n%s", expected, environment)
		}
	}
	if !strings.Contains(bootstrapEdgeScript, "libnginx-mod-http-lua") {
		t.Fatal("edge installer does not install the Nginx Lua rate limit module")
	}
	sysctlConfiguration := harness.read(t, "usr/local/lib/sysctl.d/40-simple-cdn-edge.conf")
	for _, expected := range []string{
		"-net.core.default_qdisc = fq",
		"-net.ipv4.tcp_congestion_control = bbr",
		"-net.ipv4.tcp_mtu_probing = 1",
		"-net.core.rmem_max = 16777216",
		"-net.core.wmem_max = 16777216",
		"-net.ipv4.tcp_rmem = 4096 131072 16777216",
		"-net.ipv4.tcp_wmem = 4096 16384 16777216",
	} {
		if !strings.Contains(sysctlConfiguration, expected) {
			t.Fatalf("sysctl profile does not contain %q:\n%s", expected, sysctlConfiguration)
		}
	}
	for _, unexpected := range []string{"tcp_tw_reuse", "tcp_fin_timeout", "ip_local_port_range", "disable_ipv6", "somaxconn"} {
		if strings.Contains(sysctlConfiguration, unexpected) {
			t.Fatalf("sysctl profile contains unmanaged tuning %q:\n%s", unexpected, sysctlConfiguration)
		}
	}
	baseline := harness.read(t, "opt/cdn-edge/data/sysctl-baseline.conf")
	for _, expected := range []string{
		"net.core.default_qdisc = pfifo_fast",
		"net.ipv4.tcp_congestion_control = cubic",
		"net.ipv4.tcp_mtu_probing = 0",
		"net.core.rmem_max = 4194304",
		"net.core.wmem_max = 4194304",
		"net.ipv4.tcp_rmem = 4096 131072 6291456",
		"net.ipv4.tcp_wmem = 4096 16384 4194304",
	} {
		if !strings.Contains(baseline, expected) {
			t.Fatalf("sysctl baseline does not contain %q:\n%s", expected, baseline)
		}
	}
	log := harness.read(t, "mock.log")
	for _, expected := range []string{"modprobe sch_fq", "modprobe tcp_bbr", "sysctl --system"} {
		if !strings.Contains(log, expected) {
			t.Fatalf("installer did not invoke %q:\n%s", expected, log)
		}
	}
}

func TestInstallEdgeScriptAdvertisesHTTP3OnlyWhenNginxSupportsIt(t *testing.T) {
	harness := newInstallHarness(t)
	harness.nginxVersion = "nginx version: nginx/1.26.3 configure arguments: --with-http_v2_module --with-http_v3_module"

	output, err := harness.run(t, "first-token", "edge-binary-v1", "")
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, output)
	}
	environment := harness.read(t, "opt/cdn-edge/config/edge.env")
	if !strings.Contains(environment, "EDGE_CAPABILITIES=tcp_stream_v1,edge_rate_limit_v1,nginx_capacity_v1,http3_v1") {
		t.Fatalf("HTTP/3-capable Nginx was not advertised:\n%s", environment)
	}
}

func TestInstallEdgeScriptUsesLargeTCPBuffersAboveFourGiB(t *testing.T) {
	harness := newInstallHarness(t)
	harness.write("proc/meminfo", "MemTotal:        8388608 kB\n")

	output, err := harness.run(t, "first-token", "edge-binary-v1", "")
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, output)
	}
	configuration := harness.read(t, "usr/local/lib/sysctl.d/40-simple-cdn-edge.conf")
	for _, expected := range []string{
		"-net.core.rmem_max = 33554432",
		"-net.core.wmem_max = 33554432",
		"-net.ipv4.tcp_rmem = 4096 131072 33554432",
		"-net.ipv4.tcp_wmem = 4096 16384 33554432",
	} {
		if !strings.Contains(configuration, expected) {
			t.Fatalf("large-memory sysctl profile does not contain %q:\n%s", expected, configuration)
		}
	}
}

func TestInstallEdgeScriptSkipsUnsupportedBBR(t *testing.T) {
	harness := newInstallHarness(t)

	output, err := harness.run(t, "first-token", "edge-binary-v1", "sysctl-bbr")
	if err != nil {
		t.Fatalf("install failed instead of degrading: %v\n%s", err, output)
	}
	if !strings.Contains(output, "BBR is unavailable") {
		t.Fatalf("install did not report the BBR downgrade:\n%s", output)
	}
	configuration := harness.read(t, "usr/local/lib/sysctl.d/40-simple-cdn-edge.conf")
	if strings.Contains(configuration, "tcp_congestion_control") {
		t.Fatalf("unsupported BBR setting was persisted:\n%s", configuration)
	}
	harness.requireContents(t, "run/mock-sysctl/net.ipv4.tcp_congestion_control", "cubic\n")
}

func TestInstallEdgeScriptSkipsIncompleteTCPBufferGroup(t *testing.T) {
	harness := newInstallHarness(t)

	output, err := harness.run(t, "first-token", "edge-binary-v1", "sysctl-buffer")
	if err != nil {
		t.Fatalf("install failed instead of degrading: %v\n%s", err, output)
	}
	if !strings.Contains(output, "TCP buffer tuning is not fully supported") {
		t.Fatalf("install did not report the TCP buffer downgrade:\n%s", output)
	}
	configuration := harness.read(t, "usr/local/lib/sysctl.d/40-simple-cdn-edge.conf")
	for _, key := range []string{"net.core.rmem_max", "net.core.wmem_max", "net.ipv4.tcp_rmem", "net.ipv4.tcp_wmem"} {
		if strings.Contains(configuration, key) {
			t.Fatalf("partial TCP buffer group persisted %s:\n%s", key, configuration)
		}
	}
	harness.requireContents(t, "run/mock-sysctl/net.core.rmem_max", "4194304\n")
	harness.requireContents(t, "run/mock-sysctl/net.ipv4.tcp_wmem", "4096 16384 4194304\n")
}

func TestInstallEdgeScriptHonorsLaterAdministratorSysctl(t *testing.T) {
	harness := newInstallHarness(t)
	harness.write("etc/sysctl.d/99-admin.conf", "net.ipv4.tcp_congestion_control = cubic\n")

	output, err := harness.run(t, "first-token", "edge-binary-v1", "")
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, output)
	}
	if !strings.Contains(output, "later administrator setting may override it") {
		t.Fatalf("install did not report the administrator override:\n%s", output)
	}
	configuration := harness.read(t, "usr/local/lib/sysctl.d/40-simple-cdn-edge.conf")
	if !strings.Contains(configuration, "-net.ipv4.tcp_congestion_control = bbr") {
		t.Fatalf("platform default was not persisted:\n%s", configuration)
	}
	harness.requireContents(t, "run/mock-sysctl/net.ipv4.tcp_congestion_control", "cubic\n")
}

func TestInstallEdgeScriptRequiresTokenForFreshHost(t *testing.T) {
	harness := newInstallHarness(t)
	output, err := harness.run(t, "", "edge-binary-v1", "")
	if err == nil || !strings.Contains(output, "an enrollment token is required") {
		t.Fatalf("fresh install without a token was not rejected: %v\n%s", err, output)
	}
	harness.requireAbsent(t, "opt/cdn-edge")
}

func TestInstallEdgeScriptRepairsNginxTempDirectories(t *testing.T) {
	harness := newInstallHarness(t)
	for _, name := range []string{"body", "fastcgi", "proxy", "scgi", "uwsgi"} {
		path := filepath.Join(harness.root, "var/lib/nginx", name)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	output, err := harness.run(t, "first-token", "edge-binary-v1", "")
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, output)
	}
	log := harness.read(t, "mock.log")
	for _, name := range []string{"body", "fastcgi", "proxy", "scgi", "uwsgi"} {
		path := filepath.Join(harness.root, "var/lib/nginx", name)
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatalf("stat %s: %v", path, statErr)
		}
		if mode := info.Mode().Perm(); mode != 0o700 {
			t.Errorf("%s mode = %04o, want 0700", path, mode)
		}
		if expected := "chown www-data:root " + path; !strings.Contains(log, expected) {
			t.Errorf("installer did not set ownership for %s:\n%s", path, log)
		}
	}
}

func TestInstallEdgeScriptRejectsSymlinkedNginxTempDirectory(t *testing.T) {
	harness := newInstallHarness(t)
	target := filepath.Join(harness.root, "outside-nginx-temp")
	if err := os.MkdirAll(filepath.Join(harness.root, "var/lib/nginx"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(harness.root, "var/lib/nginx/body")); err != nil {
		t.Fatal(err)
	}

	output, err := harness.run(t, "first-token", "edge-binary-v1", "")
	if err == nil || !strings.Contains(output, "Nginx temp path is not a safe directory") {
		t.Fatalf("install with symlinked Nginx temp path was not rejected: %v\n%s", err, output)
	}
}

func TestInstallEdgeScriptMigratesLegacyStateWithoutCache(t *testing.T) {
	harness := newInstallHarness(t)
	harness.seedLegacy(t)

	output, err := harness.run(t, "", "edge-binary-v2", "")
	if err != nil {
		t.Fatalf("migration failed: %v\n%s", err, output)
	}
	for _, path := range []string{
		"usr/local/bin/cdn-edge-agent", "etc/cdn-platform", "var/lib/cdn-platform",
		"var/log/cdn-platform", "var/cache/cdn-platform",
	} {
		harness.requireAbsent(t, path)
	}
	for path, contents := range map[string]string{
		"opt/cdn-edge/data/edge-client.key":         "legacy-key\n",
		"opt/cdn-edge/data/access-log-queue.ndjson": "queued\n",
		"opt/cdn-edge/data/access-log-offset":       "17\n",
		"opt/cdn-edge/config/certs/site.crt":        "site-cert\n",
		"opt/cdn-edge/logs/access.json":             "access event\n",
		"opt/cdn-edge/logs/access.json.1":           "rotated event\n",
	} {
		harness.requireContents(t, path, contents)
	}
	harness.requireAbsent(t, "opt/cdn-edge/cache/cache-object")
	configuration := harness.read(t, "opt/cdn-edge/config/nginx/cdn-platform.conf")
	for _, expected := range []string{"/opt/cdn-edge/cache", "/opt/cdn-edge/config/certs/site.crt", "/opt/cdn-edge/logs/access.json"} {
		if !strings.Contains(configuration, expected) {
			t.Fatalf("migrated Nginx configuration does not contain %q:\n%s", expected, configuration)
		}
	}
	if strings.Contains(configuration, "/var/cache/cdn-platform") || strings.Contains(configuration, "/etc/cdn-platform") || strings.Contains(configuration, "/var/log/cdn-platform") {
		t.Fatalf("migrated Nginx configuration retained legacy paths:\n%s", configuration)
	}
	if environment := harness.read(t, "opt/cdn-edge/config/edge.env"); !strings.Contains(environment, "EDGE_POLL_SECONDS=45") {
		t.Fatalf("migration did not retain poll interval:\n%s", environment)
	}
	log := harness.read(t, "mock.log")
	if !strings.Contains(log, "systemctl restart nginx.service") {
		t.Fatalf("legacy migration did not cold-start the new cache zone:\n%s", log)
	}
}

func TestInstallEdgeScriptUpgradesNewLayoutIdempotently(t *testing.T) {
	harness := newInstallHarness(t)
	if output, err := harness.run(t, "first-token", "edge-binary-v1", ""); err != nil {
		t.Fatalf("first install failed: %v\n%s", err, output)
	}
	baseline := harness.read(t, "opt/cdn-edge/data/sysctl-baseline.conf")
	harness.write("opt/cdn-edge/data/pending-state", "keep data\n")
	harness.write("opt/cdn-edge/cache/cache-object", "keep new cache\n")
	harness.write("opt/cdn-edge/config/nginx/cdn-platform-stream.conf", "# existing stream config\n")
	harness.write("proc/meminfo", "MemTotal:        8388608 kB\n")
	environmentPath := filepath.Join(harness.root, "opt/cdn-edge/config/edge.env")
	environment := strings.ReplaceAll(harness.read(t, "opt/cdn-edge/config/edge.env"), "EDGE_POLL_SECONDS=30", "EDGE_POLL_SECONDS=75")
	if err := os.WriteFile(environmentPath, []byte(environment), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := harness.run(t, "", "edge-binary-v2", "")
	if err != nil {
		t.Fatalf("upgrade failed: %v\n%s", err, output)
	}
	harness.requireContents(t, "opt/cdn-edge/bin/cdn-edge-agent", "edge-binary-v2")
	harness.requireContents(t, "opt/cdn-edge/data/pending-state", "keep data\n")
	harness.requireContents(t, "opt/cdn-edge/cache/cache-object", "keep new cache\n")
	harness.requireContents(t, "opt/cdn-edge/config/nginx/cdn-platform-stream.conf", "# existing stream config\n")
	harness.requireContents(t, "opt/cdn-edge/data/sysctl-baseline.conf", baseline)
	if configuration := harness.read(t, "usr/local/lib/sysctl.d/40-simple-cdn-edge.conf"); !strings.Contains(configuration, "33554432") {
		t.Fatalf("upgrade did not recalculate the TCP buffer ceiling:\n%s", configuration)
	}
	environment = harness.read(t, "opt/cdn-edge/config/edge.env")
	if !strings.Contains(environment, "EDGE_POLL_SECONDS=75") || !strings.Contains(environment, "ENROLLMENT_TOKEN=\n") {
		t.Fatalf("upgrade did not update environment safely:\n%s", environment)
	}
	log := harness.read(t, "mock.log")
	if !strings.Contains(log, "systemctl reload nginx.service") {
		t.Fatalf("new-layout upgrade did not retain zero-downtime reload:\n%s", log)
	}
}

func TestInstallEdgeScriptOnlineUpgradeWaitsForAgentReadiness(t *testing.T) {
	harness := newInstallHarness(t)
	if output, err := harness.run(t, "first-token", "edge-binary-v1", ""); err != nil {
		t.Fatalf("first install failed: %v\n%s", err, output)
	}
	output, err := harness.runOnline(t, "edge-binary-v2", "")
	if err != nil {
		t.Fatalf("online upgrade failed: %v\n%s", err, output)
	}
	harness.requireContents(t, "opt/cdn-edge/bin/cdn-edge-agent", "edge-binary-v2")
	harness.requireLink(t, "etc/systemd/system/cdn-edge-updater@.service", "opt/cdn-edge/systemd/cdn-edge-updater@.service")
}

func TestInstallEdgeScriptOnlineUpgradeRollsBackWithoutReadiness(t *testing.T) {
	harness := newInstallHarness(t)
	if output, err := harness.run(t, "first-token", "edge-binary-v1", ""); err != nil {
		t.Fatalf("first install failed: %v\n%s", err, output)
	}
	previousSysctl := harness.read(t, "usr/local/lib/sysctl.d/40-simple-cdn-edge.conf")
	previousBaseline := harness.read(t, "opt/cdn-edge/data/sysctl-baseline.conf")
	harness.write("proc/meminfo", "MemTotal:        8388608 kB\n")
	output, err := harness.runOnline(t, "edge-binary-v2", "readiness")
	if err == nil || !strings.Contains(output, "did not confirm a control-plane heartbeat") {
		t.Fatalf("online upgrade without readiness was not rejected: %v\n%s", err, output)
	}
	harness.requireContents(t, "opt/cdn-edge/bin/cdn-edge-agent", "edge-binary-v1")
	harness.requireLink(t, "etc/systemd/system/cdn-edge-agent.service", "opt/cdn-edge/systemd/cdn-edge-agent.service")
	harness.requireContents(t, "usr/local/lib/sysctl.d/40-simple-cdn-edge.conf", previousSysctl)
	harness.requireContents(t, "opt/cdn-edge/data/sysctl-baseline.conf", previousBaseline)
	harness.requireContents(t, "run/mock-sysctl/net.core.rmem_max", "16777216\n")
	harness.requireContents(t, "run/mock-sysctl/net.ipv4.tcp_wmem", "4096 16384 16777216\n")
}

func TestInstallEdgeScriptRestoresLegacyNginxAfterRestartFailure(t *testing.T) {
	harness := newInstallHarness(t)
	harness.seedLegacy(t)

	output, err := harness.run(t, "", "edge-binary-v2", "nginx-restart-once")
	if err == nil {
		t.Fatalf("migration unexpectedly succeeded:\n%s", output)
	}
	harness.requireAbsent(t, "opt/cdn-edge")
	harness.requireContents(t, "etc/nginx/conf.d/cdn-platform.conf", legacyNginxConfiguration)
	log := harness.read(t, "mock.log")
	if strings.Count(log, "systemctl restart nginx.service") < 2 {
		t.Fatalf("rollback did not cold-start the restored legacy configuration:\n%s", log)
	}
}

func TestInstallEdgeScriptRollsBackLegacyMigration(t *testing.T) {
	harness := newInstallHarness(t)
	harness.seedLegacy(t)

	output, err := harness.run(t, "", "edge-binary-v2", "nginx")
	if err == nil {
		t.Fatalf("migration unexpectedly succeeded:\n%s", output)
	}
	harness.requireAbsent(t, "opt/cdn-edge")
	for path, contents := range map[string]string{
		"usr/local/bin/cdn-edge-agent":              "legacy-binary\n",
		"etc/cdn-platform/edge.env":                 "CONTROL_URL=https://old.example.test\nENROLLMENT_TOKEN=old-token\nEDGE_POLL_SECONDS=45\n",
		"var/lib/cdn-platform/edge-client.key":      "legacy-key\n",
		"var/log/cdn-platform/access.json":          "access event\n",
		"etc/nginx/conf.d/cdn-platform.conf":        legacyNginxConfiguration,
		"etc/systemd/system/cdn-edge-agent.service": "legacy service\n",
		"etc/nginx/sites-enabled/default":           "default site\n",
	} {
		harness.requireContents(t, path, contents)
	}
	if !strings.Contains(harness.read(t, "mock.log"), "systemctl start cdn-edge-agent.service") {
		t.Fatalf("rollback did not restore the legacy service state:\n%s", harness.read(t, "mock.log"))
	}
	harness.requireAbsent(t, "usr/local/lib/sysctl.d/40-simple-cdn-edge.conf")
	harness.requireContents(t, "run/mock-sysctl/net.core.default_qdisc", "pfifo_fast\n")
	harness.requireContents(t, "run/mock-sysctl/net.ipv4.tcp_congestion_control", "cubic\n")
	matches, globErr := filepath.Glob(filepath.Join(harness.root, "tmp/cdn-edge-install.*"))
	if globErr != nil || len(matches) != 0 {
		t.Fatalf("installer left transaction directories after rollback: %#v, %v", matches, globErr)
	}
}

func TestInstallEdgeScriptRejectsMixedLayouts(t *testing.T) {
	harness := newInstallHarness(t)
	harness.write("opt/cdn-edge/.layout-version", "1\n")
	harness.write("opt/cdn-edge/bin/cdn-edge-agent", "new binary\n")
	harness.write("var/lib/cdn-platform/edge-client.key", "legacy key\n")

	output, err := harness.run(t, "token", "edge-binary-v2", "")
	if err == nil || !strings.Contains(output, "both /opt/cdn-edge and legacy CDN paths exist") {
		t.Fatalf("mixed layout was not rejected: %v\n%s", err, output)
	}
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

type installHarness struct {
	root               string
	mockBin            string
	logPath            string
	binaryPath         string
	servicePath        string
	updaterServicePath string
	nginxVersion       string
}

func newInstallHarness(t *testing.T) *installHarness {
	t.Helper()
	root := t.TempDir()
	harness := &installHarness{
		root:               root,
		mockBin:            filepath.Join(root, "mock-bin"),
		logPath:            filepath.Join(root, "mock.log"),
		binaryPath:         filepath.Join(root, "download-binary"),
		servicePath:        filepath.Join(root, "download-service"),
		updaterServicePath: filepath.Join(root, "download-updater-service"),
	}
	for _, directory := range []string{"tmp", "run", "mock-bin", "proc", "etc/nginx/conf.d", "etc/nginx/sites-enabled", "etc/logrotate.d", "etc/systemd/system", "etc/sysctl.d", "usr/local/lib/sysctl.d"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	harness.write("etc/nginx/nginx.conf", "worker_processes auto;\nevents {\n    worker_connections 768;\n}\nhttp {\n    include /etc/nginx/conf.d/*.conf;\n}\n")
	harness.write("proc/meminfo", "MemTotal:        4194304 kB\n")
	for key, value := range map[string]string{
		"net.core.default_qdisc":          "pfifo_fast\n",
		"net.ipv4.tcp_congestion_control": "cubic\n",
		"net.ipv4.tcp_mtu_probing":        "0\n",
		"net.core.rmem_max":               "4194304\n",
		"net.core.wmem_max":               "4194304\n",
		"net.ipv4.tcp_rmem":               "4096 131072 6291456\n",
		"net.ipv4.tcp_wmem":               "4096 16384 4194304\n",
	} {
		harness.write(filepath.Join("run/mock-sysctl", key), value)
	}
	if err := os.WriteFile(harness.servicePath, []byte(bootstrapEdgeService), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(harness.updaterServicePath, []byte(bootstrapEdgeUpdaterService), 0o644); err != nil {
		t.Fatal(err)
	}
	harness.writeMock(t, "curl", `#!/usr/bin/env bash
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
	harness.writeMock(t, "nginx", `#!/usr/bin/env bash
printf 'nginx %s\n' "$*" >>"$MOCK_LOG"
if [[ "${1:-}" == "-V" ]]; then
  printf '%s\n' "${MOCK_NGINX_VERSION:-nginx version: nginx/1.22.1}" >&2
  exit 0
fi
if [[ "${MOCK_FAILURE:-}" == "nginx" && "${1:-}" == "-t" ]]; then exit 1; fi
exit 0
`)
	harness.writeMock(t, "chown", `#!/usr/bin/env bash
printf 'chown %s\n' "$*" >>"$MOCK_LOG"
exit 0
`)
	harness.writeMock(t, "sha256sum", `#!/usr/bin/env bash
set -euo pipefail
read -r expected path
actual=$(shasum -a 256 "$path" | awk '{print $1}')
[[ "$expected" == "$actual" ]]
`)
	harness.writeMock(t, "sleep", "#!/usr/bin/env bash\nexit 0\n")
	harness.writeMock(t, "modprobe", `#!/usr/bin/env bash
printf 'modprobe %s\n' "$*" >>"$MOCK_LOG"
exit 0
`)
	harness.writeMock(t, "sysctl", `#!/usr/bin/env bash
set -euo pipefail
printf 'sysctl %s\n' "$*" >>"$MOCK_LOG"
root="$CDN_EDGE_INSTALL_ROOT"
state_dir="$root/run/mock-sysctl"

trim() {
  awk '{$1=$1; print}' <<<"$1"
}

write_setting() {
  local key="$1" value
  value=$(trim "$2")
  if [[ "${MOCK_FAILURE:-}" == "sysctl-bbr" && "$key" == "net.ipv4.tcp_congestion_control" && "$value" == "bbr" ]]; then
    return 1
  fi
  if [[ "${MOCK_FAILURE:-}" == "sysctl-buffer" && "$key" == "net.ipv4.tcp_wmem" && "$value" == *"16777216" ]]; then
    return 1
  fi
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
  -n)
    cat "$state_dir/$2"
    ;;
  -w)
    assignment="$2"
    write_setting "${assignment%%=*}" "${assignment#*=}"
    ;;
  -p)
    apply_file "$2"
    ;;
  --system)
    for file in "$root/usr/local/lib/sysctl.d/"*.conf; do
      [[ -e "$file" ]] && apply_file "$file"
    done
    for file in "$root/etc/sysctl.d/"*.conf; do
      [[ -e "$file" ]] && apply_file "$file"
    done
    ;;
  *)
    echo "unexpected sysctl arguments: $*" >&2
    exit 1
    ;;
esac
`)
	harness.writeMock(t, "systemctl", `#!/usr/bin/env bash
set -euo pipefail
printf 'systemctl %s\n' "$*" >>"$MOCK_LOG"
root="$CDN_EDGE_INSTALL_ROOT"
active="$root/run/mock-agent-active"
enabled="$root/run/mock-agent-enabled"
nginx_active="$root/run/mock-nginx-active"
command="${1:-}"
service="${*: -1}"
case "$command" in
  is-active)
    if [[ "$service" == "nginx.service" ]]; then [[ -f "$nginx_active" ]]; exit; fi
    [[ -f "$active" ]]
    ;;
  is-enabled) [[ -f "$enabled" ]] ;;
  stop)
    if [[ "$service" == "cdn-edge-agent.service" ]]; then rm -f "$active"; fi
    if [[ "$service" == "nginx.service" ]]; then rm -f "$nginx_active"; fi
    ;;
  disable) rm -f "$enabled" ;;
  enable) touch "$enabled" ;;
  start|restart)
    if [[ "$service" == "nginx.service" ]]; then
      if [[ "${MOCK_FAILURE:-}" == "nginx-restart-once" && ! -f "$root/run/mock-nginx-restart-failed" ]]; then
        touch "$root/run/mock-nginx-restart-failed"
        exit 1
      fi
      touch "$nginx_active"
    fi
    if [[ "$service" == "cdn-edge-agent.service" ]]; then
      if [[ "${MOCK_FAILURE:-}" == "agent" ]]; then exit 1; fi
      touch "$active"
      unit="$root/etc/systemd/system/cdn-edge-agent.service"
      if [[ -L "$unit" && "$(readlink "$unit")" == "$root/opt/cdn-edge/systemd/cdn-edge-agent.service" ]]; then
        mkdir -p "$root/opt/cdn-edge/data"
        for file in edge-client.key edge-client.crt edge-ca.crt; do
          [[ -s "$root/opt/cdn-edge/data/$file" ]] || printf '%s\n' "$file" >"$root/opt/cdn-edge/data/$file"
        done
      fi
	  if [[ -n "${MOCK_READINESS_FILE:-}" && "${MOCK_FAILURE:-}" != "readiness" ]]; then
		mkdir -p "$(dirname "$MOCK_READINESS_FILE")"
		shasum -a 256 "$MOCK_BINARY" | awk '{print $1}' >"$MOCK_READINESS_FILE"
	  fi
    fi
    ;;
  reload)
    if [[ "${MOCK_FAILURE:-}" == "reload" && "$service" == "nginx" ]]; then exit 1; fi
    ;;
esac
exit 0
`)
	return harness
}

func (h *installHarness) seedLegacy(t *testing.T) {
	t.Helper()
	for path, contents := range map[string]string{
		"usr/local/bin/cdn-edge-agent":                 "legacy-binary\n",
		"etc/cdn-platform/edge.env":                    "CONTROL_URL=https://old.example.test\nENROLLMENT_TOKEN=old-token\nEDGE_POLL_SECONDS=45\n",
		"etc/cdn-platform/certs/site.crt":              "site-cert\n",
		"etc/cdn-platform/certs/site.key":              "site-key\n",
		"var/lib/cdn-platform/edge-client.key":         "legacy-key\n",
		"var/lib/cdn-platform/edge-client.crt":         "legacy-cert\n",
		"var/lib/cdn-platform/edge-ca.crt":             "legacy-ca\n",
		"var/lib/cdn-platform/applied-version":         "9\n",
		"var/lib/cdn-platform/access-log-queue.ndjson": "queued\n",
		"var/lib/cdn-platform/access-log-offset":       "17\n",
		"var/log/cdn-platform/access.json":             "access event\n",
		"var/log/cdn-platform/access.json.1":           "rotated event\n",
		"var/cache/cdn-platform/cache-object":          "discard cache\n",
		"etc/nginx/conf.d/cdn-platform.conf":           legacyNginxConfiguration,
		"etc/nginx/sites-enabled/default":              "default site\n",
		"etc/systemd/system/cdn-edge-agent.service":    "legacy service\n",
	} {
		h.write(path, contents)
	}
	h.write("run/mock-agent-active", "")
	h.write("run/mock-agent-enabled", "")
	h.write("run/mock-nginx-active", "")
}

func (h *installHarness) run(t *testing.T, token, binary, failure string) (string, error) {
	t.Helper()
	if err := os.WriteFile(h.binaryPath, []byte(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(binary)))
	arguments := []string{"-s", "--", "--control-url", "https://edge-control.example.test"}
	if token != "" {
		arguments = append(arguments, "--enrollment-token", token)
	}
	serviceDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(bootstrapEdgeService)))
	updaterServiceDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(bootstrapEdgeUpdaterService)))
	arguments = append(arguments, "--binary-url", "https://downloads.example.test/edge", "--binary-sha256", digest,
		"--service-sha256", serviceDigest, "--updater-service-sha256", updaterServiceDigest)
	command := exec.Command("bash", arguments...)
	command.Stdin = strings.NewReader(bootstrapEdgeScript)
	command.Env = []string{
		"PATH=" + h.mockBin + ":/usr/bin:/bin",
		"CDN_EDGE_INSTALL_ROOT=" + h.root,
		"MOCK_LOG=" + h.logPath,
		"MOCK_BINARY=" + h.binaryPath,
		"MOCK_SERVICE=" + h.servicePath,
		"MOCK_UPDATER_SERVICE=" + h.updaterServicePath,
		"MOCK_READINESS_FILE=",
		"MOCK_FAILURE=" + failure,
		"MOCK_NGINX_VERSION=" + h.nginxVersion,
	}
	output, err := command.CombinedOutput()
	return string(output), err
}

func (h *installHarness) runOnline(t *testing.T, binary, failure string) (string, error) {
	t.Helper()
	if err := os.WriteFile(h.binaryPath, []byte(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(binary)))
	serviceDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(bootstrapEdgeService)))
	updaterServiceDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(bootstrapEdgeUpdaterService)))
	readinessPath := filepath.Join(h.root, "opt/cdn-edge/data/upgrades/online-test/ready")
	if err := os.MkdirAll(filepath.Dir(readinessPath), 0o700); err != nil {
		t.Fatal(err)
	}
	arguments := []string{
		"-s", "--", "--control-url", "https://edge-control.example.test",
		"--binary-file", h.binaryPath, "--binary-sha256", digest,
		"--service-file", h.servicePath, "--service-sha256", serviceDigest,
		"--updater-service-file", h.updaterServicePath, "--updater-service-sha256", updaterServiceDigest,
		"--readiness-file", readinessPath,
	}
	command := exec.Command("bash", arguments...)
	command.Stdin = strings.NewReader(bootstrapEdgeScript)
	command.Env = []string{
		"PATH=" + h.mockBin + ":/usr/bin:/bin",
		"CDN_EDGE_INSTALL_ROOT=" + h.root,
		"MOCK_LOG=" + h.logPath,
		"MOCK_BINARY=" + h.binaryPath,
		"MOCK_SERVICE=" + h.servicePath,
		"MOCK_UPDATER_SERVICE=" + h.updaterServicePath,
		"MOCK_READINESS_FILE=" + readinessPath,
		"MOCK_FAILURE=" + failure,
		"MOCK_NGINX_VERSION=" + h.nginxVersion,
	}
	output, err := command.CombinedOutput()
	return string(output), err
}

func (h *installHarness) writeMock(t *testing.T, name, contents string) {
	t.Helper()
	path := filepath.Join(h.mockBin, name)
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
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
