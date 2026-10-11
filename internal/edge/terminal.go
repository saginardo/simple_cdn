package edge

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"simple_cdn/internal/terminal"
)

func (a *Agent) runTerminalLoop(ctx context.Context) {
	for ctx.Err() == nil {
		err := a.terminalConnection(ctx)
		delay := 3 * time.Second
		// Older controllers do not implement the endpoint. Retry quietly.
		if errors.Is(err, errTerminalUnsupported) {
			delay = time.Minute
		}
		if !waitContext(ctx, delay) {
			return
		}
	}
}

var errTerminalUnsupported = errors.New("terminal endpoint is not supported")

func (a *Agent) terminalConnection(ctx context.Context) error {
	identity, err := terminalIdentity()
	if err != nil {
		return err
	}
	client := *a.client()
	client.Timeout = 0
	// The dedicated WebSocket transport must negotiate HTTP/1.1, while the
	// regular mTLS control requests retain their HTTP/2 connection pool.
	if transport, ok := client.Transport.(*http.Transport); ok {
		transport = transport.Clone()
		transport.ForceAttemptHTTP2 = false
		transport.TLSNextProto = nil
		if transport.TLSClientConfig != nil {
			transport.TLSClientConfig = transport.TLSClientConfig.Clone()
			transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
		}
		client.Transport = transport
		defer transport.CloseIdleConnections()
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	dialCtx, dialCancel := context.WithTimeout(ctx, 15*time.Second)
	conn, response, err := websocket.Dial(dialCtx, a.Config.ControlURL+"/api/edge/v1/terminal", &websocket.DialOptions{HTTPClient: &client})
	dialCancel()
	if err != nil {
		if response != nil && (response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed) {
			return errTerminalUnsupported
		}
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(terminal.MaxMessage)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := terminal.Write(ctx, conn, identity); err != nil {
		return err
	}
	go terminal.KeepAlive(ctx, conn)
	var active *edgeTerminalSession
	defer func() {
		if active != nil {
			active.close()
		}
	}()
	for {
		var message terminal.Message
		if err := wsjson.Read(ctx, conn, &message); err != nil {
			return err
		}
		if active != nil {
			select {
			case <-active.done:
				active = nil
			default:
			}
		}
		switch message.Type {
		case "start":
			if active != nil || message.SessionID == "" || len(message.SessionID) > 64 || !terminal.ValidSize(message.Cols, message.Rows) {
				return errors.New("invalid terminal start")
			}
			active, err = startEdgeTerminal(ctx, conn, identity, message, terminal.IdleTimeout, terminal.MaxDuration)
			if err != nil {
				if err := terminal.Write(ctx, conn, terminal.Message{Type: "exit", SessionID: message.SessionID, Reason: "shell_failed"}); err != nil {
					return err
				}
			}
		case "input", "resize", "close":
			if active == nil || active.id != message.SessionID {
				continue
			}
			if message.Type == "close" {
				active.close()
				active = nil
				continue
			}
			if (message.Type == "input" && (len(message.Data) == 0 || len(message.Data) > terminal.MaxData)) || (message.Type == "resize" && !terminal.ValidSize(message.Cols, message.Rows)) {
				return errors.New("invalid terminal input")
			}
			select {
			case active.input <- message:
			case <-active.done:
			case <-ctx.Done():
				return ctx.Err()
			}
		default:
			return errors.New("invalid terminal message")
		}
	}
}
