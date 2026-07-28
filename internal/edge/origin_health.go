package edge

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	grpcstatus "google.golang.org/grpc/status"
	"simple_cdn/internal/domain"
)

const (
	originRuntimeStateVersion  = 2
	originRuntimeLegacyVersion = 1
	originRuntimeStateName     = "origin-connections.json"
	defaultOriginProbeAgent    = "simple-cdn-origin-health/1"
)

type OriginProbeMeasurement struct {
	ConnectionReused     bool
	ConnectDuration      time.Duration
	TLSHandshakeDuration time.Duration
	HeaderDuration       time.Duration
	TotalDuration        time.Duration
	HTTPStatus           int
}

type OriginProber interface {
	Probe(context.Context, domain.OriginPool, domain.OriginProbeKind) (OriginProbeMeasurement, error)
}

type originProberLifecycle interface {
	Reconcile([]domain.OriginPool)
	Close() error
}

type originHTTPProbeClient struct {
	pool      domain.OriginPool
	transport *http.Transport
	client    *http.Client
}

type originGRPCProbeClient struct {
	pool   domain.OriginPool
	conn   *grpc.ClientConn
	health healthpb.HealthClient
}

type networkOriginProber struct {
	mu          sync.Mutex
	httpClients map[string]*originHTTPProbeClient
	grpcClients map[string]*originGRPCProbeClient
}

func newNetworkOriginProber() *networkOriginProber {
	return &networkOriginProber{
		httpClients: make(map[string]*originHTTPProbeClient),
		grpcClients: make(map[string]*originGRPCProbeClient),
	}
}

func (p *networkOriginProber) Probe(ctx context.Context, pool domain.OriginPool, kind domain.OriginProbeKind) (OriginProbeMeasurement, error) {
	switch kind {
	case domain.OriginProbeService:
		if pool.Scheme == "grpc" || pool.Scheme == "grpcs" {
			client, err := p.reusableGRPCClient(pool)
			if err != nil {
				return OriginProbeMeasurement{}, err
			}
			return probeOriginGRPC(ctx, client)
		}
		return probeOriginHTTP(ctx, pool, p.reusableHTTPClient(pool).client)
	case domain.OriginProbeCold:
		if pool.Scheme == "grpc" || pool.Scheme == "grpcs" {
			return probeOriginTransport(ctx, pool)
		}
		client := newOriginHTTPProbeClient(pool, true)
		defer client.transport.CloseIdleConnections()
		return probeOriginHTTP(ctx, pool, client.client)
	default:
		return OriginProbeMeasurement{}, fmt.Errorf("unsupported origin probe kind %q", kind)
	}
}

func (p *networkOriginProber) reusableHTTPClient(pool domain.OriginPool) *originHTTPProbeClient {
	p.mu.Lock()
	defer p.mu.Unlock()
	if client := p.httpClients[pool.ID]; client != nil && sameOriginTransport(client.pool, pool) {
		return client
	} else if client != nil {
		client.transport.CloseIdleConnections()
		delete(p.httpClients, pool.ID)
	}
	client := newOriginHTTPProbeClient(pool, false)
	p.httpClients[pool.ID] = client
	return client
}

func newOriginHTTPProbeClient(pool domain.OriginPool, disableKeepAlives bool) *originHTTPProbeClient {
	protocols := new(http.Protocols)
	switch effectivePoolHTTPVersion(pool) {
	case domain.OriginHTTPVersionHTTP2:
		protocols.SetHTTP2(true)
	case domain.OriginHTTPVersionH2C:
		protocols.SetUnencryptedHTTP2(true)
	default:
		protocols.SetHTTP1(true)
	}
	transport := &http.Transport{
		DisableKeepAlives:   disableKeepAlives,
		ForceAttemptHTTP2:   effectivePoolHTTPVersion(pool) == domain.OriginHTTPVersionHTTP2,
		Protocols:           protocols,
		MaxIdleConns:        1,
		MaxIdleConnsPerHost: 1,
		MaxConnsPerHost:     1,
		IdleConnTimeout:     90 * time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: pool.TLSServerName,
		},
	}
	return &originHTTPProbeClient{
		pool:      cloneOriginPool(pool),
		transport: transport,
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (p *networkOriginProber) reusableGRPCClient(pool domain.OriginPool) (*originGRPCProbeClient, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if client := p.grpcClients[pool.ID]; client != nil && sameOriginTransport(client.pool, pool) {
		return client, nil
	} else if client != nil {
		_ = client.conn.Close()
		delete(p.grpcClients, pool.ID)
	}
	transportCredentials := insecure.NewCredentials()
	if pool.Scheme == "grpcs" {
		transportCredentials = credentials.NewTLS(&tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: pool.TLSServerName,
		})
	}
	connection, err := grpc.NewClient(pool.Address,
		grpc.WithTransportCredentials(transportCredentials),
		grpc.WithAuthority(pool.HostHeader),
		grpc.WithUserAgent(defaultOriginProbeAgent),
		grpc.WithDisableRetry(),
	)
	if err != nil {
		return nil, err
	}
	client := &originGRPCProbeClient{
		pool: cloneOriginPool(pool), conn: connection, health: healthpb.NewHealthClient(connection),
	}
	p.grpcClients[pool.ID] = client
	return client, nil
}

