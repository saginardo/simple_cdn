package control

import (
	"os/exec"
	"strings"
	"testing"
)

func TestInstallOriginWireGuardScriptSyntax(t *testing.T) {
	command := exec.Command("bash", "-n")
	command.Stdin = strings.NewReader(installOriginWireGuardScript)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, output)
	}
}

func TestInstallOriginWireGuardScriptRestartsExistingServices(t *testing.T) {
	requireScriptOrder(t, installOriginWireGuardScript,
		`systemctl stop "simple-cdn-origin-iperf-$interface.service"`,
		`systemctl stop "wg-quick@$interface.service"`,
		`wg-quick down "$interface"`,
		`mv "$temporary_config" "$config_file"`,
		`systemctl daemon-reload`,
		`systemctl enable "wg-quick@$interface.service"`,
		`systemctl restart "wg-quick@$interface.service"`,
		`systemctl enable "simple-cdn-origin-iperf-$interface.service"`,
		`systemctl restart "simple-cdn-origin-iperf-$interface.service"`,
	)
	if strings.Contains(installOriginWireGuardScript, `systemctl enable --now "wg-quick@$interface.service"`) {
		t.Fatal("source installer may leave an active oneshot unit without a WireGuard interface")
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
