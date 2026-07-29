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
