package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/integrations"
)

type BackupVerificationStatus struct {
	JobID                    string     `json:"job_id,omitempty"`
	State                    string     `json:"state"`
	SnapshotID               string     `json:"snapshot_id,omitempty"`
	SnapshotTime             time.Time  `json:"snapshot_time,omitempty"`
	StartedAt                time.Time  `json:"started_at,omitempty"`
	FinishedAt               *time.Time `json:"finished_at,omitempty"`
	Error                    string     `json:"error,omitempty"`
	LastVerifiedAt           *time.Time `json:"last_verified_at,omitempty"`
	LastVerifiedSnapshotID   string     `json:"last_verified_snapshot_id,omitempty"`
	LastVerifiedSnapshotTime *time.Time `json:"last_verified_snapshot_time,omitempty"`
}

type BackupHealthStatus struct {
	State               string                   `json:"state"`
	Summary             string                   `json:"summary"`
	ObservedAt          time.Time                `json:"observed_at"`
	LastSucceededAt     *time.Time               `json:"last_succeeded_at,omitempty"`
	ExpiresAt           *time.Time               `json:"expires_at,omitempty"`
	Verification        BackupVerificationStatus `json:"verification"`
	VerificationDueAt   *time.Time               `json:"verification_due_at,omitempty"`
	VerificationOverdue bool                     `json:"verification_overdue"`
}

type BackupHealthManager struct {
	Server               *Server
	MaxAge               time.Duration
	StalledAfter         time.Duration
	VerificationInterval time.Duration
	RetryInterval        time.Duration
	Now                  func() time.Time
	workMu               sync.Mutex
}

func backupVerificationPath(root string) string {
	return filepath.Join(root, "backup-verification.json")
}

func readBackupVerification(root string) (BackupVerificationStatus, error) {
	contents, err := os.ReadFile(backupVerificationPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return BackupVerificationStatus{State: "never"}, nil
	}
	if err != nil {
		return BackupVerificationStatus{}, err
	}
	var status BackupVerificationStatus
	if err := json.Unmarshal(contents, &status); err != nil {
		return status, err
	}
	return status, nil
}

// Keep the last proven recovery point when a later attempt fails or is cancelled.
// This file survives replacement of the current manual/automatic restore job.
func recordBackupVerification(root string, job OnlineRestoreJob) error {
	previous, err := readBackupVerification(root)
	if err != nil {
		return fmt.Errorf("read recovery verification history: %w", err)
	}
	status := BackupVerificationStatus{
		JobID: job.ID, State: job.State, SnapshotID: job.SnapshotID, SnapshotTime: job.SnapshotTime,
		StartedAt: job.CreatedAt, FinishedAt: job.FinishedAt, Error: job.Error,
		LastVerifiedAt: previous.LastVerifiedAt, LastVerifiedSnapshotID: previous.LastVerifiedSnapshotID,
		LastVerifiedSnapshotTime: previous.LastVerifiedSnapshotTime,
	}
	if job.State == OnlineRestoreVerified && job.FinishedAt != nil {
		status.LastVerifiedAt = job.FinishedAt
		status.LastVerifiedSnapshotID = job.SnapshotID
		status.LastVerifiedSnapshotTime = &job.SnapshotTime
	}
	return writeOnlineRestoreJSON(backupVerificationPath(root), status, 0o600)
}

func (m *BackupHealthManager) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}
func (m *BackupHealthManager) verificationInterval() time.Duration {
	if m.VerificationInterval > 0 {
		return m.VerificationInterval
	}
	return 7 * 24 * time.Hour
}
func (m *BackupHealthManager) retryInterval() time.Duration {
	if m.RetryInterval > 0 {
		return m.RetryInterval
	}
	return time.Hour
}