func (p *networkOriginProber) Reconcile(pools []domain.OriginPool) {
	active := make(map[string]domain.OriginPool, len(pools))
	for _, pool := range pools {
		active[pool.ID] = pool
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, client := range p.httpClients {
		pool, exists := active[id]
		if exists && sameOriginTransport(client.pool, pool) {
			continue
		}
		client.transport.CloseIdleConnections()
		delete(p.httpClients, id)
	}
	for id, client := range p.grpcClients {
		pool, exists := active[id]
		if exists && sameOriginTransport(client.pool, pool) {
			continue
		}
		_ = client.conn.Close()
		delete(p.grpcClients, id)
	}
}

func (p *networkOriginProber) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, client := range p.httpClients {
		client.transport.CloseIdleConnections()
		delete(p.httpClients, id)
	}
	var closeErrors []error
	for id, client := range p.grpcClients {
		if err := client.conn.Close(); err != nil {
			closeErrors = append(closeErrors, err)
		}
		delete(p.grpcClients, id)
	}
	return errors.Join(closeErrors...)
}

func probeOriginHTTP(ctx context.Context, pool domain.OriginPool, client *http.Client) (OriginProbeMeasurement, error) {
	measurement := OriginProbeMeasurement{}
	started := time.Now()
	var traceMu sync.Mutex
	connectStarted := make(map[string]time.Time)
	var tlsStarted time.Time
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			traceMu.Lock()
			measurement.ConnectionReused = info.Reused
			traceMu.Unlock()
		},
		ConnectStart: func(network, address string) {
			traceMu.Lock()
			connectStarted[network+"\x00"+address] = time.Now()
			traceMu.Unlock()
		},
		ConnectDone: func(network, address string, err error) {
			finished := time.Now()
			traceMu.Lock()
			if connectionStarted := connectStarted[network+"\x00"+address]; err == nil && !connectionStarted.IsZero() {
				measurement.ConnectDuration = finished.Sub(connectionStarted)
			}
			traceMu.Unlock()
		},
		TLSHandshakeStart: func() {
			traceMu.Lock()
			tlsStarted = time.Now()
			traceMu.Unlock()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			finished := time.Now()
			traceMu.Lock()
			if err == nil && !tlsStarted.IsZero() {
				measurement.TLSHandshakeDuration = finished.Sub(tlsStarted)
			}
			traceMu.Unlock()
		},
		GotFirstResponseByte: func() {
			traceMu.Lock()
			measurement.HeaderDuration = time.Since(started)
			traceMu.Unlock()
		},
	}
	requestURL := &url.URL{Scheme: pool.Scheme, Host: pool.Address, Path: "/"}
	request, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodHead, requestURL.String(), nil)
	if err != nil {
		return measurement, err
	}
	request.Host = pool.HostHeader
	request.Header.Set("User-Agent", defaultOriginProbeAgent)
	response, err := client.Do(request)
	if err != nil {
		traceMu.Lock()
		measurement.TotalDuration = time.Since(started)
		result := measurement
		traceMu.Unlock()
		return result, err
	}
	_, readErr := io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	traceMu.Lock()
	measurement.TotalDuration = time.Since(started)
	measurement.HTTPStatus = response.StatusCode
	result := measurement
	traceMu.Unlock()
	wantedVersion := effectivePoolHTTPVersion(pool)
	if (wantedVersion == domain.OriginHTTPVersionHTTP2 || wantedVersion == domain.OriginHTTPVersionH2C) && response.ProtoMajor != 2 {
		return result, fmt.Errorf("origin negotiated %s instead of HTTP/2", response.Proto)
	}
	if wantedVersion == domain.OriginHTTPVersionHTTP1 && response.ProtoMajor != 1 {
		return result, fmt.Errorf("origin negotiated %s instead of HTTP/1.1", response.Proto)
	}
	if readErr != nil || closeErr != nil {
		return result, errors.Join(readErr, closeErr)
	}
	if response.StatusCode >= http.StatusInternalServerError {
		return result, fmt.Errorf("origin returned HTTP %d", response.StatusCode)
	}
	return result, nil
}

