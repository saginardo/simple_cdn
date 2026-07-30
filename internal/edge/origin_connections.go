package edge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"simple_cdn/internal/domain"
)

const (
	originConnectionCountTimeout = 750 * time.Millisecond
	originAddressResolutionTTL   = time.Minute
	maximumOriginResolutionCache = 1024
)

type originConnectionCounter interface {
	Count(context.Context, []domain.OriginPool) map[string]int64
}

type originConnectionCounterFunc func(context.Context, []domain.OriginPool) map[string]int64

func (function originConnectionCounterFunc) Count(ctx context.Context, pools []domain.OriginPool) map[string]int64 {
	return function(ctx, pools)
}

type originIPResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type cachedOriginAddresses struct {
	addresses []netip.Addr
	expiresAt time.Time
}

type procNginxOriginConnectionCounter struct {
	pidPath  string
	procRoot string
	resolver originIPResolver
	now      func() time.Time

	resolutionMu sync.Mutex
	resolutions  map[string]cachedOriginAddresses
}

type originTCPEndpoint struct {
	address netip.Addr
	port    uint16
}

func newNginxOriginConnectionCounter(pidPath string) originConnectionCounter {
	return &procNginxOriginConnectionCounter{
		pidPath: pidPath, procRoot: "/proc", resolver: net.DefaultResolver, now: time.Now,
		resolutions: make(map[string]cachedOriginAddresses),
	}
}

func (counter *procNginxOriginConnectionCounter) Count(ctx context.Context, pools []domain.OriginPool) map[string]int64 {
	if counter == nil || len(pools) == 0 || ctx.Err() != nil {
		return nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, originConnectionCountTimeout)
	defer cancel()

	poolEndpoints := make(map[string]map[originTCPEndpoint]struct{}, len(pools))
	owners := make(map[originTCPEndpoint]map[string]struct{}, len(pools))
	for _, pool := range pools {
		host, portText, err := net.SplitHostPort(pool.Address)
		if err != nil {
			continue
		}
		portValue, err := strconv.ParseUint(portText, 10, 16)
		if err != nil || portValue == 0 {
			continue
		}
		addresses, ok := counter.resolveHost(lookupCtx, host)
		if !ok {
			continue
		}
		endpoints := make(map[originTCPEndpoint]struct{}, len(addresses))
		for _, address := range addresses {
			endpoint := originTCPEndpoint{address: address, port: uint16(portValue)}
			endpoints[endpoint] = struct{}{}
			if owners[endpoint] == nil {
				owners[endpoint] = make(map[string]struct{})
			}
			owners[endpoint][pool.ID] = struct{}{}
		}
		if len(endpoints) > 0 {
			poolEndpoints[pool.ID] = endpoints
		}
	}
	if len(poolEndpoints) == 0 {
		return nil
	}

	// A socket only exposes its remote IP and port. If two logical pools share
	// either endpoint, Host, SNI, or HTTP version cannot be recovered reliably.
	ambiguous := make(map[string]bool)
	for _, poolIDs := range owners {
		if len(poolIDs) < 2 {
			continue
		}
		for poolID := range poolIDs {
			ambiguous[poolID] = true
		}
	}
	counts := make(map[string]int64, len(poolEndpoints))
	for poolID := range poolEndpoints {
		if !ambiguous[poolID] {
			counts[poolID] = 0
		}
	}
	if len(counts) == 0 {
		return nil
	}

	sockets, err := readEstablishedTCPSockets(counter.procRoot)
	if err != nil {
		return nil
	}
	ownedInodes, err := readNginxSocketInodes(counter.pidPath, counter.procRoot)
	if err != nil {
		return nil
	}
	for inode, endpoint := range sockets {
		if _, owned := ownedInodes[inode]; !owned {
			continue
		}
		poolIDs := owners[endpoint]
		if len(poolIDs) != 1 {
			continue
		}
		for poolID := range poolIDs {
			if _, available := counts[poolID]; available {
				counts[poolID]++
			}
		}
	}
	return counts
}

