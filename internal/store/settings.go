package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"simple_cdn/internal/domain"
)

const (
	SecretCloudflareAPIToken = "cloudflare_api_token"
	SecretSMTPPassword       = "smtp_password"
	SecretBackupAccessKey    = "backup_s3_secret_access_key"
	SecretBackupPassword     = "backup_restic_password"
)

type SMTPSettings struct {
	Override               bool
	Enabled                bool
	Host                   string
	Port                   int
	Username               string
	FromAddress            string
	Recipients             []string
	NotificationCategories []string
	Security               string
}

type ControlSettings struct {
	DNSDefaultTTLSeconds int
	CacheDefaultSizeGB   int
	Branding             domain.BrandingSettings
	SMTP                 SMTPSettings
	BackupOverride       bool
	Backup               domain.BackupSettings
	UpdatedAt            *time.Time
}

type BackupSettingsSnapshot struct {
	Override            bool
	Settings            domain.BackupSettings
	AccessKeyCiphertext []byte
	PasswordCiphertext  []byte
}

func (s *Store) ControlSettings() (ControlSettings, error) {
	settings := ControlSettings{
		DNSDefaultTTLSeconds: domain.DefaultDNSTTLSeconds,
		CacheDefaultSizeGB:   domain.DefaultCacheMaxSizeGB,
		Branding:             domain.DefaultBrandingSettings(),
		SMTP:                 SMTPSettings{NotificationCategories: defaultSMTPNotificationCategories()},
		Backup: domain.BackupSettings{
			Region:             domain.DefaultBackupRegion,
			BackupTime:         domain.DefaultBackupTime,
			RandomDelaySeconds: domain.DefaultBackupRandomDelaySeconds,
		},
	}
	var smtpOverride, smtpEnabled, backupOverride int
	var recipients, notificationCategories, updatedAt string
	err := s.db.QueryRow(`SELECT dns_default_ttl_seconds, cache_default_size_gb, smtp_override, smtp_enabled, smtp_host, smtp_port, smtp_username, smtp_from_address, smtp_recipients_json, smtp_notification_categories_json, smtp_security,
		backup_override, backup_repository, backup_access_key_id, backup_region, backup_time, backup_random_delay_seconds,
		brand_name, brand_subtitle, brand_logo_data_url, updated_at
			FROM control_settings WHERE id = 1`).
		Scan(&settings.DNSDefaultTTLSeconds, &settings.CacheDefaultSizeGB, &smtpOverride, &smtpEnabled, &settings.SMTP.Host, &settings.SMTP.Port, &settings.SMTP.Username, &settings.SMTP.FromAddress, &recipients, &notificationCategories, &settings.SMTP.Security,
			&backupOverride, &settings.Backup.Repository, &settings.Backup.AccessKeyID, &settings.Backup.Region, &settings.Backup.BackupTime, &settings.Backup.RandomDelaySeconds,
			&settings.Branding.Name, &settings.Branding.Subtitle, &settings.Branding.LogoDataURL, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return settings, nil
	}
	if err != nil {
		return ControlSettings{}, err
	}
	if err := domain.ValidateDNSTTLSeconds(settings.DNSDefaultTTLSeconds); err != nil {
		return ControlSettings{}, fmt.Errorf("stored DNS TTL: %w", err)
	}
	if err := domain.ValidateCacheMaxSizeGB(settings.CacheDefaultSizeGB); err != nil {
		return ControlSettings{}, fmt.Errorf("stored cache default: %w", err)
	}
	if err := json.Unmarshal([]byte(recipients), &settings.SMTP.Recipients); err != nil {
		return ControlSettings{}, fmt.Errorf("decode SMTP recipients: %w", err)
	}
	if err := json.Unmarshal([]byte(notificationCategories), &settings.SMTP.NotificationCategories); err != nil {
		return ControlSettings{}, fmt.Errorf("decode SMTP notification categories: %w", err)
	}
	if settings.SMTP.NotificationCategories == nil {
		settings.SMTP.NotificationCategories = defaultSMTPNotificationCategories()
	}
	settings.SMTP.Override = smtpOverride != 0
	settings.SMTP.Enabled = smtpEnabled != 0
	settings.BackupOverride = backupOverride != 0
	settings.Backup = domain.NormalizeBackupSettings(settings.Backup)
	settings.Branding = domain.NormalizeBrandingSettings(settings.Branding)
	if err := domain.ValidateBrandingSettings(settings.Branding); err != nil {
		return ControlSettings{}, fmt.Errorf("stored branding settings: %w", err)
	}
	parsed, err := parseTime(updatedAt)
	if err != nil {
		return ControlSettings{}, err
	}
	settings.UpdatedAt = &parsed
	return settings, nil
}