func probeOriginGRPC(ctx context.Context, client *originGRPCProbeClient) (OriginProbeMeasurement, error) {
	started := time.Now()
	measurement := OriginProbeMeasurement{ConnectionReused: client.conn.GetState() == connectivity.Ready}
	response, err := client.health.Check(ctx, &healthpb.HealthCheckRequest{})
	measurement.HeaderDuration = time.Since(started)
	measurement.TotalDuration = measurement.HeaderDuration
	if err != nil {
		if grpcstatus.Code(err) == codes.Unimplemented {
			return measurement, nil
		}
		return measurement, err
	}
	if response.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		return measurement, fmt.Errorf("origin gRPC health status is %s", response.GetStatus())
	}
	return measurement, nil
}

func probeOriginTransport(ctx context.Context, pool domain.OriginPool) (OriginProbeMeasurement, error) {
	measurement := OriginProbeMeasurement{}
	started := time.Now()
	connectStarted := time.Now()
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", pool.Address)
	measurement.ConnectDuration = time.Since(connectStarted)
	if err != nil {
		measurement.TotalDuration = time.Since(started)
		return measurement, err
	}
	defer connection.Close()
	if pool.Scheme == "grpcs" {
		tlsStarted := time.Now()
		tlsConnection := tls.Client(connection, &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: pool.TLSServerName,
			NextProtos: []string{"h2"},
		})
		if err := tlsConnection.HandshakeContext(ctx); err != nil {
			measurement.TLSHandshakeDuration = time.Since(tlsStarted)
			measurement.TotalDuration = time.Since(started)
			return measurement, err
		}
		measurement.TLSHandshakeDuration = time.Since(tlsStarted)
		if tlsConnection.ConnectionState().NegotiatedProtocol != "h2" {
			measurement.TotalDuration = time.Since(started)
			return measurement, errors.New("origin did not negotiate HTTP/2")
		}
	}
	measurement.TotalDuration = time.Since(started)
	return measurement, nil
}

type originPoolRuntime struct {
	Pool                        domain.OriginPool
	CircuitState                domain.OriginCircuitState
	ServiceConsecutiveFailures  int
	ServiceConsecutiveSuccesses int
	ColdConsecutiveFailures     int
	ColdConsecutiveSuccesses    int
	Status                      *domain.OriginProbeStatus
}

type persistedOriginRuntime struct {
	Version int                   `json:"version"`
	Pools   []persistedOriginPool `json:"pools"`
}

type persistedOriginPool struct {
	Pool                        domain.OriginPool         `json:"pool"`
	CircuitState                domain.OriginCircuitState `json:"circuit_state"`
	ServiceConsecutiveFailures  int                       `json:"service_consecutive_failures,omitempty"`
	ServiceConsecutiveSuccesses int                       `json:"service_consecutive_successes,omitempty"`
	ColdConsecutiveFailures     int                       `json:"cold_consecutive_failures,omitempty"`
	ColdConsecutiveSuccesses    int                       `json:"cold_consecutive_successes,omitempty"`
	ConsecutiveFailures         int                       `json:"consecutive_failures,omitempty"`
	ConsecutiveSuccesses        int                       `json:"consecutive_successes,omitempty"`
}

func (a *Agent) originRuntimePath() string {
	return filepath.Join(a.Config.StateDir, originRuntimeStateName)
}

func (a *Agent) loadOriginRuntime() error {
	contents, err := os.ReadFile(a.originRuntimePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read origin connection state: %w", err)
	}
	var persisted persistedOriginRuntime
	if err := json.Unmarshal(contents, &persisted); err != nil {
		return fmt.Errorf("decode origin connection state: %w", err)
	}
	if persisted.Version != originRuntimeStateVersion && persisted.Version != originRuntimeLegacyVersion || len(persisted.Pools) > domain.MaxOriginPools {
		return errors.New("origin connection state is invalid")
	}
	loaded := make(map[string]*originPoolRuntime, len(persisted.Pools))
	for _, item := range persisted.Pools {
		if err := a.validateOriginPool(item.Pool); err != nil {
			return fmt.Errorf("restore origin pool %s: %w", item.Pool.ID, err)
		}
		if item.CircuitState != domain.OriginCircuitClosed && item.CircuitState != domain.OriginCircuitOpen && item.CircuitState != domain.OriginCircuitRecovering {
			return fmt.Errorf("restore origin pool %s: invalid circuit state", item.Pool.ID)
		}
		serviceFailures := item.ServiceConsecutiveFailures
		serviceSuccesses := item.ServiceConsecutiveSuccesses
		coldFailures := item.ColdConsecutiveFailures
		coldSuccesses := item.ColdConsecutiveSuccesses
		if persisted.Version == originRuntimeLegacyVersion {
			coldFailures = item.ConsecutiveFailures
			coldSuccesses = item.ConsecutiveSuccesses
		}
		if !validPersistedOriginStreak(serviceFailures) || !validPersistedOriginStreak(serviceSuccesses) ||
			!validPersistedOriginStreak(coldFailures) || !validPersistedOriginStreak(coldSuccesses) {
			return fmt.Errorf("restore origin pool %s: invalid probe streak", item.Pool.ID)
		}
		if _, exists := loaded[item.Pool.ID]; exists {
			return fmt.Errorf("restore origin pool %s: duplicate ID", item.Pool.ID)
		}
		loaded[item.Pool.ID] = &originPoolRuntime{
			Pool: item.Pool, CircuitState: item.CircuitState,
			ServiceConsecutiveFailures: serviceFailures, ServiceConsecutiveSuccesses: serviceSuccesses,
			ColdConsecutiveFailures: coldFailures, ColdConsecutiveSuccesses: coldSuccesses,
		}
	}
	a.originPools = loaded
	return nil
}

