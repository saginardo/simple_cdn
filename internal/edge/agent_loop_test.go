package edge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"simple_cdn/internal/domain"
)

func TestHeartbeatLoopContinuesWhileDesiredStatePullIsBlocked(t *testing.T) {
	var heartbeatCount atomic.Int32
	desiredStarted := make(chan struct{})
	var startedOnce sync.Once
	client := &http.Client{Transport: transportTestRoundTripper(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/edge/v1/heartbeat":
			heartbeatCount.Add(1)
			return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
		case "/api/edge/v1/desired-state":
			startedOnce.Do(func() { close(desiredStarted) })
			<-request.Context().Done()
			return nil, request.Context().Err()
		default:
			return &http.Response{StatusCode: http.StatusNoContent, Status: "204 No Content", Header: make(http.Header), Body: http.NoBody}, nil
		}
	})}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: t.TempDir(), CertificateDir: filepath.Join(t.TempDir(), "certs"),
		AgentSHA256: strings.Repeat("a", 64), PollInterval: 15 * time.Millisecond, HTTPClient: client,
		Runner: &fakeRunner{}, SecurityFirewall: &fakeSecurityFirewall{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	configurationDone := make(chan struct{})
	go func() {
		agent.runPeriodic(ctx, "configuration", agent.runConfigurationRound)
		close(configurationDone)
	}()
	select {
	case <-desiredStarted:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("desired-state request did not start")
	}
	if err := agent.runHeartbeatLoop(ctx); !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("heartbeat loop result = %v", err)
	}
	<-configurationDone
	if heartbeatCount.Load() < 3 {
		t.Fatalf("blocked desired-state pull limited heartbeats to %d", heartbeatCount.Load())
	}
}

func TestControlManifestSkipsUnchangedDesiredStateAndEmptyUpgrade(t *testing.T) {
	directory := t.TempDir()
	requestCount := 0
	client := &http.Client{Transport: transportTestRoundTripper(func(*http.Request) (*http.Response, error) {
		requestCount++
		return &http.Response{StatusCode: http.StatusInternalServerError, Status: "500 Internal Server Error", Header: make(http.Header), Body: http.NoBody}, nil
	})}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: directory, CertificateDir: filepath.Join(directory, "certs"),
		AgentSHA256: strings.Repeat("e", 64), HTTPClient: client, Runner: &fakeRunner{}, SecurityFirewall: &fakeSecurityFirewall{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "applied-version"), []byte("4\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	agent.setControlManifest(&domain.EdgeControlManifest{DesiredStateVersion: 4})
	agent.runConfigurationRound(context.Background())
	agent.runUpgradeRound(context.Background())
	if requestCount != 0 {
		t.Fatalf("unchanged manifest generated %d control requests", requestCount)
	}
}
