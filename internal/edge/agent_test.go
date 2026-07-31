package edge

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"simple_cdn/internal/domain"
)

type fakeRunner struct {
	testErr           error
	applyErr          error
	listeners         [][]domain.PortConflict
	udpListeners      [][]domain.PortConflict
	tests             int
	applies           int
	listenerChecks    int
	udpListenerChecks int
}

func (f *fakeRunner) Test() error { f.tests++; return f.testErr }
func (f *fakeRunner) Apply() error {
	f.applies++
	return f.applyErr
}
func (f *fakeRunner) PortListeners(_ []int) ([]domain.PortConflict, error) {
	index := f.listenerChecks
	f.listenerChecks++
	if index >= len(f.listeners) {
		return nil, nil
	}
	return f.listeners[index], nil
}

func (f *fakeRunner) UDPPortListeners(_ []int) ([]domain.PortConflict, error) {
	index := f.udpListenerChecks
	f.udpListenerChecks++
	if index >= len(f.udpListeners) {
		return nil, nil
	}
	return f.udpListeners[index], nil
}

func TestApplyRollsBackConfigAndCertificatesOnFailedValidation(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "nginx.conf")
	streamConfigPath := filepath.Join(directory, "nginx-stream.conf")
	mainConfigPath := filepath.Join(directory, "nginx-main.conf")
	eventsConfigPath := filepath.Join(directory, "nginx-events.conf")
	certificateDir := filepath.Join(directory, "certs")
	if err := os.MkdirAll(certificateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("old-config"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(streamConfigPath, []byte("old-stream-config"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mainConfigPath, []byte("old-main-config"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(eventsConfigPath, []byte("old-events-config"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(certificateDir, "site.crt"), []byte("old-cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{testErr: errors.New("invalid config")}
	agent, err := New(Config{ControlURL: "https://control.example.test", StateDir: directory, NginxConfigPath: configPath, NginxStreamConfigPath: streamConfigPath, NginxMainConfigPath: mainConfigPath, NginxEventsConfigPath: eventsConfigPath, CertificateDir: certificateDir, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	err = agent.apply(domain.DesiredState{Version: 1, NginxConfig: "bad-config", NginxStreamConfig: "bad-stream-config", NginxMainConfig: "bad-main-config", NginxEventsConfig: "bad-events-config", Certificates: map[string]domain.TLSBundle{"site": {CertificatePEM: "new-cert", PrivateKeyPEM: "new-key"}}})
	if err == nil {
		t.Fatal("expected Nginx validation error")
	}
	config, _ := os.ReadFile(configPath)
	streamConfig, _ := os.ReadFile(streamConfigPath)
	mainConfig, _ := os.ReadFile(mainConfigPath)
	eventsConfig, _ := os.ReadFile(eventsConfigPath)
	certificate, _ := os.ReadFile(filepath.Join(certificateDir, "site.crt"))
	if string(config) != "old-config" || string(streamConfig) != "old-stream-config" || string(mainConfig) != "old-main-config" || string(eventsConfig) != "old-events-config" || string(certificate) != "old-cert" {
		t.Fatalf("state was not restored: config=%q stream=%q main=%q events=%q certificate=%q", config, streamConfig, mainConfig, eventsConfig, certificate)
	}
	if runner.applies != 0 {
		t.Fatalf("apply should not run after failed validation")
	}
}

func TestApplyWritesNginxCapacityConfigs(t *testing.T) {
	directory := t.TempDir()
	mainConfigPath := filepath.Join(directory, "nginx-main.conf")
	eventsConfigPath := filepath.Join(directory, "nginx-events.conf")
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: directory,
		NginxConfigPath:       filepath.Join(directory, "nginx.conf"),
		NginxStreamConfigPath: filepath.Join(directory, "nginx-stream.conf"),
		NginxMainConfigPath:   mainConfigPath, NginxEventsConfigPath: eventsConfigPath,
		CertificateDir: filepath.Join(directory, "certs"), Runner: &fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	state := domain.DesiredState{
		Version: 2, NginxConfig: "HTTP config", NginxStreamConfig: "stream config", PublicPorts: []int{},
		NginxMainConfig:   "worker_processes 4;\nworker_rlimit_nofile 16384;\n",
		NginxEventsConfig: "worker_connections 8192;\n",
	}
	if err := agent.apply(state); err != nil {
		t.Fatal(err)
	}
	mainConfig, err := os.ReadFile(mainConfigPath)
	if err != nil || string(mainConfig) != state.NginxMainConfig {
		t.Fatalf("main config = %q, %v", mainConfig, err)
	}
	eventsConfig, err := os.ReadFile(eventsConfigPath)
	if err != nil || string(eventsConfig) != state.NginxEventsConfig {
		t.Fatalf("events config = %q, %v", eventsConfig, err)
	}
}

func TestApplySupportsManagedTCPListenersAndStreamConfig(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "nginx.conf")
	streamConfigPath := filepath.Join(directory, "nginx-stream.conf")
	runner := &fakeRunner{listeners: [][]domain.PortConflict{
		nil,
		{{Port: 9465, PID: 11, Process: "nginx"}, {Port: 9993, PID: 12, Process: "nginx"}},
	}}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: directory,
		NginxConfigPath: configPath, NginxStreamConfigPath: streamConfigPath,
		CertificateDir: filepath.Join(directory, "certs"), Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	state := domain.DesiredState{Version: 4, NginxConfig: "# http disabled\n", NginxStreamConfig: "server { listen 9465; }\n", PublicPorts: []int{9993, 9465}}
	if err := agent.apply(state); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(streamConfigPath); err != nil || string(contents) != state.NginxStreamConfig {
		t.Fatalf("stream config = %q, %v", contents, err)
	}
	if agent.lastApplyReport == nil || agent.lastApplyReport.Status != domain.ApplySucceeded || !strings.Contains(agent.lastApplyReport.Detail, "TCP 9465 and TCP 9993") {
		t.Fatalf("apply report = %#v", agent.lastApplyReport)
	}
}

func TestApplyRequiresNginxToOwnHTTP3UDPListener(t *testing.T) {
	directory := t.TempDir()
	runner := &fakeRunner{
		listeners:    [][]domain.PortConflict{nil, {{Port: 80, PID: 11, Process: "nginx"}, {Port: 443, PID: 11, Process: "nginx"}}},
		udpListeners: [][]domain.PortConflict{nil, {{Port: 443, PID: 11, Process: "nginx"}}},
	}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: directory,
		NginxConfigPath: filepath.Join(directory, "nginx.conf"), CertificateDir: filepath.Join(directory, "certs"), Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	state := domain.DesiredState{
		Version: 5, NginxConfig: "server { listen 443 quic; }\n",
		PublicPorts: []int{80, 443}, PublicUDPPorts: []int{443},
	}
	if err := agent.apply(state); err != nil {
		t.Fatal(err)
	}
	if runner.udpListenerChecks != 2 {
		t.Fatalf("UDP listener checks = %d, want 2", runner.udpListenerChecks)
	}
	if agent.lastApplyReport == nil || agent.lastApplyReport.Status != domain.ApplySucceeded || !strings.Contains(agent.lastApplyReport.Detail, "TCP 80 and TCP 443 and UDP 443") {
		t.Fatalf("apply report = %#v", agent.lastApplyReport)
	}
}

func TestApplyRejectsForeignHTTP3UDPListener(t *testing.T) {
	directory := t.TempDir()
	runner := &fakeRunner{udpListeners: [][]domain.PortConflict{{{Port: 443, PID: 91, Process: "caddy"}}}}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: directory,
		NginxConfigPath: filepath.Join(directory, "nginx.conf"), CertificateDir: filepath.Join(directory, "certs"), Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = agent.apply(domain.DesiredState{Version: 6, NginxConfig: "server { listen 443 quic; }\n", PublicPorts: []int{80, 443}, PublicUDPPorts: []int{443}})
	if err == nil || agent.lastApplyReport == nil || agent.lastApplyReport.Code != "port_conflict" {
		t.Fatalf("apply result = %v, report = %#v", err, agent.lastApplyReport)
	}
	if runner.tests != 0 || len(agent.lastApplyReport.PortConflicts) != 1 || agent.lastApplyReport.PortConflicts[0].Protocol != "udp" || !strings.Contains(agent.lastApplyReport.Detail, "UDP 443: caddy") {
		t.Fatalf("UDP conflict report = %#v", agent.lastApplyReport)
	}
}

func TestApplyRollsBackWhenHTTP3UDPListenerDoesNotStart(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "nginx.conf")
	if err := os.WriteFile(configPath, []byte("old-config"), 0o640); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{
		listeners:    [][]domain.PortConflict{nil, {{Port: 443, PID: 11, Process: "nginx"}}},
		udpListeners: [][]domain.PortConflict{nil},
	}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: directory, NginxConfigPath: configPath,
		CertificateDir: filepath.Join(directory, "certs"), Runner: runner,
		ListenerSettleTimeout: 2 * time.Millisecond, ListenerPollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = agent.apply(domain.DesiredState{Version: 7, NginxConfig: "server { listen 443 quic; }\n", PublicPorts: []int{443}, PublicUDPPorts: []int{443}})
	if err == nil || agent.lastApplyReport == nil || agent.lastApplyReport.Code != "nginx_not_listening" || !strings.Contains(agent.lastApplyReport.Detail, "UDP 443") {
		t.Fatalf("apply result = %v, report = %#v", err, agent.lastApplyReport)
	}
	contents, readErr := os.ReadFile(configPath)
	if readErr != nil || string(contents) != "old-config" {
		t.Fatalf("old configuration was not restored: %q, %v", contents, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(directory, "applied-version")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed HTTP/3 apply advanced version: %v", statErr)
	}
}

func TestApplyDoesNotAdvanceVersionWhenReloadIsNotAdopted(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "nginx.conf")
	if err := os.WriteFile(configPath, []byte("old-config"), 0o640); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{applyErr: errors.New("Nginx reload was not adopted"), listeners: [][]domain.PortConflict{nil, nil}}
	agent, err := New(Config{ControlURL: "https://control.example.test", StateDir: directory, NginxConfigPath: configPath, CertificateDir: filepath.Join(directory, "certs"), Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	err = agent.apply(domain.DesiredState{Version: 14, NginxConfig: "new-config"})
	if err == nil || agent.lastApplyReport == nil || agent.lastApplyReport.Code != "nginx_apply_failed" {
		t.Fatalf("apply result = %v, report = %#v", err, agent.lastApplyReport)
	}
	if _, statErr := os.Stat(filepath.Join(directory, "applied-version")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed reload advanced applied version: %v", statErr)
	}
	if contents, readErr := os.ReadFile(configPath); readErr != nil || string(contents) != "old-config" {
		t.Fatalf("old configuration was not restored: %q, %v", contents, readErr)
	}
}

func TestApplyRemovesStaleManagedCertificates(t *testing.T) {
	directory := t.TempDir()
	certificateDir := filepath.Join(directory, "certs")
	if err := os.MkdirAll(certificateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	staleID := "11111111-1111-4111-8111-111111111111"
	desiredID := "22222222-2222-4222-8222-222222222222"
	for _, extension := range []string{".crt", ".key"} {
		if err := os.WriteFile(filepath.Join(certificateDir, staleID+extension), []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(certificateDir, "operator-note.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{listeners: [][]domain.PortConflict{nil, {{Port: 80, PID: 11, Process: "nginx"}, {Port: 443, PID: 12, Process: "nginx"}}}}
	agent, err := New(Config{ControlURL: "https://control.example.test", StateDir: directory, NginxConfigPath: filepath.Join(directory, "nginx.conf"), CertificateDir: certificateDir, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	state := domain.DesiredState{Version: 2, NginxConfig: "new-config", PublicPorts: []int{80, 443}, Certificates: map[string]domain.TLSBundle{
		desiredID: {CertificatePEM: "desired-cert", PrivateKeyPEM: "desired-key"},
	}}
	if err := agent.apply(state); err != nil {
		t.Fatal(err)
	}
	for _, extension := range []string{".crt", ".key"} {
		if _, err := os.Stat(filepath.Join(certificateDir, staleID+extension)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale %s was retained: %v", extension, err)
		}
		if _, err := os.Stat(filepath.Join(certificateDir, desiredID+extension)); err != nil {
			t.Fatalf("desired %s is missing: %v", extension, err)
		}
	}
	if contents, err := os.ReadFile(filepath.Join(certificateDir, "operator-note.txt")); err != nil || string(contents) != "keep" {
		t.Fatalf("unmanaged file changed: %q, %v", contents, err)
	}
}

func TestApplyRestoresRemovedCertificatesOnFailedValidation(t *testing.T) {
	directory := t.TempDir()
	certificateDir := filepath.Join(directory, "certs")
	configPath := filepath.Join(directory, "nginx.conf")
	if err := os.MkdirAll(certificateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("old-config"), 0o640); err != nil {
		t.Fatal(err)
	}
	staleID := "33333333-3333-4333-8333-333333333333"
	for _, extension := range []string{".crt", ".key"} {
		if err := os.WriteFile(filepath.Join(certificateDir, staleID+extension), []byte("stale"+extension), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runner := &fakeRunner{testErr: errors.New("invalid config")}
	agent, err := New(Config{ControlURL: "https://control.example.test", StateDir: directory, NginxConfigPath: configPath, CertificateDir: certificateDir, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.apply(domain.DesiredState{Version: 3, NginxConfig: "bad-config", Certificates: map[string]domain.TLSBundle{}}); err == nil {
		t.Fatal("expected validation failure")
	}
	for _, extension := range []string{".crt", ".key"} {
		contents, err := os.ReadFile(filepath.Join(certificateDir, staleID+extension))
		if err != nil || string(contents) != "stale"+extension {
			t.Fatalf("stale %s was not restored: %q, %v", extension, contents, err)
		}
	}
	if contents, err := os.ReadFile(configPath); err != nil || string(contents) != "old-config" {
		t.Fatalf("old configuration was not restored: %q, %v", contents, err)
	}
	if runner.tests != 2 {
		t.Fatalf("old configuration was not revalidated after certificate restore: tests=%d", runner.tests)
	}
}

func TestApplyReportsPortConflictWithoutChangingConfiguration(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "nginx.conf")
	if err := os.WriteFile(configPath, []byte("old-config"), 0o640); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{listeners: [][]domain.PortConflict{{{Port: 80, PID: 1234, Process: "caddy"}}}}
	agent, err := New(Config{ControlURL: "https://control.example.test", StateDir: directory, NginxConfigPath: configPath, CertificateDir: filepath.Join(directory, "certs"), Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	err = agent.apply(domain.DesiredState{Version: 7, NginxConfig: "new-config"})
	if err == nil || agent.lastApplyReport == nil || agent.lastApplyReport.Code != "port_conflict" {
		t.Fatalf("apply result = %v, report = %#v", err, agent.lastApplyReport)
	}
	if runner.tests != 0 || runner.applies != 0 {
		t.Fatalf("port conflict should stop before Nginx work: tests=%d applies=%d", runner.tests, runner.applies)
	}
	config, err := os.ReadFile(configPath)
	if err != nil || string(config) != "old-config" {
		t.Fatalf("configuration changed after conflict: %q, %v", config, err)
	}
}

func TestApplyRejectsNginxListenerOutsideManagedConfiguration(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "nginx.conf")
	if err := os.WriteFile(configPath, []byte("# no managed TCP listener\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{listeners: [][]domain.PortConflict{{{Port: 9465, PID: 1234, Process: "nginx"}}}}
	agent, err := New(Config{ControlURL: "https://control.example.test", StateDir: directory, NginxConfigPath: configPath, CertificateDir: filepath.Join(directory, "certs"), Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	err = agent.apply(domain.DesiredState{Version: 7, NginxConfig: "# http disabled\n", NginxStreamConfig: "server { listen 9465; }\n", PublicPorts: []int{9465}})
	if err == nil || agent.lastApplyReport == nil || agent.lastApplyReport.Code != "port_conflict" || len(agent.lastApplyReport.PortConflicts) != 1 {
		t.Fatalf("apply result = %v, report = %#v", err, agent.lastApplyReport)
	}
	if got := agent.lastApplyReport.PortConflicts[0].Process; got != "nginx (unmanaged configuration)" {
		t.Fatalf("conflict process = %q", got)
	}
	if runner.tests != 0 || runner.applies != 0 {
		t.Fatalf("unmanaged Nginx listener should stop before apply: tests=%d applies=%d", runner.tests, runner.applies)
	}
}

func TestApplyConfirmsNginxOwnsBothPublicPorts(t *testing.T) {
	directory := t.TempDir()
	runner := &fakeRunner{listeners: [][]domain.PortConflict{
		nil,
		{{Port: 80, PID: 11, Process: "nginx"}, {Port: 443, PID: 12, Process: "nginx"}},
	}}
	agent, err := New(Config{ControlURL: "https://control.example.test", StateDir: directory, NginxConfigPath: filepath.Join(directory, "nginx.conf"), CertificateDir: filepath.Join(directory, "certs"), Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.apply(domain.DesiredState{Version: 8, NginxConfig: "new-config", PublicPorts: []int{80, 443}}); err != nil {
		t.Fatal(err)
	}
	if runner.tests != 1 || runner.applies != 1 || agent.lastApplyReport == nil || agent.lastApplyReport.Status != domain.ApplySucceeded {
		t.Fatalf("unexpected apply state: tests=%d applies=%d report=%#v", runner.tests, runner.applies, agent.lastApplyReport)
	}
}

func TestApplyWaitsForNginxToOpenNewPublicListener(t *testing.T) {
	directory := t.TempDir()
	runner := &fakeRunner{listeners: [][]domain.PortConflict{
		nil,
		{{Port: 80, PID: 11, Process: "nginx"}},
		{{Port: 80, PID: 11, Process: "nginx"}},
		{{Port: 80, PID: 11, Process: "nginx"}, {Port: 443, PID: 12, Process: "nginx"}},
	}}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: directory,
		NginxConfigPath: filepath.Join(directory, "nginx.conf"), CertificateDir: filepath.Join(directory, "certs"), Runner: runner,
		ListenerSettleTimeout: 100 * time.Millisecond, ListenerPollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.apply(domain.DesiredState{Version: 10, NginxConfig: "new-config", PublicPorts: []int{80, 443}}); err != nil {
		t.Fatal(err)
	}
	if runner.listenerChecks != 4 || agent.lastApplyReport == nil || agent.lastApplyReport.Status != domain.ApplySucceeded {
		t.Fatalf("delayed listeners were not accepted: checks=%d report=%#v", runner.listenerChecks, agent.lastApplyReport)
	}
}

func TestApplyRollsBackWhenNginxListenerSettleTimesOut(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "nginx.conf")
	if err := os.WriteFile(configPath, []byte("old-config"), 0o640); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{listeners: [][]domain.PortConflict{
		nil,
		{{Port: 80, PID: 11, Process: "nginx"}},
	}}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: directory,
		NginxConfigPath: configPath, CertificateDir: filepath.Join(directory, "certs"), Runner: runner,
		ListenerSettleTimeout: 5 * time.Millisecond, ListenerPollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = agent.apply(domain.DesiredState{Version: 11, NginxConfig: "new-config", PublicPorts: []int{80, 443}})
	if err == nil || agent.lastApplyReport == nil || agent.lastApplyReport.Code != "nginx_not_listening" {
		t.Fatalf("apply result = %v, report = %#v", err, agent.lastApplyReport)
	}
	config, readErr := os.ReadFile(configPath)
	if readErr != nil || string(config) != "old-config" {
		t.Fatalf("configuration was not restored after listener timeout: %q, %v", config, readErr)
	}
	if runner.listenerChecks < 3 {
		t.Fatalf("listener readiness was not retried: checks=%d", runner.listenerChecks)
	}
}

func TestApplyStopsListenerWaitOnPortConflict(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "nginx.conf")
	if err := os.WriteFile(configPath, []byte("old-config"), 0o640); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{listeners: [][]domain.PortConflict{
		nil,
		{{Port: 80, PID: 11, Process: "nginx"}, {Port: 443, PID: 22, Process: "caddy"}},
	}}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: directory,
		NginxConfigPath: configPath, CertificateDir: filepath.Join(directory, "certs"), Runner: runner,
		ListenerSettleTimeout: time.Second, ListenerPollInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = agent.apply(domain.DesiredState{Version: 12, NginxConfig: "new-config", PublicPorts: []int{80, 443}})
	if err == nil || agent.lastApplyReport == nil || agent.lastApplyReport.Code != "port_conflict" {
		t.Fatalf("apply result = %v, report = %#v", err, agent.lastApplyReport)
	}
	if runner.listenerChecks != 2 {
		t.Fatalf("post-reload port conflict should stop waiting immediately: checks=%d", runner.listenerChecks)
	}
}

func TestApplyAllowsHealthOnlyNodeToListenOnPort80(t *testing.T) {
	directory := t.TempDir()
	runner := &fakeRunner{listeners: [][]domain.PortConflict{
		nil,
		{{Port: 80, PID: 11, Process: "nginx"}},
	}}
	agent, err := New(Config{ControlURL: "https://control.example.test", StateDir: directory, NginxConfigPath: filepath.Join(directory, "nginx.conf"), CertificateDir: filepath.Join(directory, "certs"), Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.apply(domain.DesiredState{Version: 9, NginxConfig: "health-config", PublicPorts: []int{80}}); err != nil {
		t.Fatal(err)
	}
	if agent.lastApplyReport == nil || agent.lastApplyReport.Status != domain.ApplySucceeded || !strings.Contains(agent.lastApplyReport.Detail, "TCP 80") {
		t.Fatalf("unexpected report: %#v", agent.lastApplyReport)
	}
}

func TestNewRejectsInsecureControlURL(t *testing.T) {
	if _, err := New(Config{ControlURL: "http://control.example.test", StateDir: t.TempDir(), Runner: &fakeRunner{}}); err == nil {
		t.Fatal("expected HTTP control URL to be rejected")
	}
}

func TestApplyWritesVersionedNginxFragmentDirectories(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "nginx.conf")
	streamConfigPath := filepath.Join(directory, "nginx-stream.conf")
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: directory,
		NginxConfigPath: configPath, NginxStreamConfigPath: streamConfigPath,
		CertificateDir: filepath.Join(directory, "certs"), Runner: &fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	state := domain.DesiredState{
		Version: 5, NginxConfig: "legacy HTTP config", NginxStreamConfig: "legacy stream config", PublicPorts: []int{},
		NginxFragments: &domain.NginxConfigFragments{
			HTTPBase: "# HTTP base\n", HTTPSites: []domain.NginxConfigFragment{{Name: "site-a.conf", Content: "# HTTP site A\n"}},
			StreamBase: "# stream base\n", StreamSites: []domain.NginxConfigFragment{{Name: "site-a.conf", Content: "# stream site A\n"}},
		},
	}
	if err := agent.apply(state); err != nil {
		t.Fatal(err)
	}
	httpIndex, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	streamIndex, err := os.ReadFile(streamConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(httpIndex), "/fragments/http-v5-") || !strings.Contains(string(streamIndex), "/fragments/stream-v5-") ||
		strings.Contains(string(httpIndex), state.NginxConfig) {
		t.Fatalf("fragment indexes: HTTP=%q stream=%q", httpIndex, streamIndex)
	}
	for pattern, expected := range map[string]int{
		filepath.Join(directory, "fragments", "http-v5-*", "*.conf"):   2,
		filepath.Join(directory, "fragments", "stream-v5-*", "*.conf"): 2,
	} {
		paths, err := filepath.Glob(pattern)
		if err != nil || len(paths) != expected {
			t.Fatalf("fragment files %s = %#v, %v", pattern, paths, err)
		}
	}
	if !slicesContain(agent.Config.Capabilities, domain.EdgeCapabilityNginxFragments) ||
		!slicesContain(agent.Config.Capabilities, domain.EdgeCapabilityRequestTracing) {
		t.Fatalf("agent capabilities = %#v", agent.Config.Capabilities)
	}
}

func TestFragmentApplyFailureRestoresIndexesAndRemovesStagedVersion(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "nginx.conf")
	streamConfigPath := filepath.Join(directory, "nginx-stream.conf")
	if err := os.WriteFile(configPath, []byte("old HTTP"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(streamConfigPath, []byte("old stream"), 0o640); err != nil {
		t.Fatal(err)
	}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: directory,
		NginxConfigPath: configPath, NginxStreamConfigPath: streamConfigPath,
		CertificateDir: filepath.Join(directory, "certs"), Runner: &fakeRunner{testErr: errors.New("invalid fragments")},
	})
	if err != nil {
		t.Fatal(err)
	}
	state := domain.DesiredState{Version: 6, NginxConfig: "legacy", PublicPorts: []int{}, NginxFragments: &domain.NginxConfigFragments{HTTPBase: "# base\n", StreamBase: "# stream\n"}}
	if err := agent.apply(state); err == nil {
		t.Fatal("invalid fragmented configuration was accepted")
	}
	for path, want := range map[string]string{configPath: "old HTTP", streamConfigPath: "old stream"} {
		contents, err := os.ReadFile(path)
		if err != nil || string(contents) != want {
			t.Fatalf("restored index %s = %q, %v", path, contents, err)
		}
	}
	paths, err := filepath.Glob(filepath.Join(directory, "fragments", "*-v6-*"))
	if err != nil || len(paths) != 0 {
		t.Fatalf("failed fragment version remains: %#v, %v", paths, err)
	}
}

func TestFragmentUpgradeFailureRestoresPreviousFragmentGeneration(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "nginx.conf")
	streamConfigPath := filepath.Join(directory, "nginx-stream.conf")
	runner := &fakeRunner{}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: directory,
		NginxConfigPath: configPath, NginxStreamConfigPath: streamConfigPath,
		CertificateDir: filepath.Join(directory, "certs"), Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	state := domain.DesiredState{Version: 5, PublicPorts: []int{}, NginxFragments: &domain.NginxConfigFragments{
		HTTPBase: "# old HTTP base\n", HTTPSites: []domain.NginxConfigFragment{{Name: "site-a.conf", Content: "# old HTTP site\n"}},
		StreamBase: "# old stream base\n", StreamSites: []domain.NginxConfigFragment{{Name: "site-a.conf", Content: "# old stream site\n"}},
	}}
	if err := agent.apply(state); err != nil {
		t.Fatal(err)
	}
	oldHTTPIndex, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	oldStreamIndex, err := os.ReadFile(streamConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	runner.testErr = errors.New("new fragment generation is invalid")
	state.Version = 6
	state.NginxFragments.HTTPBase = "# new HTTP base\n"
	state.NginxFragments.StreamBase = "# new stream base\n"
	if err := agent.apply(state); err == nil {
		t.Fatal("invalid fragment upgrade was accepted")
	}
	for path, want := range map[string]string{configPath: string(oldHTTPIndex), streamConfigPath: string(oldStreamIndex)} {
		contents, err := os.ReadFile(path)
		if err != nil || string(contents) != want {
			t.Fatalf("restored fragment index %s = %q, %v", path, contents, err)
		}
	}
	oldPaths, err := filepath.Glob(filepath.Join(directory, "fragments", "*-v5-*", "*.conf"))
	if err != nil || len(oldPaths) != 4 {
		t.Fatalf("previous fragment generation = %#v, %v", oldPaths, err)
	}
	newPaths, err := filepath.Glob(filepath.Join(directory, "fragments", "*-v6-*"))
	if err != nil || len(newPaths) != 0 {
		t.Fatalf("failed fragment generation remains = %#v, %v", newPaths, err)
	}
}

func slicesContain(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