func (a *Agent) persistOriginRuntimes(runtimes map[string]*originPoolRuntime) error {
	ids := make([]string, 0, len(runtimes))
	for id := range runtimes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	persisted := persistedOriginRuntime{Version: originRuntimeStateVersion, Pools: make([]persistedOriginPool, 0, len(ids))}
	for _, id := range ids {
		runtime := runtimes[id]
		persisted.Pools = append(persisted.Pools, persistedOriginPool{
			Pool: runtime.Pool, CircuitState: runtime.CircuitState,
			ServiceConsecutiveFailures:  runtime.ServiceConsecutiveFailures,
			ServiceConsecutiveSuccesses: runtime.ServiceConsecutiveSuccesses,
			ColdConsecutiveFailures:     runtime.ColdConsecutiveFailures,
			ColdConsecutiveSuccesses:    runtime.ColdConsecutiveSuccesses,
		})
	}
	contents, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	return atomicWriteFile(a.originRuntimePath(), contents, 0o640)
}

func validPersistedOriginStreak(value int) bool {
	return value >= 0 && value <= 1_000_000
}

func (a *Agent) validateDesiredOriginPools(state domain.DesiredState) error {
	if len(state.OriginPools) > domain.MaxOriginPools {
		return fmt.Errorf("too many origin connection pools")
	}
	seenIDs := make(map[string]bool, len(state.OriginPools))
	seenPaths := make(map[string]bool, len(state.OriginPools))
	for _, pool := range state.OriginPools {
		if err := a.validateOriginPool(pool); err != nil {
			return fmt.Errorf("origin pool %s: %w", pool.ID, err)
		}
		if seenIDs[pool.ID] || seenPaths[pool.ConfigPath] {
			return fmt.Errorf("origin pool %s is duplicated", pool.ID)
		}
		seenIDs[pool.ID] = true
		seenPaths[pool.ConfigPath] = true
		if !strings.Contains(state.NginxConfig, "include "+pool.ConfigPath+";") {
			return fmt.Errorf("origin pool %s is not referenced by the Nginx configuration", pool.ID)
		}
	}
	managedPrefix := "include " + strings.TrimRight(a.Config.OriginPoolConfigDirectory, string(filepath.Separator)) + string(filepath.Separator)
	if len(state.OriginPools) == 0 && strings.Contains(state.NginxConfig, managedPrefix) {
		return errors.New("Nginx configuration references origin pool files without pool metadata")
	}
	return nil
}

func (a *Agent) validateOriginPool(pool domain.OriginPool) error {
	if !domain.ValidOriginPool(pool) {
		return errors.New("metadata is invalid")
	}
	expected := filepath.Join(a.Config.OriginPoolConfigDirectory, pool.ID+".conf")
	if pool.ConfigPath != expected {
		return errors.New("configuration path is outside the managed origin pool directory")
	}
	return nil
}

type originPoolStage struct {
	agent       *Agent
	pending     map[string]*originPoolRuntime
	backups     map[string]fileBackup
	oldRuntimes map[string]*originPoolRuntime
	committed   bool
	rolledBack  bool
}

