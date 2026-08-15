package control

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallOriginWireGuardScriptSyntax(t *testing.T) {
	command := exec.Command("bash", "-n")
	command.Stdin = strings.NewReader(installOriginWireGuardScript)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, output)
	}
	if !strings.Contains(installOriginWireGuardScript, `read -r confirmation </dev/tty`) {
		t.Fatal("origin tunnel uninstall confirmation must read from /dev/tty")
	}
}

func TestInstallOriginWireGuardScriptRequiresMatchingUninstallConfirmation(t *testing.T) {
	harness := newOriginWireGuardUninstallHarness(t)
	output, err := harness.run(t, "UNINSTALL wrong-tunnel")
	if err == nil || !strings.Contains(output, "confirmation did not match; nothing was removed") {
		t.Fatalf("mismatched confirmation result = %v\n%s", err, output)
	}
	for _, path := range harness.managedPaths {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("mismatched confirmation changed %s: %v", path, err)
		}
	}
	if log := harness.read(t, harness.commandLog); log != "" {
		t.Fatalf("mismatched confirmation ran system commands:\n%s", log)
	}

	output, err = harness.run(t, "UNINSTALL "+originWireGuardTestTunnelID)
	if err != nil {
		t.Fatalf("confirmed uninstall failed: %v\n%s", err, output)
	}
	for _, path := range harness.managedPaths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("confirmed uninstall left %s: %v", path, err)
		}
	}
	log := harness.read(t, harness.commandLog)
	for _, expected := range []string{
		"systemctl disable --now simple-cdn-origin-iperf-scwg1234567812.service",
		"wg-quick down scwg1234567812",
		"nft delete table inet scwg_1234567812",
		"systemctl daemon-reload",
	} {
		if !strings.Contains(log, expected) {
			t.Fatalf("confirmed uninstall command log is missing %q:\n%s", expected, log)
		}
	}
}

func TestInstallOriginWireGuardScriptUpdatesActiveInterfaceWithoutRestart(t *testing.T) {
	requireScriptOrder(t, installOriginWireGuardScript,
		`if [[ $interface_active == 1 ]]; then`,
		`if [[ $config_changed == 1 ]]; then`,
		`wg syncconf "$interface" "$temporary_runtime"`,
		`ip -4 address replace "$origin_cidr" dev "$interface"`,
		`ip link set dev "$interface" mtu "$mtu"`,
		`mv "$temporary_config" "$config_file"`,
		`else`,
		`systemctl restart "wg-quick@$interface.service"`,
	)
	installPath := strings.Index(installOriginWireGuardScript, `temporary_config=$(mktemp`)
	if installPath < 0 {
		t.Fatal("source installer is missing its managed configuration phase")
	}
	installPhase := installOriginWireGuardScript[installPath:]
	for _, forbidden := range []string{
		`systemctl stop "wg-quick@$interface.service"`,
		`wg-quick down "$interface"`,
	} {
		if strings.Contains(installPhase, forbidden) {
			t.Fatalf("source redeployment still disrupts an active tunnel with %q", forbidden)
		}
	}
	if strings.Count(installPhase, `systemctl restart "wg-quick@$interface.service"`) != 1 {
		t.Fatal("source installer must reserve a single WireGuard restart for a missing interface")
	}
	if strings.Contains(installOriginWireGuardScript, `systemctl enable --now "wg-quick@$interface.service"`) {
		t.Fatal("source installer may leave an active oneshot unit without a WireGuard interface")
	}
}

func TestInstallOriginWireGuardScriptUsesAtomicFirewallUpdateAndRollback(t *testing.T) {
	for _, wanted := range []string{
		`flush chain inet $table_name input`,
		`nft -f "$temporary_nft_update"`,
		`rollback_live_update()`,
		`wg syncconf "$interface" "$temporary_old_runtime"`,
		`existing managed WireGuard configuration is invalid; refusing a disruptive replacement`,
	} {
		if !strings.Contains(installOriginWireGuardScript, wanted) {
			t.Fatalf("source installer is missing live-update safeguard %q", wanted)
		}
	}
}

