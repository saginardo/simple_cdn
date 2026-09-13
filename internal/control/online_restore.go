package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/project"
)

const (
	OnlineRestoreJobVersion  = 2
	OnlineRestoreQueued      = "queued"
	OnlineRestoreDownloading = "downloading"
	OnlineRestoreValidating  = "validating"
	OnlineRestoreReady       = "ready"
	OnlineRestoreCommitting  = "committing"
	OnlineRestoreCompleted   = "completed"
	OnlineRestoreFailed      = "failed"
	OnlineRestoreCancelled   = "cancelled"
	OnlineRestoreVerified    = "verified"
)

const onlineRestoreJobVersionWithoutStaticAssets = 1

var (
	errOnlineRestoreActive          = errors.New("an online restore is already active")
	errResticSnapshotID             = errors.New("a full 64-character Restic snapshot ID is required")
	errResticSnapshotConfirmation   = errors.New("confirmation must match the snapshot short ID")
	errOnlineRestoreSnapshotMissing = errors.New("backup snapshot was not found")
)

type OnlineRestoreSnapshot struct {
	ID       string    `json:"id"`
	ShortID  string    `json:"short_id"`
	Time     time.Time `json:"time"`
	Hostname string    `json:"hostname,omitempty"`
	Paths    []string  `json:"paths,omitempty"`
	Tags     []string  `json:"tags,omitempty"`
}