func (a *Agent) stageOriginPools(pools []domain.OriginPool) (*originPoolStage, error) {
	a.originMu.RLock()
	oldRuntimes := cloneOriginRuntimes(a.originPools)
	a.originMu.RUnlock()
	pending := make(map[string]*originPoolRuntime, len(pools))
	for _, pool := range pools {
		runtime := &originPoolRuntime{Pool: cloneOriginPool(pool), CircuitState: domain.OriginCircuitClosed}
		if previous := oldRuntimes[pool.ID]; previous != nil && sameOriginTransport(previous.Pool, pool) {
			runtime.CircuitState = previous.CircuitState
			runtime.ServiceConsecutiveFailures = previous.ServiceConsecutiveFailures
			runtime.ServiceConsecutiveSuccesses = previous.ServiceConsecutiveSuccesses
			runtime.ColdConsecutiveFailures = previous.ColdConsecutiveFailures
			runtime.ColdConsecutiveSuccesses = previous.ColdConsecutiveSuccesses
			if previous.Status != nil {
				status := cloneOriginProbeStatus(previous.Status)
				status.Address = pool.Address
				status.Scheme = pool.Scheme
				status.HTTPVersion = pool.HTTPVersion
				status.KeepaliveConnections = pool.KeepaliveConnections
				status.References = append([]domain.OriginPoolReference(nil), pool.References...)
				runtime.Status = status
			}
		}
		pending[pool.ID] = runtime
	}
	stage := &originPoolStage{
		agent: a, pending: pending, oldRuntimes: oldRuntimes,
		backups: make(map[string]fileBackup, len(pools)+1),
	}
	statePath := a.originRuntimePath()
	backup, err := readBackup(statePath)
	if err != nil {
		return nil, fmt.Errorf("back up origin connection state: %w", err)
	}
	stage.backups[statePath] = backup
	for _, pool := range pools {
		backup, err := readBackup(pool.ConfigPath)
		if err != nil {
			stage.Rollback()
			return nil, fmt.Errorf("back up origin pool %s: %w", pool.ID, err)
		}
		stage.backups[pool.ConfigPath] = backup
		if err := atomicWriteFile(pool.ConfigPath, originServerDirective(pool, pending[pool.ID].CircuitState), 0o640); err != nil {
			stage.Rollback()
			return nil, fmt.Errorf("write origin pool %s: %w", pool.ID, err)
		}
	}
	return stage, nil
}

func (s *originPoolStage) Persist() error {
	if s == nil {
		return nil
	}
	return s.agent.persistOriginRuntimes(s.pending)
}

func (s *originPoolStage) Commit() error {
	if s == nil || s.committed {
		return nil
	}
	s.agent.originMu.Lock()
	s.agent.originPools = cloneOriginRuntimes(s.pending)
	s.agent.originMu.Unlock()
	if lifecycle, ok := s.agent.Config.OriginProber.(originProberLifecycle); ok {
		pools := make([]domain.OriginPool, 0, len(s.pending))
		for _, runtime := range s.pending {
			pools = append(pools, cloneOriginPool(runtime.Pool))
		}
		lifecycle.Reconcile(pools)
	}
	s.agent.wakeOriginProbeScheduler()
	var cleanupErrors []error
	for id, runtime := range s.oldRuntimes {
		if _, retained := s.pending[id]; retained {
			continue
		}
		if err := os.Remove(runtime.Pool.ConfigPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove stale origin pool %s: %w", id, err))
		}
	}
	s.committed = true
	return errors.Join(cleanupErrors...)
}

func (s *originPoolStage) Rollback() {
	if s == nil || s.committed || s.rolledBack {
		return
	}
	restoreConfigurationFiles(s.backups)
	s.rolledBack = true
}

func cloneOriginRuntimes(source map[string]*originPoolRuntime) map[string]*originPoolRuntime {
	result := make(map[string]*originPoolRuntime, len(source))
	for id, runtime := range source {
		copyOfRuntime := *runtime
		copyOfRuntime.Pool = cloneOriginPool(runtime.Pool)
		if runtime.Status != nil {
			copyOfRuntime.Status = cloneOriginProbeStatus(runtime.Status)
		}
		result[id] = &copyOfRuntime
	}
	return result
}

func cloneOriginProbeStatus(status *domain.OriginProbeStatus) *domain.OriginProbeStatus {
	if status == nil {
		return nil
	}
	cloned := *status
	cloned.References = append([]domain.OriginPoolReference(nil), status.References...)
	if status.ServiceProbe != nil {
		service := *status.ServiceProbe
		cloned.ServiceProbe = &service
	}
	if status.ColdProbe != nil {
		cold := *status.ColdProbe
		cloned.ColdProbe = &cold
	}
	return &cloned
}

func cloneOriginPool(pool domain.OriginPool) domain.OriginPool {
	pool.References = append([]domain.OriginPoolReference(nil), pool.References...)
	return pool
}

func sameOriginTransport(left, right domain.OriginPool) bool {
	return left.ID == right.ID && left.Address == right.Address && left.Scheme == right.Scheme &&
		left.HostHeader == right.HostHeader && left.TLSServerName == right.TLSServerName &&
		effectivePoolHTTPVersion(left) == effectivePoolHTTPVersion(right)
}

func effectivePoolHTTPVersion(pool domain.OriginPool) domain.OriginHTTPVersion {
	if pool.HTTPVersion == "" {
		return domain.OriginHTTPVersionHTTP1
	}
	return pool.HTTPVersion
}

func originServerDirective(pool domain.OriginPool, state domain.OriginCircuitState) []byte {
	if state == domain.OriginCircuitClosed {
		return []byte("# Managed by cdn-edge-agent. Do not edit.\nserver " + pool.Address + " max_fails=1 fail_timeout=5s;\n")
	}
	return []byte("# Managed by cdn-edge-agent. Do not edit.\nserver " + pool.Address + " down;\n")
}

