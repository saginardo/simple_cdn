package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"simple_cdn/internal/domain"
)

// SiteNodeDrain is an immutable snapshot of a site's last published material
// on a node that has been removed from the assignment. It remains renderable
// until DNS has had time to expire and the edge confirms its removal.
type SiteNodeDrain struct {
	ID                    string
	SiteID                string
	NodeID                string
	Site                  domain.Site
	CertificateCiphertext []byte
	KeyCiphertext         []byte
	CertificateNotAfter   *time.Time
	DNSTTLSeconds         int
	DNSReadyAt            *time.Time
	RemoveAfter           *time.Time
	CleanupTaskID         string
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// SiteNodeDrainInput is used while committing a new publication. The input is
// deliberately a complete snapshot so a later draft change cannot mutate the
// configuration that is being drained.
type SiteNodeDrainInput struct {
	ID                    string
	SiteID                string
	NodeID                string
	Site                  domain.Site
	CertificateCiphertext []byte
	KeyCiphertext         []byte
	CertificateNotAfter   *time.Time
	DNSTTLSeconds         int
}

type SiteNodeDrainKey struct {
	SiteID string
	NodeID string
}

func (s *Store) ListSiteNodeDrains() ([]SiteNodeDrain, error) {
	rows, err := s.db.Query(`SELECT id, site_id, node_id, site_json, certificate_ciphertext,
		private_key_ciphertext, certificate_not_after, dns_ttl_seconds, dns_ready_at,
		remove_after, cleanup_task_id, created_at, updated_at
		FROM site_node_drains ORDER BY site_id, node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]SiteNodeDrain, 0)
	for rows.Next() {
		drain, err := scanSiteNodeDrain(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, drain)
	}
	return result, rows.Err()
}

func (s *Store) ListSiteNodeDrainsForSite(siteID string) ([]SiteNodeDrain, error) {
	rows, err := s.db.Query(`SELECT id, site_id, node_id, site_json, certificate_ciphertext,
		private_key_ciphertext, certificate_not_after, dns_ttl_seconds, dns_ready_at,
		remove_after, cleanup_task_id, created_at, updated_at
		FROM site_node_drains WHERE site_id = ? ORDER BY node_id`, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]SiteNodeDrain, 0)
	for rows.Next() {
		drain, err := scanSiteNodeDrain(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, drain)
	}
	return result, rows.Err()
}

func (s *Store) ListDueSiteNodeDrains(at time.Time) ([]SiteNodeDrain, error) {
	rows, err := s.db.Query(`SELECT id, site_id, node_id, site_json, certificate_ciphertext,
		private_key_ciphertext, certificate_not_after, dns_ttl_seconds, dns_ready_at,
		remove_after, cleanup_task_id, created_at, updated_at
		FROM site_node_drains
		WHERE remove_after IS NOT NULL AND remove_after <= ? AND cleanup_task_id IS NULL
		ORDER BY remove_after, site_id, node_id`, stamp(at))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]SiteNodeDrain, 0)
	for rows.Next() {
		drain, err := scanSiteNodeDrain(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, drain)
	}
	return result, rows.Err()
}

// SiteNodeDrainsConverging reports whether a removed node has not yet
// confirmed the retained snapshot. DNS must not switch away while such a node
// could still receive cached answers for the old address.
func (s *Store) SiteNodeDrainsConverging(siteID string) (bool, error) {
	var converging int
	err := s.db.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM site_node_drains drains
		LEFT JOIN nodes ON nodes.id = drains.node_id
		LEFT JOIN node_states ON node_states.node_id = drains.node_id
		WHERE drains.site_id = ? AND drains.dns_ready_at IS NULL
		AND (nodes.id IS NULL OR (nodes.status = ? AND (node_states.version IS NULL
			OR nodes.applied_version < node_states.version))))`, siteID, domain.NodeActive).Scan(&converging)
	return converging != 0, err
}

func scanSiteNodeDrain(row scanner) (SiteNodeDrain, error) {
	var drain SiteNodeDrain
	var siteJSON string
	var certificateNotAfter, dnsReadyAt, removeAfter, cleanupTaskID sql.NullString
	var createdAt, updatedAt string
	if err := row.Scan(&drain.ID, &drain.SiteID, &drain.NodeID, &siteJSON,
		&drain.CertificateCiphertext, &drain.KeyCiphertext, &certificateNotAfter,
		&drain.DNSTTLSeconds, &dnsReadyAt, &removeAfter, &cleanupTaskID,
		&createdAt, &updatedAt); errors.Is(err, sql.ErrNoRows) {
		return SiteNodeDrain{}, ErrNotFound
	} else if err != nil {
		return SiteNodeDrain{}, err
	}
	if err := json.Unmarshal([]byte(siteJSON), &drain.Site); err != nil {
		return SiteNodeDrain{}, fmt.Errorf("decode site node drain %s: %w", drain.ID, err)
	}
	var err error
	if certificateNotAfter.Valid {
		value, parseErr := parseTime(certificateNotAfter.String)
		if parseErr != nil {
			return SiteNodeDrain{}, parseErr
		}
		drain.CertificateNotAfter = &value
	}
	if dnsReadyAt.Valid {
		value, parseErr := parseTime(dnsReadyAt.String)
		if parseErr != nil {
			return SiteNodeDrain{}, parseErr
		}
		drain.DNSReadyAt = &value
	}
	if removeAfter.Valid {
		value, parseErr := parseTime(removeAfter.String)
		if parseErr != nil {
			return SiteNodeDrain{}, parseErr
		}
		drain.RemoveAfter = &value
	}
	if cleanupTaskID.Valid {
		drain.CleanupTaskID = cleanupTaskID.String
	}
	drain.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return SiteNodeDrain{}, err
	}
	drain.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return SiteNodeDrain{}, err
	}
	if drain.DNSTTLSeconds < 1 {
		drain.DNSTTLSeconds = domain.DefaultDNSTTLSeconds
	}
	return drain, nil
}

