// Package terminal defines the bounded protocol shared by the controller and edge.
package terminal

import (
	"context"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	Capability    = "terminal_v1"
	IdleTimeout   = 10 * time.Minute
	MaxDuration   = time.Hour
	AttachTimeout = 30 * time.Second
	WriteTimeout  = 5 * time.Second
	MaxMessage    = 64 << 10
	MaxData       = 16 << 10
)

// Data is base64 encoded by encoding/json, preserving arbitrary terminal bytes.
type Message struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
	Data      []byte `json:"data,omitempty"`
	Cols      uint16 `json:"cols,omitempty"`
	Rows      uint16 `json:"rows,omitempty"`
	User      string `json:"user,omitempty"`
	Shell     string `json:"shell,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func ValidSize(cols, rows uint16) bool { return cols >= 2 && cols <= 500 && rows >= 2 && rows <= 200 }

func Write(ctx context.Context, conn *websocket.Conn, message Message) error {
	ctx, cancel := context.WithTimeout(ctx, WriteTimeout)
	defer cancel()
	return wsjson.Write(ctx, conn, message)
}

// KeepAlive requires a concurrent reader to process pong frames.
func KeepAlive(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				_ = conn.CloseNow()
				return
			}
		}
	}
}