func (a *Agent) runOriginProbeLoop(ctx context.Context) {
	schedules := make(map[string]*originProbeSchedule)
	for {
		targets := a.originProbeTargets()
		if len(targets) == 0 {
			a.setComponentFailure("origin_health_service", "update service origin circuit", nil)
			a.setComponentFailure("origin_health_cold", "update cold origin circuit", nil)
		}
		active := make(map[string]bool, len(targets))
		now := time.Now()
		var servicePools []domain.OriginPool
		var coldPools []domain.OriginPool
		var nextWake time.Time
		for _, target := range targets {
			active[target.Pool.ID] = true
			schedule := schedules[target.Pool.ID]
			if schedule == nil {
				schedule = &originProbeSchedule{
					ServiceDue: now.Add(a.originProbeInitialDelay(target.Pool.ID, domain.OriginProbeService, target.Degraded)),
					ColdDue:    now.Add(a.originProbeInitialDelay(target.Pool.ID, domain.OriginProbeCold, target.Degraded)),
				}
				schedules[target.Pool.ID] = schedule
			}
			serviceInterval := a.originProbeInterval(domain.OriginProbeService, target.Degraded)
			coldInterval := a.originProbeInterval(domain.OriginProbeCold, target.Degraded)
			if maximumDue := now.Add(serviceInterval); schedule.ServiceDue.After(maximumDue) {
				schedule.ServiceDue = maximumDue
			}
			if maximumDue := now.Add(coldInterval); schedule.ColdDue.After(maximumDue) {
				schedule.ColdDue = maximumDue
			}
			if schedule.ServiceDue.After(now) {
				nextWake = earlierTime(nextWake, schedule.ServiceDue)
			} else {
				servicePools = append(servicePools, target.Pool)
			}
			if schedule.ColdDue.After(now) {
				nextWake = earlierTime(nextWake, schedule.ColdDue)
			} else {
				coldPools = append(coldPools, target.Pool)
			}
		}
		for id := range schedules {
			if !active[id] {
				delete(schedules, id)
			}
		}
		if len(servicePools) > 0 {
			a.runOriginProbeRound(ctx, domain.OriginProbeService, servicePools)
			finished := time.Now()
			for _, pool := range servicePools {
				degraded, exists := a.originProbeDegraded(pool.ID)
				if !exists {
					continue
				}
				schedules[pool.ID].ServiceDue = finished.Add(a.originProbeDelay(pool.ID, domain.OriginProbeService, degraded))
			}
		}
		if ctx.Err() != nil {
			return
		}
		if len(coldPools) > 0 {
			a.runOriginProbeRound(ctx, domain.OriginProbeCold, coldPools)
			finished := time.Now()
			for _, pool := range coldPools {
				degraded, exists := a.originProbeDegraded(pool.ID)
				if !exists {
					continue
				}
				schedules[pool.ID].ColdDue = finished.Add(a.originProbeDelay(pool.ID, domain.OriginProbeCold, degraded))
			}
		}
		if len(servicePools) > 0 || len(coldPools) > 0 {
			continue
		}
		if nextWake.IsZero() {
			nextWake = now.Add(time.Hour)
		}
		if !a.waitOriginProbeSchedule(ctx, time.Until(nextWake)) {
			return
		}
	}
}

type originProbeSchedule struct {
	ServiceDue time.Time
	ColdDue    time.Time
}

type originProbeTarget struct {
	Pool     domain.OriginPool
	Degraded bool
}

func (a *Agent) originProbeTargets() []originProbeTarget {
	a.originMu.RLock()
	targets := make([]originProbeTarget, 0, len(a.originPools))
	for _, runtime := range a.originPools {
		targets = append(targets, originProbeTarget{
			Pool: cloneOriginPool(runtime.Pool), Degraded: originRuntimeDegraded(runtime),
		})
	}
	a.originMu.RUnlock()
	sort.Slice(targets, func(i, j int) bool { return targets[i].Pool.ID < targets[j].Pool.ID })
	return targets
}

func (a *Agent) originProbeDegraded(poolID string) (bool, bool) {
	a.originMu.RLock()
	defer a.originMu.RUnlock()
	runtime := a.originPools[poolID]
	return runtime != nil && originRuntimeDegraded(runtime), runtime != nil
}

func originRuntimeDegraded(runtime *originPoolRuntime) bool {
	return runtime.CircuitState != domain.OriginCircuitClosed || runtime.ServiceConsecutiveFailures > 0 || runtime.ColdConsecutiveFailures > 0
}