func TestInstallOriginWireGuardScriptLiveUpdateExecution(t *testing.T) {
	harness := newOriginWireGuardInstallHarness(t, false)
	output, err := harness.run(t)
	if err != nil {
		t.Fatalf("source live update failed: %v\n%s", err, output)
	}
	log := harness.read(t, harness.commandLog)
	for _, expected := range []string{
		"wg syncconf scwg1234567812 ",
		"ip -4 address replace 10.253.41.1/24 dev scwg1234567812",
		"ip link set dev scwg1234567812 mtu 1380",
		"nft -f ",
		"tc class replace dev scwg1234567812 parent 1: classid 1:10 htb rate 25mbit ceil 25mbit",
	} {
		if !strings.Contains(log, expected) {
			t.Fatalf("source live update command log is missing %q:\n%s", expected, log)
		}
	}
	for _, forbidden := range []string{
		"wg-quick down ",
		"wg-quick up ",
		"systemctl restart wg-quick@",
	} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("source live update used disruptive command %q:\n%s", forbidden, log)
		}
	}
	runtime := harness.read(t, harness.runtimeConfig)
	for _, forbidden := range []string{"Address =", "MTU =", "PostUp =", "PreDown ="} {
		if strings.Contains(runtime, forbidden) {
			t.Fatalf("wg syncconf runtime contains wg-quick directive %q:\n%s", forbidden, runtime)
		}
	}
	configuration := harness.read(t, harness.configFile)
	if !strings.Contains(configuration, "Address = 10.253.41.1/24") ||
		!strings.Contains(configuration, "ListenPort = 51821") ||
		!strings.Contains(configuration, "MTU = 1380") {
		t.Fatalf("source live update did not persist the desired configuration:\n%s", configuration)
	}
	profile := harness.read(t, harness.sysctlProfileFile)
	for _, expected := range []string{
		"-net.core.default_qdisc = fq",
		"-net.ipv4.tcp_congestion_control = bbr",
		"-net.ipv4.tcp_mtu_probing = 1",
		"-net.core.rmem_max = ",
		"-net.core.wmem_max = ",
	} {
		if !strings.Contains(profile, expected) {
			t.Fatalf("origin sysctl profile is missing %q:\n%s", expected, profile)
		}
	}
	if err := os.WriteFile(harness.commandLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := harness.run(t); err != nil {
		t.Fatalf("idempotent source deployment failed: %v\n%s", err, output)
	}
	log = harness.read(t, harness.commandLog)
	for _, forbidden := range []string{
		"wg syncconf ",
		"nft -f ",
		"tc qdisc replace ",
		"systemctl restart ",
	} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("idempotent source deployment executed %q:\n%s", forbidden, log)
		}
	}
}

func TestInstallOriginWireGuardScriptSyncFailureKeepsPreviousConfig(t *testing.T) {
	harness := newOriginWireGuardInstallHarness(t, true)
	previous := harness.read(t, harness.configFile)
	output, err := harness.run(t)
	if err == nil || !strings.Contains(output, "the previous configuration remains active") {
		t.Fatalf("source sync failure = %v\n%s", err, output)
	}
	if current := harness.read(t, harness.configFile); current != previous {
		t.Fatalf("source sync failure replaced the previous config:\n%s", current)
	}
	log := harness.read(t, harness.commandLog)
	if strings.Contains(log, "wg-quick down ") || strings.Contains(log, "systemctl restart wg-quick@") {
		t.Fatalf("source sync failure fell back to a disruptive restart:\n%s", log)
	}
}