// MarkSiteNodeDrainsDNSReady starts the drain clock only after the provider
// has accepted the desired DNS set. Existing timestamps are never reset.
func (s *Store) MarkSiteNodeDrainsDNSReady(siteID string, readyAt time.Time, ttlSeconds int, grace time.Duration) (int64, error) {
	return s.MarkSiteNodeDrainsDNSReadyWithTTLs(siteID, readyAt, ttlSeconds, ttlSeconds, grace)
}

// MarkSiteNodeDrainsDNSReadyWithTTLs records the provider TTL separately from
// the conservative drain TTL. The latter may include a longer historical TTL
// from the old node's DNS answer.
func (s *Store) MarkSiteNodeDrainsDNSReadyWithTTLs(siteID string, readyAt time.Time, providerTTL, drainTTL int, grace time.Duration) (int64, error) {
	if strings.TrimSpace(siteID) == "" {
		return 0, errors.New("site ID is required")
	}
	publication, err := s.SitePublication(siteID)
	if err != nil {
		return 0, err
	}
	changed, current, err := s.MarkSiteNodeDrainsDNSReadyForPublication(
		siteID, publication.PublishedAt, readyAt, providerTTL, drainTTL, grace)
	if err != nil {
		return 0, err
	}
	if !current {
		return 0, ErrSiteChanged
	}
	return changed, nil
}

