package control

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"
	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
	"simple_cdn/internal/terminal"
)

type terminalHub struct {
	mu       sync.Mutex
	closed   bool
	peers    map[string]*terminalPeer
	sessions map[string]*terminalSession
	// Durations are fixed in production; tests exercise expiry without waiting minutes.
	idle, maximum, attach, tick time.Duration
}

type terminalPeer struct {
	conn                     *websocket.Conn
	fingerprint, user, shell string
}

type terminalEvent struct {
	message terminal.Message
	edge    bool
}

type terminalSession struct {
	record     store.TerminalSession
	token      string
	peer       *terminalPeer
	claimed    bool // protected by hub.mu; one browser may attach once
	cols, rows uint16
	attach     chan *websocket.Conn
	events     chan terminalEvent
	stop       chan string
	done       chan struct{}
}

func (s *Server) terminals() *terminalHub {
	s.terminalOnce.Do(func() {
		s.terminalHub = &terminalHub{peers: make(map[string]*terminalPeer), sessions: make(map[string]*terminalSession),
			idle: terminal.IdleTimeout, maximum: terminal.MaxDuration, attach: terminal.AttachTimeout, tick: time.Second}
	})
	return s.terminalHub
}

func (session *terminalSession) end(reason string) {
	select {
	case session.stop <- reason:
	default:
	}
}

func terminalNodeAllowed(node domain.Node) bool {
	return (node.Status == domain.NodeActive || node.Status == domain.NodeDraining) && slices.Contains(node.Capabilities, terminal.Capability)
}

func (s *Server) terminalSessions(response http.ResponseWriter, request *http.Request) {
	node, err := s.Store.GetNode(request.PathValue("id"))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	sessions, err := s.Store.ListTerminalSessions(node.ID)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	hub := s.terminals()
	hub.mu.Lock()
	peer := hub.peers[node.ID]
	hub.mu.Unlock()
	user, shell := "", ""
	if peer != nil {
		user, shell = peer.user, peer.shell
	}
	writeJSON(response, http.StatusOK, map[string]any{"sessions": sessions, "connected": peer != nil && terminalNodeAllowed(node), "user": user, "shell": shell,
		"idle_timeout_seconds": int(hub.idle.Seconds()), "max_duration_seconds": int(hub.maximum.Seconds())})
}

func (s *Server) createTerminalSession(response http.ResponseWriter, request *http.Request) {
	var input struct {
		Cols uint16 `json:"cols"`
		Rows uint16 `json:"rows"`
	}
	if !readJSON(response, request, &input) {
		return
	}
	if !terminal.ValidSize(input.Cols, input.Rows) {
		writeError(response, http.StatusBadRequest, errors.New("invalid terminal size"))
		return
	}
	node, err := s.Store.GetNode(request.PathValue("id"))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	if !terminalNodeAllowed(node) {
		writeError(response, http.StatusConflict, errors.New("node does not support terminal access; upgrade the agent and check node authorization"))
		return
	}
	hub := s.terminals()
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.closed {
		writeError(response, http.StatusServiceUnavailable, errors.New("control plane is stopping"))
		return
	}
	peer := hub.peers[node.ID]
	if peer == nil {
		writeError(response, http.StatusConflict, errors.New("node terminal is offline"))
		return
	}
	for _, session := range hub.sessions {
		if session.record.NodeID == node.ID {
			writeError(response, http.StatusConflict, errors.New("node already has an active terminal"))
			return
		}
	}
	cookie, _ := request.Cookie("cdn_session")
	record := store.TerminalSession{ID: uuid.NewString(), NodeID: node.ID, Actor: adminID(request.Context()), RemoteAddr: s.requestIP(request), User: peer.user, Shell: peer.shell, Status: "pending", CreatedAt: time.Now().UTC()}
	if err := s.Store.CreateTerminalSession(record); err != nil {
		writeStoreError(response, err)
		return
	}
	session := &terminalSession{record: record, token: cookie.Value, peer: peer, cols: input.Cols, rows: input.Rows, attach: make(chan *websocket.Conn, 1), events: make(chan terminalEvent, 32), stop: make(chan string, 1), done: make(chan struct{})}
	hub.sessions[record.ID] = session
	go s.runTerminalSession(hub, session)
	writeJSON(response, http.StatusCreated, record)
}

