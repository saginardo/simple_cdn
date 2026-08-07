package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"simple_cdn/internal/domain"
)

var ErrCacheOperationNotRetryable = errors.New("cache operation has no completed prewarm job to retry")

type CacheOperationInput struct {
	SiteID       string
	Scope        domain.CacheInvalidationScope
	Target       string
	PrewarmPaths []string
	Actor        string
	RemoteAddr   string
}

const cacheOperationColumns = `id, site_id, site_name, kind, retry_of_id, publish_task_id,
	scope, target, prewarm_paths_json, cache_generation, config_version, status, detail,
	actor, remote_addr, completed_at, created_at, updated_at`

func (s *Store) CreateCacheInvalidationOperation(input CacheOperationInput) (domain.CacheOperation, domain.Site, error) {
	input.SiteID = strings.TrimSpace(input.SiteID)
	if input.SiteID == "" {
		return domain.CacheOperation{}, domain.Site{}, errors.New("site ID is required")
	}
	if input.Scope == "" {
		input.Scope = domain.CacheInvalidationFull
	}
	if input.Scope == domain.CacheInvalidationFull {
		if strings.TrimSpace(input.Target) != "" {
			return domain.CacheOperation{}, domain.Site{}, errors.New("full cache invalidation does not accept a target")
		}
		input.Target = ""
	} else {
		target, err := domain.NormalizeCacheInvalidationTarget(input.Scope, input.Target)
		if err != nil {
			return domain.CacheOperation{}, domain.Site{}, err
		}
		input.Target = target
	}
	paths, err := domain.NormalizeCacheWarmupPaths(input.PrewarmPaths)
	if err != nil {
		return domain.CacheOperation{}, domain.Site{}, err
	}
	input.PrewarmPaths = paths

	tx, err := s.db.Begin()
	if err != nil {
		return domain.CacheOperation{}, domain.Site{}, err
	}
	defer tx.Rollback()
	if err := ensureNoActiveCachePublishTx(tx, input.SiteID); err != nil {
		return domain.CacheOperation{}, domain.Site{}, err
	}
	site, _, err := scanSite(tx.QueryRow(`SELECT `+siteSelectColumns+` FROM sites WHERE id = ?`, input.SiteID))
	if err != nil {
		return domain.CacheOperation{}, domain.Site{}, err
	}
	created := now()
	operation := domain.CacheOperation{
		ID: uuid.NewString(), SiteID: site.ID, SiteName: site.Name, Kind: domain.CacheOperationInvalidate,
		Scope: input.Scope, Target: input.Target, PrewarmPaths: paths, Status: domain.CacheOperationQueued,
		Actor: strings.TrimSpace(input.Actor), RemoteAddr: strings.TrimSpace(input.RemoteAddr),
		CreatedAt: created, UpdatedAt: created,
	}
	if err := mutateCacheInvalidationTx(tx, &site, input.Scope, input.Target, paths, operation.ID, created); err != nil {
		return domain.CacheOperation{}, domain.Site{}, err
	}
	operation.CacheGeneration = site.CacheGeneration
	operation.ConfigVersion = site.ConfigVersion
	if err := insertCacheOperationTx(tx, operation, site); err != nil {
		return domain.CacheOperation{}, domain.Site{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.CacheOperation{}, domain.Site{}, err
	}
	return operation, site, nil
}

func (s *Store) CreateCachePrewarmRetryOperation(operationID, actor, remoteAddr string) (domain.CacheOperation, domain.Site, error) {
	operationID = strings.TrimSpace(operationID)
	if operationID == "" {
		return domain.CacheOperation{}, domain.Site{}, ErrNotFound
	}
	tx, err := s.db.Begin()
	if err != nil {
		return domain.CacheOperation{}, domain.Site{}, err
	}
	defer tx.Rollback()
	original, err := scanCacheOperation(tx.QueryRow(`SELECT `+cacheOperationColumns+` FROM cache_operations WHERE id = ?`, operationID))
	if err != nil {
		return domain.CacheOperation{}, domain.Site{}, err
	}
	if len(original.PrewarmPaths) == 0 {
		return domain.CacheOperation{}, domain.Site{}, ErrCacheOperationNotRetryable
	}
	if original.PublishTaskID != "" {
		var active int
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM deployment_tasks WHERE id = ? AND status IN (?, ?, ?))`,
			original.PublishTaskID, domain.TaskQueued, domain.TaskDispatching, domain.TaskApplying).Scan(&active); err != nil {
			return domain.CacheOperation{}, domain.Site{}, err
		}
		if active != 0 {
			return domain.CacheOperation{}, domain.Site{}, ErrCacheOperationNotRetryable
		}
	}
	if err := ensureNoActiveCachePublishTx(tx, original.SiteID); err != nil {
		return domain.CacheOperation{}, domain.Site{}, err
	}
	site, _, err := scanSite(tx.QueryRow(`SELECT `+siteSelectColumns+` FROM sites WHERE id = ?`, original.SiteID))
	if err != nil {
		return domain.CacheOperation{}, domain.Site{}, err
	}
	if site.Deleting {
		return domain.CacheOperation{}, domain.Site{}, ErrSiteDeleting
	}
	if !cacheSiteAvailable(site) {
		return domain.CacheOperation{}, domain.Site{}, ErrCacheDisabled
	}
	created := now()
	operation := domain.CacheOperation{
		ID: uuid.NewString(), SiteID: site.ID, SiteName: site.Name, Kind: domain.CacheOperationPrewarmRetry,
		RetryOfID: original.ID, Scope: original.Scope, Target: original.Target,
		PrewarmPaths: append([]string(nil), original.PrewarmPaths...), Status: domain.CacheOperationQueued,
		Actor: strings.TrimSpace(actor), RemoteAddr: strings.TrimSpace(remoteAddr), CreatedAt: created, UpdatedAt: created,
	}
	if err := appendCacheWarmup(&site, operation.ID, operation.PrewarmPaths, created); err != nil {
		return domain.CacheOperation{}, domain.Site{}, err
	}
	site.ConfigVersion++
	site.Published = false
	site.UpdatedAt = created
	if err := updateCacheDraftTx(tx, site); err != nil {
		return domain.CacheOperation{}, domain.Site{}, err
	}
	operation.CacheGeneration = site.CacheGeneration
	operation.ConfigVersion = site.ConfigVersion
	if err := insertCacheOperationTx(tx, operation, site); err != nil {
		return domain.CacheOperation{}, domain.Site{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.CacheOperation{}, domain.Site{}, err
	}
	return operation, site, nil
}

func ensureNoActiveCachePublishTx(tx *sql.Tx, siteID string) error {
	var active int
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM deployment_tasks
		WHERE site_id = ? AND kind = 'publish_site' AND status IN (?, ?, ?))`, siteID,
		domain.TaskQueued, domain.TaskDispatching, domain.TaskApplying).Scan(&active); err != nil {
		return err
	}
	if active != 0 {
		return ErrSiteTaskActive
	}
	return nil
}

