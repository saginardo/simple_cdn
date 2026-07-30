package edge

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"simple_cdn/internal/domain"
)

type originIPResolverFunc func(context.Context, string) ([]net.IPAddr, error)

func (function originIPResolverFunc) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return function(ctx, host)
}

func TestParseProcNetTCPReadsEstablishedIPv4AndIPv6Sockets(t *testing.T) {
	ipv4, err := parseProcNetTCP([]byte(`  sl  local_address rem_address   st tx_queue:rx_queue tr:tm->when retrnsmt   uid  timeout inode
   0: 0200000A:C350 0100FD0A:20FB 01 00000000:00000000 00:00000000 00000000    33        0 1234 1
   1: 00000000:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000    33        0 5678 1
`), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ipv4) != 1 || ipv4[1234] != (originTCPEndpoint{address: netip.MustParseAddr("10.253.0.1"), port: 8443}) {
		t.Fatalf("parsed IPv4 sockets = %#v", ipv4)
	}

	ipv6, err := parseProcNetTCP([]byte(`  sl  local_address                         rem_address                          st tx_queue:rx_queue tr:tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:C351 B80D0120000000000000000001000000:01BB 01 00000000:00000000 00:00000000 00000000    33        0 4321 1
`), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(ipv6) != 1 || ipv6[4321] != (originTCPEndpoint{address: netip.MustParseAddr("2001:db8::1"), port: 443}) {
		t.Fatalf("parsed IPv6 sockets = %#v", ipv6)
	}
}

func TestProcNginxOriginConnectionCounterCountsOnlyOwnedSockets(t *testing.T) {
	root := t.TempDir()
	procRoot := filepath.Join(root, "proc")
	pidPath := filepath.Join(root, "nginx.pid")
	mustWriteTestFile(t, pidPath, "100\n")
	mustWriteTestFile(t, filepath.Join(procRoot, "100", "comm"), "nginx\n")
	mustWriteTestFile(t, filepath.Join(procRoot, "100", "task", "100", "children"), "101\n")
	mustWriteTestFile(t, filepath.Join(procRoot, "101", "task", "101", "children"), "")
	if err := os.MkdirAll(filepath.Join(procRoot, "100", "fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(procRoot, "101", "fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("socket:[1234]", filepath.Join(procRoot, "101", "fd", "7")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("socket:[9000]", filepath.Join(procRoot, "100", "fd", "8")); err != nil {
		t.Fatal(err)
	}
	mustWriteTestFile(t, filepath.Join(procRoot, "net", "tcp"), `  sl  local_address rem_address   st tx_queue:rx_queue tr:tm->when retrnsmt   uid  timeout inode
   0: 0200000A:C350 0100FD0A:20FB 01 00000000:00000000 00:00000000 00000000    33        0 1234 1
   1: 0300000A:C351 0100FD0A:20FB 01 00000000:00000000 00:00000000 00000000    33        0 5678 1
   2: 0400000A:C352 0200FD0A:20FB 06 00000000:00000000 00:00000000 00000000    33        0 9000 1
`)

	counter := &procNginxOriginConnectionCounter{
		pidPath: pidPath, procRoot: procRoot, resolver: net.DefaultResolver, now: time.Now,
		resolutions: make(map[string]cachedOriginAddresses),
	}
	pools := []domain.OriginPool{
		{ID: "pool-a", Address: "10.253.0.1:8443"},
		{ID: "pool-b", Address: "10.253.0.2:8443"},
	}
	counts := counter.Count(t.Context(), pools)
	if counts["pool-a"] != 1 || counts["pool-b"] != 0 || len(counts) != 2 {
		t.Fatalf("origin connection counts = %#v", counts)
	}

	duplicateEndpoint := domain.OriginPool{ID: "pool-c", Address: "10.253.0.1:8443"}
	counts = counter.Count(t.Context(), append(pools, duplicateEndpoint))
	if _, found := counts["pool-a"]; found {
		t.Fatalf("ambiguous pool-a count was reported: %#v", counts)
	}
	if _, found := counts["pool-c"]; found {
		t.Fatalf("ambiguous pool-c count was reported: %#v", counts)
	}
	if value, found := counts["pool-b"]; !found || value != 0 {
		t.Fatalf("unambiguous zero count was lost: %#v", counts)
	}
}

func TestProcNginxOriginConnectionCounterResolvesHostnames(t *testing.T) {
	calls := 0
	counter := &procNginxOriginConnectionCounter{
		resolver: originIPResolverFunc(func(_ context.Context, host string) ([]net.IPAddr, error) {
			calls++
			if host != "origin.example.test" {
				t.Fatalf("resolved host = %q", host)
			}
			return []net.IPAddr{{IP: net.ParseIP("10.253.0.1")}}, nil
		}),
		now: time.Now, resolutions: make(map[string]cachedOriginAddresses),
	}
	first, ok := counter.resolveHost(t.Context(), "origin.example.test")
	if !ok || len(first) != 1 || first[0] != netip.MustParseAddr("10.253.0.1") {
		t.Fatalf("resolved addresses = %#v, ok = %t", first, ok)
	}
	second, ok := counter.resolveHost(t.Context(), "ORIGIN.EXAMPLE.TEST")
	if !ok || len(second) != 1 || calls != 1 {
		t.Fatalf("cached addresses = %#v, calls = %d", second, calls)
	}
}

func TestAttachOriginConnectionCountsPreservesUnavailablePools(t *testing.T) {
	agent := &Agent{
		originPools: map[string]*originPoolRuntime{
			"pool-a": {Pool: domain.OriginPool{ID: "pool-a", Address: "10.253.0.1:8443"}},
			"pool-b": {Pool: domain.OriginPool{ID: "pool-b", Address: "10.253.0.2:8443"}},
		},
		originConnections: originConnectionCounterFunc(func(_ context.Context, pools []domain.OriginPool) map[string]int64 {
			if len(pools) != 2 {
				t.Fatalf("pools = %#v", pools)
			}
			return map[string]int64{"pool-a": 4}
		}),
	}
	statuses := []domain.OriginProbeStatus{{PoolID: "pool-a"}, {PoolID: "pool-b"}}
	agent.attachOriginConnectionCounts(t.Context(), statuses)
	if statuses[0].EstablishedConnections == nil || *statuses[0].EstablishedConnections != 4 {
		t.Fatalf("available count = %#v", statuses[0].EstablishedConnections)
	}
	if statuses[1].EstablishedConnections != nil {
		t.Fatalf("unavailable count = %#v", statuses[1].EstablishedConnections)
	}
}

func mustWriteTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