func evaluateBackupHealth(status *BackupRunStatus, readErr error, configured bool, now time.Time, maxAge, stalledAfter time.Duration) BackupHealthStatus {
	result := BackupHealthStatus{State: "unknown", Summary: "尚无成功备份记录", ObservedAt: now}
	if !configured {
		result.State, result.Summary = "disabled", "尚未配置备份"
		return result
	}
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		result.Summary = "备份状态无法读取"
		return result
	}
	if status == nil {
		return result
	}
	if status.UpdatedAt.After(now.Add(5 * time.Minute)) {
		result.Summary = "备份状态时间异常"
		return result
	}
	result.LastSucceededAt = status.LastSucceededAt
	if result.LastSucceededAt == nil && status.State == BackupRunSucceeded {
		result.LastSucceededAt = status.FinishedAt
	}
	if result.LastSucceededAt != nil {
		if result.LastSucceededAt.After(now.Add(5 * time.Minute)) {
			result.Summary = "备份状态时间异常"
			return result
		}
		expires := result.LastSucceededAt.Add(maxAge)
		result.ExpiresAt = &expires
	}
	active := status.State == BackupRunRunning || status.State == BackupRunRetrying
	if active && !now.Before(status.StartedAt.Add(stalledAfter)) {
		result.State, result.Summary = "stalled", "备份任务长时间未完成"
		return result
	}
	if result.ExpiresAt != nil && !now.Before(*result.ExpiresAt) {
		result.State, result.Summary = "stale", "最近成功备份已过期"
		return result
	}
	if status.State == BackupRunFailed {
		result.State, result.Summary = "failed", "最近一次备份失败"
		return result
	}
	if active {
		result.State, result.Summary = "running", "备份正在执行"
		return result
	}
	if result.LastSucceededAt != nil {
		result.State, result.Summary = "healthy", "最近成功备份仍在有效期内"
	}
	return result
}

func (m *BackupHealthManager) Status() BackupHealthStatus {
	now := m.now()
	maxAge, stalled := m.MaxAge, m.StalledAfter
	if maxAge <= 0 {
		maxAge = 26 * time.Hour
	}
	if stalled <= 0 {
		stalled = 6 * time.Hour
	}
	configured := false
	if m.Server.Settings != nil {
		runtime := m.Server.Settings.BackupRuntime()
		configured = domain.ValidateBackupSettings(runtime.Settings, runtime.SecretAccessKey, runtime.ResticPassword) == nil
		maxAge += time.Duration(runtime.Settings.RandomDelaySeconds) * time.Second
	}
	status, err := ReadBackupRunStatus(m.Server.BackupStatusPath)
	var run *BackupRunStatus
	if err == nil {
		run = &status
	}
	result := evaluateBackupHealth(run, err, configured, now, maxAge, stalled)
	result.Verification.State = "unavailable"
	if m.Server.OnlineRestore != nil {
		verification, err := readBackupVerification(m.Server.OnlineRestore.config.Root)
		if err != nil {
			verification.State, verification.Error = "unknown", "恢复校验记录无法读取"
		}
		result.Verification = verification
		if verification.LastVerifiedAt != nil {
			due := verification.LastVerifiedAt.Add(m.verificationInterval())
			result.VerificationDueAt = &due
		}
		result.VerificationOverdue = configured && (result.VerificationDueAt == nil || !now.Before(*result.VerificationDueAt))
	}
	return result
}

func (m *BackupHealthManager) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := m.Tick(ctx); err != nil && ctx.Err() == nil && m.Server.Logger != nil {
			m.Server.Logger.Warn("backup health check failed", "error", err)
		}
		timer := time.NewTimer(time.Minute)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (m *BackupHealthManager) Tick(ctx context.Context) error {
	m.workMu.Lock()
	defer m.workMu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	status := m.Status()
	if status.State == "disabled" {
		return nil
	}
	if status.State == "stale" || status.State == "stalled" || status.State == "unknown" {
		key := "missing"
		if run, err := ReadBackupRunStatus(m.Server.BackupStatusPath); err == nil {
			key = run.StartedAt.Format(time.RFC3339Nano)
		}
		if err := m.notify(ctx, key, status.State, status.Summary); err != nil {
			return err
		}
	}
	verification := status.Verification
	if verification.State == OnlineRestoreFailed || verification.State == "unknown" {
		if err := m.notify(ctx, verification.JobID, "verification_failed", "恢复校验失败，请检查备份与恢复日志"); err != nil {
			return err
		}
	}
	if m.Server.OnlineRestore == nil || (!status.VerificationOverdue && verification.State != OnlineRestoreFailed) || status.State == "running" || status.State == "stalled" {
		return nil
	}
	if current := m.Server.OnlineRestore.Current(); current != nil && onlineRestoreActive(current.State) {
		return nil
	}
	if !verification.StartedAt.IsZero() && m.now().Before(verification.StartedAt.Add(m.retryInterval())) {
		return nil
	}
	_, err := m.verifyLatest(ctx)
	if errors.Is(err, errOnlineRestoreActive) {
		return nil
	}
	return err
}