func mutateCacheInvalidationTx(tx *sql.Tx, site *domain.Site, scope domain.CacheInvalidationScope, target string, paths []string, warmupID string, changedAt time.Time) error {
	if site.Deleting {
		return ErrSiteDeleting
	}
	if !cacheSiteAvailable(*site) {
		return ErrCacheDisabled
	}
	nextConfigVersion := site.ConfigVersion + 1
	if scope == domain.CacheInvalidationFull {
		site.CacheGeneration++
		site.CacheInvalidations = []domain.CacheInvalidationRule{}
	} else {
		filtered := make([]domain.CacheInvalidationRule, 0, len(site.CacheInvalidations))
		for _, rule := range site.CacheInvalidations {
			if rule.Scope != scope || rule.Value != target {
				filtered = append(filtered, rule)
			}
		}
		if len(filtered) >= domain.MaxCacheInvalidationRules {
			site.CacheGeneration++
			filtered = nil
		}
		site.CacheInvalidations = append(filtered, domain.CacheInvalidationRule{
			Scope: scope, Value: target, Generation: nextConfigVersion,
		})
	}
	if len(paths) != 0 {
		if err := appendCacheWarmup(site, warmupID, paths, changedAt); err != nil {
			return err
		}
	}
	site.ConfigVersion = nextConfigVersion
	site.Published = false
	site.UpdatedAt = changedAt
	return updateCacheDraftTx(tx, *site)
}