// MarkSiteNodeDrainsDNSReadyForPublication starts drain timers only when the
// DNS provider call belongs to the publication generation that is still
// current. The publication check, TTL history update, and drain updates share
// one write transaction so a concurrent publication cannot slip between them.
func (s *Store) MarkSiteNodeDrainsDNSReadyForPublication(siteID string, publishedAt, readyAt time.Time, providerTTL, drainTTL int, grace time.Duration) (int64, bool, error) {
	if strings.TrimSpace(siteID) == "" {
		return 0, false, errors.New("site ID is required")
	}
	if publishedAt.IsZero() {
		return 0, false, errors.New("publication timestamp is required")
	}
	if providerTTL < 1 {
		providerTTL = domain.DefaultDNSTTLSeconds
	}
	if drainTTL < 1 {
		drainTTL = providerTTL
	}
	if drainTTL < providerTTL {
		drainTTL = providerTTL
	}
	if grace < 0 {
		return 0, false, errors.New("drain grace cannot be negative")
	}
	publishedAt = publishedAt.UTC()
	readyAt = readyAt.UTC()
	removeAfter := readyAt.Add(time.Duration(drainTTL)*time.Second + grace)
	tx, err := s.db.Begin()
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	publicationResult, err := tx.Exec(`UPDATE site_publications SET dns_ttl_seconds = ?
		WHERE site_id = ? AND published_at = ?`, providerTTL, siteID, stamp(publishedAt))
	if err != nil {
		return 0, false, err
	}
	publicationChanged, err := publicationResult.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	if publicationChanged == 0 {
		return 0, false, nil
	}
	if publicationChanged != 1 {
		return 0, false, fmt.Errorf("publication %s matched %d rows", siteID, publicationChanged)
	}
	result, err := tx.Exec(`UPDATE site_node_drains
		SET dns_ttl_seconds = ?, dns_ready_at = ?, remove_after = ?, updated_at = ?
		WHERE site_id = ? AND dns_ready_at IS NULL`, drainTTL, stamp(readyAt), stamp(removeAfter), stamp(readyAt), siteID)
	if err != nil {
		return 0, false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return changed, true, nil
}

func createSiteNodeDrainsTx(tx *sql.Tx, inputs []SiteNodeDrainInput, updatedAt time.Time) error {
	for _, input := range inputs {
		if input.SiteID == "" || input.NodeID == "" || input.Site.ID == "" || input.SiteID != input.Site.ID {
			return errors.New("invalid site node drain snapshot")
		}
		encodedSite, err := json.Marshal(input.Site)
		if err != nil {
			return err
		}
		id := input.ID
		if id == "" {
			id = uuid.NewString()
		}
		var notAfter any
		if input.CertificateNotAfter != nil {
			notAfter = stamp(*input.CertificateNotAfter)
		}
		ttl := input.DNSTTLSeconds
		if ttl < 1 {
			ttl = domain.DefaultDNSTTLSeconds
		}
		if _, err := tx.Exec(`INSERT INTO site_node_drains(id, site_id, node_id, site_json,
			certificate_ciphertext, private_key_ciphertext, certificate_not_after,
			dns_ttl_seconds, dns_ready_at, remove_after, cleanup_task_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, NULL, ?, ?)
			ON CONFLICT(site_id, node_id) DO NOTHING`, id, input.SiteID, input.NodeID, string(encodedSite),
			input.CertificateCiphertext, input.KeyCiphertext, notAfter, ttl,
			stamp(updatedAt), stamp(updatedAt)); err != nil {
			return err
		}
	}
	return nil
}

func cancelSiteNodeDrainsTx(tx *sql.Tx, keys []SiteNodeDrainKey, updatedAt time.Time) error {
	for _, key := range keys {
		if key.SiteID == "" || key.NodeID == "" {
			return errors.New("invalid site node drain key")
		}
		// A new publication supersedes a pending cleanup task. Desired-state
		// versions are monotonic, so an in-flight old target cannot overwrite it.
		if _, err := tx.Exec(`UPDATE deployment_tasks SET status = ?, detail = ?, updated_at = ?
			WHERE id = (SELECT cleanup_task_id FROM site_node_drains WHERE site_id = ? AND node_id = ?)
			AND status IN (?, ?, ?)`, domain.TaskFailed, "drain cleanup superseded by a new publication", stamp(updatedAt),
			key.SiteID, key.NodeID, domain.TaskQueued, domain.TaskDispatching, domain.TaskApplying); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM site_node_drains WHERE site_id = ? AND node_id = ?`, key.SiteID, key.NodeID); err != nil {
			return err
		}
	}
	return nil
}

// CreateOrGetActiveDrainTask serializes cleanup work per site. The task is
// intentionally separate from publish_site so the public publish status keeps
// describing the user's publication rather than an internal drain operation.
func (s *Store) CreateOrGetActiveDrainTask(siteID string, deadline time.Time) (domain.DeploymentTask, bool, error) {
	if strings.TrimSpace(siteID) == "" {
		return domain.DeploymentTask{}, false, errors.New("site ID is required")
	}
	for range 2 {
		created := now()
		task := domain.DeploymentTask{ID: uuid.NewString(), Kind: "drain_site", SiteID: siteID,
			Status: domain.TaskQueued, Detail: "preparing expired node drain cleanup",
			DeadlineAt: &deadline, CreatedAt: created, UpdatedAt: created}
		result, err := s.db.Exec(`INSERT OR IGNORE INTO deployment_tasks(id, kind, site_id, status, detail, deadline_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, task.ID, task.Kind, task.SiteID, task.Status, task.Detail,
			stamp(deadline), stamp(created), stamp(created))
		if err != nil {
			return domain.DeploymentTask{}, false, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return domain.DeploymentTask{}, false, err
		}
		if changed == 1 {
			return task, true, nil
		}
		existing, err := scanTask(s.db.QueryRow(`SELECT id, kind, site_id, status, detail, deadline_at, created_at, updated_at
			FROM deployment_tasks WHERE site_id = ? AND kind = 'drain_site' AND status IN (?, ?, ?)
			ORDER BY created_at DESC LIMIT 1`, siteID, domain.TaskQueued, domain.TaskDispatching, domain.TaskApplying))
		if err == nil {
			return existing, false, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return domain.DeploymentTask{}, false, err
		}
	}
	return domain.DeploymentTask{}, false, errors.New("drain cleanup task changed while being queued; retry request")
}

func (s *Store) StageDrainCleanup(taskID string, drainIDs []string, updates []NodeStateUpdate, targets []PublishTaskNode) error {
	if strings.TrimSpace(taskID) == "" || len(drainIDs) == 0 {
		return errors.New("drain cleanup is missing task or drain IDs")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := ensureNodesNotUpgradingTx(tx, upgradedNodeIDs(updates, targets)); err != nil {
		return err
	}
	updatedAt := now()
	for _, drainID := range drainIDs {
		if strings.TrimSpace(drainID) == "" {
			return errors.New("drain cleanup contains an empty drain ID")
		}
		result, err := tx.Exec(`UPDATE site_node_drains SET cleanup_task_id = ?, updated_at = ?
			WHERE id = ? AND cleanup_task_id IS NULL AND remove_after IS NOT NULL AND remove_after <= ?`,
			taskID, stamp(updatedAt), drainID, stamp(updatedAt))
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return fmt.Errorf("drain %s is no longer eligible for cleanup", drainID)
		}
	}
	if err := createPublishTaskNodesTx(tx, taskID, targets); err != nil {
		return err
	}
	if err := saveNodeStatesTx(tx, updates, stamp(updatedAt)); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE deployment_tasks SET status = ?, detail = ?, updated_at = ? WHERE id = ? AND status IN (?, ?, ?)`,
		domain.TaskApplying, "waiting for edge confirmation of expired drain removal", stamp(updatedAt), taskID,
		domain.TaskQueued, domain.TaskDispatching, domain.TaskApplying); err != nil {
		return err
	}
	return tx.Commit()
}

// FinishDrainCleanup removes only rows whose edge target succeeded. Failed
// rows are released for a later retry, preserving the old configuration.
func (s *Store) FinishDrainCleanup(taskID string, status domain.TaskStatus) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := finishDrainCleanupTx(tx, taskID, status, now()); err != nil {
		return err
	}
	return tx.Commit()
}

// completeDrainCleanup keeps the task's terminal status and drain ownership in
// one transaction. Otherwise another reconciliation can start in the gap and
// rebuild a node from a half-finalized set of snapshots.
func (s *Store) completeDrainCleanup(taskID string, status domain.TaskStatus, detail string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	updatedAt := now()
	result, err := tx.Exec(`UPDATE deployment_tasks SET status = ?, detail = ?, updated_at = ?
		WHERE id = ? AND kind = 'drain_site' AND status IN (?, ?, ?)`, status, detail, stamp(updatedAt), taskID,
		domain.TaskQueued, domain.TaskDispatching, domain.TaskApplying)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		// A publication may have superseded this cleanup after the active-task
		// snapshot was read. Its transaction owns the final state in that case.
		return nil
	}
	if err := finishDrainCleanupTx(tx, taskID, status, updatedAt); err != nil {
		return err
	}
	return tx.Commit()
}

func finishDrainCleanupTx(tx *sql.Tx, taskID string, status domain.TaskStatus, updatedAt time.Time) error {
	switch status {
	case domain.TaskSucceeded:
		if _, err := tx.Exec(`DELETE FROM site_node_drains WHERE cleanup_task_id = ?`, taskID); err != nil {
			return err
		}
		return nil
	case domain.TaskPartial, domain.TaskFailed, domain.TaskRolledBack:
		if _, err := tx.Exec(`DELETE FROM site_node_drains
			WHERE cleanup_task_id = ? AND EXISTS (
				SELECT 1 FROM publish_task_nodes
				WHERE publish_task_nodes.task_id = ? AND publish_task_nodes.node_id = site_node_drains.node_id
				AND publish_task_nodes.status = ?)`, taskID, taskID, domain.PublishNodeSucceeded); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE site_node_drains SET cleanup_task_id = NULL, updated_at = ? WHERE cleanup_task_id = ?`, stamp(updatedAt), taskID); err != nil {
			return err
		}
		return nil
	default:
		return fmt.Errorf("drain cleanup task %s has non-terminal status %s", taskID, status)
	}
}

