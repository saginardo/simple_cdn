package edge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"simple_cdn/internal/domain"
)

type machineStatusReporter interface {
	Collect() (*domain.MachineStatus, error)
}

type machineNetworkStatusReporter interface {
	CollectNetwork() (*domain.MachineNetworkStatus, error)
}

type machineStatusCollector struct {
	mu              sync.Mutex
	readFile        func(string) ([]byte, error)
	statFilesystem  func(string) (int64, int64, error)
	now             func() time.Time
	logicalCPUs     func() int
	nginxStatus     func() (*domain.NginxRuntimeStatus, error)
	previous        *machineSample
	previousNetwork *machineNetworkSample
}

type machineSample struct {
	at               time.Time
	cpu              cpuCounters
	networkInterface string
	network          networkCounters
}

type machineNetworkSample struct {
	at               time.Time
	networkInterface string
	network          networkCounters
}

type cpuCounters struct {
	total uint64
	idle  uint64
}

type networkCounters struct {
	rx uint64
	tx uint64
}

func newMachineStatusCollector(nginxStatusSocketPath string) *machineStatusCollector {
	return &machineStatusCollector{
		readFile:       os.ReadFile,
		statFilesystem: rootFilesystemUsage,
		now:            time.Now,
		logicalCPUs:    runtime.NumCPU,
		nginxStatus:    newNginxStatusReader(nginxStatusSocketPath),
	}
}

func (c *machineStatusCollector) Collect() (*domain.MachineStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	collectedAt := c.now().UTC()
	distribution, version, err := collectOSVersion(c.readFile)
	if err != nil {
		return nil, err
	}
	uptime, err := readUptime(c.readFile)
	if err != nil {
		return nil, err
	}
	load1, load5, load15, err := readLoadAverage(c.readFile)
	if err != nil {
		return nil, err
	}
	cpu, err := readCPUCounters(c.readFile)
	if err != nil {
		return nil, err
	}
	memoryUsed, memoryTotal, err := readMemoryUsage(c.readFile)
	if err != nil {
		return nil, err
	}
	diskUsed, diskTotal, err := c.statFilesystem("/")
	if err != nil {
		return nil, fmt.Errorf("read root filesystem usage: %w", err)
	}
	networkInterface, network, err := readNetworkCounters(c.readFile)
	if err != nil {
		return nil, err
	}

	status := &domain.MachineStatus{
		Distribution: distribution, Version: version, UptimeSeconds: uptime,
		Load1: load1, Load5: load5, Load15: load15, CPULogicalCores: c.logicalCPUs(),
		MemoryUsedBytes: memoryUsed, MemoryTotalBytes: memoryTotal,
		DiskUsedBytes: diskUsed, DiskTotalBytes: diskTotal,
		NetworkInterface: networkInterface, CollectedAt: collectedAt,
	}
	if c.nginxStatus != nil {
		// Runtime Nginx telemetry is optional. A stopped or restarting Nginx must
		// not suppress the host snapshot; the UI simply omits this section.
		status.Nginx, _ = c.nginxStatus()
	}
	if c.previous != nil {
		sampleSeconds := collectedAt.Sub(c.previous.at).Seconds()
		if sampleSeconds > 0 && sampleSeconds <= 24*60*60 {
			status.SampleSeconds = sampleSeconds
			status.CPUUsagePercent = cpuUsagePercent(c.previous.cpu, cpu)
			if c.previous.networkInterface == networkInterface {
				status.NetworkRXBytesPerSec = counterRate(c.previous.network.rx, network.rx, sampleSeconds)
				status.NetworkTXBytesPerSec = counterRate(c.previous.network.tx, network.tx, sampleSeconds)
			}
		}
	}
	if !domain.ValidMachineStatus(*status) {
		return nil, errors.New("collected machine status is invalid")
	}
	c.previous = &machineSample{at: collectedAt, cpu: cpu, networkInterface: networkInterface, network: network}
	return status, nil
}