func (a *Agent) originProbeInterval(kind domain.OriginProbeKind, degraded bool) time.Duration {
	if kind == domain.OriginProbeCold {
		if degraded {
			return a.Config.OriginDegradedColdInterval
		}
		return a.Config.OriginColdProbeInterval
	}
	if degraded {
		return a.Config.OriginDegradedProbeInterval
	}
	return a.Config.OriginProbeInterval
}

func (a *Agent) originProbeInitialDelay(poolID string, kind domain.OriginProbeKind, degraded bool) time.Duration {
	interval := a.originProbeInterval(kind, degraded)
	return a.stableJitterWithin("origin-probe-start-"+string(kind)+"-"+poolID, interval/5)
}

func (a *Agent) originProbeDelay(poolID string, kind domain.OriginProbeKind, degraded bool) time.Duration {
	interval := a.originProbeInterval(kind, degraded)
	state := "stable"
	if degraded {
		state = "degraded"
	}
	return a.stableInterval("origin-probe-"+string(kind)+"-"+state+"-"+poolID, interval)
}

func (a *Agent) waitOriginProbeSchedule(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-a.originProbeWake:
		return true
	case <-timer.C:
		return true
	}
}

func earlierTime(current, candidate time.Time) time.Time {
	if current.IsZero() || candidate.Before(current) {
		return candidate
	}
	return current
}

func (a *Agent) wakeOriginProbeScheduler() {
	select {
	case a.originProbeWake <- struct{}{}:
	default:
	}
}

func (a *Agent) runOriginProbeRound(ctx context.Context, kind domain.OriginProbeKind, pools []domain.OriginPool) {
	if len(pools) == 0 {
		return
	}
	workers := min(a.Config.OriginProbeWorkers, len(pools))
	jobs := make(chan domain.OriginPool)
	errorsFound := make(chan error, len(pools))
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for pool := range jobs {
				probeCtx, cancel := context.WithTimeout(ctx, a.Config.OriginProbeTimeout)
				measurement, probeErr := a.Config.OriginProber.Probe(probeCtx, pool, kind)
				cancel()
				if err := a.applyOriginProbeResult(pool, kind, measurement, probeErr); err != nil {
					errorsFound <- err
				}
			}
		}()
	}
	for _, pool := range pools {
		select {
		case jobs <- pool:
		case <-ctx.Done():
			close(jobs)
			group.Wait()
			close(errorsFound)
			return
		}
	}
	close(jobs)
	group.Wait()
	close(errorsFound)
	var roundErrors []error
	for err := range errorsFound {
		roundErrors = append(roundErrors, err)
	}
	a.setComponentFailure("origin_health_"+string(kind), "update "+string(kind)+" origin circuit", errors.Join(roundErrors...))
}

