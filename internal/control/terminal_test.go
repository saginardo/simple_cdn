package control

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
	"simple_cdn/internal/terminal"
)

type terminalFixture struct {
	server *Server
	http   *httptest.Server
	node   domain.Node
	edge   *websocket.Conn
	base   string
}

func newTerminalFixture(t *testing.T, idle, maximum, attach time.Duration) *terminalFixture {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.CreateInitialAdmin("hash", "secret"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, token := range []string{"session-token", "other-session"} {
		if err := database.CreateAuthenticatedSession("admin", token, "csrf-token", "totp", "", now, now.Add(time.Hour), now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	node, err := database.CreateNode("terminal-edge", "203.0.113.40")
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{Raw: []byte("terminal-test-certificate")}
	if err := database.SetNodeCertificate(node.ID, CertificateFingerprintDER(certificate.Raw)); err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeCapabilities(node.ID, []string{terminal.Capability}); err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeStatus(node.ID, domain.NodeActive); err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database}
	hub := server.terminals()
	hub.idle = idle
	hub.maximum = maximum
	hub.attach = attach
	hub.tick = 5 * time.Millisecond
	handler := server.Handler()
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Edge") == "yes" {
			r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{certificate}}
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		server.ShutdownTerminals(ctx)
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	edge, _, err := websocket.Dial(ctx, httpServer.URL+"/api/edge/v1/terminal", &websocket.DialOptions{HTTPHeader: http.Header{"X-Test-Edge": []string{"yes"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = edge.CloseNow() })
	writeTerminalTest(t, edge, terminal.Message{Type: "hello", User: "root", Shell: "/bin/bash"})
	deadline := time.Now().Add(time.Second)
	for {
		hub.mu.Lock()
		connected := hub.peers[node.ID] != nil
		hub.mu.Unlock()
		if connected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent did not connect")
		}
		time.Sleep(time.Millisecond)
	}
	return &terminalFixture{server: server, http: httpServer, node: node, edge: edge, base: "/api/nodes/" + node.ID + "/terminal-sessions"}
}

func (f *terminalFixture) create(t *testing.T) store.TerminalSession {
	t.Helper()
	response := settingsRequestWithSession(t, f.server, "POST", f.base, map[string]int{"cols": 80, "rows": 24}, "session-token", "csrf-token")
	if response.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", response.Code, response.Body.String())
	}
	var record store.TerminalSession
	if err := json.Unmarshal(response.Body.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func (f *terminalFixture) connect(t *testing.T, record store.TerminalSession) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, f.http.URL+f.base+"/"+record.ID+"/connect", &websocket.DialOptions{HTTPHeader: http.Header{"Cookie": []string{"cdn_session=session-token"}, "Origin": []string{f.http.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func writeTerminalTest(t *testing.T, conn *websocket.Conn, message terminal.Message) {
	t.Helper()
	if err := terminal.Write(context.Background(), conn, message); err != nil {
		t.Fatal(err)
	}
}
func readTerminalTest(t *testing.T, conn *websocket.Conn) terminal.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var message terminal.Message
	if err := wsjson.Read(ctx, conn, &message); err != nil {
		t.Fatal(err)
	}
	return message
}

func TestTerminalAuthOwnershipAndRelay(t *testing.T) {
	f := newTerminalFixture(t, time.Minute, time.Hour, time.Minute)
	for _, test := range []struct {
		token, csrf string
		want        int
	}{{"", "", 401}, {"session-token", "", 403}} {
		response := settingsRequestWithSession(t, f.server, "POST", f.base, map[string]int{"cols": 80, "rows": 24}, test.token, test.csrf)
		if response.Code != test.want {
			t.Fatalf("auth=%d want %d", response.Code, test.want)
		}
	}
	if err := f.server.Store.CreateSession("admin", "old-session", "csrf-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	response := settingsRequestWithSession(t, f.server, "POST", f.base, map[string]int{"cols": 80, "rows": 24}, "old-session", "csrf-token")
	if response.Code != 428 {
		t.Fatalf("recent auth=%d", response.Code)
	}
	unauthorized := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(unauthorized, httptest.NewRequest("GET", "/api/edge/v1/terminal", nil))
	if unauthorized.Code != 401 {
		t.Fatalf("edge auth=%d", unauthorized.Code)
	}
	record := f.create(t)
	for _, test := range []struct {
		token, origin string
		want          int
	}{{"session-token", "https://evil.example", 403}, {"session-token", "", 403}, {"other-session", f.http.URL, 404}} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		conn, response, err := websocket.Dial(ctx, f.http.URL+f.base+"/"+record.ID+"/connect", &websocket.DialOptions{HTTPHeader: http.Header{"Cookie": []string{"cdn_session=" + test.token}, "Origin": []string{test.origin}}})
		cancel()
		if conn != nil {
			_ = conn.CloseNow()
		}
		if err == nil || response == nil || response.StatusCode != test.want {
			t.Fatalf("upgrade auth: response=%v error=%v", response, err)
		}
	}
	browser := f.connect(t, record)
	replayCtx, replayCancel := context.WithTimeout(context.Background(), time.Second)
	replay, replayResponse, replayErr := websocket.Dial(replayCtx, f.http.URL+f.base+"/"+record.ID+"/connect", &websocket.DialOptions{HTTPHeader: http.Header{"Cookie": []string{"cdn_session=session-token"}, "Origin": []string{f.http.URL}}})
	replayCancel()
	if replay != nil {
		_ = replay.CloseNow()
	}
	if replayErr == nil || replayResponse == nil || replayResponse.StatusCode != 404 {
		t.Fatal("second browser attached to the same terminal")
	}
	start := readTerminalTest(t, f.edge)
	if start.Type != "start" || start.SessionID != record.ID || start.Cols != 80 {
		t.Fatalf("start=%+v", start)
	}
	writeTerminalTest(t, f.edge, terminal.Message{Type: "ready", SessionID: record.ID})
	if message := readTerminalTest(t, browser); message.Type != "ready" || message.User != "root" {
		t.Fatalf("ready=%+v", message)
	}
	writeTerminalTest(t, browser, terminal.Message{Type: "input", Data: []byte("pwd\r")})
	if message := readTerminalTest(t, f.edge); message.Type != "input" || string(message.Data) != "pwd\r" {
		t.Fatalf("input=%+v", message)
	}
	writeTerminalTest(t, browser, terminal.Message{Type: "resize", Cols: 120, Rows: 40})
	if message := readTerminalTest(t, f.edge); message.Cols != 120 || message.Rows != 40 {
		t.Fatalf("resize=%+v", message)
	}
	writeTerminalTest(t, f.edge, terminal.Message{Type: "output", SessionID: record.ID, Data: []byte{0xff, 0, 27}})
	if message := readTerminalTest(t, browser); string(message.Data) != string([]byte{0xff, 0, 27}) {
		t.Fatalf("output=%+v", message)
	}
	response = settingsRequestWithSession(t, f.server, "POST", f.base, map[string]int{"cols": 80, "rows": 24}, "session-token", "csrf-token")
	if response.Code != 409 {
		t.Fatalf("duplicate=%d", response.Code)
	}
	writeTerminalTest(t, browser, terminal.Message{Type: "close"})
	if message := readTerminalTest(t, browser); message.Reason != "user_closed" {
		t.Fatalf("close=%+v", message)
	}
	if message := readTerminalTest(t, f.edge); message.Type != "close" {
		t.Fatalf("agent close=%+v", message)
	}
	records, err := f.server.Store.ListTerminalSessions(f.node.ID)
	if err != nil || len(records) != 1 || records[0].EndedAt == nil || records[0].StartedAt == nil || records[0].Reason != "user_closed" {
		t.Fatalf("audit=%+v err=%v", records, err)
	}
}

func TestTerminalTimeoutsAndRevocation(t *testing.T) {
	for _, kind := range []string{"connect_timeout", "idle_timeout", "max_duration", "authentication_expired", "node_unauthorized", "agent_disconnected", "browser_disconnected"} {
		t.Run(kind, func(t *testing.T) {
			idle, maximum, attach := time.Minute, time.Hour, time.Minute
			switch kind {
			case "connect_timeout":
				attach = 80 * time.Millisecond
			case "idle_timeout":
				idle = 100 * time.Millisecond
			case "max_duration":
				maximum = 180 * time.Millisecond
			}
			f := newTerminalFixture(t, idle, maximum, attach)
			record := f.create(t)
			if kind != "connect_timeout" {
				browser := f.connect(t, record)
				_ = readTerminalTest(t, f.edge)
				writeTerminalTest(t, f.edge, terminal.Message{Type: "ready", SessionID: record.ID})
				_ = readTerminalTest(t, browser)
				switch kind {
				case "authentication_expired":
					if err := f.server.Store.DeleteSession("session-token"); err != nil {
						t.Fatal(err)
					}
				case "node_unauthorized":
					if err := f.server.Store.SetNodeStatus(f.node.ID, domain.NodeRevoked); err != nil {
						t.Fatal(err)
					}
				case "agent_disconnected":
					_ = f.edge.CloseNow()
				case "browser_disconnected":
					_ = browser.CloseNow()
				case "idle_timeout":
					writeTerminalTest(t, f.edge, terminal.Message{Type: "output", SessionID: record.ID, Data: []byte("output does not extend idle time")})
					_ = readTerminalTest(t, browser)
				}
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				records, err := f.server.Store.ListTerminalSessions(f.node.ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(records) == 1 && records[0].EndedAt != nil {
					if records[0].Reason != kind {
						t.Fatalf("reason=%s want %s", records[0].Reason, kind)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("session did not end")
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}
