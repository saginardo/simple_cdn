package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"simple_cdn/internal/domain"
)

// SitePublication is the immutable configuration and certificate material
// currently intended to be served by edge nodes. The sites table remains the
// editable draft presented by the control API.
type SitePublication struct {
	Site                  domain.Site
	CertificateCiphertext []byte
	KeyCiphertext         []byte
	CertificateNotAfter   *time.Time
	// DNSTTLSeconds is the effective TTL accepted by the provider on the most
	// recent successful DNS reconciliation. Zero means it has not been
	// observed yet (for example, on a pre-migration snapshot).
	DNSTTLSeconds int
	PublishedAt   time.Time
}

func (s *Store) SitePublication(siteID string) (SitePublication, error) {
	return scanSitePublication(s.db.QueryRow(`SELECT site_json, certificate_ciphertext, private_key_ciphertext,
		certificate_not_after, dns_ttl_seconds, published_at FROM site_publications WHERE site_id = ?`, siteID))
}

// SitePublicationCurrent verifies that a previously loaded publication is
// still the active generation. The timestamp is used as an immutable token so
// a DNS reconciliation cannot finalize drains created by a newer publication.
func (s *Store) SitePublicationCurrent(siteID string, publishedAt time.Time) (bool, error) {
	if strings.TrimSpace(siteID) == "" {
		return false, errors.New("site ID is required")
	}
	if publishedAt.IsZero() {
		return false, errors.New("publication timestamp is required")
	}
	var current int
	err := s.db.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM site_publications WHERE site_id = ? AND published_at = ?)`,
		siteID, stamp(publishedAt)).Scan(&current)
	return current != 0, err
}

func (s *Store) ListSitePublications() ([]SitePublication, error) {
	rows, err := s.db.Query(`SELECT site_json, certificate_ciphertext, private_key_ciphertext,
		certificate_not_after, dns_ttl_seconds, published_at FROM site_publications ORDER BY site_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	publications := make([]SitePublication, 0)
	for rows.Next() {
		publication, err := scanSitePublication(rows)
		if err != nil {
			return nil, err
		}
		publications = append(publications, publication)
	}
	return publications, rows.Err()
}

