package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"simple_cdn/internal/domain"
)

const powPolicyColumns = `id, name, enabled, site_ids_json, path_pattern, difficulty_bits,
	challenge_ttl_seconds, pass_ttl_seconds, priority, created_at, updated_at`

const maxPOWPolicies = 100

type POWPolicyMaterial struct {
	Policy           domain.POWPolicy
	SecretCiphertext []byte
}

func (s *Store) ListPOWPolicies() ([]domain.POWPolicy, error) {
	rows, err := s.db.Query(`SELECT ` + powPolicyColumns + ` FROM pow_policies ORDER BY priority, created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	policies := make([]domain.POWPolicy, 0)
	for rows.Next() {
		policy, err := scanPOWPolicy(rows)
		if err != nil {
			return nil, err
		}
		policies = append(policies, policy)
	}
	return policies, rows.Err()
}

func (s *Store) POWPolicy(id string) (domain.POWPolicy, error) {
	return scanPOWPolicy(s.db.QueryRow(`SELECT `+powPolicyColumns+` FROM pow_policies WHERE id = ?`, id))
}

func (s *Store) ListEnabledPOWPolicyMaterials() ([]POWPolicyMaterial, error) {
	rows, err := s.db.Query(`SELECT ` + powPolicyColumns + `, secret_ciphertext
		FROM pow_policies WHERE enabled = 1 ORDER BY priority, created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	materials := make([]POWPolicyMaterial, 0)
	for rows.Next() {
		material, err := scanPOWPolicyMaterial(rows)
		if err != nil {
			return nil, err
		}
		materials = append(materials, material)
	}
	return materials, rows.Err()
}

func (s *Store) CreatePOWPolicy(policy domain.POWPolicy, secretCiphertext []byte) (domain.POWPolicy, error) {
	var err error
	policy, err = domain.NormalizePOWPolicy(policy)
	if err != nil {
		return domain.POWPolicy{}, err
	}
	if len(secretCiphertext) == 0 {
		return domain.POWPolicy{}, errors.New("proof-of-work policy secret is required")
	}
	policy.ID = uuid.NewString()
	policy.CreatedAt = now()
	policy.UpdatedAt = policy.CreatedAt
	tx, err := s.db.Begin()
	if err != nil {
		return domain.POWPolicy{}, err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pow_policies`).Scan(&count); err != nil {
		return domain.POWPolicy{}, err
	}
	if count >= maxPOWPolicies {
		return domain.POWPolicy{}, fmt.Errorf("proof-of-work policy limit of %d reached", maxPOWPolicies)
	}
	if err := validateSecuritySiteIDsTx(tx, policy.SiteIDs); err != nil {
		return domain.POWPolicy{}, err
	}
	siteIDs, err := json.Marshal(policy.SiteIDs)
	if err != nil {
		return domain.POWPolicy{}, err
	}
	_, err = tx.Exec(`INSERT INTO pow_policies(id, name, enabled, site_ids_json, path_pattern,
		difficulty_bits, challenge_ttl_seconds, pass_ttl_seconds, priority, secret_ciphertext,
		created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, policy.ID, policy.Name,
		boolInt(policy.Enabled), string(siteIDs), policy.PathPattern, policy.DifficultyBits,
		policy.ChallengeTTLSeconds, policy.PassTTLSeconds, policy.Priority, secretCiphertext,
		stamp(policy.CreatedAt), stamp(policy.UpdatedAt))
	if err != nil {
		return domain.POWPolicy{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.POWPolicy{}, err
	}
	return policy, nil
}

func (s *Store) UpdatePOWPolicy(id string, policy domain.POWPolicy) (domain.POWPolicy, error) {
	var err error
	policy, err = domain.NormalizePOWPolicy(policy)
	if err != nil {
		return domain.POWPolicy{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return domain.POWPolicy{}, err
	}
	defer tx.Rollback()
	if err := validateSecuritySiteIDsTx(tx, policy.SiteIDs); err != nil {
		return domain.POWPolicy{}, err
	}
	siteIDs, err := json.Marshal(policy.SiteIDs)
	if err != nil {
		return domain.POWPolicy{}, err
	}
	result, err := tx.Exec(`UPDATE pow_policies SET name = ?, enabled = ?, site_ids_json = ?,
		path_pattern = ?, difficulty_bits = ?, challenge_ttl_seconds = ?, pass_ttl_seconds = ?,
		priority = ?, updated_at = ? WHERE id = ?`, policy.Name, boolInt(policy.Enabled),
		string(siteIDs), policy.PathPattern, policy.DifficultyBits, policy.ChallengeTTLSeconds,
		policy.PassTTLSeconds, policy.Priority, stamp(now()), id)
	if err != nil {
		return domain.POWPolicy{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return domain.POWPolicy{}, err
	}
	if changed != 1 {
		return domain.POWPolicy{}, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return domain.POWPolicy{}, err
	}
	return s.POWPolicy(id)
}

func (s *Store) DeletePOWPolicy(id string) error {
	result, err := s.db.Exec(`DELETE FROM pow_policies WHERE id = ?`, id)
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

func scanPOWPolicy(row scanner) (domain.POWPolicy, error) {
	var policy domain.POWPolicy
	var enabled int
	var siteIDs, createdAt, updatedAt string
	err := row.Scan(&policy.ID, &policy.Name, &enabled, &siteIDs, &policy.PathPattern,
		&policy.DifficultyBits, &policy.ChallengeTTLSeconds, &policy.PassTTLSeconds,
		&policy.Priority, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.POWPolicy{}, ErrNotFound
	}
	if err != nil {
		return domain.POWPolicy{}, err
	}
	return finishPOWPolicy(policy, enabled, siteIDs, createdAt, updatedAt)
}

func scanPOWPolicyMaterial(row scanner) (POWPolicyMaterial, error) {
	var material POWPolicyMaterial
	var enabled int
	var siteIDs, createdAt, updatedAt string
	err := row.Scan(&material.Policy.ID, &material.Policy.Name, &enabled, &siteIDs,
		&material.Policy.PathPattern, &material.Policy.DifficultyBits,
		&material.Policy.ChallengeTTLSeconds, &material.Policy.PassTTLSeconds,
		&material.Policy.Priority, &createdAt, &updatedAt, &material.SecretCiphertext)
	if err != nil {
		return POWPolicyMaterial{}, err
	}
	policy, err := finishPOWPolicy(material.Policy, enabled, siteIDs, createdAt, updatedAt)
	if err != nil {
		return POWPolicyMaterial{}, err
	}
	material.Policy = policy
	return material, nil
}

func finishPOWPolicy(policy domain.POWPolicy, enabled int, siteIDs, createdAt, updatedAt string) (domain.POWPolicy, error) {
	policy.Enabled = enabled != 0
	if err := json.Unmarshal([]byte(siteIDs), &policy.SiteIDs); err != nil {
		return domain.POWPolicy{}, fmt.Errorf("decode proof-of-work policy sites: %w", err)
	}
	var err error
	if policy.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.POWPolicy{}, err
	}
	if policy.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return domain.POWPolicy{}, err
	}
	normalized, err := domain.NormalizePOWPolicy(policy)
	if err != nil {
		return domain.POWPolicy{}, fmt.Errorf("normalize stored proof-of-work policy %s: %w", policy.ID, err)
	}
	normalized.ID = policy.ID
	normalized.CreatedAt = policy.CreatedAt
	normalized.UpdatedAt = policy.UpdatedAt
	return normalized, nil
}