func (c *machineStatusCollector) CollectNetwork() (*domain.MachineNetworkStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	collectedAt := c.now().UTC()
	networkInterface, network, err := readNetworkCounters(c.readFile)
	if err != nil {
		return nil, err
	}
	status := &domain.MachineNetworkStatus{
		NetworkInterface: networkInterface,
		CollectedAt:      collectedAt,
	}
	if c.previousNetwork != nil {
		sampleSeconds := collectedAt.Sub(c.previousNetwork.at).Seconds()
		if sampleSeconds > 0 && sampleSeconds <= 24*60*60 {
			status.SampleSeconds = sampleSeconds
			if c.previousNetwork.networkInterface == networkInterface {
				status.NetworkRXBytesPerSec = counterRate(c.previousNetwork.network.rx, network.rx, sampleSeconds)
				status.NetworkTXBytesPerSec = counterRate(c.previousNetwork.network.tx, network.tx, sampleSeconds)
			}
		}
	}
	if !domain.ValidMachineNetworkStatus(*status) {
		return nil, errors.New("collected machine network status is invalid")
	}
	c.previousNetwork = &machineNetworkSample{
		at: collectedAt, networkInterface: networkInterface, network: network,
	}
	return status, nil
}

const maxNginxStatusBodyBytes = 4096

func newNginxStatusReader(socketPath string) func() (*domain.NginxRuntimeStatus, error) {
	socketPath = strings.TrimSpace(socketPath)
	if socketPath == "" {
		return nil
	}
	dialer := &net.Dialer{Timeout: 500 * time.Millisecond}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
		MaxIdleConns:        1,
		MaxIdleConnsPerHost: 1,
		MaxConnsPerHost:     1,
		IdleConnTimeout:     30 * time.Second,
	}
	client := &http.Client{Transport: transport, Timeout: 750 * time.Millisecond}
	return func() (*domain.NginxRuntimeStatus, error) {
		request, err := http.NewRequest(http.MethodGet, "http://nginx/stub_status", nil)
		if err != nil {
			return nil, err
		}
		response, err := client.Do(request)
		if err != nil {
			return nil, fmt.Errorf("read Nginx status: %w", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("read Nginx status: HTTP %d", response.StatusCode)
		}
		contents, err := io.ReadAll(io.LimitReader(response.Body, maxNginxStatusBodyBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read Nginx status body: %w", err)
		}
		if len(contents) > maxNginxStatusBodyBytes {
			return nil, errors.New("Nginx status body is too large")
		}
		return parseNginxStubStatus(contents)
	}
}

func parseNginxStubStatus(contents []byte) (*domain.NginxRuntimeStatus, error) {
	lines := make([]string, 0, 4)
	for _, line := range strings.Split(string(contents), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) != 4 || lines[1] != "server accepts handled requests" {
		return nil, errors.New("invalid Nginx stub_status response")
	}
	status := &domain.NginxRuntimeStatus{}
	var trailing string
	if count, err := fmt.Sscanf(lines[0], "Active connections: %d %s", &status.ActiveConnections, &trailing); count != 1 || err != nil && !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid Nginx active connection count")
	}
	if count, err := fmt.Sscanf(lines[2], "%d %d %d %s", &status.AcceptedConnections, &status.HandledConnections, &status.Requests, &trailing); count != 3 || err != nil && !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid Nginx connection counters")
	}
	if count, err := fmt.Sscanf(lines[3], "Reading: %d Writing: %d Waiting: %d %s", &status.Reading, &status.Writing, &status.Waiting, &trailing); count != 3 || err != nil && !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid Nginx connection states")
	}
	if !domain.ValidNginxRuntimeStatus(*status) {
		return nil, errors.New("invalid Nginx runtime status")
	}
	return status, nil
}

func collectOSVersion(readFile func(string) ([]byte, error)) (string, string, error) {
	contents, err := readFile("/etc/os-release")
	if err != nil {
		contents, err = readFile("/usr/lib/os-release")
	}
	if err != nil {
		return "", "", fmt.Errorf("read os-release: %w", err)
	}
	values := parseOSRelease(contents)
	distribution := strings.TrimSpace(values["NAME"])
	if distribution == "" {
		distribution = strings.TrimSpace(values["ID"])
	}
	if distribution == "" {
		distribution = "Linux"
	}
	version := strings.TrimSpace(values["VERSION_ID"])
	if strings.EqualFold(strings.TrimSpace(values["ID"]), "debian") {
		if debianVersion, readErr := readFile("/etc/debian_version"); readErr == nil {
			if value := strings.TrimSpace(string(debianVersion)); value != "" && len(value) <= 128 {
				version = value
			}
		}
	}
	if version == "" {
		version = strings.TrimSpace(values["VERSION"])
	}
	if version == "" {
		if kernelVersion, readErr := readFile("/proc/sys/kernel/osrelease"); readErr == nil {
			version = strings.TrimSpace(string(kernelVersion))
		}
	}
	if version == "" {
		return "", "", errors.New("operating system version is unavailable")
	}
	return distribution, version, nil
}