func TestInstallOriginWireGuardScriptSysctlFailureRestoresCurrentRuntime(t *testing.T) {
	harness := newOriginWireGuardInstallHarness(t, false)
	baseline := `net.core.rmem_max = 1000000
net.core.wmem_max = 1100000
net.ipv4.tcp_rmem = 4096 80000 1200000
net.ipv4.tcp_wmem = 4096 12000 1300000
`
	if err := os.WriteFile(harness.sysctlBaselineFile, []byte(baseline), 0o600); err != nil {
		t.Fatal(err)
	}
	previousProfile := `# Managed by simple_cdn origin WireGuard installer.
-net.core.rmem_max = 2000000
-net.core.wmem_max = 2100000
-net.ipv4.tcp_rmem = 4096 100000 2200000
-net.ipv4.tcp_wmem = 4096 16000 2300000
`
	if err := os.WriteFile(harness.sysctlProfileFile, []byte(previousProfile), 0o644); err != nil {
		t.Fatal(err)
	}
	writeOriginInstallCommand(t, filepath.Join(harness.root, "bin"), "sysctl", harness.commandLog, `
if [ "$1" = "-n" ]; then
  case "$2" in
    net.core.default_qdisc) printf '%s\n' fq ;;
    net.ipv4.tcp_congestion_control) printf '%s\n' bbr ;;
    net.ipv4.tcp_mtu_probing) printf '%s\n' 1 ;;
    net.core.rmem_max) printf '%s\n' 2000000 ;;
    net.core.wmem_max) printf '%s\n' 2100000 ;;
    net.ipv4.tcp_rmem) printf '%s\n' '4096 100000 2200000' ;;
    net.ipv4.tcp_wmem) printf '%s\n' '4096 16000 2300000' ;;
  esac
  exit 0
fi
if [ "$1" = "-q" ] && [ "$2" = "-w" ]; then
  case "$3" in
    "net.ipv4.tcp_wmem=4096 16384 "*) exit 1 ;;
  esac
fi
exit 0
`)
	if err := os.WriteFile(harness.commandLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := harness.run(t)
	if err != nil {
		t.Fatalf("source deployment with partial sysctl support failed: %v\n%s", err, output)
	}
	log := harness.read(t, harness.commandLog)
	for _, expected := range []string{
		"sysctl -q -w net.core.rmem_max=2000000",
		"sysctl -q -w net.core.wmem_max=2100000",
		"sysctl -q -w net.ipv4.tcp_rmem=4096 100000 2200000",
		"sysctl -q -w net.ipv4.tcp_wmem=4096 16000 2300000",
	} {
		if !strings.Contains(log, expected) {
			t.Fatalf("sysctl rollback is missing %q:\n%s", expected, log)
		}
	}
	if strings.Contains(log, "sysctl -q -w net.core.rmem_max=1000000") {
		t.Fatalf("sysctl rollback used the original installation baseline:\n%s", log)
	}
	profile := harness.read(t, harness.sysctlProfileFile)
	for _, expected := range []string{
		"-net.core.rmem_max = 2000000",
		"-net.core.wmem_max = 2100000",
		"-net.ipv4.tcp_rmem = 4096 100000 2200000",
		"-net.ipv4.tcp_wmem = 4096 16000 2300000",
	} {
		if !strings.Contains(profile, expected) {
			t.Fatalf("sysctl profile did not preserve %q:\n%s", expected, profile)
		}
	}
}

func TestInstallOriginWireGuardScriptStartsMissingInterface(t *testing.T) {
	harness := newOriginWireGuardInstallHarness(t, false)
	writeOriginInstallCommand(t, filepath.Join(harness.root, "bin"), "ip", harness.commandLog, `
if [ "$*" = "link show dev scwg1234567812" ]; then exit 1; fi
exit 0
`)
	output, err := harness.run(t)
	if err != nil {
		t.Fatalf("source missing-interface recovery failed: %v\n%s", err, output)
	}
	log := harness.read(t, harness.commandLog)
	if !strings.Contains(log, "systemctl restart wg-quick@scwg1234567812.service") {
		t.Fatalf("source missing-interface recovery did not start WireGuard:\n%s", log)
	}
	if strings.Contains(log, "wg syncconf ") {
		t.Fatalf("source missing-interface recovery attempted a live sync:\n%s", log)
	}
}

func TestInstallOriginWireGuardScriptPersistsEgressShaping(t *testing.T) {
	for _, wanted := range []string{
		`origin_egress_limit_mbps=$(jq -r '.origin_egress_limit_mbps // 0'`,
		`PostUp = $tc_binary qdisc replace dev %i root handle 1: htb default 10`,
		`PostUp = $tc_binary class replace dev %i parent 1: classid 1:10 htb rate ${origin_egress_limit_mbps}mbit ceil ${origin_egress_limit_mbps}mbit`,
		`PostUp = $tc_binary qdisc replace dev %i parent 1:10 handle 10: fq_codel`,
		`PreDown = $tc_binary qdisc delete dev %i root 2>/dev/null || true`,
	} {
		if !strings.Contains(installOriginWireGuardScript, wanted) {
			t.Fatalf("source installer is missing egress shaping command %q", wanted)
		}
	}
}

func requireScriptOrder(t *testing.T, script string, commands ...string) {
	t.Helper()
	offset := 0
	for _, command := range commands {
		index := strings.Index(script[offset:], command)
		if index < 0 {
			t.Fatalf("script is missing ordered command %q", command)
		}
		offset += index + len(command)
	}
}

type originWireGuardInstallHarness struct {
	root               string
	script             string
	commandLog         string
	runtimeConfig      string
	configFile         string
	sysctlProfileFile  string
	sysctlBaselineFile string
}

const originWireGuardTestTunnelID = "12345678-1234-4234-8234-123456789abc"

type originWireGuardUninstallHarness struct {
	root         string
	script       string
	commandLog   string
	managedPaths []string
}

func newOriginWireGuardUninstallHarness(t *testing.T) originWireGuardUninstallHarness {
	t.Helper()
	const interfaceName = "scwg1234567812"
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	configRoot := filepath.Join(root, "wireguard")
	sysctlRoot := filepath.Join(root, "sysctl.d")
	binRoot := filepath.Join(root, "bin")
	for _, directory := range []string{stateRoot, configRoot, sysctlRoot, binRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	commandLog := filepath.Join(root, "commands.log")
	if err := os.WriteFile(commandLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"systemctl", "wg-quick", "nft"} {
		writeOriginInstallCommand(t, binRoot, name, commandLog, "exit 0")
	}
	managedPaths := []string{
		filepath.Join(stateRoot, "simple-cdn-origin-iperf-"+interfaceName+".service"),
		filepath.Join(configRoot, interfaceName+".conf"),
		filepath.Join(configRoot, interfaceName+".nft"),
		filepath.Join(stateRoot, originWireGuardTestTunnelID+".key"),
		filepath.Join(stateRoot, originWireGuardTestTunnelID+".json"),
	}
	for _, path := range managedPaths {
		if err := os.WriteFile(path, []byte("managed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	script := installOriginWireGuardScript
	script = replaceOriginInstallScriptOnce(t, script, `if [[ $EUID -ne 0 ]]; then`, `if false; then`)
	script = replaceOriginInstallScriptOnce(t, script, `STATE_ROOT="/var/lib/simple-cdn-origin-wireguard"`, fmt.Sprintf("STATE_ROOT=%q", stateRoot))
	script = replaceOriginInstallScriptOnce(t, script, `CONFIG_ROOT="/etc/wireguard"`, fmt.Sprintf("CONFIG_ROOT=%q", configRoot))
	script = replaceOriginInstallScriptOnce(t, script, `sysctl_profile_dir="/etc/sysctl.d"`, fmt.Sprintf("sysctl_profile_dir=%q", sysctlRoot))
	script = replaceOriginInstallScriptOnce(t, script,
		`service_file="/etc/systemd/system/simple-cdn-origin-iperf-$interface.service"`,
		`service_file="$STATE_ROOT/simple-cdn-origin-iperf-$interface.service"`)
	script = replaceOriginInstallScriptOnce(t, script,
		`IFS= read -r confirmation </dev/tty`,
		`IFS= read -r confirmation <<<"$SIMPLE_CDN_TEST_CONFIRMATION"`)
	scriptPath := filepath.Join(root, "install-origin-wireguard.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return originWireGuardUninstallHarness{
		root: root, script: scriptPath, commandLog: commandLog, managedPaths: managedPaths,
	}
}

func (h originWireGuardUninstallHarness) run(t *testing.T, confirmation string) (string, error) {
	t.Helper()
	command := exec.Command("bash", h.script,
		"--tunnel-id", originWireGuardTestTunnelID,
		"--tunnel-name", "origin test tunnel",
		"--origin-address", "10.253.41.1",
		"--uninstall",
	)
	command.Env = append(os.Environ(),
		"PATH="+filepath.Join(h.root, "bin")+":/usr/bin:/usr/sbin:/bin",
		"SIMPLE_CDN_TEST_CONFIRMATION="+confirmation,
	)
	output, err := command.CombinedOutput()
	return string(output), err
}

func (h originWireGuardUninstallHarness) read(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func newOriginWireGuardInstallHarness(t *testing.T, failSync bool) originWireGuardInstallHarness {
	t.Helper()
	const tunnelID = originWireGuardTestTunnelID
	const interfaceName = "scwg1234567812"
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	configRoot := filepath.Join(root, "wireguard")
	sysctlRoot := filepath.Join(root, "sysctl.d")
	binRoot := filepath.Join(root, "bin")
	for _, directory := range []string{stateRoot, configRoot, sysctlRoot, binRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	commandLog := filepath.Join(root, "commands.log")
	runtimeConfig := filepath.Join(root, "runtime.conf")
	responseFile := filepath.Join(root, "response.json")
	failFile := filepath.Join(root, "fail-sync")
	if failSync {
		if err := os.WriteFile(failFile, []byte("fail\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeOriginInstallCommand(t, binRoot, "apt-get", commandLog, "exit 0")
	writeOriginInstallCommand(t, binRoot, "sysctl", commandLog, `
if [ "$1" = "-n" ]; then
  case "$2" in
    net.core.default_qdisc) printf '%s\n' pfifo_fast ;;
    net.ipv4.tcp_congestion_control) printf '%s\n' cubic ;;
    net.ipv4.tcp_mtu_probing) printf '%s\n' 0 ;;
    net.core.rmem_max|net.core.wmem_max) printf '%s\n' 4194304 ;;
    net.ipv4.tcp_rmem) printf '%s\n' '4096 131072 4194304' ;;
    net.ipv4.tcp_wmem) printf '%s\n' '4096 16384 4194304' ;;
  esac
fi
exit 0
`)
	writeOriginInstallCommand(t, binRoot, "curl", commandLog, fmt.Sprintf(`
output=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output) output="$2"; shift 2 ;;
    *) shift ;;
  esac
done
cp %q "$output"
printf '200'
`, responseFile))
	writeOriginInstallCommand(t, binRoot, "wg", commandLog, fmt.Sprintf(`
case "$1" in
  genkey) printf '%%s\n' %q ;;
  pubkey) cat >/dev/null; printf '%%s\n' %q ;;
  show) exit 0 ;;
  syncconf)
    if [ -f %q ]; then exit 1; fi
    cp "$3" %q
    ;;
esac
exit 0
`, controlWireGuardTestKey(9), controlWireGuardTestKey(8), failFile, runtimeConfig))
	writeOriginInstallCommand(t, binRoot, "wg-quick", commandLog, "exit 0")
	writeOriginInstallCommand(t, binRoot, "ip", commandLog, "exit 0")
	writeOriginInstallCommand(t, binRoot, "tc", commandLog, `
if [ "$*" = "qdisc show dev scwg1234567812" ]; then
  printf '%s\n' 'qdisc htb 1: root'
fi
exit 0
`)
	writeOriginInstallCommand(t, binRoot, "nft", commandLog, "exit 0")
	writeOriginInstallCommand(t, binRoot, "iperf3", commandLog, "exit 0")
	writeOriginInstallCommand(t, binRoot, "systemctl", commandLog, "exit 0")
	writeOriginInstallCommand(t, binRoot, "sleep", commandLog, "exit 0")

	response := fmt.Sprintf(`{
  "tunnel_id": %q,
  "interface_name": %q,
  "origin_address_cidr": "10.253.41.1/24",
  "listen_port": 51821,
  "performance_port": 5201,
  "origin_egress_limit_mbps": 25,
  "mtu": 1380,
  "revision": 4,
  "peers": [{
    "public_key": %q,
    "allowed_ip": "10.253.41.2/32",
    "public_ipv4": "198.51.100.20"
  }]
}
`, tunnelID, interfaceName, controlWireGuardTestKey(3))
	if err := os.WriteFile(responseFile, []byte(response), 0o600); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(stateRoot, tunnelID+".key")
	if err := os.WriteFile(keyFile, []byte(controlWireGuardTestKey(9)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tcPath := filepath.Join(binRoot, "tc")
	nftFile := filepath.Join(configRoot, interfaceName+".nft")
	configFile := filepath.Join(configRoot, interfaceName+".conf")
	previousConfig := fmt.Sprintf(`# Managed by simple_cdn. Re-run the generated command to update peers.
[Interface]
Address = 10.253.40.1/24
ListenPort = 51820
PrivateKey = %s
MTU = 1420
PostUp = nft -f %s
PostUp = %s qdisc replace dev %%i root handle 1: htb default 10
PostUp = %s class replace dev %%i parent 1: classid 1:10 htb rate 10mbit ceil 10mbit
PostUp = %s qdisc replace dev %%i parent 1:10 handle 10: fq_codel
PreDown = %s qdisc delete dev %%i root 2>/dev/null || true
PreDown = nft delete table inet scwg_1234567812 2>/dev/null || true

[Peer]
PublicKey = %s
AllowedIPs = 10.253.40.2/32
`, controlWireGuardTestKey(9), nftFile, tcPath, tcPath, tcPath, tcPath, controlWireGuardTestKey(2))
	if err := os.WriteFile(configFile, []byte(previousConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nftFile, []byte("old nft configuration\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	script := installOriginWireGuardScript
	script = replaceOriginInstallScriptOnce(t, script, `if [[ $EUID -ne 0 ]]; then`, `if false; then`)
	script = replaceOriginInstallScriptOnce(t, script, `STATE_ROOT="/var/lib/simple-cdn-origin-wireguard"`, fmt.Sprintf("STATE_ROOT=%q", stateRoot))
	script = replaceOriginInstallScriptOnce(t, script, `CONFIG_ROOT="/etc/wireguard"`, fmt.Sprintf("CONFIG_ROOT=%q", configRoot))
	script = replaceOriginInstallScriptOnce(t, script, `sysctl_profile_dir="/etc/sysctl.d"`, fmt.Sprintf("sysctl_profile_dir=%q", sysctlRoot))
	script = replaceOriginInstallScriptOnce(t, script,
		`service_file="/etc/systemd/system/simple-cdn-origin-iperf-$interface.service"`,
		`service_file="$STATE_ROOT/simple-cdn-origin-iperf-$interface.service"`)
	script = replaceOriginInstallScriptOnce(t, script,
		`temporary_service=$(mktemp "/etc/systemd/system/.simple-cdn-origin-iperf-${interface}.XXXXXX")`,
		`temporary_service=$(mktemp "$STATE_ROOT/.simple-cdn-origin-iperf-${interface}.XXXXXX")`)
	scriptPath := filepath.Join(root, "install-origin-wireguard.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return originWireGuardInstallHarness{
		root: root, script: scriptPath, commandLog: commandLog, runtimeConfig: runtimeConfig, configFile: configFile,
		sysctlProfileFile:  filepath.Join(sysctlRoot, "40-simple-cdn-origin-wireguard.conf"),
		sysctlBaselineFile: filepath.Join(stateRoot, "sysctl-baseline.conf"),
	}
}

func (h originWireGuardInstallHarness) run(t *testing.T) (string, error) {
	t.Helper()
	command := exec.Command("bash", h.script,
		"--control-url", "https://control.example.test",
		"--token", "test-token",
		"--tunnel-id", "12345678-1234-4234-8234-123456789abc",
	)
	command.Env = append(os.Environ(), "PATH="+filepath.Join(h.root, "bin")+":/usr/bin:/usr/sbin:/bin")
	output, err := command.CombinedOutput()
	return string(output), err
}

func (h originWireGuardInstallHarness) read(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func writeOriginInstallCommand(t *testing.T, directory, name, logPath, body string) {
	t.Helper()
	path := filepath.Join(directory, name)
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"%s $*\" >> %q\n%s\n", name, logPath, body)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func replaceOriginInstallScriptOnce(t *testing.T, script, old, replacement string) string {
	t.Helper()
	if strings.Count(script, old) != 1 {
		t.Fatalf("source installer test replacement count for %q = %d", old, strings.Count(script, old))
	}
	return strings.Replace(script, old, replacement, 1)
}
