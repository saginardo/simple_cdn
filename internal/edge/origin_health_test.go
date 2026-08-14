package edge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"simple_cdn/internal/domain"
)

type originHealthRoundTripFunc func(*http.Request) (*http.Response, error)

func (function originHealthRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestOriginCircuitSeparatesLayersAndRequiresBothToRecover(t *testing.T) {
	agent, runner, pool := newOriginTestAgent(t)
	if err := agent.apply(originTestDesiredState(1, pool)); err != nil {
		t.Fatal(err)
	}
	if runner.applies != 1 {
		t.Fatalf("initial apply count = %d", runner.applies)
	}
	failure := errors.New("dial tcp: connection refused")
	if err := agent.applyOriginProbeResult(pool, domain.OriginProbeService, OriginProbeMeasurement{TotalDuration: time.Millisecond}, failure); err != nil {
		t.Fatal(err)
	}
	assertOriginCircuit(t, agent, pool.ID, domain.OriginCircuitClosed, 1, 0, 0, 0)

	coldSuccess := OriginProbeMeasurement{ConnectDuration: time.Millisecond, TotalDuration: 2 * time.Millisecond}
	if err := agent.applyOriginProbeResult(pool, domain.OriginProbeCold, coldSuccess, nil); err != nil {
		t.Fatal(err)
	}
	assertOriginCircuit(t, agent, pool.ID, domain.OriginCircuitClosed, 1, 0, 0, 1)
	for failureCount := 2; failureCount <= agent.Config.OriginProbeFailureThreshold; failureCount++ {
		if err := agent.applyOriginProbeResult(pool, domain.OriginProbeService, OriginProbeMeasurement{}, failure); err != nil {
			t.Fatal(err)
		}
		if failureCount < agent.Config.OriginProbeFailureThreshold {
			assertOriginCircuit(t, agent, pool.ID, domain.OriginCircuitClosed, failureCount, 0, 0, 1)
		}
	}
	assertOriginCircuit(t, agent, pool.ID, domain.OriginCircuitOpen, 5, 0, 0, 0)
	assertFileContains(t, pool.ConfigPath, "server "+pool.Address+" down;")
	if runner.applies != 2 {
		t.Fatalf("opening circuit apply count = %d, want 2", runner.applies)
	}

	serviceSuccess := OriginProbeMeasurement{
		ConnectionReused: true, HeaderDuration: 3 * time.Millisecond,
		TotalDuration: 4 * time.Millisecond, HTTPStatus: http.StatusNoContent,
	}
	if err := agent.applyOriginProbeResult(pool, domain.OriginProbeService, serviceSuccess, nil); err != nil {
		t.Fatal(err)
	}
	assertOriginCircuit(t, agent, pool.ID, domain.OriginCircuitOpen, 0, 1, 0, 0)
	if err := agent.applyOriginProbeResult(pool, domain.OriginProbeCold, coldSuccess, nil); err != nil {
		t.Fatal(err)
	}
	assertOriginCircuit(t, agent, pool.ID, domain.OriginCircuitRecovering, 0, 1, 0, 1)
	assertFileContains(t, pool.ConfigPath, "server "+pool.Address+" down;")
	if runner.applies != 2 {
		t.Fatalf("recovering probe unexpectedly reloaded Nginx %d times", runner.applies)
	}
	if err := agent.applyOriginProbeResult(pool, domain.OriginProbeService, serviceSuccess, nil); err != nil {
		t.Fatal(err)
	}
	assertOriginCircuit(t, agent, pool.ID, domain.OriginCircuitRecovering, 0, 2, 0, 1)
	if err := agent.applyOriginProbeResult(pool, domain.OriginProbeCold, coldSuccess, nil); err != nil {
		t.Fatal(err)
	}
	assertOriginCircuit(t, agent, pool.ID, domain.OriginCircuitClosed, 0, 2, 0, 2)
	assertFileContains(t, pool.ConfigPath, "max_fails=1 fail_timeout=5s;")
	if runner.applies != 3 {
		t.Fatalf("closing circuit apply count = %d, want 3", runner.applies)
	}
	statuses := agent.originProbeStatuses()
	if len(statuses) != 1 || !statuses[0].Healthy || statuses[0].ServiceProbe == nil || statuses[0].ColdProbe == nil ||
		statuses[0].ServiceProbe.HTTPStatus != http.StatusNoContent || !statuses[0].ServiceProbe.ConnectionReused ||
		statuses[0].ColdProbe.ConnectMS <= 0 {
		t.Fatalf("origin statuses = %#v", statuses)
	}
}

func TestOriginCircuitTransitionRollsBackWhenNginxReloadFails(t *testing.T) {
	agent, runner, pool := newOriginTestAgent(t)
	if err := agent.apply(originTestDesiredState(1, pool)); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("origin unavailable")
	for range agent.Config.OriginProbeFailureThreshold - 1 {
		if err := agent.applyOriginProbeResult(pool, domain.OriginProbeService, OriginProbeMeasurement{}, failure); err != nil {
			t.Fatal(err)
		}
	}
	runner.applyErr = errors.New("reload failed")
	if err := agent.applyOriginProbeResult(pool, domain.OriginProbeService, OriginProbeMeasurement{}, failure); err == nil {
		t.Fatal("expected circuit reload failure")
	}
	assertOriginCircuit(t, agent, pool.ID, domain.OriginCircuitClosed, 4, 0, 0, 0)
	assertFileContains(t, pool.ConfigPath, "max_fails=1 fail_timeout=5s;")

	restarted, err := New(originTestConfig(agent.Config.StateDir, agent.Config.NginxConfigPath, &fakeRunner{}))
	if err != nil {
		t.Fatal(err)
	}
	assertOriginCircuit(t, restarted, pool.ID, domain.OriginCircuitClosed, 4, 0, 0, 0)
}

func TestOriginRuntimeRestoresOpenCircuitWithoutPublishingStaleTelemetry(t *testing.T) {
	agent, _, pool := newOriginTestAgent(t)
	if err := agent.apply(originTestDesiredState(1, pool)); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("timeout")
	for range agent.Config.OriginProbeFailureThreshold {
		if err := agent.applyOriginProbeResult(pool, domain.OriginProbeCold, OriginProbeMeasurement{}, failure); err != nil {
			t.Fatal(err)
		}
	}

	restarted, err := New(originTestConfig(agent.Config.StateDir, agent.Config.NginxConfigPath, &fakeRunner{}))
	if err != nil {
		t.Fatal(err)
	}
	assertOriginCircuit(t, restarted, pool.ID, domain.OriginCircuitOpen, 0, 0, 5, 0)
	if statuses := restarted.originProbeStatuses(); len(statuses) != 0 {
		t.Fatalf("restart exposed stale probe data: %#v", statuses)
	}
	if err := restarted.apply(originTestDesiredState(2, pool)); err != nil {
		t.Fatal(err)
	}
	assertFileContains(t, pool.ConfigPath, "server "+pool.Address+" down;")
}

func TestOriginRuntimeMigratesLegacyColdProbeStreaks(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "nginx.conf")
	config := originTestConfig(directory, configPath, &fakeRunner{})
	pool := domain.OriginPool{
		ID: strings.Repeat("b", 24), Address: "203.0.113.11:443", Scheme: "https",
		HostHeader: "origin.example.test", TLSServerName: "origin.example.test",
		ConfigPath:           filepath.Join(directory, "origin-pools", strings.Repeat("b", 24)+".conf"),
		KeepaliveConnections: 16, References: []domain.OriginPoolReference{{SiteID: "site-1", Role: "primary"}},
	}
	legacy := persistedOriginRuntime{Version: originRuntimeLegacyVersion, Pools: []persistedOriginPool{{
		Pool: pool, CircuitState: domain.OriginCircuitOpen, ConsecutiveFailures: 2,
	}}}
	contents, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, originRuntimeStateName), contents, 0o640); err != nil {
		t.Fatal(err)
	}
	agent, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	assertOriginCircuit(t, agent, pool.ID, domain.OriginCircuitOpen, 0, 0, 2, 0)
}