func cacheSiteAvailable(site domain.Site) bool {
	origin := strings.ToLower(strings.TrimSpace(site.PrimaryOrigin.URL))
	return !site.Passthrough && !site.TCPOnly && site.OriginResponseBuffering &&
		(strings.HasPrefix(origin, "http://") || strings.HasPrefix(origin, "https://"))
}

func appendCacheWarmup(site *domain.Site, warmupID string, paths []string, createdAt time.Time) error {
	paths, err := domain.NormalizeCacheWarmupPaths(paths)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return nil
	}
	warmup := domain.CacheWarmup{ID: warmupID, SiteID: site.ID, Host: site.Domains[0], Paths: paths, CreatedAt: createdAt}
	if len(site.CacheWarmups) >= domain.MaxCacheWarmups {
		site.CacheWarmups = append([]domain.CacheWarmup(nil), site.CacheWarmups[len(site.CacheWarmups)-domain.MaxCacheWarmups+1:]...)
	}
	site.CacheWarmups = append(site.CacheWarmups, warmup)
	return nil
}

func updateCacheDraftTx(tx *sql.Tx, site domain.Site) error {
	rules, err := json.Marshal(site.CacheInvalidations)
	if err != nil {
		return err
	}
	warmups, err := json.Marshal(site.CacheWarmups)
	if err != nil {
		return err
	}
	result, err := tx.Exec(`UPDATE sites SET cache_generation = ?, cache_invalidations_json = ?, cache_warmups_json = ?,
		config_version = ?, published = 0, updated_at = ? WHERE id = ? AND deleting = 0`, site.CacheGeneration,
		string(rules), string(warmups), site.ConfigVersion, stamp(site.UpdatedAt), site.ID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrNotFound
	}
	return nil
}

func insertCacheOperationTx(tx *sql.Tx, operation domain.CacheOperation, site domain.Site) error {
	paths, err := json.Marshal(operation.PrewarmPaths)
	if err != nil {
		return err
	}
	var retryOf any
	if operation.RetryOfID != "" {
		retryOf = operation.RetryOfID
	}
	if _, err := tx.Exec(`INSERT INTO cache_operations(id, site_id, site_name, kind, retry_of_id,
		publish_task_id, scope, target, prewarm_paths_json, cache_generation, config_version, status,
		detail, actor, remote_addr, completed_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, '', ?, ?, NULL, ?, ?)`, operation.ID,
		operation.SiteID, operation.SiteName, operation.Kind, retryOf, operation.Scope, operation.Target,
		string(paths), operation.CacheGeneration, operation.ConfigVersion, operation.Status, operation.Actor,
		operation.RemoteAddr, stamp(operation.CreatedAt), stamp(operation.UpdatedAt)); err != nil {
		return err
	}
	for _, nodeID := range site.Nodes {
		var name, capabilitiesJSON string
		var status domain.NodeStatus
		if err := tx.QueryRow(`SELECT name, status, capabilities_json FROM nodes WHERE id = ?`, nodeID).Scan(&name, &status, &capabilitiesJSON); err != nil {
			return err
		}
		var capabilities []string
		if err := json.Unmarshal([]byte(capabilitiesJSON), &capabilities); err != nil {
			return err
		}
		warmupStatus := initialCacheWarmupStatus(status, capabilities, len(operation.PrewarmPaths) != 0)
		if _, err := tx.Exec(`INSERT INTO cache_operation_nodes(operation_id, node_id, node_name,
			target_version, configuration_status, warmup_status, attempted_urls, succeeded_urls, failures_json,
			reported_at) VALUES (?, ?, ?, 0, ?, ?, 0, 0, '[]', NULL)`, operation.ID, nodeID, name,
			domain.CacheConfigurationNotTargeted, warmupStatus); err != nil {
			return err
		}
	}
	return nil
}