func (m *BackupHealthManager) VerifyNow(ctx context.Context) (OnlineRestoreJob, error) {
	m.workMu.Lock()
	defer m.workMu.Unlock()
	return m.verifyLatest(ctx)
}

func (m *BackupHealthManager) verifyLatest(ctx context.Context) (OnlineRestoreJob, error) {
	restore := m.Server.OnlineRestore
	if restore == nil {
		return OnlineRestoreJob{}, errors.New("recovery verification is unavailable")
	}
	if current := restore.Current(); current != nil && onlineRestoreActive(current.State) {
		return OnlineRestoreJob{}, errOnlineRestoreActive
	}
	snapshots, err := restore.ListSnapshots(ctx)
	if err == nil && len(snapshots) == 0 {
		err = errors.New("no backup snapshots are available for verification")
	}
	if err != nil {
		now := m.now()
		id, idErr := newOnlineRestoreID()
		if idErr != nil {
			return OnlineRestoreJob{}, idErr
		}
		job := OnlineRestoreJob{ID: id, State: OnlineRestoreFailed, VerifyOnly: true, CreatedAt: now, FinishedAt: &now,
			Error: redactRestoreError(err.Error(), restore.config.Settings.BackupRuntime())}
		if writeErr := recordBackupVerification(restore.config.Root, job); writeErr != nil {
			return job, errors.Join(err, writeErr)
		}
		return job, err
	}
	return restore.StartVerification(snapshots[0].ID)
}

func (m *BackupHealthManager) notify(ctx context.Context, key, state, summary string) error {
	if key == "" {
		key = "current"
	}
	if m.Server.Store == nil {
		return nil
	}
	_, created, err := m.Server.Store.CreateMessageOnce(domain.Message{Severity: domain.MessageWarning, Category: "backup",
		Title: summary, Body: "请在设置中的备份与恢复页面检查备份时效和恢复校验结果。", SourceType: "backup_health", SourceID: key,
		SourceStatus: state, CreatedAt: m.now()})
	if err != nil || !created || m.Server.Notifier == nil {
		return err
	}
	return integrations.SendNotification(ctx, m.Server.Notifier, integrations.Notification{
		Category: integrations.NotificationCategoryBackup, Severity: integrations.NotificationSeverityWarning,
		Subject: "[CDN] " + summary, Message: summary, OccurredAt: m.now(), Key: "backup-health:" + key + ":" + state, Cooldown: time.Hour,
	})
}

func (s *Server) backupHealthStatus(w http.ResponseWriter, r *http.Request) {
	if s.BackupHealth == nil {
		writeJSON(w, http.StatusOK, nil)
		return
	}
	writeJSON(w, http.StatusOK, s.BackupHealth.Status())
}
func (s *Server) verifyLatestBackup(w http.ResponseWriter, r *http.Request) {
	if s.BackupHealth == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("recovery verification is unavailable"))
		return
	}
	job, err := s.BackupHealth.VerifyNow(r.Context())
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errOnlineRestoreActive) {
			status = http.StatusConflict
		}
		writeError(w, status, err)
		return
	}
	s.audit(r, adminID(r.Context()), "verify_backup", "backup_snapshot", job.SnapshotID, job.ID)
	writeJSON(w, http.StatusAccepted, job)
}