func parseOSRelease(contents []byte) map[string]string {
	result := make(map[string]string)
	for _, line := range strings.Split(string(contents), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
			value = value[1 : len(value)-1]
		} else if strings.HasPrefix(value, "\"") {
			if unquoted, err := strconv.Unquote(value); err == nil {
				value = unquoted
			}
		}
		if key != "" {
			result[key] = value
		}
	}
	return result
}

func readUptime(readFile func(string) ([]byte, error)) (int64, error) {
	contents, err := readFile("/proc/uptime")
	if err != nil {
		return 0, fmt.Errorf("read uptime: %w", err)
	}
	fields := strings.Fields(string(contents))
	if len(fields) == 0 {
		return 0, errors.New("invalid /proc/uptime")
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds > float64(math.MaxInt64) {
		return 0, errors.New("invalid /proc/uptime")
	}
	return int64(seconds), nil
}

func readLoadAverage(readFile func(string) ([]byte, error)) (float64, float64, float64, error) {
	contents, err := readFile("/proc/loadavg")
	if err != nil {
		return 0, 0, 0, fmt.Errorf("read load average: %w", err)
	}
	fields := strings.Fields(string(contents))
	if len(fields) < 3 {
		return 0, 0, 0, errors.New("invalid /proc/loadavg")
	}
	values := [3]float64{}
	for index := range values {
		values[index], err = strconv.ParseFloat(fields[index], 64)
		if err != nil || math.IsNaN(values[index]) || math.IsInf(values[index], 0) || values[index] < 0 {
			return 0, 0, 0, errors.New("invalid /proc/loadavg")
		}
	}
	return values[0], values[1], values[2], nil
}

func readCPUCounters(readFile func(string) ([]byte, error)) (cpuCounters, error) {
	contents, err := readFile("/proc/stat")
	if err != nil {
		return cpuCounters{}, fmt.Errorf("read CPU counters: %w", err)
	}
	line, _, _ := strings.Cut(string(contents), "\n")
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuCounters{}, errors.New("invalid aggregate CPU counters")
	}
	values := make([]uint64, 0, 8)
	for index := 1; index < len(fields) && index <= 8; index++ {
		value, parseErr := strconv.ParseUint(fields[index], 10, 64)
		if parseErr != nil {
			return cpuCounters{}, errors.New("invalid aggregate CPU counters")
		}
		values = append(values, value)
	}
	if len(values) < 4 {
		return cpuCounters{}, errors.New("invalid aggregate CPU counters")
	}
	var total uint64
	for _, value := range values {
		total += value
	}
	idle := values[3]
	if len(values) > 4 {
		idle += values[4]
	}
	return cpuCounters{total: total, idle: idle}, nil
}

func cpuUsagePercent(previous, current cpuCounters) float64 {
	if current.total <= previous.total || current.idle < previous.idle {
		return 0
	}
	total := current.total - previous.total
	idle := current.idle - previous.idle
	if idle >= total {
		return 0
	}
	return math.Min(100, float64(total-idle)/float64(total)*100)
}

func readMemoryUsage(readFile func(string) ([]byte, error)) (int64, int64, error) {
	contents, err := readFile("/proc/meminfo")
	if err != nil {
		return 0, 0, fmt.Errorf("read memory usage: %w", err)
	}
	values := make(map[string]uint64)
	for _, line := range strings.Split(string(contents), "\n") {
		key, raw, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		fields := strings.Fields(raw)
		if len(fields) == 0 {
			continue
		}
		value, parseErr := strconv.ParseUint(fields[0], 10, 64)
		if parseErr == nil {
			values[strings.TrimSpace(key)] = value
		}
	}
	totalKiB := values["MemTotal"]
	if totalKiB == 0 {
		return 0, 0, errors.New("MemTotal is missing from /proc/meminfo")
	}
	availableKiB := values["MemAvailable"]
	if availableKiB == 0 {
		availableKiB = values["MemFree"] + values["Buffers"] + values["Cached"] + values["SReclaimable"]
		if shmem := values["Shmem"]; shmem < availableKiB {
			availableKiB -= shmem
		}
	}
	if availableKiB > totalKiB {
		availableKiB = totalKiB
	}
	total, err := kibibytesToBytes(totalKiB)
	if err != nil {
		return 0, 0, err
	}
	available, err := kibibytesToBytes(availableKiB)
	if err != nil {
		return 0, 0, err
	}
	return total - available, total, nil
}