// CheckPublicationMigrationSafety prevents an upgraded controller from
// rebuilding around a legacy pending draft whose last published inputs were
// never stored separately. Publishing that site first creates its snapshot and
// clears the ambiguity.
func (s *Store) CheckPublicationMigrationSafety(excludedSiteID string) error {
	var siteName string
	err := s.db.QueryRow(`SELECT sites.name FROM sites
		WHERE sites.id <> ?
		AND NOT EXISTS (SELECT 1 FROM site_publications WHERE site_id = sites.id)
		AND EXISTS (
			SELECT 1 FROM deployment_tasks tasks
			WHERE tasks.site_id = sites.id AND tasks.kind = 'publish_site'
			AND (tasks.status IN (?, ?) OR EXISTS (
				SELECT 1 FROM publish_task_nodes targets WHERE targets.task_id = tasks.id
			))
		)
		ORDER BY sites.name LIMIT 1`, excludedSiteID, domain.TaskSucceeded, domain.TaskPartial).Scan(&siteName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("site %s has legacy deployed state without a published snapshot; publish it before rebuilding edge configuration", siteName)
}

func (s *Store) PublicationMigrationRequired(siteID string) (bool, error) {
	var required int
	err := s.db.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM sites
		WHERE sites.id = ?
		AND NOT EXISTS (SELECT 1 FROM site_publications WHERE site_id = sites.id)
		AND EXISTS (
			SELECT 1 FROM deployment_tasks tasks
			WHERE tasks.site_id = sites.id AND tasks.kind = 'publish_site'
			AND (tasks.status IN (?, ?) OR EXISTS (
				SELECT 1 FROM publish_task_nodes targets WHERE targets.task_id = tasks.id
			))
		)
	)`, siteID, domain.TaskSucceeded, domain.TaskPartial).Scan(&required)
	return required != 0, err
}

func scanSitePublication(row scanner) (SitePublication, error) {
	var publication SitePublication
	var siteJSON, publishedAt string
	var certificateNotAfter sql.NullString
	var dnsTTL sql.NullInt64
	if err := row.Scan(&siteJSON, &publication.CertificateCiphertext, &publication.KeyCiphertext,
		&certificateNotAfter, &dnsTTL, &publishedAt); errors.Is(err, sql.ErrNoRows) {
		return SitePublication{}, ErrNotFound
	} else if err != nil {
		return SitePublication{}, err
	}
	if err := json.Unmarshal([]byte(siteJSON), &publication.Site); err != nil {
		return SitePublication{}, fmt.Errorf("decode published site %q: %w", publication.Site.ID, err)
	}
	var err error
	publication.PublishedAt, err = parseTime(publishedAt)
	if err != nil {
		return SitePublication{}, err
	}
	if certificateNotAfter.Valid {
		notAfter, err := parseTime(certificateNotAfter.String)
		if err != nil {
			return SitePublication{}, err
		}
		publication.CertificateNotAfter = &notAfter
	}
	if dnsTTL.Valid && dnsTTL.Int64 > 0 {
		publication.DNSTTLSeconds = int(dnsTTL.Int64)
	}
	return publication, nil
}

// CommitSitePublication atomically exposes node desired states and promotes
// the current site draft. A later publication can therefore never observe a
// new node state with an old site snapshot, or the inverse.
func (s *Store) CommitSitePublication(siteID string, expectedConfigVersion int64, publishTaskID string, updates []NodeStateUpdate, targets []PublishTaskNode) (domain.Site, error) {
	return s.CommitSitePublicationWithDrains(siteID, expectedConfigVersion, publishTaskID, updates, targets, nil, nil)
}

// CommitSitePublicationWithDrains atomically promotes a publication, stages
// its desired states, and records any removed-node snapshots. Keeping these
// operations in one transaction prevents a restart from exposing a new DNS
// assignment while losing the old node's serving material.
func (s *Store) CommitSitePublicationWithDrains(siteID string, expectedConfigVersion int64, publishTaskID string, updates []NodeStateUpdate, targets []PublishTaskNode, drains []SiteNodeDrainInput, cancelDrains []SiteNodeDrainKey) (domain.Site, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return domain.Site{}, err
	}
	defer tx.Rollback()

	site, _, err := scanSite(tx.QueryRow(`SELECT `+siteSelectColumns+` FROM sites WHERE id = ?`, siteID))
	if err != nil {
		return domain.Site{}, err
	}
	if site.Deleting {
		return domain.Site{}, ErrSiteDeleting
	}
	if expectedConfigVersion != 0 && site.ConfigVersion != expectedConfigVersion {
		return domain.Site{}, ErrSiteChanged
	}
	if err := ensureNodesNotUpgradingTx(tx, upgradedNodeIDs(updates, targets)); err != nil {
		return domain.Site{}, err
	}
	previousIPv6Enabled := false
	var previousSiteJSON string
	if err := tx.QueryRow(`SELECT site_json FROM site_publications WHERE site_id = ?`, site.ID).Scan(&previousSiteJSON); err == nil {
		var previousSite domain.Site
		if err := json.Unmarshal([]byte(previousSiteJSON), &previousSite); err != nil {
			return domain.Site{}, fmt.Errorf("decode previous published site %s: %w", site.ID, err)
		}
		previousIPv6Enabled = previousSite.IPv6Enabled
	} else if !errors.Is(err, sql.ErrNoRows) {
		return domain.Site{}, err
	}
	publishedAt := now()
	site.Published = true
	site.UpdatedAt = publishedAt
	if err := createPublishTaskNodesTx(tx, publishTaskID, targets); err != nil {
		return domain.Site{}, err
	}
	if err := saveSitePublicationTx(tx, site, publishedAt); err != nil {
		return domain.Site{}, err
	}
	if err := saveNodeStatesTx(tx, updates, stamp(publishedAt)); err != nil {
		return domain.Site{}, err
	}
	if err := cancelSiteNodeDrainsTx(tx, cancelDrains, publishedAt); err != nil {
		return domain.Site{}, err
	}
	if err := createSiteNodeDrainsTx(tx, drains, publishedAt); err != nil {
		return domain.Site{}, err
	}
	if err := replaceSiteDomainClaims(tx, site.ID, site.Domains); err != nil {
		return domain.Site{}, err
	}
	if site.IPv6Enabled != previousIPv6Enabled {
		if _, err := tx.Exec(`UPDATE site_node_health SET ipv6_consecutive_failures = 0, ipv6_consecutive_successes = 0, ipv6_dns_eligible = 0, ipv6_last_checked_at = NULL, ipv6_last_error = '' WHERE site_id = ?`, site.ID); err != nil {
			return domain.Site{}, err
		}
	}
	result, err := tx.Exec(`UPDATE sites SET published = 1, updated_at = ? WHERE id = ? AND deleting = 0`,
		stamp(publishedAt), site.ID)
	if err != nil {
		return domain.Site{}, err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return domain.Site{}, err
	} else if changed != 1 {
		return domain.Site{}, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return domain.Site{}, err
	}
	return site, nil
}

func saveSitePublicationTx(tx *sql.Tx, site domain.Site, publishedAt time.Time) error {
	encodedSite, err := json.Marshal(site)
	if err != nil {
		return err
	}
	var certificate, key []byte
	var notAfter sql.NullString
	var previousDNSTTL sql.NullInt64
	err = tx.QueryRow(`SELECT certificate_ciphertext, private_key_ciphertext, not_after
		FROM certificates WHERE site_id = ?`, site.ID).Scan(&certificate, &key, &notAfter)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := tx.QueryRow(`SELECT dns_ttl_seconds FROM site_publications WHERE site_id = ?`, site.ID).Scan(&previousDNSTTL); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var encodedNotAfter any
	if notAfter.Valid {
		encodedNotAfter = notAfter.String
	}
	var encodedDNSTTL any
	if previousDNSTTL.Valid && previousDNSTTL.Int64 > 0 {
		encodedDNSTTL = previousDNSTTL.Int64
	}
	_, err = tx.Exec(`INSERT INTO site_publications(site_id, site_json, certificate_ciphertext,
		private_key_ciphertext, certificate_not_after, dns_ttl_seconds, published_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(site_id) DO UPDATE SET site_json=excluded.site_json,
		certificate_ciphertext=excluded.certificate_ciphertext,
		private_key_ciphertext=excluded.private_key_ciphertext,
		certificate_not_after=excluded.certificate_not_after,
		dns_ttl_seconds=excluded.dns_ttl_seconds,
		published_at=excluded.published_at`, site.ID, string(encodedSite), certificate, key,
		encodedNotAfter, encodedDNSTTL, stamp(publishedAt))
	return err
}

func replaceSiteDomainClaims(tx *sql.Tx, siteID string, domains []string) error {
	if _, err := tx.Exec(`DELETE FROM site_domains WHERE site_id = ?`, siteID); err != nil {
		return err
	}
	return claimDomains(tx, siteID, domains)
}

func publishedSiteDomains(tx *sql.Tx, siteID string) ([]string, error) {
	var siteJSON string
	err := tx.QueryRow(`SELECT site_json FROM site_publications WHERE site_id = ?`, siteID).Scan(&siteJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var site domain.Site
	if err := json.Unmarshal([]byte(siteJSON), &site); err != nil {
		return nil, fmt.Errorf("decode published site %s: %w", siteID, err)
	}
	return site.Domains, nil
}

// Existing databases already use published=true to identify drafts that match
// their deployed state. Capture those rows once when the snapshot table is
// introduced; future migrations leave existing snapshots untouched.
func backfillSitePublicationsTx(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT ` + siteSelectColumns + ` FROM sites ORDER BY name`)
	if err != nil {
		return err
	}
	sites := make([]domain.Site, 0)
	for rows.Next() {
		site, _, err := scanSite(rows)
		if err != nil {
			rows.Close()
			return err
		}
		sites = append(sites, site)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, site := range sites {
		if !site.Published {
			continue
		}
		var exists int
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM site_publications WHERE site_id = ?)`, site.ID).Scan(&exists); err != nil {
			return err
		}
		if exists != 0 {
			continue
		}
		if err := saveSitePublicationTx(tx, site, now()); err != nil {
			return fmt.Errorf("backfill published site %s: %w", site.ID, err)
		}
	}
	return nil
}