func TestApplyRejectsUnmanagedOriginPoolPathAndRollsBackInclude(t *testing.T) {
	agent, runner, pool := newOriginTestAgent(t)
	invalid := pool
	invalid.ConfigPath = filepath.Join(agent.Config.StateDir, "outside.conf")
	if err := agent.apply(originTestDesiredState(1, invalid)); err == nil {
		t.Fatal("expected unmanaged pool path to be rejected")
	}
	if runner.tests != 0 || runner.applies != 0 {
		t.Fatalf("invalid desired state reached Nginx: tests=%d applies=%d", runner.tests, runner.applies)
	}

	runner.testErr = errors.New("invalid Nginx configuration")
	if err := agent.apply(originTestDesiredState(2, pool)); err == nil {
		t.Fatal("expected Nginx validation failure")
	}
	if _, err := os.Stat(pool.ConfigPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed apply retained origin include: %v", err)
	}
	if _, err := os.Stat(agent.originRuntimePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed apply retained origin runtime state: %v", err)
	}
}

func TestNetworkOriginProberReusesServiceHTTPAndIsolatesColdConnections(t *testing.T) {
	var accepted atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodHead || request.Host != "health.example.test" {
			t.Errorf("probe request = %s host %s", request.Method, request.Host)
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			accepted.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	pool := domain.OriginPool{
		ID: strings.Repeat("c", 24), Address: parsed.Host, Scheme: "http", HostHeader: "health.example.test",
	}
	prober := newNetworkOriginProber()
	defer prober.Close()
	first, err := prober.Probe(context.Background(), pool, domain.OriginProbeService)
	if err != nil {
		t.Fatal(err)
	}
	second, err := prober.Probe(context.Background(), pool, domain.OriginProbeService)
	if err != nil {
		t.Fatal(err)
	}
	if first.ConnectionReused || !second.ConnectionReused || accepted.Load() != 1 {
		t.Fatalf("service probes: first=%#v second=%#v accepted=%d", first, second, accepted.Load())
	}
	for range 2 {
		measurement, err := prober.Probe(context.Background(), pool, domain.OriginProbeCold)
		if err != nil {
			t.Fatal(err)
		}
		if measurement.ConnectionReused {
			t.Fatalf("cold probe reused a connection: %#v", measurement)
		}
	}
	if accepted.Load() != 3 {
		t.Fatalf("accepted connections = %d, want 3", accepted.Load())
	}
	if second.HTTPStatus != http.StatusNoContent || second.HeaderDuration <= 0 || second.TotalDuration <= 0 {
		t.Fatalf("service probe measurement = %#v", second)
	}
	prober.Reconcile(nil)
	if len(prober.httpClients) != 0 {
		t.Fatalf("stale HTTP probe clients = %d", len(prober.httpClients))
	}
}

func TestProbeOriginHTTPUsesConfiguredHealthRequest(t *testing.T) {
	client := &http.Client{Transport: originHealthRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Path != "/health" || request.Host != "health.example.test" {
			t.Errorf("probe request = %s %s host %s", request.Method, request.URL.Path, request.Host)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Proto:      "HTTP/1.1",
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"status":"ok"}`)),
		}, nil
	})}
	pool := domain.OriginPool{
		ID: strings.Repeat("f", 24), Address: "203.0.113.10:80", Scheme: "http", HostHeader: "health.example.test",
		HealthCheckMethod: domain.OriginHealthCheckMethodGET, HealthCheckPath: "/health",
	}
	measurement, err := probeOriginHTTP(context.Background(), pool, client)
	if err != nil {
		t.Fatal(err)
	}
	if measurement.HTTPStatus != http.StatusOK {
		t.Fatalf("health status = %d", measurement.HTTPStatus)
	}
}

func TestNetworkOriginProberUsesH2CAndReusesTheSession(t *testing.T) {
	var accepted atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.ProtoMajor != 2 {
			t.Errorf("probe protocol = %s", request.Proto)
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	server.Config.Protocols = protocols
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			accepted.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	pool := domain.OriginPool{
		ID: strings.Repeat("2", 24), Address: parsed.Host, Scheme: "http", HTTPVersion: domain.OriginHTTPVersionH2C,
		HostHeader: "health.example.test",
	}
	prober := newNetworkOriginProber()
	defer prober.Close()
	first, err := prober.Probe(t.Context(), pool, domain.OriginProbeService)
	if err != nil {
		t.Fatal(err)
	}
	second, err := prober.Probe(t.Context(), pool, domain.OriginProbeService)
	if err != nil {
		t.Fatal(err)
	}
	if first.ConnectionReused || !second.ConnectionReused || accepted.Load() != 1 {
		t.Fatalf("H2C probes: first=%#v second=%#v accepted=%d", first, second, accepted.Load())
	}
	cold, err := prober.Probe(t.Context(), pool, domain.OriginProbeCold)
	if err != nil {
		t.Fatal(err)
	}
	if cold.ConnectionReused || accepted.Load() != 2 {
		t.Fatalf("H2C cold probe = %#v, accepted=%d", cold, accepted.Load())
	}
}

func TestNetworkOriginProberUsesReusableGRPCHealthClient(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	counted := &countingListener{Listener: listener}
	server := grpc.NewServer()
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(server, healthServer)
	go func() { _ = server.Serve(counted) }()
	defer server.Stop()

	pool := domain.OriginPool{
		ID: strings.Repeat("d", 24), Address: counted.Addr().String(), Scheme: "grpc", HostHeader: "health.example.test",
	}
	prober := newNetworkOriginProber()
	defer prober.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	first, err := prober.Probe(ctx, pool, domain.OriginProbeService)
	if err != nil {
		t.Fatal(err)
	}
	second, err := prober.Probe(ctx, pool, domain.OriginProbeService)
	if err != nil {
		t.Fatal(err)
	}
	if first.ConnectionReused || !second.ConnectionReused || counted.accepted.Load() != 1 {
		t.Fatalf("gRPC probes: first=%#v second=%#v accepted=%d", first, second, counted.accepted.Load())
	}
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	if _, err := prober.Probe(ctx, pool, domain.OriginProbeService); err == nil {
		t.Fatal("NOT_SERVING gRPC health response was accepted")
	}
}

func TestNetworkOriginProberAcceptsGRPCWithoutHealthService(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	prober := newNetworkOriginProber()
	defer prober.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = prober.Probe(ctx, domain.OriginPool{
		ID: strings.Repeat("e", 24), Address: listener.Addr().String(), Scheme: "grpc", HostHeader: "health.example.test",
	}, domain.OriginProbeService)
	if err != nil {
		t.Fatalf("gRPC transport without optional health service failed: %v", err)
	}
}

func TestOriginProbeDefaultsAndJitterBounds(t *testing.T) {
	agent, _, pool := newOriginTestAgent(t)
	if agent.Config.OriginProbeInterval != 5*time.Second || agent.Config.OriginColdProbeInterval != 40*time.Second ||
		agent.Config.OriginDegradedProbeInterval != 5*time.Second || agent.Config.OriginDegradedColdInterval != 8*time.Second ||
		agent.Config.OriginProbeTimeout != 3*time.Second || agent.Config.OriginProbeFailureThreshold != 5 {
		t.Fatalf("origin probe defaults = %#v", agent.Config)
	}
	for _, test := range []struct {
		kind     domain.OriginProbeKind
		degraded bool
		base     time.Duration
	}{
		{domain.OriginProbeService, false, 5 * time.Second},
		{domain.OriginProbeCold, false, 40 * time.Second},
		{domain.OriginProbeService, true, 5 * time.Second},
		{domain.OriginProbeCold, true, 8 * time.Second},
	} {
		delay := agent.originProbeDelay(pool.ID, test.kind, test.degraded)
		if delay < test.base*4/5 || delay > test.base*6/5 {
			t.Fatalf("%s degraded=%t delay = %s", test.kind, test.degraded, delay)
		}
	}
}

func TestOriginProbeSchedulerRunsServiceLayerMoreFrequently(t *testing.T) {
	agent, _, pool := newOriginTestAgent(t)
	recorder := &recordingOriginProber{calls: make(chan domain.OriginProbeKind, 128)}
	agent.Config.OriginProber = recorder
	agent.Config.OriginProbeInterval = 20 * time.Millisecond
	agent.Config.OriginColdProbeInterval = 200 * time.Millisecond
	agent.Config.OriginDegradedProbeInterval = 10 * time.Millisecond
	agent.Config.OriginDegradedColdInterval = 50 * time.Millisecond
	agent.Config.OriginProbeTimeout = 100 * time.Millisecond
	if err := agent.apply(originTestDesiredState(1, pool)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		agent.runOriginProbeLoop(ctx)
		close(done)
	}()
	serviceCalls := 0
	coldCalls := 0
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for serviceCalls < 6 || coldCalls < 1 {
		select {
		case kind := <-recorder.calls:
			if kind == domain.OriginProbeCold {
				coldCalls++
			} else {
				serviceCalls++
			}
		case <-deadline.C:
			cancel()
			<-done
			t.Fatalf("probe calls before deadline: service=%d cold=%d", serviceCalls, coldCalls)
		}
	}
	cancel()
	<-done
	if serviceCalls < 2*coldCalls {
		t.Fatalf("layer cadence collapsed: service=%d cold=%d", serviceCalls, coldCalls)
	}
}

func TestOriginProbeLayerFailureIsIncludedInHeartbeatError(t *testing.T) {
	agent, _, _ := newOriginTestAgent(t)
	agent.setComponentFailure("origin_health_cold", "update cold origin circuit", errors.New("reload failed"))
	if detail := agent.heartbeatError(); !strings.Contains(detail, "cold origin circuit") || !strings.Contains(detail, "reload failed") {
		t.Fatalf("heartbeat error = %q", detail)
	}
}

type recordingOriginProber struct {
	calls chan domain.OriginProbeKind
}

func (p *recordingOriginProber) Probe(ctx context.Context, _ domain.OriginPool, kind domain.OriginProbeKind) (OriginProbeMeasurement, error) {
	select {
	case p.calls <- kind:
		return OriginProbeMeasurement{}, nil
	case <-ctx.Done():
		return OriginProbeMeasurement{}, ctx.Err()
	}
}

type countingListener struct {
	net.Listener
	accepted atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	connection, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return connection, err
}

func newOriginTestAgent(t *testing.T) (*Agent, *fakeRunner, domain.OriginPool) {
	t.Helper()
	directory := t.TempDir()
	runner := &fakeRunner{}
	configPath := filepath.Join(directory, "nginx.conf")
	agent, err := New(originTestConfig(directory, configPath, runner))
	if err != nil {
		t.Fatal(err)
	}
	pool := domain.OriginPool{
		ID: strings.Repeat("a", 24), Address: "203.0.113.10:8080", Scheme: "http", HostHeader: "origin.example.test",
		ConfigPath:           filepath.Join(agent.Config.OriginPoolConfigDirectory, strings.Repeat("a", 24)+".conf"),
		KeepaliveConnections: 16, References: []domain.OriginPoolReference{{SiteID: "site-1", Role: "primary"}},
	}
	return agent, runner, pool
}

func originTestConfig(stateDirectory, configPath string, runner Runner) Config {
	return Config{
		ControlURL: "https://control.example.test", StateDir: stateDirectory, NginxConfigPath: configPath,
		CertificateDir: filepath.Join(stateDirectory, "certs"), Runner: runner,
		AgentSHA256: strings.Repeat("f", 64), OriginProbeFailureThreshold: 5, OriginProbeRecoveryThreshold: 2,
	}
}

func originTestDesiredState(version int64, pool domain.OriginPool) domain.DesiredState {
	return domain.DesiredState{
		Version:     version,
		PublicPorts: []int{},
		NginxConfig: "# generated\nupstream origin_pool_" + pool.ID + " {\n    include " + pool.ConfigPath + ";\n    keepalive 16;\n}\n",
		OriginPools: []domain.OriginPool{pool},
	}
}

func assertOriginCircuit(t *testing.T, agent *Agent, poolID string, state domain.OriginCircuitState, serviceFailures, serviceSuccesses, coldFailures, coldSuccesses int) {
	t.Helper()
	agent.originMu.RLock()
	defer agent.originMu.RUnlock()
	runtime := agent.originPools[poolID]
	if runtime == nil || runtime.CircuitState != state ||
		runtime.ServiceConsecutiveFailures != serviceFailures || runtime.ServiceConsecutiveSuccesses != serviceSuccesses ||
		runtime.ColdConsecutiveFailures != coldFailures || runtime.ColdConsecutiveSuccesses != coldSuccesses {
		t.Fatalf("origin runtime = %#v, want state=%s service=%d/%d cold=%d/%d", runtime, state, serviceFailures, serviceSuccesses, coldFailures, coldSuccesses)
	}
}

func assertFileContains(t *testing.T, path, expected string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), expected) {
		t.Fatalf("%s does not contain %q: %s", path, expected, contents)
	}
}
