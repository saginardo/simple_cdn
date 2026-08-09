package store

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"simple_cdn/internal/domain"
)

var nginxArtifactVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

func (s *Store) SaveNginxArtifactCandidate(artifact domain.NginxArtifact) (domain.NginxArtifact, bool, error) {
	artifact = normalizeNginxArtifact(artifact)
	if err := validateNginxArtifact(artifact); err != nil {
		return domain.NginxArtifact{}, false, err
	}
	if artifact.DownloadedAt.IsZero() {
		artifact.DownloadedAt = now()
	}
	artifact.State = domain.NginxArtifactCandidate
	tx, err := s.db.Begin()
	if err != nil {
		return domain.NginxArtifact{}, false, err
	}
	defer tx.Rollback()
	existing, err := scanNginxArtifact(tx.QueryRow(nginxArtifactSelect+` WHERE version = ?`, artifact.Version))
	if err == nil {
		if existing.SHA256 != artifact.SHA256 {
			return domain.NginxArtifact{}, false, fmt.Errorf("Nginx version %s is already cataloged with a different SHA-256", artifact.Version)
		}
		if existing.State == domain.NginxArtifactCurrent {
			return existing, false, nil
		}
		if _, err := tx.Exec(`UPDATE nginx_artifacts SET state = 'retired' WHERE state = 'candidate' AND sha256 <> ?`, artifact.SHA256); err != nil {
			return domain.NginxArtifact{}, false, err
		}
		if _, err := tx.Exec(`UPDATE nginx_artifacts SET state = 'candidate', source_url = ?, official_source_url = ?,
			source_sha256 = ?, build_commit = ?, size_bytes = ?, downloaded_at = ? WHERE sha256 = ?`,
			artifact.SourceURL, artifact.OfficialSourceURL, artifact.SourceSHA256, artifact.BuildCommit,
			artifact.SizeBytes, stamp(artifact.DownloadedAt), artifact.SHA256); err != nil {
			return domain.NginxArtifact{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return domain.NginxArtifact{}, false, err
		}
		artifact.PromotedAt = existing.PromotedAt
		return artifact, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return domain.NginxArtifact{}, false, err
	}
	if _, err := tx.Exec(`UPDATE nginx_artifacts SET state = 'retired' WHERE state = 'candidate'`); err != nil {
		return domain.NginxArtifact{}, false, err
	}
	if _, err := tx.Exec(`INSERT INTO nginx_artifacts(
		sha256, version, state, release_tag, source_url, official_source_url,
		source_sha256, build_commit, size_bytes, downloaded_at, promoted_at)
		VALUES (?, ?, 'candidate', ?, ?, ?, ?, ?, ?, ?, NULL)`, artifact.SHA256, artifact.Version,
		artifact.ReleaseTag, artifact.SourceURL, artifact.OfficialSourceURL, artifact.SourceSHA256,
		artifact.BuildCommit, artifact.SizeBytes, stamp(artifact.DownloadedAt)); err != nil {
		return domain.NginxArtifact{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return domain.NginxArtifact{}, false, err
	}
	return artifact, true, nil
}

func (s *Store) NginxArtifactBySHA(sha256 string) (domain.NginxArtifact, error) {
	return scanNginxArtifact(s.db.QueryRow(nginxArtifactSelect+` WHERE sha256 = ?`, strings.ToLower(strings.TrimSpace(sha256))))
}

func (s *Store) NginxArtifactByVersion(version string) (domain.NginxArtifact, error) {
	return scanNginxArtifact(s.db.QueryRow(nginxArtifactSelect+` WHERE version = ?`, strings.TrimSpace(version)))
}

func (s *Store) CurrentNginxArtifact() (domain.NginxArtifact, error) {
	return scanNginxArtifact(s.db.QueryRow(nginxArtifactSelect + ` WHERE state = 'current' LIMIT 1`))
}

func (s *Store) CandidateNginxArtifact() (domain.NginxArtifact, error) {
	return scanNginxArtifact(s.db.QueryRow(nginxArtifactSelect + ` WHERE state = 'candidate' ORDER BY downloaded_at DESC LIMIT 1`))
}

func (s *Store) PromoteNginxArtifact(sha256 string) (domain.NginxArtifact, bool, error) {
	sha256 = strings.ToLower(strings.TrimSpace(sha256))
	if !validNginxArtifactDigest(sha256) {
		return domain.NginxArtifact{}, false, errors.New("invalid Nginx artifact SHA-256")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return domain.NginxArtifact{}, false, err
	}
	defer tx.Rollback()
	artifact, err := scanNginxArtifact(tx.QueryRow(nginxArtifactSelect+` WHERE sha256 = ?`, sha256))
	if err != nil {
		return domain.NginxArtifact{}, false, err
	}
	if artifact.State == domain.NginxArtifactCurrent {
		return artifact, false, nil
	}
	if artifact.State != domain.NginxArtifactCandidate {
		return domain.NginxArtifact{}, false, errors.New("only the downloaded candidate can become the Nginx upgrade target")
	}
	promotedAt := now()
	if _, err := tx.Exec(`UPDATE nginx_artifacts SET state = 'retired' WHERE state = 'current'`); err != nil {
		return domain.NginxArtifact{}, false, err
	}
	if _, err := tx.Exec(`UPDATE nginx_artifacts SET state = 'current', promoted_at = ? WHERE sha256 = ? AND state = 'candidate'`, stamp(promotedAt), sha256); err != nil {
		return domain.NginxArtifact{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return domain.NginxArtifact{}, false, err
	}
	artifact.State = domain.NginxArtifactCurrent
	artifact.PromotedAt = &promotedAt
	return artifact, true, nil
}

const nginxArtifactSelect = `SELECT sha256, version, state, release_tag, source_url,
	official_source_url, source_sha256, build_commit, size_bytes, downloaded_at, promoted_at
	FROM nginx_artifacts`

func scanNginxArtifact(scanner interface{ Scan(...any) error }) (domain.NginxArtifact, error) {
	var artifact domain.NginxArtifact
	var downloadedAt string
	var promotedAt sql.NullString
	if err := scanner.Scan(&artifact.SHA256, &artifact.Version, &artifact.State, &artifact.ReleaseTag,
		&artifact.SourceURL, &artifact.OfficialSourceURL, &artifact.SourceSHA256, &artifact.BuildCommit,
		&artifact.SizeBytes, &downloadedAt, &promotedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.NginxArtifact{}, ErrNotFound
		}
		return domain.NginxArtifact{}, err
	}
	parsed, err := parseTime(downloadedAt)
	if err != nil {
		return domain.NginxArtifact{}, err
	}
	artifact.DownloadedAt = parsed
	if promotedAt.Valid {
		parsed, err := parseTime(promotedAt.String)
		if err != nil {
			return domain.NginxArtifact{}, err
		}
		artifact.PromotedAt = &parsed
	}
	return artifact, nil
}

func normalizeNginxArtifact(artifact domain.NginxArtifact) domain.NginxArtifact {
	artifact.SHA256 = strings.ToLower(strings.TrimSpace(artifact.SHA256))
	artifact.Version = strings.TrimSpace(artifact.Version)
	artifact.ReleaseTag = strings.TrimSpace(artifact.ReleaseTag)
	artifact.SourceURL = strings.TrimSpace(artifact.SourceURL)
	artifact.OfficialSourceURL = strings.TrimSpace(artifact.OfficialSourceURL)
	artifact.SourceSHA256 = strings.ToLower(strings.TrimSpace(artifact.SourceSHA256))
	artifact.BuildCommit = strings.ToLower(strings.TrimSpace(artifact.BuildCommit))
	return artifact
}

func validateNginxArtifact(artifact domain.NginxArtifact) error {
	if !validNginxArtifactDigest(artifact.SHA256) || !validNginxArtifactDigest(artifact.SourceSHA256) ||
		!nginxArtifactVersionPattern.MatchString(artifact.Version) || artifact.ReleaseTag != "nginx-v"+artifact.Version ||
		artifact.SourceURL == "" || artifact.OfficialSourceURL == "" || artifact.SizeBytes <= 0 {
		return errors.New("invalid Nginx artifact metadata")
	}
	if len(artifact.BuildCommit) != 40 {
		return errors.New("invalid Nginx artifact build commit")
	}
	if _, err := hex.DecodeString(artifact.BuildCommit); err != nil {
		return errors.New("invalid Nginx artifact build commit")
	}
	return nil
}

func validNginxArtifactDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}
