package nginx

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"simple_cdn/internal/domain"
)

func TestRenderedHTTP3ServesARealQUICRequest(t *testing.T) {
	nginxBinary, err := exec.LookPath("nginx")
	if err != nil {
		t.Skip("nginx is not installed")
	}
	nginxVersion, err := exec.Command(nginxBinary, "-V").CombinedOutput()
	if err != nil || !strings.Contains(string(nginxVersion), "--with-http_v3_module") {
		t.Skip("nginx does not include ngx_http_v3_module")
	}
	curlBinary, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl is not installed")
	}
	curlVersion, err := exec.Command(curlBinary, "--version").CombinedOutput()
	if err != nil || !strings.Contains(string(curlVersion), "HTTP3") {
		t.Skip("curl does not support HTTP/3")
	}

	origin := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(response, "http3-ok")
	}))
	defer origin.Close()

	const siteID = "http3-integration"
	configuration, err := RenderWithRuntimeOptions([]domain.Site{{
		ID: siteID, Name: "HTTP/3 integration", Domains: []string{"h3.test"},
		PrimaryOrigin: domain.Origin{URL: origin.URL, Enabled: true}, Enabled: true,
	}}, nil, nil, RenderRuntimeOptions{DefaultCacheSizeGB: domain.DefaultCacheMaxSizeGB, HTTP3Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	directory := t.TempDir()
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	cacheDirectory := filepath.Join(directory, "cache")
	if err := os.MkdirAll(cacheDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	certificatePath := filepath.Join(directory, siteID+".crt")
	privateKeyPath := filepath.Join(directory, siteID+".key")
	writeHTTP3TestCertificate(t, certificatePath, privateKeyPath)
	httpPorts := reserveHTTP3TCPPorts(t, 2)
	httpsPort := reserveHTTP3DualProtocolPort(t)
	configuration = strings.Replace(configuration, "listen 80 default_server;", fmt.Sprintf("listen 127.0.0.1:%d default_server;", httpPorts[0]), 1)
	configuration = strings.Replace(configuration, "listen 80;", fmt.Sprintf("listen 127.0.0.1:%d;", httpPorts[1]), 1)
	configuration = strings.Replace(configuration, "listen 443 ssl default_server;", fmt.Sprintf("listen 127.0.0.1:%d ssl default_server;", httpsPort), 1)
	configuration = strings.Replace(configuration, "listen 443 quic reuseport default_server;", fmt.Sprintf("listen 127.0.0.1:%d quic reuseport default_server;", httpsPort), 1)
	configuration = strings.Replace(configuration, "listen 443 ssl;", fmt.Sprintf("listen 127.0.0.1:%d ssl;", httpsPort), 1)
	configuration = strings.Replace(configuration, "listen 443 quic;", fmt.Sprintf("listen 127.0.0.1:%d quic;", httpsPort), 1)
	configuration = strings.ReplaceAll(configuration, `h3=":443"`, fmt.Sprintf(`h3=":%d"`, httpsPort))
	configuration = strings.ReplaceAll(configuration, DefaultCachePath, cacheDirectory)
	configuration = strings.ReplaceAll(configuration, "/opt/cdn-edge/logs/access.json", filepath.Join(directory, "access.json"))
	configuration = strings.ReplaceAll(configuration, "/opt/cdn-edge/config/certs/"+siteID+".crt", certificatePath)
	configuration = strings.ReplaceAll(configuration, "/opt/cdn-edge/config/certs/"+siteID+".key", privateKeyPath)
	for _, unsafePath := range []string{"listen 80", "listen 443", "/opt/cdn-edge/config/certs/" + siteID, DefaultCachePath} {
		if strings.Contains(configuration, unsafePath) {
			t.Fatalf("isolated HTTP/3 configuration still contains %q:\n%s", unsafePath, configuration)
		}
	}

	configurationPath := filepath.Join(directory, "nginx.conf")
	errorLogPath := filepath.Join(directory, "error.log")
	nginxConfiguration := buildIsolatedNginxConfiguration(t, directory, "worker_processes 2;", errorLogPath, configuration)
	if err := os.WriteFile(configurationPath, []byte(nginxConfiguration), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(nginxBinary, "-t", "-c", configurationPath, "-p", directory).CombinedOutput(); err != nil {
		t.Fatalf("nginx -t: %v\n%s\n%s", err, output, nginxConfiguration)
	}

	command := exec.Command(nginxBinary, "-c", configurationPath, "-p", directory, "-g", "daemon off;")
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	defer func() {
		if command.Process != nil {
			_ = command.Process.Signal(syscall.SIGTERM)
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			if command.Process != nil {
				_ = command.Process.Kill()
			}
			<-done
		}
	}()

	var output []byte
	deadline := time.Now().Add(5 * time.Second)
	for {
		requestContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		output, err = exec.CommandContext(requestContext, curlBinary,
			"--http3-only", "--insecure", "--noproxy", "*", "--silent", "--show-error", "--dump-header", "-",
			"--resolve", fmt.Sprintf("h3.test:%d:127.0.0.1", httpsPort),
			"--write-out", "\nHTTP_VERSION=%{http_version}",
			fmt.Sprintf("https://h3.test:%d/", httpsPort),
		).CombinedOutput()
		cancel()
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		errorLog, _ := os.ReadFile(errorLogPath)
		t.Fatalf("HTTP/3 request: %v\n%s\nNginx error log:\n%s", err, output, errorLog)
	}
	result := string(output)
	for _, expected := range []string{"http3-ok", "HTTP_VERSION=3", fmt.Sprintf(`alt-svc: h3=":%d"; ma=86400`, httpsPort)} {
		if !strings.Contains(strings.ToLower(result), strings.ToLower(expected)) {
			t.Fatalf("HTTP/3 response is missing %q:\n%s", expected, result)
		}
	}
	for _, fallback := range []struct {
		option  string
		version string
	}{
		{option: "--http2", version: "2"},
		{option: "--http1.1", version: "1.1"},
	} {
		fallbackOutput, fallbackErr := exec.Command(curlBinary,
			fallback.option, "--insecure", "--noproxy", "*", "--silent", "--show-error",
			"--resolve", fmt.Sprintf("h3.test:%d:127.0.0.1", httpsPort),
			"--write-out", "\nHTTP_VERSION=%{http_version}",
			fmt.Sprintf("https://h3.test:%d/", httpsPort),
		).CombinedOutput()
		if fallbackErr != nil || !strings.Contains(string(fallbackOutput), "http3-ok") || !strings.Contains(string(fallbackOutput), "HTTP_VERSION="+fallback.version) {
			t.Fatalf("%s fallback request: %v\n%s", fallback.option, fallbackErr, fallbackOutput)
		}
	}
}

func reserveHTTP3DualProtocolPort(t *testing.T) int {
	t.Helper()
	for attempt := 0; attempt < 20; attempt++ {
		tcpListener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := tcpListener.Addr().(*net.TCPAddr).Port
		udpListener, udpErr := net.ListenPacket("udp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		_ = tcpListener.Close()
		if udpErr != nil {
			continue
		}
		_ = udpListener.Close()
		return port
	}
	t.Fatal("could not reserve a TCP/UDP integration port")
	return 0
}

func reserveHTTP3TCPPorts(t *testing.T, count int) []int {
	t.Helper()
	listeners := make([]net.Listener, 0, count)
	ports := make([]int, 0, count)
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	for len(ports) < count {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, listener)
		ports = append(ports, listener.Addr().(*net.TCPAddr).Port)
	}
	return ports
}

func writeHTTP3TestCertificate(t *testing.T, certificatePath, privateKeyPath string) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "h3.test"}, DNSNames: []string{"h3.test"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	encodedKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})
	if err := os.WriteFile(certificatePath, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(privateKeyPath, encodedKey, 0o600); err != nil {
		t.Fatal(err)
	}
}
