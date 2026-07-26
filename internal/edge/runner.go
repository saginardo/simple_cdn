package edge

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"simple_cdn/internal/domain"
)

type Runner interface {
	Test() error
	Apply() error
	PortListeners(ports []int) ([]domain.PortConflict, error)
	UDPPortListeners(ports []int) ([]domain.PortConflict, error)
}

type NginxRunner struct {
	Binary             string
	ReloadTimeout      time.Duration
	ReloadPollInterval time.Duration
	command            func(string, ...string) ([]byte, error)
	snapshot           func() (nginxProcessSnapshot, error)
}

type nginxProcessSnapshot struct {
	MasterPID int
	Workers   []int
}

func (r NginxRunner) Test() error {
	binary := r.Binary
	if binary == "" {
		binary = "nginx"
	}
	output, err := r.run(binary, "-t")
	if err != nil {
		return fmt.Errorf("nginx -t: %w: %s", err, output)
	}
	return nil
}

func (r NginxRunner) Apply() error {
	_, activeErr := r.run("systemctl", "is-active", "--quiet", "nginx")
	active := activeErr == nil
	if active {
		return r.reload()
	}
	// A failed unit retains its failed state after a previous port conflict.
	// Clearing it here makes a later Publish a real recovery action once the
	// conflicting listener has been removed.
	_, _ = r.run("systemctl", "reset-failed", "nginx")
	output, err := r.run("systemctl", "start", "nginx")
	if err != nil {
		return fmt.Errorf("start nginx: %w: %s", err, output)
	}
	return nil
}

func (r NginxRunner) reload() error {
	before, err := r.processSnapshot()
	if err != nil {
		return fmt.Errorf("inspect Nginx workers before reload: %w", err)
	}
	binary := r.Binary
	if binary == "" {
		binary = "nginx"
	}
	output, err := r.run(binary, "-s", "reload")
	if err != nil {
		return fmt.Errorf("nginx reload: %w: %s", err, output)
	}
	return r.waitForNewWorker(before)
}

func (r NginxRunner) waitForNewWorker(before nginxProcessSnapshot) error {
	timeout := r.ReloadTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	pollInterval := r.ReloadPollInterval
	if pollInterval <= 0 {
		pollInterval = 100 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	var last nginxProcessSnapshot
	var lastErr error
	for {
		last, lastErr = r.processSnapshot()
		if lastErr == nil && hasNewWorker(before.Workers, last.Workers) {
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(pollInterval)
	}
	if lastErr != nil {
		return fmt.Errorf("Nginx reload was not adopted within %s: %w", timeout, lastErr)
	}
	return fmt.Errorf("Nginx reload was not adopted within %s: master %d workers %v remained master %d workers %v", timeout, before.MasterPID, before.Workers, last.MasterPID, last.Workers)
}

func hasNewWorker(before, after []int) bool {
	existing := make(map[int]bool, len(before))
	for _, pid := range before {
		existing[pid] = true
	}
	for _, pid := range after {
		if !existing[pid] {
			return true
		}
	}
	return false
}

func (r NginxRunner) run(name string, arguments ...string) ([]byte, error) {
	if r.command != nil {
		return r.command(name, arguments...)
	}
	return exec.Command(name, arguments...).CombinedOutput()
}

func (r NginxRunner) processSnapshot() (nginxProcessSnapshot, error) {
	if r.snapshot != nil {
		return r.snapshot()
	}
	return readNginxProcessSnapshot()
}

func readNginxProcessSnapshot() (nginxProcessSnapshot, error) {
	pidBytes, err := os.ReadFile("/run/nginx.pid")
	if err != nil {
		return nginxProcessSnapshot{}, err
	}
	masterPID, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil || masterPID <= 0 {
		return nginxProcessSnapshot{}, fmt.Errorf("invalid Nginx master PID %q", strings.TrimSpace(string(pidBytes)))
	}
	childrenPath := fmt.Sprintf("/proc/%d/task/%d/children", masterPID, masterPID)
	childrenBytes, err := os.ReadFile(childrenPath)
	if err != nil {
		return nginxProcessSnapshot{}, err
	}
	workers := make([]int, 0)
	for _, value := range strings.Fields(string(childrenBytes)) {
		pid, parseErr := strconv.Atoi(value)
		if parseErr != nil || pid <= 0 {
			continue
		}
		commandLine, readErr := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if readErr != nil {
			continue
		}
		title := strings.ReplaceAll(string(commandLine), "\x00", " ")
		if strings.Contains(title, "nginx: worker process") {
			workers = append(workers, pid)
		}
	}
	sort.Ints(workers)
	return nginxProcessSnapshot{MasterPID: masterPID, Workers: workers}, nil
}

var listenerProcess = regexp.MustCompile(`\("([^"]+)",pid=([0-9]+)`) // ss users:(()) output

func (r NginxRunner) PortListeners(ports []int) ([]domain.PortConflict, error) {
	return r.portListeners("TCP", "tcp", "-ltnp", ports)
}

func (r NginxRunner) UDPPortListeners(ports []int) ([]domain.PortConflict, error) {
	return r.portListeners("UDP", "udp", "-lunp", ports)
}

func (r NginxRunner) portListeners(label, protocol, ssOptions string, ports []int) ([]domain.PortConflict, error) {
	seen := make(map[string]domain.PortConflict)
	for _, port := range ports {
		output, err := r.run("ss", "-H", ssOptions, fmt.Sprintf("( sport = :%d )", port))
		if err != nil {
			return nil, fmt.Errorf("inspect %s port %d: %w: %s", label, port, err, output)
		}
		matches := listenerProcess.FindAllStringSubmatch(string(output), -1)
		if len(matches) == 0 && strings.TrimSpace(string(output)) != "" {
			key := fmt.Sprintf("%s:%d:unknown:0", protocol, port)
			seen[key] = domain.PortConflict{Protocol: protocol, Port: port, Process: "unknown"}
		}
		for _, match := range matches {
			pid, _ := strconv.Atoi(match[2])
			listener := domain.PortConflict{Protocol: protocol, Port: port, PID: pid, Process: match[1]}
			key := fmt.Sprintf("%s:%d:%s:%d", protocol, listener.Port, listener.Process, listener.PID)
			seen[key] = listener
		}
	}
	listeners := make([]domain.PortConflict, 0, len(seen))
	for _, listener := range seen {
		listeners = append(listeners, listener)
	}
	sort.Slice(listeners, func(i, j int) bool {
		if listeners[i].Port != listeners[j].Port {
			return listeners[i].Port < listeners[j].Port
		}
		if listeners[i].Process != listeners[j].Process {
			return listeners[i].Process < listeners[j].Process
		}
		return listeners[i].PID < listeners[j].PID
	})
	return listeners, nil
}