func (counter *procNginxOriginConnectionCounter) resolveHost(ctx context.Context, host string) ([]netip.Addr, bool) {
	if address, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{address.WithZone("").Unmap()}, true
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || counter.resolver == nil {
		return nil, false
	}
	now := time.Now()
	if counter.now != nil {
		now = counter.now()
	}
	counter.resolutionMu.Lock()
	if counter.resolutions == nil {
		counter.resolutions = make(map[string]cachedOriginAddresses)
	}
	entry, found := counter.resolutions[host]
	if found && now.Before(entry.expiresAt) {
		addresses := append([]netip.Addr(nil), entry.addresses...)
		counter.resolutionMu.Unlock()
		return addresses, len(addresses) > 0
	}
	counter.resolutionMu.Unlock()

	resolved, err := counter.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, false
	}
	seen := make(map[netip.Addr]struct{}, len(resolved))
	addresses := make([]netip.Addr, 0, len(resolved))
	for _, item := range resolved {
		address, ok := netip.AddrFromSlice(item.IP)
		if !ok {
			continue
		}
		address = address.WithZone("").Unmap()
		if _, duplicate := seen[address]; duplicate {
			continue
		}
		seen[address] = struct{}{}
		addresses = append(addresses, address)
	}
	if len(addresses) == 0 {
		return nil, false
	}
	counter.resolutionMu.Lock()
	if len(counter.resolutions) >= maximumOriginResolutionCache {
		for key, cached := range counter.resolutions {
			if !now.Before(cached.expiresAt) {
				delete(counter.resolutions, key)
			}
		}
		if len(counter.resolutions) >= maximumOriginResolutionCache {
			for key := range counter.resolutions {
				delete(counter.resolutions, key)
				break
			}
		}
	}
	counter.resolutions[host] = cachedOriginAddresses{
		addresses: append([]netip.Addr(nil), addresses...), expiresAt: now.Add(originAddressResolutionTTL),
	}
	counter.resolutionMu.Unlock()
	return addresses, true
}