func (s *Store) reconcileDrainTaskFinalizations() error {
	rows, err := s.db.Query(`SELECT id, status FROM deployment_tasks
		WHERE kind = 'drain_site' AND status IN (?, ?, ?, ?)
		AND EXISTS (SELECT 1 FROM site_node_drains WHERE cleanup_task_id = deployment_tasks.id)
		ORDER BY updated_at`, domain.TaskSucceeded, domain.TaskPartial, domain.TaskFailed, domain.TaskRolledBack)
	if err != nil {
		return err
	}
	type finalization struct {
		taskID string
		status domain.TaskStatus
	}
	finalizations := make([]finalization, 0)
	for rows.Next() {
		var item finalization
		if err := rows.Scan(&item.taskID, &item.status); err != nil {
			rows.Close()
			return err
		}
		finalizations = append(finalizations, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range finalizations {
		if err := s.FinishDrainCleanup(item.taskID, item.status); err != nil {
			return err
		}
	}
	return nil
}

// PruneAppliedSiteNodeDrains handles a controller restart after the edge has
// already applied the removal but before the cleanup task was finalized.
func (s *Store) PruneAppliedSiteNodeDrains(drainIDs []string) (int64, error) {
	if len(drainIDs) == 0 {
		return 0, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var removed int64
	for _, drainID := range drainIDs {
		result, err := tx.Exec(`DELETE FROM site_node_drains
			WHERE id = ? AND cleanup_task_id IS NULL
			AND EXISTS (
				SELECT 1 FROM nodes
				JOIN node_states ON node_states.node_id = nodes.id
				WHERE nodes.id = site_node_drains.node_id
				AND nodes.status = ? AND nodes.applied_version >= node_states.version)`, drainID, domain.NodeActive)
		if err != nil {
			return 0, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		removed += changed
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return removed, nil
}
