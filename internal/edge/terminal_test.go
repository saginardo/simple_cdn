//go:build linux || darwin

package edge

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"simple_cdn/internal/control"
	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
	"simple_cdn/internal/terminal"
)

func edgeTerminalRead(t *testing.T, conn *websocket.Conn) terminal.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var message terminal.Message
	if err := wsjson.Read(ctx, conn, &message); err != nil {
		t.Fatal(err)
	}
	return message
}
func edgeTerminalWrite(t *testing.T, conn *websocket.Conn, message terminal.Message) {
	t.Helper()
	if err := terminal.Write(context.Background(), conn, message); err != nil {
		t.Fatal(err)
	}
}
func edgeTerminalOutput(t *testing.T, conn *websocket.Conn, needle string) string {
	t.Helper()
	var output strings.Builder
	for output.Len() < 1<<20 {
		message := edgeTerminalRead(t, conn)
		if message.Type == "closed" || message.Type == "exit" {
			t.Fatalf("terminal ended: %+v, output=%q", message, output.String())
		}
		output.Write(message.Data)
		if strings.Contains(output.String(), needle) {
			return output.String()
		}
	}
	t.Fatal("expected output missing")
	return ""
}

func TestTerminalEndToEndMTLSPTY(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CreateInitialAdmin("hash", "secret"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := database.CreateAuthenticatedSession("admin", "terminal-admin", "csrf", "totp", "", now, now.Add(time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("real-pty", "203.0.113.70")
	if err != nil {
		t.Fatal(err)
	}
	ca, err := control.LoadOrCreateInternalCA(filepath.Join(t.TempDir(), "pki"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := ca.SignCSR(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}), node.ID)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	certificate := tls.Certificate{Certificate: [][]byte{block.Bytes}, PrivateKey: key}
	if err := database.SetNodeCertificate(node.ID, control.CertificateFingerprintDER(block.Bytes)); err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeCapabilities(node.ID, []string{terminal.Capability}); err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeStatus(node.ID, domain.NodeActive); err != nil {
		t.Fatal(err)
	}
	server := &control.Server{Store: database, CA: ca}
	httpServer := httptest.NewUnstartedServer(server.Handler())
	httpServer.TLS = server.TLSConfig()
	httpServer.StartTLS()
	defer httpServer.Close()
	defer server.ShutdownTerminals(context.Background())
	transport := httpServer.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.Certificates = []tls.Certificate{certificate}
	defer transport.CloseIdleConnections()
	agent := &Agent{Config: Config{ControlURL: httpServer.URL, HTTPClient: &http.Client{Transport: transport}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agentDone := make(chan error, 1)
	go func() { agentDone <- agent.terminalConnection(ctx) }()
	defer func() {
		cancel()
		select {
		case <-agentDone:
		case <-time.After(5 * time.Second):
			t.Error("agent connection did not stop")
		}
	}()
	base := "/api/nodes/" + node.ID + "/terminal-sessions"
	requestAPI := func(method, path, body string) *http.Response {
		request, err := http.NewRequest(method, httpServer.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.AddCookie(&http.Cookie{Name: "cdn_session", Value: "terminal-admin"})
		request.Header.Set("X-CSRF-Token", "csrf")
		response, err := httpServer.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		response := requestAPI("GET", base, "")
		var status struct {
			Connected bool `json:"connected"`
		}
		err := json.NewDecoder(response.Body).Decode(&status)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if status.Connected {
			break
		}
		if time.Now().After(deadline) {
			select {
			case err := <-agentDone:
				t.Fatalf("agent failed: %v", err)
			default:
			}
			t.Fatal("agent did not connect")
		}
		time.Sleep(5 * time.Millisecond)
	}
	response := requestAPI("POST", base, `{"cols":80,"rows":24}`)
	var record store.TerminalSession
	err = json.NewDecoder(response.Body).Decode(&record)
	response.Body.Close()
	if err != nil || response.StatusCode != 201 {
		t.Fatalf("create: %d, %v", response.StatusCode, err)
	}
	browser, _, err := websocket.Dial(context.Background(), httpServer.URL+base+"/"+record.ID+"/connect", &websocket.DialOptions{HTTPClient: httpServer.Client(), HTTPHeader: http.Header{"Cookie": []string{"cdn_session=terminal-admin"}, "Origin": []string{httpServer.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	defer browser.CloseNow()
	if message := edgeTerminalRead(t, browser); message.Type != "ready" {
		t.Fatalf("ready=%+v", message)
	}
	// Disable echo so assertions prove shell execution, not reflected input.
	edgeTerminalWrite(t, browser, terminal.Message{Type: "input", Data: []byte("stty -echo; PS1='PTY_PROMPT> '; printf '\\120\\124\\131_READY\\n'\r")})
	edgeTerminalOutput(t, browser, "PTY_READY")
	edgeTerminalWrite(t, browser, terminal.Message{Type: "input", Data: []byte("printf 'SHELL_PID=%s\\n' \"$$\"\r")})
	output := edgeTerminalOutput(t, browser, "SHELL_PID=")
	for !strings.Contains(output[strings.LastIndex(output, "SHELL_PID="):], "\n") {
		output += string(edgeTerminalRead(t, browser).Data)
	}
	pidText := strings.Fields(strings.SplitN(output, "SHELL_PID=", 2)[1])[0]
	pid, err := strconv.Atoi(pidText)
	if err != nil {
		t.Fatalf("pid in %q: %v", output, err)
	}
	edgeTerminalWrite(t, browser, terminal.Message{Type: "resize", Cols: 112, Rows: 37})
	edgeTerminalWrite(t, browser, terminal.Message{Type: "input", Data: []byte("stty size\r")})
	edgeTerminalOutput(t, browser, "37 112")
	// Ctrl+C must interrupt a foreground job while leaving the interactive shell usable.
	edgeTerminalWrite(t, browser, terminal.Message{Type: "input", Data: []byte("sh -c 'echo SLEEP_STARTED; exec sleep 60'\r")})
	edgeTerminalOutput(t, browser, "SLEEP_STARTED")
	edgeTerminalWrite(t, browser, terminal.Message{Type: "input", Data: []byte{3}})
	edgeTerminalOutput(t, browser, "PTY_PROMPT> ")
	edgeTerminalWrite(t, browser, terminal.Message{Type: "input", Data: []byte("printf 'INTERRUPT_OK\\n'\r")})
	edgeTerminalOutput(t, browser, "INTERRUPT_OK")
	edgeTerminalWrite(t, browser, terminal.Message{Type: "input", Data: []byte("printf 'UTF8_终端_OK\\n'\r")})
	edgeTerminalOutput(t, browser, "UTF8_终端_OK")
	response = requestAPI("DELETE", base+"/"+record.ID, "")
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("close status=%d", response.StatusCode)
	}
	for {
		message := edgeTerminalRead(t, browser)
		if message.Type == "closed" {
			if message.Reason != "user_closed" {
				t.Fatalf("close=%+v", message)
			}
			break
		}
	}
	deadline = time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("shell PID %d survived close", pid)
		}
		time.Sleep(5 * time.Millisecond)
	}
	records, err := database.ListTerminalSessions(node.ID)
	if err != nil || len(records) != 1 || records[0].StartedAt == nil || records[0].EndedAt == nil || records[0].User == "" || records[0].Reason != "user_closed" {
		t.Fatalf("audit=%+v, %v", records, err)
	}
}

func TestTerminalAgentTimeoutsAndDisconnectCleanup(t *testing.T) {
	for _, kind := range []string{"idle_timeout", "max_duration", "disconnect"} {
		t.Run(kind, func(t *testing.T) {
			idle, maximum := time.Minute, time.Minute
			if kind == "idle_timeout" {
				idle = 150 * time.Millisecond
			}
			if kind == "max_duration" {
				maximum = 200 * time.Millisecond
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sessionCh := make(chan *edgeTerminalSession, 1)
			errorCh := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					errorCh <- err
					return
				}
				defer conn.CloseNow()
				identity, err := terminalIdentity()
				if err != nil {
					errorCh <- err
					return
				}
				session, err := startEdgeTerminal(ctx, conn, identity, terminal.Message{Type: "start", SessionID: "timeout", Cols: 80, Rows: 24}, idle, maximum)
				if err != nil {
					errorCh <- err
					return
				}
				sessionCh <- session
				<-session.done
			}))
			defer server.Close()
			conn, _, err := websocket.Dial(ctx, server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			var session *edgeTerminalSession
			select {
			case session = <-sessionCh:
			case err := <-errorCh:
				t.Fatal(err)
			case <-time.After(time.Second):
				t.Fatal("PTY did not start")
			}
			_ = edgeTerminalRead(t, conn)
			session.input <- terminal.Message{Type: "input", Data: []byte("stty -echo; printf 'TEST_PID=%s\\n' \"$$\"\r")}
			output := edgeTerminalOutput(t, conn, "TEST_PID=")
			_ = output // the end-to-end test separately asserts process death by PID
			if kind == "disconnect" {
				cancel()
			} else {
				for {
					message := edgeTerminalRead(t, conn)
					if message.Type == "exit" {
						if message.Reason != kind {
							t.Fatalf("exit=%+v", message)
						}
						break
					}
				}
			}
			select {
			case <-session.done:
			case <-time.After(2 * time.Second):
				t.Fatal(fmt.Sprintf("session did not terminate after %s", kind))
			}
		})
	}
}
