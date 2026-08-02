package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"simple_cdn/internal/domain"
)

const maxStaticAssets = 1000

const staticAssetColumns = `id, name, original_name, sha256, size_bytes, content_type, created_at, updated_at`
const staticAssetBindingColumns = `id, asset_id, site_id, url_path, cache_control, created_at, updated_at`

func (s *Store) ListStaticAssets() ([]domain.StaticAsset, error) {
	rows, err := s.db.Query(`SELECT ` + staticAssetColumns + ` FROM static_assets ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, err
	}
	assets := make([]domain.StaticAsset, 0)
	byID := make(map[string]int)
	for rows.Next() {
		asset, err := scanStaticAsset(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		asset.Bindings = []domain.StaticAssetBinding{}
		byID[asset.ID] = len(assets)
		assets = append(assets, asset)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	bindings, err := s.listStaticAssetBindings("")
	if err != nil {
		return nil, err
	}
	for _, binding := range bindings {
		if index, found := byID[binding.AssetID]; found {
			assets[index].Bindings = append(assets[index].Bindings, binding)
		}
	}
	return assets, nil
}

func (s *Store) StaticAsset(id string) (domain.StaticAsset, error) {
	asset, err := scanStaticAsset(s.db.QueryRow(`SELECT `+staticAssetColumns+` FROM static_assets WHERE id = ?`, id))
	if err != nil {
		return domain.StaticAsset{}, err
	}
	asset.Bindings, err = s.listStaticAssetBindings(id)
	return asset, err
}

func (s *Store) StaticAssetBySHA256(digest string) (domain.StaticAsset, error) {
	digest = strings.ToLower(strings.TrimSpace(digest))
	if !domain.ValidStaticAssetSHA256(digest) {
		return domain.StaticAsset{}, errors.New("invalid static asset SHA-256")
	}
	asset, err := scanStaticAsset(s.db.QueryRow(`SELECT `+staticAssetColumns+` FROM static_assets WHERE sha256 = ?`, digest))
	if err != nil {
		return domain.StaticAsset{}, err
	}
	asset.Bindings, err = s.listStaticAssetBindings(asset.ID)
	return asset, err
}

func (s *Store) CreateStaticAsset(asset domain.StaticAsset) (domain.StaticAsset, error) {
	var err error
	asset, err = domain.NormalizeStaticAsset(asset)
	if err != nil {
		return domain.StaticAsset{}, err
	}
	asset.ID = uuid.NewString()
	asset.CreatedAt = now()
	asset.UpdatedAt = asset.CreatedAt
	tx, err := s.db.Begin()
	if err != nil {
		return domain.StaticAsset{}, err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM static_assets`).Scan(&count); err != nil {
		return domain.StaticAsset{}, err
	}
	if count >= maxStaticAssets {
		return domain.StaticAsset{}, fmt.Errorf("static asset limit of %d reached", maxStaticAssets)
	}
	_, err = tx.Exec(`INSERT INTO static_assets(id, name, original_name, sha256, size_bytes,
		content_type, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, asset.ID, asset.Name,
		asset.OriginalName, asset.SHA256, asset.SizeBytes, asset.ContentType,
		stamp(asset.CreatedAt), stamp(asset.UpdatedAt))
	if err != nil {
		return domain.StaticAsset{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.StaticAsset{}, err
	}
	asset.Bindings = []domain.StaticAssetBinding{}
	return asset, nil
}

func (s *Store) UpdateStaticAssetName(id, name string) (domain.StaticAsset, error) {
	asset, err := s.StaticAsset(id)
	if err != nil {
		return domain.StaticAsset{}, err
	}
	asset.Name = name
	asset, err = domain.NormalizeStaticAsset(asset)
	if err != nil {
		return domain.StaticAsset{}, err
	}
	result, err := s.db.Exec(`UPDATE static_assets SET name = ?, updated_at = ? WHERE id = ?`, asset.Name, stamp(now()), id)
	if err != nil {
		return domain.StaticAsset{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return domain.StaticAsset{}, err
	}
	if changed != 1 {
		return domain.StaticAsset{}, ErrNotFound
	}
	return s.StaticAsset(id)
}

func (s *Store) DeleteStaticAsset(id string) (domain.StaticAsset, error) {
	asset, err := s.StaticAsset(id)
	if err != nil {
		return domain.StaticAsset{}, err
	}
	result, err := s.db.Exec(`DELETE FROM static_assets WHERE id = ?`, id)
	if err != nil {
		return domain.StaticAsset{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return domain.StaticAsset{}, err
	}
	if changed != 1 {
		return domain.StaticAsset{}, ErrNotFound
	}
	return asset, nil
}

func (s *Store) CreateStaticAssetBinding(binding domain.StaticAssetBinding) (domain.StaticAssetBinding, error) {
	var err error
	binding, err = domain.NormalizeStaticAssetBinding(binding)
	if err != nil {
		return domain.StaticAssetBinding{}, err
	}
	binding.ID = uuid.NewString()
	binding.CreatedAt = now()
	binding.UpdatedAt = binding.CreatedAt
	tx, err := s.db.Begin()
	if err != nil {
		return domain.StaticAssetBinding{}, err
	}
	defer tx.Rollback()
	if err := validateStaticAssetBindingReferencesTx(tx, binding); err != nil {
		return domain.StaticAssetBinding{}, err
	}
	_, err = tx.Exec(`INSERT INTO static_asset_bindings(id, asset_id, site_id, url_path, cache_control,
		created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, binding.ID, binding.AssetID,
		binding.SiteID, binding.URLPath, binding.CacheControl, stamp(binding.CreatedAt), stamp(binding.UpdatedAt))
	if err != nil {
		return domain.StaticAssetBinding{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.StaticAssetBinding{}, err
	}
	return binding, nil
}

func (s *Store) StaticAssetBinding(assetID, bindingID string) (domain.StaticAssetBinding, error) {
	return scanStaticAssetBinding(s.db.QueryRow(`SELECT `+staticAssetBindingColumns+
		` FROM static_asset_bindings WHERE id = ? AND asset_id = ?`, bindingID, assetID))
}

func (s *Store) RestoreStaticAssetBinding(binding domain.StaticAssetBinding) error {
	var err error
	binding, err = domain.NormalizeStaticAssetBinding(binding)
	if err != nil {
		return err
	}
	if strings.TrimSpace(binding.ID) == "" || binding.CreatedAt.IsZero() || binding.UpdatedAt.IsZero() {
		return errors.New("static asset binding restore metadata is incomplete")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateStaticAssetBindingReferencesTx(tx, binding); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO static_asset_bindings(id, asset_id, site_id, url_path,
		cache_control, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, binding.ID,
		binding.AssetID, binding.SiteID, binding.URLPath, binding.CacheControl,
		stamp(binding.CreatedAt), stamp(binding.UpdatedAt)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpdateStaticAssetBinding(assetID, bindingID string, binding domain.StaticAssetBinding) (domain.StaticAssetBinding, error) {
	binding.AssetID = assetID
	var err error
	binding, err = domain.NormalizeStaticAssetBinding(binding)
	if err != nil {
		return domain.StaticAssetBinding{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return domain.StaticAssetBinding{}, err
	}
	defer tx.Rollback()
	if err := validateStaticAssetBindingReferencesTx(tx, binding); err != nil {
		return domain.StaticAssetBinding{}, err
	}
	result, err := tx.Exec(`UPDATE static_asset_bindings SET site_id = ?, url_path = ?, cache_control = ?,
		updated_at = ? WHERE id = ? AND asset_id = ?`, binding.SiteID, binding.URLPath,
		binding.CacheControl, stamp(now()), bindingID, assetID)
	if err != nil {
		return domain.StaticAssetBinding{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return domain.StaticAssetBinding{}, err
	}
	if changed != 1 {
		return domain.StaticAssetBinding{}, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return domain.StaticAssetBinding{}, err
	}
	return scanStaticAssetBinding(s.db.QueryRow(`SELECT `+staticAssetBindingColumns+
		` FROM static_asset_bindings WHERE id = ?`, bindingID))
}

func (s *Store) DeleteStaticAssetBinding(assetID, bindingID string) error {
	result, err := s.db.Exec(`DELETE FROM static_asset_bindings WHERE id = ? AND asset_id = ?`, bindingID, assetID)
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

func (s *Store) ListStaticAssetReferences() ([]domain.StaticAssetReference, error) {
	rows, err := s.db.Query(`SELECT a.id, b.id, b.site_id, b.url_path, a.sha256, a.size_bytes,
		a.content_type, b.cache_control FROM static_asset_bindings b
		JOIN static_assets a ON a.id = b.asset_id ORDER BY b.site_id, b.url_path, b.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.StaticAssetReference, 0)
	for rows.Next() {
		var reference domain.StaticAssetReference
		if err := rows.Scan(&reference.AssetID, &reference.BindingID, &reference.SiteID,
			&reference.URLPath, &reference.SHA256, &reference.SizeBytes, &reference.ContentType,
			&reference.CacheControl); err != nil {
			return nil, err
		}
		result = append(result, reference)
	}
	return result, rows.Err()
}

func (s *Store) listStaticAssetBindings(assetID string) ([]domain.StaticAssetBinding, error) {
	query := `SELECT ` + staticAssetBindingColumns + ` FROM static_asset_bindings`
	arguments := []any{}
	if assetID != "" {
		query += ` WHERE asset_id = ?`
		arguments = append(arguments, assetID)
	}
	query += ` ORDER BY site_id, url_path, id`
	rows, err := s.db.Query(query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	bindings := make([]domain.StaticAssetBinding, 0)
	for rows.Next() {
		binding, err := scanStaticAssetBinding(rows)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	return bindings, rows.Err()
}

func validateStaticAssetBindingReferencesTx(tx *sql.Tx, binding domain.StaticAssetBinding) error {
	var assetExists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM static_assets WHERE id = ?`, binding.AssetID).Scan(&assetExists); err != nil {
		return err
	}
	if assetExists != 1 {
		return errors.New("static asset does not exist")
	}
	var tcpOnly, deleting int
	err := tx.QueryRow(`SELECT tcp_only, deleting FROM sites WHERE id = ?`, binding.SiteID).Scan(&tcpOnly, &deleting)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("static asset site does not exist")
	}
	if err != nil {
		return err
	}
	if tcpOnly != 0 || deleting != 0 {
		return errors.New("static assets require an active HTTP site")
	}
	return nil
}

func scanStaticAsset(row scanner) (domain.StaticAsset, error) {
	var asset domain.StaticAsset
	var createdAt, updatedAt string
	err := row.Scan(&asset.ID, &asset.Name, &asset.OriginalName, &asset.SHA256, &asset.SizeBytes,
		&asset.ContentType, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.StaticAsset{}, ErrNotFound
	}
	if err != nil {
		return domain.StaticAsset{}, err
	}
	if asset.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.StaticAsset{}, err
	}
	if asset.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return domain.StaticAsset{}, err
	}
	asset.Bindings = []domain.StaticAssetBinding{}
	return asset, nil
}

func scanStaticAssetBinding(row scanner) (domain.StaticAssetBinding, error) {
	var binding domain.StaticAssetBinding
	var createdAt, updatedAt string
	err := row.Scan(&binding.ID, &binding.AssetID, &binding.SiteID, &binding.URLPath,
		&binding.CacheControl, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.StaticAssetBinding{}, ErrNotFound
	}
	if err != nil {
		return domain.StaticAssetBinding{}, err
	}
	if binding.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.StaticAssetBinding{}, err
	}
	if binding.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return domain.StaticAssetBinding{}, err
	}
	return binding, nil
}
