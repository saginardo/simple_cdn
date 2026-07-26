package edge

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"simple_cdn/internal/domain"
)

func TestNginxRunnerInspectsUDPListeners(t *testing.T) {
	var command string
	runner := NginxRunner{command: func(name string, arguments ...string) ([]byte, error) {
		command = name + " " + strings.Join(arguments, " ")
		return []byte(`UNCONN 0 0 0.0.0.0:443 0.0.0.0:* users:(("nginx",pid=42,fd=7))`), nil
	}}
	listeners, err := runner.UDPPortListeners([]int{443})
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.PortConflict{{Protocol: "udp", Port: 443, PID: 42, Process: "nginx"}}
	if !reflect.DeepEqual(listeners, want) {
		t.Fatalf("UDP listeners = %#v, want %#v", listeners, want)
	}
	if !strings.Contains(command, "ss -H -lunp ( sport = :443 )") {
		t.Fatalf("UDP listener command = %q", command)
	}
}

func TestNginxRunnerReloadWaitsForNewWorker(t *testing.T) {
	snapshots := []nginxProcessSnapshot{
		{MasterPID: 10, Workers: []int{11, 12}},
		{MasterPID: 10, Workers: []int{21, 22}},
	}
	runner := testNginxRunner(snapshots, nil)
	if err := runner.Apply(); err != nil {
		t.Fatal(err)
	}
}

func TestNginxRunnerRejectsAsynchronousReloadFailure(t *testing.T) {
	snapshots := []nginxProcessSnapshot{
		{MasterPID: 10, Workers: []int{11, 12}},
		{MasterPID: 10, Workers: []int{11, 12}},
	}
	runner := testNginxRunner(snapshots, nil)
	err := runner.Apply()
	if err == nil || !strings.Contains(err.Error(), "reload was not adopted") {
		t.Fatalf("reload result = %v", err)
	}
}

func TestNginxRunnerReportsReloadCommandFailure(t *testing.T) {
	runner := testNginxRunner([]nginxProcessSnapshot{{MasterPID: 10, Workers: []int{11}}}, errors.New("signal failed"))
	err := runner.Apply()
	if err == nil || !strings.Contains(err.Error(), "signal failed") {
		t.Fatalf("reload result = %v", err)
	}
}

func TestNginxRunnerStartsInactiveServiceWithoutReloadSnapshot(t *testing.T) {
	commands := make([]string, 0)
	runner := NginxRunner{command: func(name string, arguments ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(arguments, " "))
		if name == "systemctl" && len(arguments) > 0 && arguments[0] == "is-active" {
			return nil, errors.New("inactive")
		}
		return nil, nil
	}, snapshot: func() (nginxProcessSnapshot, error) {
		t.Fatal("inactive service should not inspect reload workers")
		return nginxProcessSnapshot{}, nil
	}}
	if err := runner.Apply(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(commands, "\n")
	if !strings.Contains(joined, "systemctl start nginx") || strings.Contains(joined, "nginx -s reload") {
		t.Fatalf("unexpected commands:\n%s", joined)
	}
}

func testNginxRunner(snapshots []nginxProcessSnapshot, reloadErr error) NginxRunner {
	index := 0
	return NginxRunner{
		ReloadTimeout:      5 * time.Millisecond,
		ReloadPollInterval: time.Millisecond,
		command: func(name string, arguments ...string) ([]byte, error) {
			if name == "systemctl" {
				return nil, nil
			}
			if len(arguments) == 2 && arguments[0] == "-s" && arguments[1] == "reload" {
				return nil, reloadErr
			}
			return nil, nil
		},
		snapshot: func() (nginxProcessSnapshot, error) {
			if len(snapshots) == 0 {
				return nginxProcessSnapshot{}, errors.New("missing snapshot")
			}
			if index >= len(snapshots) {
				return snapshots[len(snapshots)-1], nil
			}
			result := snapshots[index]
			index++
			return result, nil
		},
	}
}
