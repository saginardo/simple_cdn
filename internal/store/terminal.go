package store

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type TerminalSession struct {
	ID         string     `json:"id"`
	NodeID     string     `json:"node_id"`
	Actor      string     `json:"actor"`
	RemoteAddr string     `json:"remote_addr"`
	User       string     `json:"user"`
	Shell      string     `json:"shell"`
	Status     string     `json:"status"`
	Reason     string     `json:"reason"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
}

func migrateTerminalSessions(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS terminal_sessions (
		id TEXT PRIMARY KEY, node_id TEXT NOT NULL, actor TEXT NOT NULL,
		remote_addr TEXT NOT NULL, user TEXT NOT NULL, shell TEXT NOT NULL,
		status TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL, started_at TEXT, ended_at TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_terminal_sessions_node ON terminal_sessions(node_id, created_at DESC);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_terminal_sessions_active ON terminal_sessions(node_id) WHERE ended_at IS NULL;`)
	return err
}

func terminalAudit(tx *sql.Tx, session TerminalSession, action string) error {
	detail, err := json.Marshal(session)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO audit_logs(id, actor, action, resource_type, resource_id, remote_addr, detail, created_at)
		VALUES (?, ?, ?, 'node', ?, ?, ?, ?)`, uuid.NewString(), session.Actor, action, session.NodeID, session.RemoteAddr, string(detail), stamp(now()))
	return err
}

func (s *Store) CreateTerminalSession(session TerminalSession) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO terminal_sessions(id,node_id,actor,remote_addr,user,shell,status,created_at)
		VALUES (?,?,?,?,?,?,'pending',?)`, session.ID, session.NodeID, session.Actor, session.RemoteAddr, session.User, session.Shell, stamp(session.CreatedAt))
	if err != nil {
		return err
	}
	if err := terminalAudit(tx, session, "terminal_created"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) StartTerminalSession(session TerminalSession) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`UPDATE terminal_sessions SET status='running',started_at=? WHERE id=? AND ended_at IS NULL`, stamp(*session.StartedAt), session.ID)
	if err != nil {
		return err
	}
	if err := terminalAudit(tx, session, "terminal_started"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) EndTerminalSession(session TerminalSession) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE terminal_sessions SET status='closed',reason=?,ended_at=? WHERE id=? AND ended_at IS NULL`, session.Reason, stamp(*session.EndedAt), session.ID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed > 0 {
		if err := terminalAudit(tx, session, "terminal_closed"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListTerminalSessions(nodeID string) ([]TerminalSession, error) {
	return s.terminalSessions(`WHERE node_id=? ORDER BY created_at DESC, id DESC LIMIT 50`, nodeID)
}

func (s *Store) terminalSessions(clause string, args ...any) ([]TerminalSession, error) {
	rows, err := s.db.Query(`SELECT id,node_id,actor,remote_addr,user,shell,status,reason,created_at,started_at,ended_at FROM terminal_sessions `+clause, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]TerminalSession, 0)
	for rows.Next() {
		var item TerminalSession
		var created string
		var started, ended sql.NullString
		if err := rows.Scan(&item.ID, &item.NodeID, &item.Actor, &item.RemoteAddr, &item.User, &item.Shell, &item.Status, &item.Reason, &created, &started, &ended); err != nil {
			return nil, err
		}
		if item.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		if started.Valid {
			value, e := parseTime(started.String)
			if e != nil {
				return nil, e
			}
			item.StartedAt = &value
		}
		if ended.Valid {
			value, e := parseTime(ended.String)
			if e != nil {
				return nil, e
			}
			item.EndedAt = &value
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) RecoverTerminalSessions() error {
	sessions, err := s.terminalSessions(`WHERE ended_at IS NULL`)
	if err != nil {
		return err
	}
	for _, session := range sessions {
		ended := now()
		session.EndedAt = &ended
		session.Status = "closed"
		session.Reason = "control_restarted"
		if err := s.EndTerminalSession(session); err != nil {
			return err
		}
	}
	return nil
}