func (s *Store) SaveBrandingSettings(settings domain.BrandingSettings) error {
	settings = domain.NormalizeBrandingSettings(settings)
	if err := domain.ValidateBrandingSettings(settings); err != nil {
		return err
	}
	_, err := s.db.Exec(`INSERT INTO control_settings(id, brand_name, brand_subtitle, brand_logo_data_url, updated_at) VALUES (1, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET brand_name=excluded.brand_name, brand_subtitle=excluded.brand_subtitle, brand_logo_data_url=excluded.brand_logo_data_url, updated_at=excluded.updated_at`,
		settings.Name, settings.Subtitle, settings.LogoDataURL, stamp(now()))
	return err
}

// BackupSettingsSnapshot reads settings and both secrets in one SQLite
// statement so a concurrent web update cannot produce mixed credentials.
func (s *Store) BackupSettingsSnapshot() (BackupSettingsSnapshot, error) {
	snapshot := BackupSettingsSnapshot{Settings: domain.BackupSettings{
		Region:             domain.DefaultBackupRegion,
		BackupTime:         domain.DefaultBackupTime,
		RandomDelaySeconds: domain.DefaultBackupRandomDelaySeconds,
	}}
	var override int
	err := s.db.QueryRow(`SELECT settings.backup_override, settings.backup_repository, settings.backup_access_key_id,
		settings.backup_region, settings.backup_time, settings.backup_random_delay_seconds,
		COALESCE(access_key.ciphertext, X''), COALESCE(repository_password.ciphertext, X'')
		FROM control_settings AS settings
		LEFT JOIN secrets AS access_key ON access_key.name = ?
		LEFT JOIN secrets AS repository_password ON repository_password.name = ?
		WHERE settings.id = 1`, SecretBackupAccessKey, SecretBackupPassword).
		Scan(&override, &snapshot.Settings.Repository, &snapshot.Settings.AccessKeyID,
			&snapshot.Settings.Region, &snapshot.Settings.BackupTime, &snapshot.Settings.RandomDelaySeconds,
			&snapshot.AccessKeyCiphertext, &snapshot.PasswordCiphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return snapshot, nil
	}
	if err != nil {
		return BackupSettingsSnapshot{}, err
	}
	snapshot.Override = override != 0
	snapshot.Settings = domain.NormalizeBackupSettings(snapshot.Settings)
	return snapshot, nil
}