func initialCacheWarmupStatus(status domain.NodeStatus, capabilities []string, requested bool) domain.CacheWarmupStatus {
	if !requested {
		return domain.CacheWarmupNotRequested
	}
	if status != domain.NodeActive {
		return domain.CacheWarmupNotTargeted
	}
	if !slices.Contains(capabilities, domain.EdgeCapabilityCacheControl) {
		return domain.CacheWarmupUnsupported
	}
	if !slices.Contains(capabilities, domain.EdgeCapabilityCacheWarmupResults) {
		return domain.CacheWarmupUnreported
	}
	return domain.CacheWarmupPending
}

func (s *Store) AttachCacheOperationTask(operationID, taskID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	task, err := scanTask(tx.QueryRow(`SELECT id, kind, site_id, status, detail, deadline_at, created_at, updated_at
		FROM deployment_tasks WHERE id = ?`, taskID))
	if err != nil {
		return err
	}
	result, err := tx.Exec(`UPDATE cache_operations SET publish_task_id = ?, status = ?, detail = ?, updated_at = ? WHERE id = ?`,
		task.ID, domain.CacheOperationApplying, task.Detail, stamp(now()), operationID)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return err
	} else if changed != 1 {
		return ErrNotFound
	}
	rows, err := tx.Query(`SELECT node_id, target_version FROM publish_task_nodes WHERE task_id = ?`, taskID)
	if err != nil {
		return err
	}
	var targets []PublishTaskNode
	for rows.Next() {
		var target PublishTaskNode
		if err := rows.Scan(&target.NodeID, &target.TargetVersion); err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, target := range targets {
		if _, err := tx.Exec(`UPDATE cache_operation_nodes SET target_version = ?, configuration_status = ?
			WHERE operation_id = ? AND node_id = ?`, target.TargetVersion, domain.CacheConfigurationPending,
			operationID, target.NodeID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) FailCacheOperation(operationID, taskID, detail string) error {
	var task any
	if strings.TrimSpace(taskID) != "" {
		task = taskID
	}
	completed := now()
	result, err := s.db.Exec(`UPDATE cache_operations SET publish_task_id = COALESCE(?, publish_task_id),
		status = ?, detail = ?, completed_at = ?, updated_at = ? WHERE id = ?`, task,
		domain.CacheOperationFailed, strings.TrimSpace(detail), stamp(completed), stamp(completed), operationID)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return err
	} else if changed != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) RecordCacheWarmupResults(nodeID string, values []domain.CacheWarmupResult) error {
	values, err := domain.NormalizeCacheWarmupResults(values)
	if err != nil {
		return err
	}
	if len(values) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var nodeName string
	if err := tx.QueryRow(`SELECT name FROM nodes WHERE id = ?`, nodeID).Scan(&nodeName); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	for _, value := range values {
		var siteID, pathsJSON string
		err := tx.QueryRow(`SELECT site_id, prewarm_paths_json FROM cache_operations WHERE id = ?`, value.WarmupID).Scan(&siteID, &pathsJSON)
		if errors.Is(err, sql.ErrNoRows) {
			// Legacy jobs are valid but have no operation history row.
			continue
		}
		if err != nil {
			return err
		}
		var paths []string
		if err := json.Unmarshal([]byte(pathsJSON), &paths); err != nil {
			return err
		}
		if value.SiteID != siteID || value.AttemptedURLs != len(paths) {
			return errors.New("cache prewarm result does not match its operation")
		}
		failures, err := json.Marshal(value.Failures)
		if err != nil {
			return err
		}
		result, err := tx.Exec(`UPDATE cache_operation_nodes SET warmup_status = ?, attempted_urls = ?,
			succeeded_urls = ?, failures_json = ?, reported_at = ? WHERE operation_id = ? AND node_id = ?`,
			value.Status, value.AttemptedURLs, value.SucceededURLs, string(failures), stamp(value.CompletedAt),
			value.WarmupID, nodeID)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil {
			return err
		} else if changed == 0 {
			// A stale job can arrive after the site assignment changed. It is
			// acknowledged but does not create a misleading target row.
			continue
		}
		if _, err := tx.Exec(`UPDATE cache_operations SET updated_at = ? WHERE id = ?`, stamp(now()), value.WarmupID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListCacheOperations(siteID string, limit int) ([]domain.CacheOperation, error) {
	if err := s.ReconcilePublishTasks(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	siteID = strings.TrimSpace(siteID)
	query := `SELECT ` + cacheOperationColumns + ` FROM cache_operations ORDER BY created_at DESC LIMIT ?`
	args := []any{limit}
	if siteID != "" {
		query = `SELECT ` + cacheOperationColumns + ` FROM cache_operations WHERE site_id = ? ORDER BY created_at DESC LIMIT ?`
		args = []any{siteID, limit}
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	operations := make([]domain.CacheOperation, 0)
	for rows.Next() {
		operation, err := scanCacheOperation(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		operations = append(operations, operation)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range operations {
		if err := s.hydrateCacheOperation(&operations[index]); err != nil {
			return nil, err
		}
	}
	return operations, nil
}

func (s *Store) GetCacheOperation(operationID string) (domain.CacheOperation, error) {
	if err := s.ReconcilePublishTasks(); err != nil {
		return domain.CacheOperation{}, err
	}
	operation, err := scanCacheOperation(s.db.QueryRow(`SELECT `+cacheOperationColumns+` FROM cache_operations WHERE id = ?`, operationID))
	if err != nil {
		return domain.CacheOperation{}, err
	}
	if err := s.hydrateCacheOperation(&operation); err != nil {
		return domain.CacheOperation{}, err
	}
	return operation, nil
}

func scanCacheOperation(row scanner) (domain.CacheOperation, error) {
	var operation domain.CacheOperation
	var retryOf, taskID, completedAt sql.NullString
	var pathsJSON, createdAt, updatedAt string
	if err := row.Scan(&operation.ID, &operation.SiteID, &operation.SiteName, &operation.Kind, &retryOf,
		&taskID, &operation.Scope, &operation.Target, &pathsJSON, &operation.CacheGeneration,
		&operation.ConfigVersion, &operation.Status, &operation.Detail, &operation.Actor, &operation.RemoteAddr,
		&completedAt, &createdAt, &updatedAt); errors.Is(err, sql.ErrNoRows) {
		return domain.CacheOperation{}, ErrNotFound
	} else if err != nil {
		return domain.CacheOperation{}, err
	}
	if err := json.Unmarshal([]byte(pathsJSON), &operation.PrewarmPaths); err != nil {
		return domain.CacheOperation{}, fmt.Errorf("decode cache operation prewarm paths: %w", err)
	}
	operation.Nodes = make([]domain.CacheOperationNode, 0)
	operation.RetryOfID = retryOf.String
	operation.PublishTaskID = taskID.String
	var err error
	operation.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return domain.CacheOperation{}, err
	}
	operation.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return domain.CacheOperation{}, err
	}
	if completedAt.Valid {
		value, err := parseTime(completedAt.String)
		if err != nil {
			return domain.CacheOperation{}, err
		}
		operation.CompletedAt = &value
	}
	return operation, nil
}

func (s *Store) hydrateCacheOperation(operation *domain.CacheOperation) error {
	rows, err := s.db.Query(`SELECT node_id, node_name, target_version, configuration_status, warmup_status,
		attempted_urls, succeeded_urls, failures_json, reported_at FROM cache_operation_nodes
		WHERE operation_id = ? ORDER BY node_name`, operation.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var node domain.CacheOperationNode
		var failuresJSON string
		var reportedAt sql.NullString
		if err := rows.Scan(&node.NodeID, &node.NodeName, &node.TargetVersion, &node.ConfigurationStatus,
			&node.WarmupStatus, &node.AttemptedURLs, &node.SucceededURLs, &failuresJSON, &reportedAt); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(failuresJSON), &node.Failures); err != nil {
			rows.Close()
			return fmt.Errorf("decode cache operation node failures: %w", err)
		}
		if node.Failures == nil {
			node.Failures = []domain.CacheWarmupFailure{}
		}
		if reportedAt.Valid {
			value, err := parseTime(reportedAt.String)
			if err != nil {
				rows.Close()
				return err
			}
			node.ReportedAt = &value
		}
		operation.Nodes = append(operation.Nodes, node)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	var task *domain.DeploymentTask
	if operation.PublishTaskID != "" {
		value, err := s.GetTask(operation.PublishTaskID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err == nil {
			task = &value
		}
		if task != nil {
			targetRows, err := s.db.Query(`SELECT node_id, target_version, status FROM publish_task_nodes WHERE task_id = ?`, task.ID)
			if err != nil {
				return err
			}
			statuses := make(map[string]struct {
				version int64
				status  domain.PublishNodeStatus
			})
			for targetRows.Next() {
				var nodeID string
				var value struct {
					version int64
					status  domain.PublishNodeStatus
				}
				if err := targetRows.Scan(&nodeID, &value.version, &value.status); err != nil {
					targetRows.Close()
					return err
				}
				statuses[nodeID] = value
			}
			if err := targetRows.Err(); err != nil {
				targetRows.Close()
				return err
			}
			if err := targetRows.Close(); err != nil {
				return err
			}
			for index := range operation.Nodes {
				node := &operation.Nodes[index]
				value, found := statuses[node.NodeID]
				if !found {
					continue
				}
				node.TargetVersion = value.version
				node.ConfigurationStatus = domain.CacheConfigurationStatus(value.status)
				if (value.status == domain.PublishNodeFailed || value.status == domain.PublishNodeTimedOut) && node.WarmupStatus == domain.CacheWarmupPending {
					node.WarmupStatus = domain.CacheWarmupSkipped
				}
			}
		}
	}

	status, detail, completedAt := cacheOperationPresentation(*operation, task)
	storedStatus, storedDetail, storedCompletedAt := operation.Status, operation.Detail, operation.CompletedAt
	operation.Status, operation.Detail, operation.CompletedAt = status, detail, completedAt
	if status != storedStatus || detail != storedDetail || !optionalTimesEqual(storedCompletedAt, completedAt) {
		updatedAt := now()
		var completed any
		if completedAt != nil {
			completed = stamp(*completedAt)
		}
		if _, err := s.db.Exec(`UPDATE cache_operations SET status = ?, detail = ?, completed_at = ?, updated_at = ? WHERE id = ?`,
			status, detail, completed, stamp(updatedAt), operation.ID); err != nil {
			return err
		}
		operation.UpdatedAt = updatedAt
	}
	return nil
}

func optionalTimesEqual(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Equal(*right)
}

func cacheOperationPresentation(operation domain.CacheOperation, task *domain.DeploymentTask) (domain.CacheOperationStatus, string, *time.Time) {
	if operation.Status == domain.CacheOperationFailed && task == nil {
		return operation.Status, operation.Detail, operation.CompletedAt
	}
	if task == nil {
		return domain.CacheOperationQueued, operation.Detail, nil
	}
	detail := task.Detail
	active := task.Status == domain.TaskQueued || task.Status == domain.TaskDispatching || task.Status == domain.TaskApplying
	if active {
		return domain.CacheOperationApplying, detail, nil
	}
	completedAt := task.UpdatedAt
	if task.Status == domain.TaskFailed || task.Status == domain.TaskRolledBack {
		return domain.CacheOperationFailed, detail, &completedAt
	}
	basePartial := task.Status == domain.TaskPartial
	if len(operation.PrewarmPaths) == 0 {
		if basePartial {
			return domain.CacheOperationPartial, detail, &completedAt
		}
		return domain.CacheOperationSucceeded, detail, &completedAt
	}
	resultPending := false
	warmupIssue := false
	for index := range operation.Nodes {
		node := &operation.Nodes[index]
		if node.ConfigurationStatus != domain.CacheConfigurationSucceeded {
			continue
		}
		switch node.WarmupStatus {
		case domain.CacheWarmupPending:
			if now().Sub(task.UpdatedAt) > 2*time.Minute {
				node.WarmupStatus = domain.CacheWarmupUnreported
				warmupIssue = true
			} else {
				resultPending = true
			}
		case domain.CacheWarmupSucceeded:
		case domain.CacheWarmupNotRequested, domain.CacheWarmupNotTargeted:
		default:
			warmupIssue = true
		}
	}
	if resultPending {
		return domain.CacheOperationApplying, "configuration applied; waiting for edge prewarm results", nil
	}
	if basePartial || warmupIssue {
		return domain.CacheOperationPartial, detail, &completedAt
	}
	return domain.CacheOperationSucceeded, detail, &completedAt
}