func kibibytesToBytes(value uint64) (int64, error) {
	if value > uint64(math.MaxInt64)/1024 {
		return 0, errors.New("memory counter exceeds supported range")
	}
	return int64(value * 1024), nil
}

func rootFilesystemUsage(path string) (int64, int64, error) {
	var status syscall.Statfs_t
	if err := syscall.Statfs(path, &status); err != nil {
		return 0, 0, err
	}
	if status.Bsize <= 0 || status.Blocks == 0 {
		return 0, 0, errors.New("filesystem returned an invalid capacity")
	}
	blockSize := uint64(status.Bsize)
	if status.Blocks > uint64(math.MaxInt64)/blockSize || status.Bfree > status.Blocks {
		return 0, 0, errors.New("filesystem capacity exceeds supported range")
	}
	total := int64(status.Blocks * blockSize)
	free := int64(status.Bfree * blockSize)
	return total - free, total, nil
}

func readNetworkCounters(readFile func(string) ([]byte, error)) (string, networkCounters, error) {
	preferred := ""
	if routes, err := readFile("/proc/net/route"); err == nil {
		preferred = defaultRouteInterface(routes)
	}
	contents, err := readFile("/proc/net/dev")
	if err != nil {
		return "", networkCounters{}, fmt.Errorf("read network counters: %w", err)
	}
	interfaces := make(map[string]networkCounters)
	for _, line := range strings.Split(string(contents), "\n") {
		name, counters, found := parseNetworkDeviceLine(line)
		if found && name != "lo" {
			interfaces[name] = counters
		}
	}
	if preferred != "" {
		if counters, found := interfaces[preferred]; found {
			return preferred, counters, nil
		}
	}
	if len(interfaces) == 0 {
		return "", networkCounters{}, errors.New("no non-loopback network interface is available")
	}
	if len(interfaces) == 1 {
		for name, counters := range interfaces {
			return name, counters, nil
		}
	}
	var aggregate networkCounters
	for _, counters := range interfaces {
		aggregate.rx += counters.rx
		aggregate.tx += counters.tx
	}
	return "all", aggregate, nil
}

func defaultRouteInterface(contents []byte) string {
	selected := ""
	selectedMetric := uint64(math.MaxUint64)
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 || fields[0] == "Iface" || fields[1] != "00000000" || fields[7] != "00000000" {
			continue
		}
		flags, flagsErr := strconv.ParseUint(fields[3], 16, 64)
		metric, metricErr := strconv.ParseUint(fields[6], 10, 64)
		if flagsErr != nil || metricErr != nil || flags&1 == 0 {
			continue
		}
		if metric < selectedMetric {
			selected = fields[0]
			selectedMetric = metric
		}
	}
	return selected
}

func parseNetworkDeviceLine(line string) (string, networkCounters, bool) {
	name, raw, found := strings.Cut(line, ":")
	if !found {
		return "", networkCounters{}, false
	}
	name = strings.TrimSpace(name)
	fields := strings.Fields(raw)
	if name == "" || len(fields) < 9 {
		return "", networkCounters{}, false
	}
	rx, rxErr := strconv.ParseUint(fields[0], 10, 64)
	tx, txErr := strconv.ParseUint(fields[8], 10, 64)
	if rxErr != nil || txErr != nil {
		return "", networkCounters{}, false
	}
	return name, networkCounters{rx: rx, tx: tx}, true
}

func counterRate(previous, current uint64, seconds float64) int64 {
	if current < previous || seconds <= 0 {
		return 0
	}
	rate := float64(current-previous) / seconds
	if rate >= float64(int64(1<<60)) {
		return 1 << 60
	}
	return int64(math.Round(rate))
}