func readEstablishedTCPSockets(procRoot string) (map[uint64]originTCPEndpoint, error) {
	result := make(map[uint64]originTCPEndpoint)
	readAny := false
	for _, source := range []struct {
		name string
		ipv6 bool
	}{{name: "tcp"}, {name: "tcp6", ipv6: true}} {
		contents, err := os.ReadFile(filepath.Join(procRoot, "net", source.name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		readAny = true
		parsed, err := parseProcNetTCP(contents, source.ipv6)
		if err != nil {
			return nil, err
		}
		for inode, endpoint := range parsed {
			result[inode] = endpoint
		}
	}
	if !readAny {
		return nil, errors.New("TCP socket tables are unavailable")
	}
	return result, nil
}

func parseProcNetTCP(contents []byte, ipv6 bool) (map[uint64]originTCPEndpoint, error) {
	result := make(map[uint64]originTCPEndpoint)
	scanner := bufio.NewScanner(bytes.NewReader(contents))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 || fields[3] != "01" {
			continue
		}
		endpoint, err := parseProcTCPEndpoint(fields[2], ipv6)
		if err != nil {
			continue
		}
		inode, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil || inode == 0 {
			continue
		}
		result[inode] = endpoint
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func parseProcTCPEndpoint(value string, ipv6 bool) (originTCPEndpoint, error) {
	addressText, portText, found := strings.Cut(value, ":")
	if !found {
		return originTCPEndpoint{}, errors.New("missing TCP endpoint port")
	}
	port, err := strconv.ParseUint(portText, 16, 16)
	if err != nil || port == 0 {
		return originTCPEndpoint{}, errors.New("invalid TCP endpoint port")
	}
	raw, err := hex.DecodeString(addressText)
	if err != nil {
		return originTCPEndpoint{}, err
	}
	if ipv6 {
		if len(raw) != 16 {
			return originTCPEndpoint{}, errors.New("invalid IPv6 socket address")
		}
		for offset := 0; offset < len(raw); offset += 4 {
			reverseBytes(raw[offset : offset+4])
		}
		var bytes [16]byte
		copy(bytes[:], raw)
		return originTCPEndpoint{address: netip.AddrFrom16(bytes).Unmap(), port: uint16(port)}, nil
	}
	if len(raw) != 4 {
		return originTCPEndpoint{}, errors.New("invalid IPv4 socket address")
	}
	reverseBytes(raw)
	var bytes [4]byte
	copy(bytes[:], raw)
	return originTCPEndpoint{address: netip.AddrFrom4(bytes), port: uint16(port)}, nil
}

func reverseBytes(value []byte) {
	for left, right := 0, len(value)-1; left < right; left, right = left+1, right-1 {
		value[left], value[right] = value[right], value[left]
	}
}

func readNginxSocketInodes(pidPath, procRoot string) (map[uint64]struct{}, error) {
	contents, err := os.ReadFile(pidPath)
	if err != nil {
		return nil, err
	}
	masterPID, err := strconv.Atoi(strings.TrimSpace(string(contents)))
	if err != nil || masterPID <= 0 {
		return nil, errors.New("invalid Nginx master PID")
	}
	masterRoot := filepath.Join(procRoot, strconv.Itoa(masterPID))
	command, err := os.ReadFile(filepath.Join(masterRoot, "comm"))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(command)) != "nginx" {
		return nil, errors.New("Nginx master PID belongs to another process")
	}

	inodes := make(map[uint64]struct{})
	queue := []int{masterPID}
	seen := make(map[int]bool)
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		processRoot := filepath.Join(procRoot, strconv.Itoa(pid))
		entries, readErr := os.ReadDir(filepath.Join(processRoot, "fd"))
		if readErr != nil && pid == masterPID {
			return nil, readErr
		}
		if readErr == nil {
			for _, entry := range entries {
				target, linkErr := os.Readlink(filepath.Join(processRoot, "fd", entry.Name()))
				if linkErr != nil {
					continue
				}
				if inode, ok := parseSocketInode(target); ok {
					inodes[inode] = struct{}{}
				}
			}
		}
		childrenPath := filepath.Join(processRoot, "task", strconv.Itoa(pid), "children")
		children, childrenErr := os.ReadFile(childrenPath)
		if childrenErr != nil {
			if pid == masterPID {
				return nil, childrenErr
			}
			continue
		}
		for _, value := range strings.Fields(string(children)) {
			childPID, parseErr := strconv.Atoi(value)
			if parseErr == nil && childPID > 0 {
				queue = append(queue, childPID)
			}
		}
	}
	return inodes, nil
}

func parseSocketInode(target string) (uint64, bool) {
	if !strings.HasPrefix(target, "socket:[") || !strings.HasSuffix(target, "]") {
		return 0, false
	}
	value := strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")
	inode, err := strconv.ParseUint(value, 10, 64)
	return inode, err == nil && inode > 0
}

func (a *Agent) attachOriginConnectionCounts(ctx context.Context, statuses []domain.OriginProbeStatus) {
	if a.originConnections == nil || len(statuses) == 0 {
		return
	}
	wanted := make(map[string]struct{}, len(statuses))
	for _, status := range statuses {
		wanted[status.PoolID] = struct{}{}
	}
	a.originMu.RLock()
	pools := make([]domain.OriginPool, 0, len(wanted))
	for poolID := range wanted {
		if runtime := a.originPools[poolID]; runtime != nil {
			pools = append(pools, cloneOriginPool(runtime.Pool))
		}
	}
	a.originMu.RUnlock()
	counts := a.originConnections.Count(ctx, pools)
	for index := range statuses {
		count, available := counts[statuses[index].PoolID]
		if !available {
			continue
		}
		copyOfCount := count
		statuses[index].EstablishedConnections = &copyOfCount
	}
}