func (s *Store) SaveBackupSettings(settings domain.BackupSettings, accessKeyCiphertext []byte, replaceAccessKey bool, passwordCiphertext []byte, replacePassword bool) error {
	settings = domain.NormalizeBackupSettings(settings)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO control_settings(id, backup_override, backup_repository, backup_access_key_id, backup_region, backup_time, backup_random_delay_seconds, updated_at)
		VALUES (1, 1, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET backup_override=1, backup_repository=excluded.backup_repository,
		backup_access_key_id=excluded.backup_access_key_id, backup_region=excluded.backup_region,
		backup_time=excluded.backup_time, backup_random_delay_seconds=excluded.backup_random_delay_seconds,
		updated_at=excluded.updated_at`, settings.Repository, settings.AccessKeyID, settings.Region,
		settings.BackupTime, settings.RandomDelaySeconds, stamp(now()))
	if err != nil {
		return err
	}
	if replaceAccessKey {
		if len(accessKeyCiphertext) == 0 {
			return errors.New("encrypted S3 secret access key is required")
		}
		if _, err := tx.Exec(`INSERT INTO secrets(name, ciphertext, updated_at) VALUES (?, ?, ?) ON CONFLICT(name) DO UPDATE SET ciphertext=excluded.ciphertext, updated_at=excluded.updated_at`, SecretBackupAccessKey, accessKeyCiphertext, stamp(now())); err != nil {
			return err
		}
	}
	if replacePassword {
		if len(passwordCiphertext) == 0 {
			return errors.New("encrypted Restic repository password is required")
		}
		if _, err := tx.Exec(`INSERT INTO secrets(name, ciphertext, updated_at) VALUES (?, ?, ?) ON CONFLICT(name) DO UPDATE SET ciphertext=excluded.ciphertext, updated_at=excluded.updated_at`, SecretBackupPassword, passwordCiphertext, stamp(now())); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ClearBackupSettings() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE control_settings SET backup_override=0, updated_at=? WHERE id=1`, stamp(now())); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM secrets WHERE name IN (?, ?)`, SecretBackupAccessKey, SecretBackupPassword); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SaveDNSDefaultTTL(seconds int) error {
	if err := domain.ValidateDNSTTLSeconds(seconds); err != nil {
		return err
	}
	_, err := s.db.Exec(`INSERT INTO control_settings(id, dns_default_ttl_seconds, updated_at) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET dns_default_ttl_seconds=excluded.dns_default_ttl_seconds, updated_at=excluded.updated_at`, seconds, stamp(now()))
	return err
}

func (s *Store) SaveCacheDefaultSizeGB(size int) error {
	if err := domain.ValidateCacheMaxSizeGB(size); err != nil {
		return err
	}
	_, err := s.db.Exec(`INSERT INTO control_settings(id, cache_default_size_gb, updated_at) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET cache_default_size_gb=excluded.cache_default_size_gb, updated_at=excluded.updated_at`, size, stamp(now()))
	return err
}

func (s *Store) SaveSMTPSettings(settings SMTPSettings, passwordCiphertext []byte, replacePassword bool) error {
	recipients, err := json.Marshal(settings.Recipients)
	if err != nil {
		return err
	}
	if settings.NotificationCategories == nil {
		settings.NotificationCategories = defaultSMTPNotificationCategories()
	}
	notificationCategories, err := json.Marshal(settings.NotificationCategories)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO control_settings(id, smtp_override, smtp_enabled, smtp_host, smtp_port, smtp_username, smtp_from_address, smtp_recipients_json, smtp_notification_categories_json, smtp_security, updated_at)
		VALUES (1, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET smtp_override=1, smtp_enabled=excluded.smtp_enabled, smtp_host=excluded.smtp_host, smtp_port=excluded.smtp_port, smtp_username=excluded.smtp_username, smtp_from_address=excluded.smtp_from_address, smtp_recipients_json=excluded.smtp_recipients_json, smtp_notification_categories_json=excluded.smtp_notification_categories_json, smtp_security=excluded.smtp_security, updated_at=excluded.updated_at`,
		boolInt(settings.Enabled), settings.Host, settings.Port, settings.Username, settings.FromAddress, string(recipients), string(notificationCategories), settings.Security, stamp(now()))
	if err != nil {
		return err
	}
	if replacePassword {
		if len(passwordCiphertext) == 0 {
			if _, err := tx.Exec(`DELETE FROM secrets WHERE name = ?`, SecretSMTPPassword); err != nil {
				return err
			}
		} else if _, err := tx.Exec(`INSERT INTO secrets(name, ciphertext, updated_at) VALUES (?, ?, ?) ON CONFLICT(name) DO UPDATE SET ciphertext=excluded.ciphertext, updated_at=excluded.updated_at`, SecretSMTPPassword, passwordCiphertext, stamp(now())); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func defaultSMTPNotificationCategories() []string {
	return []string{"availability", "monitoring", "certificate", "backup"}
}

func (s *Store) ClearSMTPSettings() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE control_settings SET smtp_override=0, updated_at=? WHERE id=1`, stamp(now())); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM secrets WHERE name = ?`, SecretSMTPPassword); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeleteSecret(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("secret name is required")
	}
	_, err := s.db.Exec(`DELETE FROM secrets WHERE name = ?`, name)
	return err
}

func OpenReadOnly(path string) (*Store, error) {
	return openReadOnly(path, false)
}

// OpenImmutable opens a completed SQLite snapshot without consulting or
// creating WAL sidecars. Callers must guarantee that the file cannot change.
func OpenImmutable(path string) (*Store, error) {
	return openReadOnly(path, true)
}

func openReadOnly(path string, immutable bool) (*Store, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	query := url.Values{"mode": []string{"ro"}}
	if immutable {
		query.Set("immutable", "1")
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, readOnly: true}, nil
}

func (s *Store) IntegrityCheck() error {
	rows, err := s.db.Query(`PRAGMA quick_check`)
	if err != nil {
		return fmt.Errorf("run SQLite quick_check: %w", err)
	}
	defer rows.Close()
	var failures []string
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return fmt.Errorf("read SQLite quick_check: %w", err)
		}
		if result != "ok" {
			failures = append(failures, result)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read SQLite quick_check: %w", err)
	}
	if len(failures) != 0 {
		return fmt.Errorf("SQLite quick_check failed: %s", strings.Join(failures, "; "))
	}
	return nil
}

func ReadSecret(path, name string) ([]byte, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("secret name is required")
	}
	store, err := OpenReadOnly(path)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	var tableCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='secrets'`).Scan(&tableCount); err != nil {
		return nil, err
	}
	if tableCount == 0 {
		return nil, ErrNotFound
	}
	var ciphertext []byte
	err = store.db.QueryRow(`SELECT ciphertext FROM secrets WHERE name = ?`, name).Scan(&ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return ciphertext, err
}
