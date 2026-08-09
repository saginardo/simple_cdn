package control

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type OnlineRestoreApplyConfig struct {
	Root             string
	DataDir          string
	TLSDir           string
	ControlTLSDomain string
	Cipher           *Cipher
	ClickHouse       ClickHouseRestoreAdmin
	ReadyTimeout     time.Duration
	ApplyTimeout     time.Duration
	Now              func() time.Time
}

type restorePathSwap struct {
	live      string
	staged    string
	backup    string
	hadLive   bool
	absent    bool
	installed bool
}

type restoreTLSCutover struct {
	root      string
	stage     string
	backup    string
	backedUp  []string
	installed []string
}

var errOnlineRestoreManualRollback = errors.New("online restore filesystem state requires manual rollback")

func ApplyPendingOnlineRestore(ctx context.Context, config OnlineRestoreApplyConfig) (bool, error) {
	if strings.TrimSpace(config.Root) == "" {
		return false, nil
	}
	config.Root = filepath.Clean(config.Root)
	job, err := readOnlineRestoreJob(config.Root)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	maintenancePath := onlineRestoreMaintenancePath(config.Root)
	if job.State != OnlineRestoreCommitting {
		if _, lockErr := os.Stat(maintenancePath); lockErr == nil && job.State == OnlineRestoreFailed {
			return false, errors.New("online restore maintenance lock is retained after an incomplete rollback")
		}
		_ = removeOnlineRestoreMaintenanceLock(config.Root, job.ID)
		return false, nil
	}
	if config.Cipher == nil || config.ClickHouse == nil {
		return false, errors.New("online restore apply cipher and ClickHouse client are required")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.ReadyTimeout <= 0 {
		config.ReadyTimeout = 2 * time.Minute
	}
	if config.ApplyTimeout <= 0 {
		config.ApplyTimeout = 30 * time.Minute
	}
	if config.DataDir == "" {
		config.DataDir = "/var/lib/cdn-platform"
	}
	if config.TLSDir == "" {
		config.TLSDir = "/var/lib/cdn-control-tls"
	}
	applyContext, cancel := context.WithTimeout(ctx, config.ApplyTimeout)
	defer cancel()
	operationLock, err := acquireOnlineRestoreOperationLock(applyContext, config.Root)
	if err != nil {
		return false, fmt.Errorf("acquire online restore operation lock: %w", err)
	}
	defer operationLock.Close()
	if err := waitForRestoreClickHouse(applyContext, config.ClickHouse, job.Database, config.ReadyTimeout); err != nil {
		return false, failPendingOnlineRestore(config, job, err, nil)
	}

	jobRoot := filepath.Join(config.Root, "jobs", job.ID)
	artifacts, err := verifyOnlineRestoreArtifactHashes(jobRoot, *job)
	if err != nil {
		return false, failPendingOnlineRestore(config, job, err, nil)
	}
	revalidated, err := validateOnlineRestoreSnapshot(jobRoot, config.Cipher, config.ControlTLSDomain)
	if err != nil {
		return false, failPendingOnlineRestore(config, job, err, nil)
	}
	if revalidated.DatabaseSHA256 != job.DatabaseSHA256 || revalidated.SecretsSHA256 != job.SecretsSHA256 || revalidated.TLSSHA256 != job.TLSSHA256 || revalidated.CAFingerprint != job.CAFingerprint {
		return false, failPendingOnlineRestore(config, job, errors.New("restore artifacts no longer match the verified job"), nil)
	}
	if err := config.ClickHouse.ValidateDatabase(applyContext, job.TemporaryDatabase); err != nil {
		return false, failPendingOnlineRestore(config, job, fmt.Errorf("temporary ClickHouse database: %w", err), nil)
	}

	dataStage := filepath.Join(config.DataDir, ".online-restore-"+job.ID)
	tlsStage := filepath.Join(config.TLSDir, ".online-restore-"+job.ID)
	_ = os.RemoveAll(dataStage)
	_ = os.RemoveAll(tlsStage)
	if err := os.MkdirAll(dataStage, 0o700); err != nil {
		return false, failPendingOnlineRestore(config, job, err, nil)
	}
	defer os.RemoveAll(dataStage)
	defer os.RemoveAll(tlsStage)
	stagedDatabase := filepath.Join(dataStage, "control.db")
	if err := copyRestoreFile(artifacts.DatabasePath, stagedDatabase, 0o600); err != nil {
		return false, failPendingOnlineRestore(config, job, err, nil)
	}
	secretsStage := filepath.Join(dataStage, "secrets")
	if err := extractRestoreArchive(artifacts.SecretsArchive, secretsStage); err != nil {
		return false, failPendingOnlineRestore(config, job, err, nil)
	}
	if err := extractRestoreArchive(artifacts.TLSArchive, tlsStage); err != nil {
		return false, failPendingOnlineRestore(config, job, err, nil)
	}

	clickHousePromoted, oldDatabaseRenamed, err := promoteRestoreClickHouse(applyContext, config.ClickHouse, job)
	if err != nil {
		rollbackErr := rollbackRestoreClickHouse(applyContext, config.ClickHouse, job, clickHousePromoted, oldDatabaseRenamed)
		manualRollbackErr := error(nil)
		if errors.Is(err, errOnlineRestoreManualRollback) {
			manualRollbackErr = err
		}
		return false, failPendingOnlineRestore(config, job, err, errors.Join(manualRollbackErr, rollbackErr))
	}
	job.Phase = "clickhouse_promoted"
	job.UpdatedAt = config.Now().UTC()
	if err := writeOnlineRestoreJob(config.Root, *job); err != nil {
		rollbackErr := rollbackRestoreClickHouse(applyContext, config.ClickHouse, job, clickHousePromoted, oldDatabaseRenamed)
		return false, failPendingOnlineRestore(config, job, err, rollbackErr)
	}

	databaseSwap := restorePathSwap{
		live:   filepath.Join(config.DataDir, "control.db"),
		staged: stagedDatabase,
		backup: filepath.Join(config.DataDir, "control.db.before-restore-"+job.ID),
	}
	databaseWALSwap := restorePathSwap{
		live:   filepath.Join(config.DataDir, "control.db-wal"),
		staged: filepath.Join(dataStage, "control.db-wal"),
		backup: filepath.Join(config.DataDir, "control.db-wal.before-restore-"+job.ID),
	}
	databaseSHMSwap := restorePathSwap{
		live:   filepath.Join(config.DataDir, "control.db-shm"),
		staged: filepath.Join(dataStage, "control.db-shm"),
		backup: filepath.Join(config.DataDir, "control.db-shm.before-restore-"+job.ID),
	}
	pkiSwap := restorePathSwap{
		live:   filepath.Join(config.DataDir, "pki"),
		staged: filepath.Join(secretsStage, "pki"),
		backup: filepath.Join(config.DataDir, "pki.before-restore-"+job.ID),
	}
	letsencryptSwap := restorePathSwap{
		live:   filepath.Join(config.DataDir, "letsencrypt"),
		staged: filepath.Join(secretsStage, "letsencrypt"),
		backup: filepath.Join(config.DataDir, "letsencrypt.before-restore-"+job.ID),
	}
	nginxArtifactsSwap := restorePathSwap{
		live:   filepath.Join(config.DataDir, "nginx-artifacts"),
		staged: filepath.Join(secretsStage, "nginx-artifacts"),
		backup: filepath.Join(config.DataDir, "nginx-artifacts.before-restore-"+job.ID),
	}
	tlsCutover := restoreTLSCutover{
		root:   config.TLSDir,
		stage:  tlsStage,
		backup: filepath.Join(config.TLSDir, "before-restore-"+job.ID),
	}
	var completedSwaps []*restorePathSwap
	rollbackFiles := func() error {
		var failures []error
		if err := tlsCutover.rollback(); err != nil {
			failures = append(failures, err)
		}
		for index := len(completedSwaps) - 1; index >= 0; index-- {
			if err := completedSwaps[index].rollback(); err != nil {
				failures = append(failures, err)
			}
		}
		return errors.Join(failures...)
	}
	for _, swap := range []*restorePathSwap{&databaseWALSwap, &databaseSHMSwap, &databaseSwap, &pkiSwap, &letsencryptSwap, &nginxArtifactsSwap} {
		if err := swap.apply(); err != nil {
			currentRollbackErr := swap.rollback()
			manualRollbackErr := error(nil)
			if errors.Is(err, errOnlineRestoreManualRollback) {
				manualRollbackErr = err
			}
			fileRollbackErr := errors.Join(manualRollbackErr, currentRollbackErr, rollbackFiles())
			clickHouseRollbackErr := rollbackRestoreClickHouse(applyContext, config.ClickHouse, job, clickHousePromoted, oldDatabaseRenamed)
			return false, failPendingOnlineRestore(config, job, err, errors.Join(fileRollbackErr, clickHouseRollbackErr))
		}
		completedSwaps = append(completedSwaps, swap)
	}
	if err := tlsCutover.apply(); err != nil {
		manualRollbackErr := error(nil)
		if errors.Is(err, errOnlineRestoreManualRollback) {
			manualRollbackErr = err
		}
		fileRollbackErr := errors.Join(manualRollbackErr, rollbackFiles())
		clickHouseRollbackErr := rollbackRestoreClickHouse(applyContext, config.ClickHouse, job, clickHousePromoted, oldDatabaseRenamed)
		return false, failPendingOnlineRestore(config, job, err, errors.Join(fileRollbackErr, clickHouseRollbackErr))
	}
	if err := validatePromotedRestore(config, *job); err != nil {
		fileRollbackErr := rollbackFiles()
		clickHouseRollbackErr := rollbackRestoreClickHouse(applyContext, config.ClickHouse, job, clickHousePromoted, oldDatabaseRenamed)
		return false, failPendingOnlineRestore(config, job, err, errors.Join(fileRollbackErr, clickHouseRollbackErr))
	}

	now := config.Now().UTC()
	job.State = OnlineRestoreCompleted
	job.Phase = "completed"
	job.Detail = "Verified S3 snapshot was applied successfully. Previous data is retained for rollback."
	job.Error = ""
	job.UpdatedAt = now
	job.FinishedAt = &now
	if err := writeOnlineRestoreJob(config.Root, *job); err != nil {
		return false, err
	}
	if err := removeOnlineRestoreMaintenanceLock(config.Root, job.ID); err != nil {
		return false, err
	}
	_ = os.RemoveAll(jobRoot)
	return true, nil
}

func waitForRestoreClickHouse(ctx context.Context, clickHouse ClickHouseRestoreAdmin, database string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, err := clickHouse.DatabaseExists(ctx, database)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ClickHouse did not become ready within %s: %w", timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func promoteRestoreClickHouse(ctx context.Context, clickHouse ClickHouseRestoreAdmin, job *OnlineRestoreJob) (promoted, oldRenamed bool, err error) {
	database := job.Database
	temporaryExists, err := clickHouse.DatabaseExists(ctx, job.TemporaryDatabase)
	if err != nil {
		return false, false, err
	}
	currentExists, err := clickHouse.DatabaseExists(ctx, database)
	if err != nil {
		return false, false, err
	}
	rollbackExists, err := clickHouse.DatabaseExists(ctx, job.RollbackDatabase)
	if err != nil {
		return false, false, err
	}
	if !temporaryExists {
		if currentExists && rollbackExists {
			if err := clickHouse.ValidateDatabase(ctx, database); err != nil {
				return false, true, err
			}
			return true, true, nil
		}
		return false, false, errors.New("verified temporary ClickHouse database no longer exists")
	}
	if rollbackExists {
		return false, false, errors.New("ClickHouse rollback database already exists before cutover")
	}
	if currentExists {
		if err := clickHouse.ValidateDatabase(ctx, database); err != nil {
			return false, false, fmt.Errorf("current ClickHouse database: %w", err)
		}
		renamed, err := renameRestoreDatabaseObserved(ctx, clickHouse, database, job.RollbackDatabase)
		if err != nil {
			return false, renamed, err
		}
		oldRenamed = renamed
	}
	promoted, err = renameRestoreDatabaseObserved(ctx, clickHouse, job.TemporaryDatabase, database)
	if err != nil {
		return promoted, oldRenamed, err
	}
	if err := clickHouse.ValidateDatabase(ctx, database); err != nil {
		return true, oldRenamed, err
	}
	return true, oldRenamed, nil
}

func renameRestoreDatabaseObserved(ctx context.Context, clickHouse ClickHouseRestoreAdmin, source, target string) (bool, error) {
	renameErr := clickHouse.RenameDatabase(ctx, source, target)
	if renameErr == nil {
		return true, nil
	}
	sourceExists, sourceErr := clickHouse.DatabaseExists(ctx, source)
	targetExists, targetErr := clickHouse.DatabaseExists(ctx, target)
	if sourceErr != nil || targetErr != nil {
		return false, errors.Join(renameErr, fmt.Errorf("%w: cannot inspect ClickHouse rename %s to %s: %v", errOnlineRestoreManualRollback, source, target, errors.Join(sourceErr, targetErr)))
	}
	if !sourceExists && targetExists {
		return true, nil
	}
	if sourceExists && !targetExists {
		return false, renameErr
	}
	return false, errors.Join(renameErr, fmt.Errorf("%w: ambiguous ClickHouse rename %s to %s", errOnlineRestoreManualRollback, source, target))
}

func rollbackRestoreClickHouse(ctx context.Context, clickHouse ClickHouseRestoreAdmin, job *OnlineRestoreJob, promoted, oldRenamed bool) error {
	rollbackContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	var failures []error
	if promoted {
		if err := clickHouse.RenameDatabase(rollbackContext, job.Database, job.TemporaryDatabase); err != nil {
			failures = append(failures, fmt.Errorf("demote restored ClickHouse database: %w", err))
			return errors.Join(failures...)
		}
	}
	if oldRenamed {
		if err := clickHouse.RenameDatabase(rollbackContext, job.RollbackDatabase, job.Database); err != nil {
			failures = append(failures, fmt.Errorf("restore previous ClickHouse database: %w", err))
		}
	}
	return errors.Join(failures...)
}

func (s *restorePathSwap) apply() error {
	_, backupErr := os.Lstat(s.backup)
	backupExists := backupErr == nil
	if backupErr != nil && !errors.Is(backupErr, os.ErrNotExist) {
		return backupErr
	}
	_, liveErr := os.Lstat(s.live)
	liveExists := liveErr == nil
	if liveErr != nil && !errors.Is(liveErr, os.ErrNotExist) {
		return liveErr
	}
	_, stagedErr := os.Lstat(s.staged)
	stagedExists := stagedErr == nil
	if stagedErr != nil && !errors.Is(stagedErr, os.ErrNotExist) {
		return stagedErr
	}
	absentMarker := s.backup + ".absent"
	_, markerErr := os.Lstat(absentMarker)
	markerExists := markerErr == nil
	if markerErr != nil && !errors.Is(markerErr, os.ErrNotExist) {
		return markerErr
	}
	if backupExists && markerExists {
		return fmt.Errorf("%w: both rollback data and an absence marker exist for %s", errOnlineRestoreManualRollback, s.live)
	}
	if backupExists || markerExists {
		if !stagedExists {
			if !liveExists {
				return fmt.Errorf("%w: staged and live data are both missing for %s", errOnlineRestoreManualRollback, s.live)
			}
			s.hadLive = backupExists
			s.absent = markerExists
			s.installed = true
			return nil
		}
		if liveExists {
			equivalent, err := restorePathsEquivalent(s.live, s.staged)
			if err != nil {
				return fmt.Errorf("%w: compare live and staged data for %s: %v", errOnlineRestoreManualRollback, s.live, err)
			}
			if !equivalent {
				return fmt.Errorf("%w: live and staged data differ for %s", errOnlineRestoreManualRollback, s.live)
			}
			s.hadLive = backupExists
			s.absent = markerExists
			s.installed = true
			if err := os.RemoveAll(s.staged); err != nil {
				return fmt.Errorf("%w: remove duplicate staged data for %s: %v", errOnlineRestoreManualRollback, s.live, err)
			}
			return nil
		}
		s.hadLive = backupExists
		s.absent = markerExists
		if err := os.Rename(s.staged, s.live); err != nil {
			return err
		}
		s.installed = true
		return nil
	}
	if liveExists {
		if err := os.Rename(s.live, s.backup); err != nil {
			return err
		}
		s.hadLive = true
	} else if stagedExists {
		marker, err := os.OpenFile(absentMarker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		s.absent = true
		if err := marker.Close(); err != nil {
			return err
		}
	}
	if stagedExists {
		if err := os.Rename(s.staged, s.live); err != nil {
			return err
		}
		s.installed = true
	}
	return nil
}

func (s *restorePathSwap) rollback() error {
	if s.installed {
		if err := os.Rename(s.live, s.staged); err != nil {
			return err
		}
	}
	if s.hadLive {
		return os.Rename(s.backup, s.live)
	}
	if s.absent {
		if err := os.Remove(s.backup + ".absent"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (c *restoreTLSCutover) apply() error {
	if backupInfo, err := os.Stat(c.backup); err == nil && backupInfo.IsDir() {
		stagedEntries, stageErr := os.ReadDir(c.stage)
		if stageErr != nil {
			return fmt.Errorf("%w: inspect staged control TLS data: %v", errOnlineRestoreManualRollback, stageErr)
		}
		if len(stagedEntries) == 0 {
			if err := c.captureAppliedState(); err != nil {
				return fmt.Errorf("%w: inspect applied control TLS data: %v", errOnlineRestoreManualRollback, err)
			}
			return nil
		}
		equivalent, compareErr := c.liveMatchesStage(stagedEntries)
		if compareErr != nil {
			return fmt.Errorf("%w: compare live and staged control TLS data: %v", errOnlineRestoreManualRollback, compareErr)
		}
		if !equivalent {
			return fmt.Errorf("%w: partial control TLS restore", errOnlineRestoreManualRollback)
		}
		if err := c.captureAppliedState(); err != nil {
			return fmt.Errorf("%w: inspect applied control TLS data: %v", errOnlineRestoreManualRollback, err)
		}
		for _, entry := range stagedEntries {
			if err := os.RemoveAll(filepath.Join(c.stage, entry.Name())); err != nil {
				return fmt.Errorf("%w: remove duplicate staged TLS data: %v", errOnlineRestoreManualRollback, err)
			}
		}
		return nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(c.backup, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return err
	}
	stageName := filepath.Base(c.stage)
	backupName := filepath.Base(c.backup)
	for _, entry := range entries {
		name := entry.Name()
		if name == stageName || name == backupName || strings.HasPrefix(name, "before-restore-") {
			continue
		}
		if err := os.Rename(filepath.Join(c.root, name), filepath.Join(c.backup, name)); err != nil {
			return err
		}
		c.backedUp = append(c.backedUp, name)
	}
	stagedEntries, err := os.ReadDir(c.stage)
	if err != nil {
		return err
	}
	for _, entry := range stagedEntries {
		name := entry.Name()
		if err := os.Rename(filepath.Join(c.stage, name), filepath.Join(c.root, name)); err != nil {
			return err
		}
		c.installed = append(c.installed, name)
	}
	return nil
}

func (c *restoreTLSCutover) captureAppliedState() error {
	backupEntries, err := os.ReadDir(c.backup)
	if err != nil {
		return err
	}
	for _, entry := range backupEntries {
		c.backedUp = append(c.backedUp, entry.Name())
	}
	currentEntries, err := os.ReadDir(c.root)
	if err != nil {
		return err
	}
	stageName := filepath.Base(c.stage)
	backupName := filepath.Base(c.backup)
	for _, entry := range currentEntries {
		name := entry.Name()
		if name == stageName || name == backupName || strings.HasPrefix(name, "before-restore-") {
			continue
		}
		c.installed = append(c.installed, name)
	}
	return nil
}

func (c *restoreTLSCutover) liveMatchesStage(stagedEntries []os.DirEntry) (bool, error) {
	stagedNames := make(map[string]struct{}, len(stagedEntries))
	for _, entry := range stagedEntries {
		stagedNames[entry.Name()] = struct{}{}
		equivalent, err := restorePathsEquivalent(filepath.Join(c.root, entry.Name()), filepath.Join(c.stage, entry.Name()))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return false, nil
			}
			return false, err
		}
		if !equivalent {
			return false, nil
		}
	}
	currentEntries, err := os.ReadDir(c.root)
	if err != nil {
		return false, err
	}
	stageName := filepath.Base(c.stage)
	backupName := filepath.Base(c.backup)
	for _, entry := range currentEntries {
		name := entry.Name()
		if name == stageName || name == backupName || strings.HasPrefix(name, "before-restore-") {
			continue
		}
		if _, found := stagedNames[name]; !found {
			return false, nil
		}
	}
	return true, nil
}

func (c *restoreTLSCutover) rollback() error {
	var failures []error
	for index := len(c.installed) - 1; index >= 0; index-- {
		name := c.installed[index]
		if err := os.Rename(filepath.Join(c.root, name), filepath.Join(c.stage, name)); err != nil {
			failures = append(failures, err)
		}
	}
	for index := len(c.backedUp) - 1; index >= 0; index-- {
		name := c.backedUp[index]
		if err := os.Rename(filepath.Join(c.backup, name), filepath.Join(c.root, name)); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) == 0 {
		_ = os.Remove(c.backup)
	}
	return errors.Join(failures...)
}

func restorePathsEquivalent(left, right string) (bool, error) {
	leftDigest, err := restorePathDigest(left)
	if err != nil {
		return false, err
	}
	rightDigest, err := restorePathDigest(right)
	if err != nil {
		return false, err
	}
	return leftDigest == rightDigest, nil
}

func restorePathDigest(root string) ([sha256.Size]byte, error) {
	digest := sha256.New()
	writeEntry := func(path string, info os.FileInfo) error {
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(digest, "%s\x00%d\x00", filepath.ToSlash(relative), info.Mode())
		switch {
		case info.Mode().IsRegular():
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(digest, file)
			closeErr := file.Close()
			return errors.Join(copyErr, closeErr)
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_, _ = io.WriteString(digest, target)
		}
		return nil
	}
	info, err := os.Lstat(root)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	if !info.IsDir() {
		if err := writeEntry(root, info); err != nil {
			return [sha256.Size]byte{}, err
		}
		var result [sha256.Size]byte
		copy(result[:], digest.Sum(nil))
		return result, nil
	}
	err = filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		return writeEntry(path, info)
	})
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	var result [sha256.Size]byte
	copy(result[:], digest.Sum(nil))
	return result, nil
}

func validatePromotedRestore(config OnlineRestoreApplyConfig, job OnlineRestoreJob) error {
	if hash, err := fileSHA256(filepath.Join(config.DataDir, "control.db")); err != nil {
		return err
	} else if hash != job.DatabaseSHA256 {
		return errors.New("promoted SQLite database hash does not match the verified snapshot")
	}
	certificate, err := os.ReadFile(filepath.Join(config.DataDir, "pki", "edge-ca.crt"))
	if err != nil {
		return err
	}
	fingerprint, err := CertificateFingerprintPEM(certificate)
	if err != nil {
		return err
	}
	if fingerprint != job.CAFingerprint {
		return errors.New("promoted internal CA does not match the verified snapshot")
	}
	if domain := strings.TrimSpace(config.ControlTLSDomain); domain != "" {
		_, err := tls.LoadX509KeyPair(filepath.Join(config.TLSDir, "live", domain, "fullchain.pem"), filepath.Join(config.TLSDir, "live", domain, "privkey.pem"))
		if err != nil {
			return fmt.Errorf("promoted control TLS key pair: %w", err)
		}
	}
	return nil
}

func failPendingOnlineRestore(config OnlineRestoreApplyConfig, job *OnlineRestoreJob, failure, rollbackFailure error) error {
	now := config.Now().UTC()
	job.State = OnlineRestoreFailed
	job.Phase = "rolled_back"
	job.Detail = "Online restore failed; live data was not changed or was rolled back."
	job.Error = truncateRestoreDetail(failure.Error())
	job.UpdatedAt = now
	job.FinishedAt = &now
	if rollbackFailure != nil {
		job.Phase = "rollback_failed"
		job.Detail = "Online restore failed and automatic rollback was incomplete. Maintenance lock is retained."
		job.Error = truncateRestoreDetail(errors.Join(failure, rollbackFailure).Error())
	}
	writeErr := writeOnlineRestoreJob(config.Root, *job)
	if rollbackFailure == nil {
		_ = removeOnlineRestoreMaintenanceLock(config.Root, job.ID)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = config.ClickHouse.DropDatabase(ctx, job.TemporaryDatabase)
		_ = os.RemoveAll(filepath.Join(config.Root, "jobs", job.ID))
	}
	return errors.Join(failure, rollbackFailure, writeErr)
}

func copyRestoreFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return err
	}
	if err := output.Sync(); err != nil {
		output.Close()
		return err
	}
	return output.Close()
}
