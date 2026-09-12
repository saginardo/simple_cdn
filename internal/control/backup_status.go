package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	BackupRunStatusVersion = 1
	BackupRunRunning       = "running"
	BackupRunRetrying      = "retrying"
	BackupRunSucceeded     = "succeeded"
	BackupRunFailed        = "failed"
	BackupRunSkipped       = "skipped"
)

type BackupRunStatus struct {
	Version         int        `json:"version"`
	State           string     `json:"state"`
	Attempt         int        `json:"attempt"`
	MaxAttempts     int        `json:"max_attempts"`
	Host            string     `json:"host,omitempty"`
	StartedAt       time.Time  `json:"started_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	LastSucceededAt *time.Time `json:"last_succeeded_at,omitempty"`
	Error           string     `json:"error,omitempty"`
}

func NewBackupRunStatus(state string, attempt, maxAttempts int, host string, startedAt, updatedAt time.Time, detail string) (BackupRunStatus, error) {
	status := BackupRunStatus{
		Version:     BackupRunStatusVersion,
		State:       strings.TrimSpace(state),
		Attempt:     attempt,
		MaxAttempts: maxAttempts,
		Host:        strings.TrimSpace(host),
		StartedAt:   startedAt.UTC(),
		UpdatedAt:   updatedAt.UTC(),
		Error:       truncateBackupStatusDetail(detail),
	}
	if status.State == BackupRunSucceeded || status.State == BackupRunFailed || status.State == BackupRunSkipped {
		finishedAt := status.UpdatedAt
		status.FinishedAt = &finishedAt
	}
	if err := status.Validate(); err != nil {
		return BackupRunStatus{}, err
	}
	return status, nil
}

func (s BackupRunStatus) Validate() error {
	if s.Version != BackupRunStatusVersion {
		return fmt.Errorf("unsupported backup status version %d", s.Version)
	}
	switch s.State {
	case BackupRunRunning, BackupRunRetrying, BackupRunSucceeded, BackupRunFailed, BackupRunSkipped:
	default:
		return fmt.Errorf("invalid backup state %q", s.State)
	}
	if s.MaxAttempts < 1 || s.MaxAttempts > 10 {
		return errors.New("backup max attempts must be between 1 and 10")
	}
	if s.Attempt < 1 || s.Attempt > s.MaxAttempts {
		return errors.New("backup attempt must be between 1 and max attempts")
	}
	if s.StartedAt.IsZero() || s.UpdatedAt.IsZero() {
		return errors.New("backup status timestamps are required")
	}
	if s.UpdatedAt.Before(s.StartedAt) {
		return errors.New("backup status update cannot precede its start")
	}
	terminal := s.State == BackupRunSucceeded || s.State == BackupRunFailed || s.State == BackupRunSkipped
	if terminal != (s.FinishedAt != nil) {
		return errors.New("only terminal backup states may have a finish time")
	}
	if s.State == BackupRunFailed && s.Error == "" {
		return errors.New("failed backup status requires an error")
	}
	if s.State != BackupRunFailed && s.State != BackupRunRetrying && s.Error != "" {
		return errors.New("backup error is only valid for failed or retrying states")
	}
	return nil
}

func WriteBackupRunStatus(path string, status BackupRunStatus) error {
	if err := status.Validate(); err != nil {
		return err
	}
	path = filepath.Clean(path)
	if status.State == BackupRunSucceeded {
		finished := *status.FinishedAt
		status.LastSucceededAt = &finished
	} else if previous, err := ReadBackupRunStatus(path); err == nil {
		status.LastSucceededAt = previous.LastSucceededAt
		if status.LastSucceededAt == nil && previous.State == BackupRunSucceeded {
			status.LastSucceededAt = previous.FinishedAt
		}
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return fmt.Errorf("create backup status directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".backup-status-*")
	if err != nil {
		return fmt.Errorf("create backup status: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return fmt.Errorf("secure backup status: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(status); err != nil {
		temporary.Close()
		return fmt.Errorf("encode backup status: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync backup status: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close backup status: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("install backup status: %w", err)
	}
	return nil
}

func ReadBackupRunStatus(path string) (BackupRunStatus, error) {
	contents, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return BackupRunStatus{}, err
	}
	var status BackupRunStatus
	if err := json.Unmarshal(contents, &status); err != nil {
		return BackupRunStatus{}, fmt.Errorf("decode backup status: %w", err)
	}
	if err := status.Validate(); err != nil {
		return BackupRunStatus{}, fmt.Errorf("validate backup status: %w", err)
	}
	return status, nil
}

func truncateBackupStatusDetail(value string) string {
	value = strings.TrimSpace(value)
	const limit = 4096
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
