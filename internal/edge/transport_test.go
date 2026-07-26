package edge

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"simple_cdn/internal/domain"
)

type transportTestFirewall struct{}

func (transportTestFirewall) Replace([]domain.SecurityBan) error { return nil }

type transportTestIdentity struct {
	caPEM         []byte
	serverCertPEM []byte
	serverKeyPEM  []byte
	clientCertPEM []byte
	clientKeyPEM  []byte
	clientCert    *x509.Certificate
}

func newTransportTestIdentity(t *testing.T) transportTestIdentity {
	t.Helper()
	now := time.Now().Add(-time.Minute)
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "transport test CA"},
		NotBefore:             now,
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	issue := func(serial int64, template *x509.Certificate) ([]byte, []byte, *x509.Certificate) {
		key, keyErr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if keyErr != nil {
			t.Fatal(keyErr)
		}
		template.SerialNumber = big.NewInt(serial)
		der, certErr := x509.CreateCertificate(rand.Reader, template, caTemplate, &key.PublicKey, caKey)
		if certErr != nil {
			t.Fatal(certErr)
		}
		keyDER, keyErr := x509.MarshalPKCS8PrivateKey(key)
		if keyErr != nil {
			t.Fatal(keyErr)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
			x509Cert(t, der)
	}

	serverCertPEM, serverKeyPEM, _ := issue(2, &x509.Certificate{
		Subject:     pkix.Name{CommonName: "localhost"},
		DNSNames:    []string{"localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		NotBefore:   now,
		NotAfter:    now.Add(24 * time.Hour),
	})
	clientCertPEM, clientKeyPEM, clientCert := issue(3, &x509.Certificate{
		Subject:     pkix.Name{CommonName: "edge-client"},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		NotBefore:   now,
		NotAfter:    now.Add(24 * time.Hour),
	})
	return transportTestIdentity{
		caPEM: caPEM, serverCertPEM: serverCertPEM, serverKeyPEM: serverKeyPEM,
		clientCertPEM: clientCertPEM, clientKeyPEM: clientKeyPEM, clientCert: clientCert,
	}
}

func x509Cert(t *testing.T, der []byte) *x509.Certificate {
	t.Helper()
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

func writeTransportClientIdentity(t *testing.T, directory string, identity transportTestIdentity) (string, string, string) {
	t.Helper()
	certPath := filepath.Join(directory, "edge-client.crt")
	keyPath := filepath.Join(directory, "edge-client.key")
	caPath := filepath.Join(directory, "edge-ca.crt")
	for path, file := range map[string]struct {
		contents []byte
		mode     os.FileMode
	}{
		certPath: {contents: identity.clientCertPEM, mode: 0o600},
		keyPath:  {contents: identity.clientKeyPEM, mode: 0o600},
		caPath:   {contents: identity.caPEM, mode: 0o644},
	} {
		if err := os.WriteFile(path, file.contents, file.mode); err != nil {
			t.Fatal(err)
		}
	}
	return certPath, keyPath, caPath
}

func newTransportTestAgent(t *testing.T, controlURL string, directory string, identity transportTestIdentity) *Agent {
	t.Helper()
	certPath, keyPath, caPath := writeTransportClientIdentity(t, directory, identity)
	agent, err := New(Config{
		ControlURL:       controlURL,
		StateDir:         directory,
		CertificateDir:   filepath.Join(directory, "certs"),
		ClientCertPath:   certPath,
		ClientKeyPath:    keyPath,
		CAPath:           caPath,
		AgentSHA256:      strings.Repeat("a", 64),
		Runner:           &fakeRunner{},
		SecurityFirewall: transportTestFirewall{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

func TestClientCachesMTLSTransportAndEnablesHTTP2(t *testing.T) {
	directory := t.TempDir()
	identity := newTransportTestIdentity(t)
	agent := newTransportTestAgent(t, "https://control.example.test", directory, identity)

	first := agent.client()
	second := agent.client()
	if first != second {
		t.Fatal("client() did not return the cached client")
	}
	transport, ok := first.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport type = %T, want *http.Transport", first.Transport)
	}
	if !transport.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 is false")
	}
	if transport.MaxIdleConnsPerHost != 8 {
		t.Fatalf("MaxIdleConnsPerHost = %d, want 8", transport.MaxIdleConnsPerHost)
	}

	replacement := newTransportTestIdentity(t)
	writeTransportClientIdentity(t, directory, replacement)
	agent.resetControlClient()
	third := agent.client()
	if third == first {
		t.Fatal("client was not rebuilt after reset")
	}
	reloadedTransport, ok := third.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("reloaded client transport type = %T, want *http.Transport", third.Transport)
	}
	loadedCertificate := reloadedTransport.TLSClientConfig.Certificates[0].Certificate[0]
	if !bytes.Equal(loadedCertificate, replacement.clientCert.Raw) {
		t.Fatal("client did not reload the replacement certificate")
	}
	agent.resetControlClient()
}

type transportTestRoundTripper func(*http.Request) (*http.Response, error)

func (function transportTestRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type transportTestBody struct {
	reader *strings.Reader
	eof    bool
	closed bool
}

func (body *transportTestBody) Read(value []byte) (int, error) {
	count, err := body.reader.Read(value)
	if err == io.EOF {
		body.eof = true
	}
	return count, err
}

func (body *transportTestBody) Close() error {
	body.closed = true
	return nil
}

func TestHeartbeatDrainsResponseBodyForTransportReuse(t *testing.T) {
	body := &transportTestBody{reader: strings.NewReader(`{"ok":true}`)}
	client := &http.Client{Transport: transportTestRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       body,
		}, nil
	})}
	agent, err := New(Config{
		ControlURL:       "https://control.example.test",
		StateDir:         t.TempDir(),
		CertificateDir:   t.TempDir(),
		AgentSHA256:      strings.Repeat("b", 64),
		HTTPClient:       client,
		Runner:           &fakeRunner{},
		SecurityFirewall: transportTestFirewall{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Heartbeat(t.Context(), 1, "", nil); err != nil {
		t.Fatal(err)
	}
	if !body.eof || !body.closed {
		t.Fatalf("heartbeat response body eof=%v closed=%v", body.eof, body.closed)
	}
}

func TestHeartbeatStoresControlManifest(t *testing.T) {
	revision := strings.Repeat("c", 64)
	taskID := "11111111-1111-4111-8111-111111111111"
	body := fmt.Sprintf(`{"ok":true,"control":{"desired_state_version":9,"monitoring_revision":%q,"security_revision":%q,"upgrade_task_id":%q,"access_log_gzip":true}}`, revision, revision, taskID)
	client := &http.Client{Transport: transportTestRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: t.TempDir(), CertificateDir: t.TempDir(),
		AgentSHA256: strings.Repeat("d", 64), HTTPClient: client, Runner: &fakeRunner{}, SecurityFirewall: transportTestFirewall{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Heartbeat(t.Context(), 3, "", nil); err != nil {
		t.Fatal(err)
	}
	manifest, ok := agent.controlManifest()
	if !ok || manifest.DesiredStateVersion != 9 || manifest.MonitoringRevision != revision || manifest.UpgradeTaskID != taskID || !agent.logs.compressionEnabled.Load() {
		t.Fatalf("stored manifest = %#v, ok=%v", manifest, ok)
	}
}

func TestHeartbeatAndMachineStatusUseOneHTTP2MTLSConnection(t *testing.T) {
	identity := newTransportTestIdentity(t)
	serverCertificate, err := tls.X509KeyPair(identity.serverCertPEM, identity.serverKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(identity.caPEM) {
		t.Fatal("failed to load test CA")
	}
	var protocols []string
	var remoteAddresses []string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		protocols = append(protocols, request.Proto)
		remoteAddresses = append(remoteAddresses, request.RemoteAddr)
		if request.URL.Path == "/api/edge/v1/machine-status" {
			response.WriteHeader(http.StatusAccepted)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"ok":true}`)
	}))
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCertificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
		MinVersion:   tls.VersionTLS13,
	}
	server.StartTLS()
	defer server.Close()

	agent := newTransportTestAgent(t, server.URL, t.TempDir(), identity)
	defer agent.resetControlClient()
	if err := agent.Heartbeat(t.Context(), 1, "", nil); err != nil {
		t.Fatal(err)
	}
	report := domain.MachineStatus{
		Distribution: "Debian", Version: "13", UptimeSeconds: 60,
		Load1: 0.1, Load5: 0.2, Load15: 0.3, CPUUsagePercent: 25, CPULogicalCores: 2,
		MemoryUsedBytes: 1, MemoryTotalBytes: 2, DiskUsedBytes: 1, DiskTotalBytes: 2,
		NetworkInterface: "eth0", SampleSeconds: 5, CollectedAt: time.Now().UTC(),
	}
	if err := agent.ReportMachineStatus(t.Context(), report); err != nil {
		t.Fatal(err)
	}
	if err := agent.Heartbeat(t.Context(), 1, "", nil); err != nil {
		t.Fatal(err)
	}
	if len(protocols) != 3 {
		t.Fatalf("request protocols = %#v, want three HTTP/2.0 requests", protocols)
	}
	for _, protocol := range protocols {
		if protocol != "HTTP/2.0" {
			t.Fatalf("request protocols = %#v, want HTTP/2.0", protocols)
		}
	}
	if remoteAddresses[0] != remoteAddresses[1] || remoteAddresses[1] != remoteAddresses[2] {
		t.Fatalf("remote addresses = %#v, want one reused connection", remoteAddresses)
	}
}