// Creation requires POST+CSRF and recent authentication. The upgrade additionally
// requires the exact creating login session and a same-origin browser request.
func (s *Server) connectTerminalSession(response http.ResponseWriter, request *http.Request) {
	origin, err := url.Parse(request.Header.Get("Origin"))
	if err != nil || origin.Host == "" || !strings.EqualFold(origin.Host, request.Host) || (origin.Scheme != "https" && origin.Scheme != "http") {
		writeError(response, http.StatusForbidden, errors.New("same-origin terminal connection required"))
		return
	}
	hub := s.terminals()
	cookie, _ := request.Cookie("cdn_session")
	hub.mu.Lock()
	session := hub.sessions[request.PathValue("sessionID")]
	if session == nil || session.record.NodeID != request.PathValue("id") || session.token != cookie.Value || session.claimed {
		hub.mu.Unlock()
		writeError(response, http.StatusNotFound, errors.New("terminal session is unavailable"))
		return
	}
	session.claimed = true
	hub.mu.Unlock()
	conn, err := websocket.Accept(response, request, nil)
	if err != nil {
		session.end("connect_failed")
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(terminal.MaxMessage)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go terminal.KeepAlive(ctx, conn)
	select {
	case session.attach <- conn:
	case <-session.done:
		return
	}
	go func() {
		select {
		case <-session.done:
			_ = conn.CloseNow()
		case <-ctx.Done():
		}
	}()
	for {
		var message terminal.Message
		if err := wsjson.Read(ctx, conn, &message); err != nil {
			session.end("browser_disconnected")
			return
		}
		if (message.Type == "input" && (len(message.Data) == 0 || len(message.Data) > terminal.MaxData)) ||
			(message.Type == "resize" && !terminal.ValidSize(message.Cols, message.Rows)) ||
			(message.Type != "input" && message.Type != "resize" && message.Type != "close") {
			session.end("invalid_message")
			return
		}
		message.SessionID = session.record.ID
		select {
		case session.events <- terminalEvent{message: message}:
		case <-session.done:
			return
		}
	}
}

func (s *Server) closeTerminalSession(response http.ResponseWriter, request *http.Request) {
	hub := s.terminals()
	hub.mu.Lock()
	session := hub.sessions[request.PathValue("sessionID")]
	hub.mu.Unlock()
	if session == nil || session.record.NodeID != request.PathValue("id") {
		writeError(response, http.StatusNotFound, errors.New("terminal session is unavailable"))
		return
	}
	session.end("user_closed")
	select {
	case <-session.done:
		writeJSON(response, http.StatusOK, map[string]bool{"ok": true})
	case <-request.Context().Done():
	}
}

func (s *Server) edgeTerminal(response http.ResponseWriter, request *http.Request) {
	nodeID := edgeNodeID(request.Context())
	node, err := s.Store.GetNode(nodeID)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	if node.Status != domain.NodeActive && node.Status != domain.NodeDraining {
		writeError(response, http.StatusForbidden, errors.New("node is not active"))
		return
	}
	conn, err := websocket.Accept(response, request, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(terminal.MaxMessage)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var hello terminal.Message
	helloCtx, helloCancel := context.WithTimeout(ctx, 10*time.Second)
	err = wsjson.Read(helloCtx, conn, &hello)
	helloCancel()
	if err != nil || hello.Type != "hello" || hello.User == "" || len(hello.User) > 128 || len(hello.Shell) > 256 || !strings.HasPrefix(hello.Shell, "/") {
		return
	}
	peer := &terminalPeer{conn: conn, fingerprint: CertificateFingerprintDER(request.TLS.PeerCertificates[0].Raw), user: hello.User, shell: hello.Shell}
	hub := s.terminals()
	hub.mu.Lock()
	if hub.closed {
		hub.mu.Unlock()
		return
	}
	previous := hub.peers[nodeID]
	hub.peers[nodeID] = peer
	hub.mu.Unlock()
	if previous != nil {
		_ = previous.conn.CloseNow()
	}
	defer func() {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		if hub.peers[nodeID] == peer {
			delete(hub.peers, nodeID)
		}
		for _, session := range hub.sessions {
			if session.peer == peer {
				session.end("agent_disconnected")
			}
		}
	}()
	go terminal.KeepAlive(ctx, conn)
	go func() {
		ticker := time.NewTicker(hub.tick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := s.Store.NodeIDByFingerprint(peer.fingerprint); err != nil {
					hub.mu.Lock()
					for _, session := range hub.sessions {
						if session.peer == peer {
							session.end("node_unauthorized")
						}
					}
					hub.mu.Unlock()
					_ = conn.CloseNow()
					return
				}
			}
		}
	}()
	for {
		var message terminal.Message
		if err := wsjson.Read(ctx, conn, &message); err != nil {
			return
		}
		if len(message.Data) > terminal.MaxData || (message.Type != "ready" && message.Type != "output" && message.Type != "exit") {
			return
		}
		hub.mu.Lock()
		session := hub.sessions[message.SessionID]
		hub.mu.Unlock()
		if session == nil || session.peer != peer {
			continue
		}
		select {
		case session.events <- terminalEvent{message: message, edge: true}:
		case <-session.done:
		case <-ctx.Done():
			return
		}
	}
}

// ShutdownTerminals closes hijacked connections, which http.Server.Shutdown
// does not manage, and persists the final audit before the store is closed.
func (s *Server) ShutdownTerminals(ctx context.Context) {
	hub := s.terminals()
	hub.mu.Lock()
	hub.closed = true
	sessions := make([]*terminalSession, 0, len(hub.sessions))
	for _, session := range hub.sessions {
		sessions = append(sessions, session)
		session.end("control_stopped")
	}
	peers := make([]*terminalPeer, 0, len(hub.peers))
	for _, peer := range hub.peers {
		peers = append(peers, peer)
	}
	hub.mu.Unlock()
	for _, session := range sessions {
		select {
		case <-session.done:
		case <-ctx.Done():
		}
	}
	for _, peer := range peers {
		_ = peer.conn.CloseNow()
	}
}

func (s *Server) runTerminalSession(hub *terminalHub, session *terminalSession) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var browser *websocket.Conn
	record := session.record
	lastInput := record.CreatedAt
	reason := "control_error"
	defer func() {
		_ = terminal.Write(ctx, session.peer.conn, terminal.Message{Type: "close", SessionID: record.ID, Reason: reason})
		ended := time.Now().UTC()
		record.Status = "closed"
		record.Reason = reason
		record.EndedAt = &ended
		if err := s.Store.EndTerminalSession(record); err != nil && s.Logger != nil {
			s.Logger.Error("persist terminal close audit", "session_id", record.ID, "error", err)
		}
		if browser != nil {
			_ = terminal.Write(ctx, browser, terminal.Message{Type: "closed", Reason: reason})
		}
		hub.mu.Lock()
		delete(hub.sessions, record.ID)
		close(session.done)
		hub.mu.Unlock()
	}()
	ticker := time.NewTicker(hub.tick)
	defer ticker.Stop()
	for {
		select {
		case reason = <-session.stop:
			return
		case browser = <-session.attach:
			if err := terminal.Write(ctx, session.peer.conn, terminal.Message{Type: "start", SessionID: record.ID, Cols: session.cols, Rows: session.rows}); err != nil {
				reason = "agent_disconnected"
				return
			}
		case <-ticker.C:
			now := time.Now()
			if now.Sub(record.CreatedAt) >= hub.maximum {
				reason = "max_duration"
				return
			}
			if record.StartedAt == nil && now.Sub(record.CreatedAt) >= hub.attach {
				reason = "connect_timeout"
				return
			}
			if now.Sub(lastInput) >= hub.idle {
				reason = "idle_timeout"
				return
			}
			if _, err := s.Store.Session(session.token); err != nil {
				reason = "authentication_expired"
				return
			}
			node, err := s.Store.GetNode(record.NodeID)
			if err != nil || !terminalNodeAllowed(node) {
				reason = "node_unauthorized"
				return
			}
		case event := <-session.events:
			message := event.message
			if event.edge {
				switch message.Type {
				case "ready":
					if browser == nil || record.StartedAt != nil {
						reason = "invalid_message"
						return
					}
					started := time.Now().UTC()
					record.StartedAt = &started
					record.Status = "running"
					lastInput = started
					if err := s.Store.StartTerminalSession(record); err != nil {
						reason = "audit_failed"
						return
					}
					message.User = record.User
					message.Shell = record.Shell
				case "output":
					if browser == nil || record.StartedAt == nil {
						reason = "invalid_message"
						return
					}
				case "exit":
					reason = message.Reason
					if !slices.Contains([]string{"shell_exited", "shell_failed", "idle_timeout", "max_duration", "io_error"}, reason) {
						reason = "shell_exited"
					}
					return
				}
				if err := terminal.Write(ctx, browser, message); err != nil {
					reason = "browser_disconnected"
					return
				}
			} else {
				if message.Type == "close" {
					reason = "user_closed"
					return
				}
				if message.Type == "input" {
					lastInput = time.Now()
				}
				if err := terminal.Write(ctx, session.peer.conn, message); err != nil {
					reason = "agent_disconnected"
					return
				}
			}
		}
	}
}