type OnlineRestoreJob struct {
	Version            int        `json:"version"`
	ID                 string     `json:"id"`
	SnapshotID         string     `json:"snapshot_id"`
	SnapshotShortID    string     `json:"snapshot_short_id"`
	SnapshotTime       time.Time  `json:"snapshot_time,omitempty"`
	VerifyOnly         bool       `json:"verify_only,omitempty"`
	State              string     `json:"state"`
	Phase              string     `json:"phase,omitempty"`
	Detail             string     `json:"detail,omitempty"`
	Error              string     `json:"error,omitempty"`
	SchemaVersion      int        `json:"schema_version,omitempty"`
	TemporaryDatabase  string     `json:"temporary_database,omitempty"`
	RollbackDatabase   string     `json:"rollback_database,omitempty"`
	DatabaseSHA256     string     `json:"database_sha256,omitempty"`
	SecretsSHA256      string     `json:"secrets_sha256,omitempty"`
	TLSSHA256          string     `json:"tls_sha256,omitempty"`
	StaticAssetsSHA256 string     `json:"static_assets_sha256,omitempty"`
	CAFingerprint      string     `json:"ca_fingerprint,omitempty"`
	Database           string     `json:"database,omitempty"`
	SourceDatabase     string     `json:"source_database,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	ReadyAt            *time.Time `json:"ready_at,omitempty"`
	FinishedAt         *time.Time `json:"finished_at,omitempty"`
}

type OnlineRestoreManagerConfig struct {
	Root                  string
	Settings              *SettingsManager
	Cipher                *Cipher
	ClickHouse            ClickHouseRestoreAdmin
	Database              string
	ResticBinary          string
	ControlTLSDomain      string
	ClickHouseGroupID     int
	RestoreTimeout        time.Duration
	SnapshotListTimeout   time.Duration
	SnapshotDeleteTimeout time.Duration
	QuiesceTimeout        time.Duration
	Now                   func() time.Time
}

type OnlineRestoreManager struct {
	config OnlineRestoreManagerConfig

	mu            sync.Mutex
	snapshotMu    sync.Mutex
	job           *OnlineRestoreJob
	jobCancel     context.CancelFunc
	operationLock *os.File
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
}

func NewOnlineRestoreManager(config OnlineRestoreManagerConfig) (*OnlineRestoreManager, error) {
	if config.Settings == nil || config.Cipher == nil || config.ClickHouse == nil {
		return nil, errors.New("online restore settings, cipher, and ClickHouse client are required")
	}
	if strings.TrimSpace(config.Root) == "" {
		return nil, errors.New("online restore root is required")
	}
	config.Root = filepath.Clean(config.Root)
	if config.ResticBinary == "" {
		config.ResticBinary = "restic"
	}
	if strings.TrimSpace(config.Database) == "" {
		config.Database = project.ClickHouseDatabase
	}
	if config.RestoreTimeout <= 0 {
		config.RestoreTimeout = 2 * time.Hour
	}
	if config.SnapshotListTimeout <= 0 {
		config.SnapshotListTimeout = time.Minute
	}
	if config.SnapshotDeleteTimeout <= 0 {
		config.SnapshotDeleteTimeout = 30 * time.Minute
	}
	if config.QuiesceTimeout <= 0 {
		config.QuiesceTimeout = 2 * time.Minute
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if err := os.MkdirAll(config.Root, 0o2750); err != nil {
		return nil, fmt.Errorf("create online restore root: %w", err)
	}
	if err := os.Chmod(config.Root, 0o2750); err != nil {
		return nil, fmt.Errorf("secure online restore root: %w", err)
	}
	if config.ClickHouseGroupID >= 0 {
		if err := os.Chown(config.Root, -1, config.ClickHouseGroupID); err != nil {
			return nil, fmt.Errorf("share online restore root with ClickHouse: %w", err)
		}
	}
	manager := &OnlineRestoreManager{config: config}
	manager.ctx, manager.cancel = context.WithCancel(context.Background())
	job, err := readOnlineRestoreJob(config.Root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		manager.cancel()
		return nil, err
	}
	if job != nil {
		manager.job = job
		if job.Version != OnlineRestoreJobVersion && onlineRestoreActive(job.State) {
			now := config.Now().UTC()
			manager.job.State = OnlineRestoreFailed
			manager.job.Phase = "incompatible_job"
			manager.job.Error = fmt.Sprintf("restore job format version %d predates managed static asset restore coverage", job.Version)
			manager.job.Detail = "The restore must be started again with the current control version. Live data was not changed."
			manager.job.UpdatedAt = now
			manager.job.FinishedAt = &now
			if err := writeOnlineRestoreJob(config.Root, *manager.job); err != nil {
				manager.cancel()
				return nil, err
			}
			cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
			_ = config.ClickHouse.DropDatabase(cleanupContext, manager.job.TemporaryDatabase)
			cleanupCancel()
			_ = os.RemoveAll(manager.jobRoot(manager.job.ID))
			_ = removeOnlineRestoreMaintenanceLock(config.Root, manager.job.ID)
			return manager, nil
		}
		switch job.State {
		case OnlineRestoreQueued, OnlineRestoreDownloading, OnlineRestoreValidating:
			now := config.Now().UTC()
			manager.job.State = OnlineRestoreFailed
			manager.job.Error = "restore preparation was interrupted by a control-plane restart"
			manager.job.Detail = "Preparation must be started again. Live data was not changed."
			manager.job.UpdatedAt = now
			manager.job.FinishedAt = &now
			if err := writeOnlineRestoreJob(config.Root, *manager.job); err != nil {
				manager.cancel()
				return nil, err
			}
			cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
			_ = config.ClickHouse.DropDatabase(cleanupContext, manager.job.TemporaryDatabase)
			cleanupCancel()
			_ = os.RemoveAll(manager.jobRoot(manager.job.ID))
		case OnlineRestoreCommitting:
			manager.cancel()
			return nil, errors.New("pending online restore cutover was not applied before server initialization")
		default:
			_ = removeOnlineRestoreMaintenanceLock(config.Root, job.ID)
		}
	}
	return manager, nil
}

func (m *OnlineRestoreManager) Stop() {
	m.cancel()
	m.mu.Lock()
	if m.jobCancel != nil {
		m.jobCancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
	m.mu.Lock()
	if m.operationLock != nil {
		_ = m.operationLock.Close()
		m.operationLock = nil
	}
	m.mu.Unlock()
}

func (m *OnlineRestoreManager) Current() *OnlineRestoreJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job == nil {
		return nil
	}
	copy := *m.job
	return &copy
}

func (m *OnlineRestoreManager) ListSnapshots(ctx context.Context) ([]OnlineRestoreSnapshot, error) {
	runtime := m.config.Settings.BackupRuntime()
	if err := domain.ValidateBackupSettings(runtime.Settings, runtime.SecretAccessKey, runtime.ResticPassword); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, m.config.SnapshotListTimeout)
	defer cancel()
	runtimeDir, cleanup, err := m.resticRuntime(runtime)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	output, err := m.runRestic(ctx, runtime, runtimeDir, "snapshots", "--no-lock", "--json", "--tag", "cdn-control-compose")
	if err != nil {
		return nil, err
	}
	var snapshots []OnlineRestoreSnapshot
	if err := json.Unmarshal(output, &snapshots); err != nil {
		return nil, fmt.Errorf("decode Restic snapshots: %w", err)
	}
	for index := range snapshots {
		if snapshots[index].ShortID == "" && len(snapshots[index].ID) >= 8 {
			snapshots[index].ShortID = snapshots[index].ID[:8]
		}
	}
	sort.Slice(snapshots, func(left, right int) bool { return snapshots[left].Time.After(snapshots[right].Time) })
	if len(snapshots) > 200 {
		snapshots = snapshots[:200]
	}
	return snapshots, nil
}

func (m *OnlineRestoreManager) DeleteSnapshot(ctx context.Context, snapshotID, confirmation string) (OnlineRestoreSnapshot, error) {
	snapshotID = strings.ToLower(strings.TrimSpace(snapshotID))
	confirmation = strings.ToLower(strings.TrimSpace(confirmation))
	if !validResticSnapshotID(snapshotID) {
		return OnlineRestoreSnapshot{}, errResticSnapshotID
	}
	if confirmation != snapshotID[:8] {
		return OnlineRestoreSnapshot{}, errResticSnapshotConfirmation
	}

	m.snapshotMu.Lock()
	defer m.snapshotMu.Unlock()
	m.mu.Lock()
	active := m.job != nil && onlineRestoreActive(m.job.State)
	m.mu.Unlock()
	if active {
		return OnlineRestoreSnapshot{}, errOnlineRestoreActive
	}

	runtime := m.config.Settings.BackupRuntime()
	if err := domain.ValidateBackupSettings(runtime.Settings, runtime.SecretAccessKey, runtime.ResticPassword); err != nil {
		return OnlineRestoreSnapshot{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, m.config.SnapshotDeleteTimeout)
	defer cancel()
	operationLock, err := acquireOnlineRestoreOperationLock(ctx, m.config.Root)
	if err != nil {
		return OnlineRestoreSnapshot{}, fmt.Errorf("wait for backup and certificate operations to finish: %w", err)
	}
	defer operationLock.Close()

	snapshots, err := m.listSnapshotsWithRuntime(ctx, runtime)
	if err != nil {
		return OnlineRestoreSnapshot{}, err
	}
	var selected OnlineRestoreSnapshot
	for _, snapshot := range snapshots {
		if strings.EqualFold(snapshot.ID, snapshotID) {
			selected = snapshot
			break
		}
	}
	if selected.ID == "" {
		return OnlineRestoreSnapshot{}, errOnlineRestoreSnapshotMissing
	}
	if selected.ShortID == "" {
		selected.ShortID = snapshotID[:8]
	}
	runtimeDir, cleanup, err := m.resticRuntime(runtime)
	if err != nil {
		return OnlineRestoreSnapshot{}, err
	}
	defer cleanup()
	if _, err := m.runRestic(ctx, runtime, runtimeDir, "forget", snapshotID, "--prune"); err != nil {
		return OnlineRestoreSnapshot{}, err
	}
	return selected, nil
}

func (m *OnlineRestoreManager) Start(snapshotID, confirmation string) (OnlineRestoreJob, error) {
	return m.start(snapshotID, confirmation, false)
}

func (m *OnlineRestoreManager) StartVerification(snapshotID string) (OnlineRestoreJob, error) {
	if !validResticSnapshotID(snapshotID) {
		return OnlineRestoreJob{}, errResticSnapshotID
	}
	return m.start(snapshotID, snapshotID[:8], true)
}

func (m *OnlineRestoreManager) start(snapshotID, confirmation string, verifyOnly bool) (OnlineRestoreJob, error) {
	if m.ctx.Err() != nil {
		return OnlineRestoreJob{}, m.ctx.Err()
	}
	snapshotID = strings.ToLower(strings.TrimSpace(snapshotID))
	confirmation = strings.ToLower(strings.TrimSpace(confirmation))
	if !validResticSnapshotID(snapshotID) {
		return OnlineRestoreJob{}, errResticSnapshotID
	}
	if confirmation != snapshotID[:8] {
		return OnlineRestoreJob{}, errResticSnapshotConfirmation
	}
	m.snapshotMu.Lock()
	defer m.snapshotMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.jobCancel != nil || (m.job != nil && onlineRestoreActive(m.job.State)) {
		return OnlineRestoreJob{}, errOnlineRestoreActive
	}
	if m.job != nil {
		_ = os.RemoveAll(m.jobRoot(m.job.ID))
	}
	id, err := newOnlineRestoreID()
	if err != nil {
		return OnlineRestoreJob{}, err
	}
	now := m.config.Now().UTC()
	job := OnlineRestoreJob{
		Version:           OnlineRestoreJobVersion,
		ID:                id,
		SnapshotID:        snapshotID,
		SnapshotShortID:   snapshotID[:8],
		VerifyOnly:        verifyOnly,
		State:             OnlineRestoreQueued,
		Detail:            "Waiting to download and validate the selected snapshot.",
		Database:          m.config.Database,
		TemporaryDatabase: m.config.Database + "_restore_" + id,
		RollbackDatabase:  m.config.Database + "_before_restore_" + id,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := writeOnlineRestoreJob(m.config.Root, job); err != nil {
		return OnlineRestoreJob{}, err
	}
	m.job = &job
	jobContext, cancel := context.WithTimeout(m.ctx, m.config.RestoreTimeout)
	m.jobCancel = cancel
	m.wg.Add(1)
	go m.stage(jobContext, job.ID)
	return job, nil
}

func (m *OnlineRestoreManager) Commit(jobID, confirmation string) (OnlineRestoreJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job == nil || m.job.ID != jobID {
		return OnlineRestoreJob{}, errors.New("restore job was not found")
	}
	if m.job.State != OnlineRestoreReady {
		return OnlineRestoreJob{}, fmt.Errorf("restore job is %s, not ready", m.job.State)
	}
	if m.job.VerifyOnly {
		return OnlineRestoreJob{}, errors.New("a verification job cannot replace live data")
	}
	if strings.TrimSpace(confirmation) != "RESTORE" {
		return OnlineRestoreJob{}, errors.New("confirmation must be RESTORE")
	}
	jobRoot := m.jobRoot(jobID)
	if _, err := verifyOnlineRestoreArtifactHashes(jobRoot, *m.job); err != nil {
		return OnlineRestoreJob{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := m.config.ClickHouse.ValidateDatabase(ctx, m.job.TemporaryDatabase); err != nil {
		return OnlineRestoreJob{}, fmt.Errorf("revalidate temporary ClickHouse database: %w", err)
	}
	if err := writeOnlineRestoreMaintenanceLock(m.config.Root, m.job.ID); err != nil {
		return OnlineRestoreJob{}, err
	}
	lockContext, cancelLock := context.WithTimeout(context.Background(), m.config.QuiesceTimeout)
	defer cancelLock()
	operationLock, err := acquireOnlineRestoreOperationLock(lockContext, m.config.Root)
	if err != nil {
		_ = removeOnlineRestoreMaintenanceLock(m.config.Root, m.job.ID)
		return OnlineRestoreJob{}, fmt.Errorf("wait for backup and certificate operations to finish: %w", err)
	}
	m.operationLock = operationLock
	now := m.config.Now().UTC()
	m.job.State = OnlineRestoreCommitting
	m.job.Detail = "Control plane is restarting to apply the verified restore."
	m.job.Error = ""
	m.job.UpdatedAt = now
	if err := writeOnlineRestoreJob(m.config.Root, *m.job); err != nil {
		_ = m.operationLock.Close()
		m.operationLock = nil
		_ = removeOnlineRestoreMaintenanceLock(m.config.Root, m.job.ID)
		return OnlineRestoreJob{}, err
	}
	copy := *m.job
	return copy, nil
}

func (m *OnlineRestoreManager) Cancel(jobID string) (OnlineRestoreJob, error) {
	m.mu.Lock()
	if m.job == nil || m.job.ID != jobID {
		m.mu.Unlock()
		return OnlineRestoreJob{}, errors.New("restore job was not found")
	}
	if m.job.State == OnlineRestoreCommitting || m.job.State == OnlineRestoreCompleted || m.job.State == OnlineRestoreVerified {
		state := m.job.State
		m.mu.Unlock()
		return OnlineRestoreJob{}, fmt.Errorf("restore job cannot be cancelled while %s", state)
	}
	preparing := m.job.State == OnlineRestoreQueued || m.job.State == OnlineRestoreDownloading || m.job.State == OnlineRestoreValidating
	if m.jobCancel != nil {
		m.jobCancel()
	}
	now := m.config.Now().UTC()
	m.job.State = OnlineRestoreCancelled
	m.job.Detail = "Restore preparation was cancelled. Live data was not changed."
	m.job.Error = ""
	m.job.UpdatedAt = now
	m.job.FinishedAt = &now
	job := *m.job
	err := writeOnlineRestoreJob(m.config.Root, job)
	m.mu.Unlock()
	_ = removeOnlineRestoreMaintenanceLock(m.config.Root, job.ID)
	if preparing {
		return job, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_ = m.config.ClickHouse.DropDatabase(ctx, job.TemporaryDatabase)
	_ = os.RemoveAll(m.jobRoot(job.ID))
	return job, err
}

func (m *OnlineRestoreManager) stage(ctx context.Context, jobID string) {
	defer m.wg.Done()
	defer func() {
		m.mu.Lock()
		m.jobCancel = nil
		cleanup := m.job != nil && m.job.ID == jobID && (m.job.State == OnlineRestoreCancelled || m.job.State == OnlineRestoreFailed)
		temporaryDatabase := ""
		if m.job != nil && m.job.ID == jobID {
			temporaryDatabase = m.job.TemporaryDatabase
		}
		m.mu.Unlock()
		if cleanup {
			cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
			_ = m.config.ClickHouse.DropDatabase(cleanupContext, temporaryDatabase)
			cleanupCancel()
			_ = os.RemoveAll(m.jobRoot(jobID))
		}
	}()
	job, ok := m.updateJob(jobID, func(job *OnlineRestoreJob) {
		job.State = OnlineRestoreDownloading
		job.Detail = "Downloading the encrypted Restic snapshot."
	})
	if !ok {
		return
	}
	runtime := m.config.Settings.BackupRuntime()
	if err := domain.ValidateBackupSettings(runtime.Settings, runtime.SecretAccessKey, runtime.ResticPassword); err != nil {
		m.failJob(jobID, err)
		return
	}
	snapshots, err := m.listSnapshotsWithRuntime(ctx, runtime)
	if err != nil {
		m.failJob(jobID, err)
		return
	}
	found := false
	for _, snapshot := range snapshots {
		if strings.EqualFold(snapshot.ID, job.SnapshotID) {
			found = true
			if _, ok := m.updateJob(jobID, func(job *OnlineRestoreJob) { job.SnapshotTime = snapshot.Time }); !ok {
				return
			}
			break
		}
	}
	if !found {
		m.failJob(jobID, errors.New("selected snapshot is not tagged as a cdn-control-compose backup"))
		return
	}
	jobRoot := m.jobRoot(jobID)
	if err := os.MkdirAll(jobRoot, 0o2750); err != nil {
		m.failJob(jobID, err)
		return
	}
	runtimeDir, cleanupRuntime, err := m.resticRuntime(runtime)
	if err != nil {
		m.failJob(jobID, err)
		return
	}
	defer cleanupRuntime()
	snapshotRoot := filepath.Join(jobRoot, "snapshot")
	if _, err := m.runRestic(ctx, runtime, runtimeDir, "restore", job.SnapshotID, "--target", snapshotRoot); err != nil {
		m.failJob(jobID, err)
		return
	}
	if _, ok := m.updateJob(jobID, func(job *OnlineRestoreJob) {
		job.State = OnlineRestoreValidating
		job.Detail = "Validating SQLite, encryption, certificates, managed static assets, and ClickHouse backup data."
	}); !ok {
		return
	}
	artifacts, err := validateOnlineRestoreSnapshot(jobRoot, m.config.Cipher, m.config.ControlTLSDomain)
	if err != nil {
		m.failJob(jobID, err)
		return
	}
	if err := prepareClickHouseBackupPermissions(m.config.Root, artifacts.ClickHouseBackup, m.config.ClickHouseGroupID); err != nil {
		m.failJob(jobID, err)
		return
	}
	backupRelativePath, err := filepath.Rel(artifacts.SnapshotRoot, artifacts.ClickHouseBackup)
	if err != nil {
		m.failJob(jobID, err)
		return
	}
	diskPath := filepath.ToSlash(filepath.Join("online-restore", "jobs", jobID, "snapshot", backupRelativePath))
	if err := m.config.ClickHouse.DropDatabase(ctx, job.TemporaryDatabase); err != nil {
		m.failJob(jobID, err)
		return
	}
	if err := m.config.ClickHouse.RestoreDatabase(ctx, artifacts.ClickHouseDatabase, job.TemporaryDatabase, diskPath); err != nil {
		m.failJob(jobID, fmt.Errorf("restore temporary ClickHouse database: %w", err))
		return
	}
	if err := m.config.ClickHouse.ValidateDatabase(ctx, job.TemporaryDatabase); err != nil {
		_ = m.config.ClickHouse.DropDatabase(context.Background(), job.TemporaryDatabase)
		m.failJob(jobID, fmt.Errorf("validate temporary ClickHouse database: %w", err))
		return
	}
	if job.VerifyOnly {
		if err := m.config.ClickHouse.DropDatabase(ctx, job.TemporaryDatabase); err != nil {
			m.failJob(jobID, fmt.Errorf("clean up verified ClickHouse database: %w", err))
			return
		}
		if err := os.RemoveAll(jobRoot); err != nil {
			m.failJob(jobID, fmt.Errorf("clean up verified snapshot: %w", err))
			return
		}
		m.updateJob(jobID, func(job *OnlineRestoreJob) {
			now := m.config.Now().UTC()
			job.State = OnlineRestoreVerified
			job.Detail = "Isolated recovery verification passed; temporary data was removed."
			job.SchemaVersion = artifacts.SchemaVersion
			job.FinishedAt = &now
		})
		return
	}
	m.updateJob(jobID, func(job *OnlineRestoreJob) {
		now := m.config.Now().UTC()
		job.State = OnlineRestoreReady
		job.Detail = "Snapshot is verified and restored to a temporary ClickHouse database. Live data is unchanged."
		job.SchemaVersion = artifacts.SchemaVersion
		job.SourceDatabase = artifacts.ClickHouseDatabase
		job.DatabaseSHA256 = artifacts.DatabaseSHA256
		job.SecretsSHA256 = artifacts.SecretsSHA256
		job.TLSSHA256 = artifacts.TLSSHA256
		job.StaticAssetsSHA256 = artifacts.StaticAssetsSHA256
		job.CAFingerprint = artifacts.CAFingerprint
		job.ReadyAt = &now
	})
}

func (m *OnlineRestoreManager) listSnapshotsWithRuntime(ctx context.Context, runtime BackupRuntime) ([]OnlineRestoreSnapshot, error) {
	runtimeDir, cleanup, err := m.resticRuntime(runtime)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	output, err := m.runRestic(ctx, runtime, runtimeDir, "snapshots", "--no-lock", "--json", "--tag", "cdn-control-compose")
	if err != nil {
		return nil, err
	}
	var snapshots []OnlineRestoreSnapshot
	if err := json.Unmarshal(output, &snapshots); err != nil {
		return nil, fmt.Errorf("decode Restic snapshots: %w", err)
	}
	return snapshots, nil
}

func (m *OnlineRestoreManager) resticRuntime(runtime BackupRuntime) (string, func(), error) {
	runtimeDir, err := os.MkdirTemp(m.config.Root, ".restic-runtime-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(runtimeDir) }
	passwordPath := filepath.Join(runtimeDir, "repository-password")
	if err := os.WriteFile(passwordPath, []byte(runtime.ResticPassword), 0o600); err != nil {
		cleanup()
		return "", nil, err
	}
	return runtimeDir, cleanup, nil
}

func (m *OnlineRestoreManager) runRestic(ctx context.Context, runtime BackupRuntime, runtimeDir string, arguments ...string) ([]byte, error) {
	passwordPath := filepath.Join(runtimeDir, "repository-password")
	command := resticCommandContext(ctx, m.config.ResticBinary, arguments...)
	command.Env = backupCommandEnvironment(runtime, runtimeDir, passwordPath)
	output, err := command.CombinedOutput()
	if err == nil {
		return output, nil
	}
	detail := redactRestoreError(string(output), runtime)
	if detail == "" {
		detail = err.Error()
	}
	return nil, fmt.Errorf("Restic %s failed: %s", arguments[0], detail)
}

func (m *OnlineRestoreManager) updateJob(jobID string, update func(*OnlineRestoreJob)) (OnlineRestoreJob, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job == nil || m.job.ID != jobID || m.job.State == OnlineRestoreCancelled {
		return OnlineRestoreJob{}, false
	}
	update(m.job)
	m.job.UpdatedAt = m.config.Now().UTC()
	if err := writeOnlineRestoreJob(m.config.Root, *m.job); err != nil {
		now := m.config.Now().UTC()
		m.job.State = OnlineRestoreFailed
		m.job.Detail = "Restore preparation failed. Live data was not changed."
		m.job.Error = truncateRestoreDetail(err.Error())
		m.job.UpdatedAt = now
		m.job.FinishedAt = &now
		_ = writeOnlineRestoreJob(m.config.Root, *m.job)
		return *m.job, false
	}
	return *m.job, true
}

func (m *OnlineRestoreManager) failJob(jobID string, failure error) {
	m.mu.Lock()
	if m.job == nil || m.job.ID != jobID || m.job.State == OnlineRestoreCancelled {
		m.mu.Unlock()
		return
	}
	now := m.config.Now().UTC()
	m.job.State = OnlineRestoreFailed
	m.job.Detail = "Restore preparation failed. Live data was not changed."
	m.job.Error = truncateRestoreDetail(failure.Error())
	m.job.UpdatedAt = now
	m.job.FinishedAt = &now
	job := *m.job
	_ = writeOnlineRestoreJob(m.config.Root, job)
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_ = m.config.ClickHouse.DropDatabase(ctx, job.TemporaryDatabase)
	_ = os.RemoveAll(m.jobRoot(job.ID))
}

func (m *OnlineRestoreManager) jobRoot(jobID string) string {
	return filepath.Join(m.config.Root, "jobs", jobID)
}

func onlineRestoreActive(state string) bool {
	switch state {
	case OnlineRestoreQueued, OnlineRestoreDownloading, OnlineRestoreValidating, OnlineRestoreReady, OnlineRestoreCommitting:
		return true
	default:
		return false
	}
}

func validResticSnapshotID(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func newOnlineRestoreID() (string, error) {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func redactRestoreError(value string, runtime BackupRuntime) string {
	for _, secret := range []string{runtime.SecretAccessKey, runtime.ResticPassword} {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	return truncateRestoreDetail(strings.Join(strings.Fields(value), " "))
}

func truncateRestoreDetail(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 2000 {
		value = value[:2000]
	}
	return value
}

func onlineRestoreJobPath(root string) string {
	return filepath.Join(root, "restore-job.json")
}

func onlineRestoreMaintenancePath(root string) string {
	return filepath.Join(root, "maintenance.lock")
}

func onlineRestoreOperationLockPath(root string) string {
	return filepath.Join(root, "operations.lock")
}

func acquireOnlineRestoreOperationLock(ctx context.Context, root string) (*os.File, error) {
	file, err := os.OpenFile(onlineRestoreOperationLockPath(root), os.O_CREATE|os.O_RDWR, 0o660)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0o660); err != nil {
		file.Close()
		return nil, err
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func readOnlineRestoreJob(root string) (*OnlineRestoreJob, error) {
	contents, err := os.ReadFile(onlineRestoreJobPath(root))
	if err != nil {
		return nil, err
	}
	var job OnlineRestoreJob
	if err := json.Unmarshal(contents, &job); err != nil {
		return nil, fmt.Errorf("decode online restore job: %w", err)
	}
	if job.Database == "" {
		job.Database = project.ClickHouseDatabase
		if job.State == OnlineRestoreCommitting {
			job.Database = project.LegacyClickHouseDatabase
		}
	}
	if job.SourceDatabase == "" {
		job.SourceDatabase = project.LegacyClickHouseDatabase
	}
	knownVersion := job.Version == onlineRestoreJobVersionWithoutStaticAssets || job.Version == OnlineRestoreJobVersion
	requiresStaticAssetDigest := job.Version == OnlineRestoreJobVersion && (job.State == OnlineRestoreReady || job.State == OnlineRestoreCommitting)
	if !knownVersion ||
		(requiresStaticAssetDigest && !domain.ValidStaticAssetSHA256(job.StaticAssetsSHA256)) ||
		!validRestoreIdentifier(job.Database) ||
		!validRestoreIdentifier(job.SourceDatabase) ||
		!validRestoreIdentifier(job.TemporaryDatabase) ||
		!validRestoreIdentifier(job.RollbackDatabase) {
		return nil, errors.New("online restore job is invalid")
	}
	return &job, nil
}

func writeOnlineRestoreJob(root string, job OnlineRestoreJob) error {
	if err := writeOnlineRestoreJSON(onlineRestoreJobPath(root), job, 0o600); err != nil {
		return err
	}
	if job.VerifyOnly {
		return recordBackupVerification(root, job)
	}
	return nil
}

func writeOnlineRestoreMaintenanceLock(root, jobID string) error {
	path := onlineRestoreMaintenancePath(root)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	installed := false
	defer func() {
		if !installed {
			_ = os.Remove(path)
		}
	}()
	if err := json.NewEncoder(file).Encode(map[string]string{"job_id": jobID}); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	installed = true
	return nil
}

func removeOnlineRestoreMaintenanceLock(root, jobID string) error {
	path := onlineRestoreMaintenancePath(root)
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var marker struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(contents, &marker); err != nil {
		return fmt.Errorf("decode online restore maintenance marker: %w", err)
	}
	if marker.JobID == "" || marker.JobID != jobID {
		return errors.New("online restore maintenance marker is owned by another operation")
	}
	return os.Remove(path)
}

func writeOnlineRestoreJSON(path string, value any, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o2750); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".restore-state-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}