func (a *Agent) applyOriginProbeResult(pool domain.OriginPool, kind domain.OriginProbeKind, measurement OriginProbeMeasurement, probeErr error) error {
	a.nginxMu.Lock()
	defer a.nginxMu.Unlock()
	a.originMu.Lock()
	defer a.originMu.Unlock()
	current := a.originPools[pool.ID]
	if current == nil || !sameOriginTransport(current.Pool, pool) {
		return nil
	}
	next := *current
	next.Pool = cloneOriginPool(current.Pool)
	healthy := probeErr == nil
	failures, successes := originProbeStreaks(&next, kind)
	if healthy {
		*successes = incrementProbeStreak(*successes)
		*failures = 0
		if next.CircuitState == domain.OriginCircuitOpen && originProbeLayersSucceeded(&next, 1) {
			next.CircuitState = domain.OriginCircuitRecovering
		}
		if next.CircuitState == domain.OriginCircuitRecovering && originProbeLayersSucceeded(&next, a.Config.OriginProbeRecoveryThreshold) {
			next.CircuitState = domain.OriginCircuitClosed
		}
	} else {
		*failures = incrementProbeStreak(*failures)
		*successes = 0
		if next.CircuitState != domain.OriginCircuitClosed || *failures >= a.Config.OriginProbeFailureThreshold {
			next.CircuitState = domain.OriginCircuitOpen
			next.ServiceConsecutiveSuccesses = 0
			next.ColdConsecutiveSuccesses = 0
		}
	}
	status := cloneOriginProbeStatus(current.Status)
	if status == nil {
		status = &domain.OriginProbeStatus{}
	}
	checkedAt := time.Now().UTC()
	*status = domain.OriginProbeStatus{
		PoolID: pool.ID, Address: pool.Address, Scheme: pool.Scheme, HTTPVersion: pool.HTTPVersion,
		KeepaliveConnections: pool.KeepaliveConnections, References: append([]domain.OriginPoolReference(nil), pool.References...),
		CircuitState:               next.CircuitState,
		ServiceConsecutiveFailures: next.ServiceConsecutiveFailures, ServiceConsecutiveSuccesses: next.ServiceConsecutiveSuccesses,
		ColdConsecutiveFailures: next.ColdConsecutiveFailures, ColdConsecutiveSuccesses: next.ColdConsecutiveSuccesses,
		ServiceProbe: status.ServiceProbe, ColdProbe: status.ColdProbe, CheckedAt: checkedAt,
	}
	sample := &domain.OriginProbeSample{
		Healthy: healthy, ConnectionReused: measurement.ConnectionReused,
		ConnectMS: durationMilliseconds(measurement.ConnectDuration), TLSHandshakeMS: durationMilliseconds(measurement.TLSHandshakeDuration),
		HeaderMS: durationMilliseconds(measurement.HeaderDuration), TotalMS: durationMilliseconds(measurement.TotalDuration),
		HTTPStatus: measurement.HTTPStatus, CheckedAt: checkedAt,
	}
	if probeErr != nil {
		sample.Error = sanitizeOriginError(probeErr.Error())
	}
	if kind == domain.OriginProbeCold {
		status.ColdProbe = sample
	} else {
		status.ServiceProbe = sample
	}
	for _, currentSample := range []*domain.OriginProbeSample{status.ServiceProbe, status.ColdProbe} {
		if currentSample != nil && currentSample.CheckedAt.After(status.CheckedAt) {
			status.CheckedAt = currentSample.CheckedAt
		}
	}
	status.Healthy = originProbeSamplesHealthy(status)
	next.Status = status
	if next.CircuitState == current.CircuitState {
		a.originPools[pool.ID] = &next
		return nil
	}
	pending := cloneOriginRuntimes(a.originPools)
	pending[pool.ID] = &next
	if err := a.persistOriginRuntimes(pending); err != nil {
		return fmt.Errorf("persist origin pool %s transition: %w", pool.ID, err)
	}
	availabilityChanged := (current.CircuitState == domain.OriginCircuitClosed) != (next.CircuitState == domain.OriginCircuitClosed)
	if availabilityChanged {
		backup, err := readBackup(pool.ConfigPath)
		if err == nil {
			err = atomicWriteFile(pool.ConfigPath, originServerDirective(pool, next.CircuitState), 0o640)
		}
		if err == nil {
			err = a.Config.Runner.Test()
		}
		applied := false
		if err == nil {
			err = a.Config.Runner.Apply()
			applied = err == nil
		}
		if err != nil {
			restoreConfigurationFiles(map[string]fileBackup{pool.ConfigPath: backup})
			if applied || a.Config.Runner.Test() == nil {
				_ = a.Config.Runner.Apply()
			}
			persistErr := a.persistOriginRuntimes(a.originPools)
			return errors.Join(fmt.Errorf("switch origin pool %s circuit: %w", pool.ID, err), persistErr)
		}
	}
	a.originPools = pending
	return nil
}

func originProbeStreaks(runtime *originPoolRuntime, kind domain.OriginProbeKind) (*int, *int) {
	if kind == domain.OriginProbeCold {
		return &runtime.ColdConsecutiveFailures, &runtime.ColdConsecutiveSuccesses
	}
	return &runtime.ServiceConsecutiveFailures, &runtime.ServiceConsecutiveSuccesses
}

func originProbeLayersSucceeded(runtime *originPoolRuntime, threshold int) bool {
	return runtime.ServiceConsecutiveSuccesses >= threshold && runtime.ColdConsecutiveSuccesses >= threshold
}

func originProbeSamplesHealthy(status *domain.OriginProbeStatus) bool {
	seen := false
	for _, sample := range []*domain.OriginProbeSample{status.ServiceProbe, status.ColdProbe} {
		if sample == nil {
			continue
		}
		seen = true
		if !sample.Healthy {
			return false
		}
	}
	return seen
}

func (a *Agent) originProbeStatuses() []domain.OriginProbeStatus {
	a.originMu.RLock()
	defer a.originMu.RUnlock()
	statuses := make([]domain.OriginProbeStatus, 0, len(a.originPools))
	for _, runtime := range a.originPools {
		if runtime.Status == nil || runtime.Status.CheckedAt.IsZero() {
			continue
		}
		statuses = append(statuses, *cloneOriginProbeStatus(runtime.Status))
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].PoolID < statuses[j].PoolID })
	return statuses
}

func incrementProbeStreak(value int) int {
	if value >= 1_000_000 {
		return 1_000_000
	}
	return value + 1
}

func durationMilliseconds(duration time.Duration) float64 {
	if duration <= 0 {
		return 0
	}
	milliseconds := float64(duration) / float64(time.Millisecond)
	if milliseconds > 60_000 {
		return 60_000
	}
	return milliseconds
}

func sanitizeOriginError(detail string) string {
	detail = strings.TrimSpace(strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return ' '
		}
		return character
	}, detail))
	runes := []rune(detail)
	if len(runes) > 512 {
		detail = string(runes[:512])
	}
	return detail
}
