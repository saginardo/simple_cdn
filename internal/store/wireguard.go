package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"simple_cdn/internal/domain"
)

var (
	ErrWireGuardTunnelInUse       = errors.New("WireGuard tunnel is referenced by a site")
	ErrWireGuardInstallToken      = errors.New("WireGuard install token is invalid or expired")
	ErrWireGuardPeersPending      = errors.New("WireGuard edge peer keys are not ready")
	ErrWireGuardPerformanceActive = errors.New("the edge already has an active WireGuard performance test")
)

func (s *Store) SuggestedWireGuardCIDR() (string, error) {
	rows, err := s.db.Query(`SELECT address_cidr FROM wireguard_tunnels`)
	if err != nil {
		return "", err
	}
	used := make([]*net.IPNet, 0)
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			rows.Close()
			return "", err
		}
		if _, network, parseErr := net.ParseCIDR(value); parseErr == nil {
			used = append(used, network)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", err
	}
	if err := rows.Close(); err != nil {
		return "", err
	}
	for subnet := 0; subnet < 256; subnet++ {
		candidate := fmt.Sprintf("10.253.%d.0/24", subnet)
		_, network, _ := net.ParseCIDR(candidate)
		conflict := false
		for _, existing := range used {
			if networksOverlap(network, existing) {
				conflict = true
				break
			}
		}
		if !conflict {
			return candidate, nil
		}
	}
	return "", errors.New("automatic WireGuard address pool is exhausted; specify a custom private CIDR")
}

func networksOverlap(left, right *net.IPNet) bool {
	return left.Contains(right.IP) || right.Contains(left.IP)
}

func (s *Store) validateWireGuardCIDRAvailable(cidr, excludedID string) error {
	_, wanted, err := net.ParseCIDR(cidr)
	if err != nil {
		return err
	}
	rows, err := s.db.Query(`SELECT name, address_cidr FROM wireguard_tunnels WHERE id<>?`, excludedID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name, existingCIDR string
		if err := rows.Scan(&name, &existingCIDR); err != nil {
			return err
		}
		_, existing, parseErr := net.ParseCIDR(existingCIDR)
		if parseErr != nil {
			return parseErr
		}
		if networksOverlap(wanted, existing) {
			return fmt.Errorf("WireGuard address range overlaps tunnel %s (%s)", name, existingCIDR)
		}
	}
	return rows.Err()
}

func (s *Store) CreateWireGuardTunnel(tunnel domain.WireGuardTunnel, nodeIDs []string) (domain.WireGuardTunnel, error) {
	if err := domain.NormalizeAndValidateWireGuardTunnel(&tunnel); err != nil {
		return domain.WireGuardTunnel{}, err
	}
	nodeIDs, err := normalizeWireGuardNodeIDs(nodeIDs)
	if err != nil {
		return domain.WireGuardTunnel{}, err
	}
	if err := s.validateWireGuardCIDRAvailable(tunnel.AddressCIDR, ""); err != nil {
		return domain.WireGuardTunnel{}, err
	}
	addresses, err := domain.AllocateWireGuardPeerAddresses(tunnel.AddressCIDR, nodeIDs, nil)
	if err != nil {
		return domain.WireGuardTunnel{}, err
	}
	tunnel.ID = uuid.NewString()
	tunnel.Revision = 1
	tunnel.CreatedAt = now()
	tunnel.UpdatedAt = tunnel.CreatedAt
	tx, err := s.db.Begin()
	if err != nil {
		return domain.WireGuardTunnel{}, err
	}
	defer tx.Rollback()
	if err := validateSiteNodes(tx, nodeIDs); err != nil {
		return domain.WireGuardTunnel{}, err
	}
	if _, err := tx.Exec(`INSERT INTO wireguard_tunnels(
		id, name, endpoint_host, listen_port, address_cidr, origin_address, mtu,
		persistent_keepalive_seconds, performance_port, origin_public_key, revision,
		origin_configured_revision, origin_configured_at, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', 1, 0, NULL, ?, ?)`,
		tunnel.ID, tunnel.Name, tunnel.EndpointHost, tunnel.ListenPort, tunnel.AddressCIDR,
		tunnel.OriginAddress, tunnel.MTU, tunnel.PersistentKeepaliveSecs, tunnel.PerformancePort,
		stamp(tunnel.CreatedAt), stamp(tunnel.UpdatedAt)); err != nil {
		return domain.WireGuardTunnel{}, err
	}
	for _, nodeID := range nodeIDs {
		if _, err := tx.Exec(`INSERT INTO wireguard_tunnel_nodes(tunnel_id, node_id, address) VALUES (?, ?, ?)`, tunnel.ID, nodeID, addresses[nodeID]); err != nil {
			return domain.WireGuardTunnel{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.WireGuardTunnel{}, err
	}
	return s.GetWireGuardTunnel(tunnel.ID)
}

func (s *Store) UpdateWireGuardTunnel(tunnel domain.WireGuardTunnel, nodeIDs []string) (domain.WireGuardTunnel, error) {
	current, err := s.GetWireGuardTunnel(tunnel.ID)
	if err != nil {
		return domain.WireGuardTunnel{}, err
	}
	tunnel.OriginPublicKey = current.OriginPublicKey
	if err := domain.NormalizeAndValidateWireGuardTunnel(&tunnel); err != nil {
		return domain.WireGuardTunnel{}, err
	}
	nodeIDs, err = normalizeWireGuardNodeIDs(nodeIDs)
	if err != nil {
		return domain.WireGuardTunnel{}, err
	}
	if err := s.validateWireGuardCIDRAvailable(tunnel.AddressCIDR, tunnel.ID); err != nil {
		return domain.WireGuardTunnel{}, err
	}
	references, err := s.WireGuardTunnelReferences(tunnel.ID)
	if err != nil {
		return domain.WireGuardTunnel{}, err
	}
	assigned := make(map[string]bool, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		assigned[nodeID] = true
	}
	for _, site := range references {
		for _, nodeID := range site.Nodes {
			if !assigned[nodeID] {
				return domain.WireGuardTunnel{}, fmt.Errorf("%w: site %s still assigns node %s", ErrWireGuardTunnelInUse, site.Name, nodeID)
			}
		}
	}
	existing := make(map[string]string, len(current.Peers))
	for _, peer := range current.Peers {
		existing[peer.NodeID] = peer.Address
	}
	addresses, err := domain.AllocateWireGuardPeerAddresses(tunnel.AddressCIDR, nodeIDs, existing)
	if err != nil {
		return domain.WireGuardTunnel{}, err
	}
	updatedAt := now()
	tx, err := s.db.Begin()
	if err != nil {
		return domain.WireGuardTunnel{}, err
	}
	defer tx.Rollback()
	if err := validateSiteNodes(tx, nodeIDs); err != nil {
		return domain.WireGuardTunnel{}, err
	}
	result, err := tx.Exec(`UPDATE wireguard_tunnels SET name=?, endpoint_host=?, listen_port=?, address_cidr=?,
		origin_address=?, mtu=?, persistent_keepalive_seconds=?, performance_port=?, revision=revision+1,
		updated_at=? WHERE id=?`, tunnel.Name, tunnel.EndpointHost, tunnel.ListenPort, tunnel.AddressCIDR,
		tunnel.OriginAddress, tunnel.MTU, tunnel.PersistentKeepaliveSecs, tunnel.PerformancePort, stamp(updatedAt), tunnel.ID)
	if err != nil {
		return domain.WireGuardTunnel{}, err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return domain.WireGuardTunnel{}, err
	} else if changed != 1 {
		return domain.WireGuardTunnel{}, ErrNotFound
	}
	if _, err := tx.Exec(`DELETE FROM wireguard_tunnel_nodes WHERE tunnel_id=? AND node_id NOT IN (`+sqlPlaceholders(len(nodeIDs))+`)`, append([]any{tunnel.ID}, stringSliceAny(nodeIDs)...)...); err != nil {
		return domain.WireGuardTunnel{}, err
	}
	for _, nodeID := range nodeIDs {
		if _, err := tx.Exec(`INSERT INTO wireguard_tunnel_nodes(tunnel_id, node_id, address) VALUES (?, ?, ?)
			ON CONFLICT(tunnel_id, node_id) DO UPDATE SET address=excluded.address`, tunnel.ID, nodeID, addresses[nodeID]); err != nil {
			return domain.WireGuardTunnel{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.WireGuardTunnel{}, err
	}
	return s.GetWireGuardTunnel(tunnel.ID)
}

func sqlPlaceholders(count int) string {
	if count == 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func stringSliceAny(values []string) []any {
	result := make([]any, len(values))
	for index := range values {
		result[index] = values[index]
	}
	return result
}

func normalizeWireGuardNodeIDs(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, errors.New("WireGuard tunnel requires at least one edge node")
	}
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			return nil, errors.New("WireGuard edge node IDs must be non-empty and unique")
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func (s *Store) GetWireGuardTunnel(id string) (domain.WireGuardTunnel, error) {
	tunnel, err := scanWireGuardTunnel(s.db.QueryRow(`SELECT id, name, endpoint_host, listen_port, address_cidr,
		origin_address, mtu, persistent_keepalive_seconds, performance_port, origin_public_key, revision,
		origin_configured_revision, origin_configured_at, created_at, updated_at FROM wireguard_tunnels WHERE id=?`, id))
	if err != nil {
		return domain.WireGuardTunnel{}, err
	}
	tunnel.Peers, err = s.listWireGuardPeers(id)
	return tunnel, err
}

func (s *Store) ListWireGuardTunnels() ([]domain.WireGuardTunnel, error) {
	rows, err := s.db.Query(`SELECT id, name, endpoint_host, listen_port, address_cidr,
		origin_address, mtu, persistent_keepalive_seconds, performance_port, origin_public_key, revision,
		origin_configured_revision, origin_configured_at, created_at, updated_at FROM wireguard_tunnels ORDER BY name`)
	if err != nil {
		return nil, err
	}
	tunnels := make([]domain.WireGuardTunnel, 0)
	for rows.Next() {
		tunnel, scanErr := scanWireGuardTunnel(rows)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		tunnels = append(tunnels, tunnel)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range tunnels {
		tunnels[index].Peers, err = s.listWireGuardPeers(tunnels[index].ID)
		if err != nil {
			return nil, err
		}
	}
	return tunnels, nil
}

func scanWireGuardTunnel(row scanner) (domain.WireGuardTunnel, error) {
	var tunnel domain.WireGuardTunnel
	var configuredAt sql.NullString
	var createdAt, updatedAt string
	err := row.Scan(&tunnel.ID, &tunnel.Name, &tunnel.EndpointHost, &tunnel.ListenPort, &tunnel.AddressCIDR,
		&tunnel.OriginAddress, &tunnel.MTU, &tunnel.PersistentKeepaliveSecs, &tunnel.PerformancePort,
		&tunnel.OriginPublicKey, &tunnel.Revision, &tunnel.OriginConfiguredRevision, &configuredAt, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WireGuardTunnel{}, ErrNotFound
	}
	if err != nil {
		return domain.WireGuardTunnel{}, err
	}
	tunnel.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return domain.WireGuardTunnel{}, err
	}
	tunnel.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return domain.WireGuardTunnel{}, err
	}
	if configuredAt.Valid {
		value, parseErr := parseTime(configuredAt.String)
		if parseErr != nil {
			return domain.WireGuardTunnel{}, parseErr
		}
		tunnel.OriginConfiguredAt = &value
	}
	return tunnel, nil
}

func (s *Store) listWireGuardPeers(tunnelID string) ([]domain.WireGuardPeer, error) {
	rows, err := s.db.Query(`SELECT peers.node_id, nodes.name, nodes.public_ipv4, peers.address, peers.public_key, peers.applied_revision,
		peers.latest_handshake_at, peers.rx_bytes, peers.tx_bytes, peers.last_reported_at, peers.last_error
		FROM wireguard_tunnel_nodes peers JOIN nodes ON nodes.id=peers.node_id
		WHERE peers.tunnel_id=? ORDER BY nodes.name`, tunnelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	peers := make([]domain.WireGuardPeer, 0)
	for rows.Next() {
		var peer domain.WireGuardPeer
		var handshake, reported sql.NullString
		if err := rows.Scan(&peer.NodeID, &peer.NodeName, &peer.NodePublicIPv4, &peer.Address, &peer.PublicKey, &peer.AppliedRevision,
			&handshake, &peer.RXBytes, &peer.TXBytes, &reported, &peer.LastError); err != nil {
			return nil, err
		}
		if handshake.Valid {
			value, parseErr := parseTime(handshake.String)
			if parseErr != nil {
				return nil, parseErr
			}
			peer.LatestHandshakeAt = &value
		}
		if reported.Valid {
			value, parseErr := parseTime(reported.String)
			if parseErr != nil {
				return nil, parseErr
			}
			peer.LastReportedAt = &value
		}
		peers = append(peers, peer)
	}
	return peers, rows.Err()
}

func (s *Store) WireGuardTunnelReferences(tunnelID string) ([]domain.Site, error) {
	sites, err := s.ListSites()
	if err != nil {
		return nil, err
	}
	result := make([]domain.Site, 0)
	for _, site := range sites {
		if site.PrimaryOrigin.WireGuardTunnelID == tunnelID || site.BackupOrigin != nil && site.BackupOrigin.WireGuardTunnelID == tunnelID {
			result = append(result, site)
		}
	}
	return result, nil
}

func (s *Store) DeleteWireGuardTunnel(id string) error {
	references, err := s.WireGuardTunnelReferences(id)
	if err != nil {
		return err
	}
	if len(references) != 0 {
		return fmt.Errorf("%w: %s", ErrWireGuardTunnelInUse, references[0].Name)
	}
	result, err := s.db.Exec(`DELETE FROM wireguard_tunnels WHERE id=?`, id)
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

func (s *Store) WireGuardEdgeConfigs(nodeID string) ([]domain.WireGuardEdgeConfig, error) {
	rows, err := s.db.Query(`SELECT tunnels.id, tunnels.name, tunnels.revision, peers.address,
		tunnels.origin_address, tunnels.origin_public_key, tunnels.endpoint_host, tunnels.listen_port,
		tunnels.mtu, tunnels.persistent_keepalive_seconds, tunnels.performance_port
		FROM wireguard_tunnel_nodes peers JOIN wireguard_tunnels tunnels ON tunnels.id=peers.tunnel_id
		WHERE peers.node_id=? ORDER BY tunnels.id`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	configs := make([]domain.WireGuardEdgeConfig, 0)
	for rows.Next() {
		var config domain.WireGuardEdgeConfig
		var address, endpointHost string
		var listenPort int
		if err := rows.Scan(&config.TunnelID, &config.Name, &config.Revision, &address, &config.OriginAddress,
			&config.OriginPublicKey, &endpointHost, &listenPort, &config.MTU, &config.PersistentKeepaliveSecs,
			&config.PerformancePort); err != nil {
			return nil, err
		}
		config.InterfaceName = domain.WireGuardInterfaceName(config.TunnelID)
		config.Address = address + "/32"
		config.Endpoint = net.JoinHostPort(endpointHost, fmt.Sprintf("%d", listenPort))
		config.DirectPerformanceHost = endpointHost
		configs = append(configs, config)
	}
	return configs, rows.Err()
}

func (s *Store) UpdateWireGuardPeerReports(nodeID string, reports []domain.WireGuardPeerReport) error {
	if len(reports) > domain.MaxWireGuardPeersPerTunnel {
		return errors.New("too many WireGuard peer reports")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	when := stamp(now())
	seen := make(map[string]bool, len(reports))
	for _, report := range reports {
		if !domain.ValidWireGuardPeerReport(report) || seen[report.TunnelID] {
			return errors.New("invalid or duplicate WireGuard peer report")
		}
		seen[report.TunnelID] = true
		var currentKey string
		var currentRevision int64
		if err := tx.QueryRow(`SELECT peers.public_key, tunnels.revision FROM wireguard_tunnel_nodes peers
			JOIN wireguard_tunnels tunnels ON tunnels.id=peers.tunnel_id
			WHERE peers.tunnel_id=? AND peers.node_id=?`, report.TunnelID, nodeID).Scan(&currentKey, &currentRevision); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if report.Revision > currentRevision {
			return errors.New("WireGuard peer report revision is newer than the desired tunnel revision")
		}
		if currentKey != report.PublicKey {
			if _, err := tx.Exec(`UPDATE wireguard_tunnels SET revision=revision+1, updated_at=? WHERE id=?`, when, report.TunnelID); err != nil {
				return err
			}
		}
		var handshake any
		if report.LatestHandshake != nil {
			handshake = stamp(*report.LatestHandshake)
		}
		if _, err := tx.Exec(`UPDATE wireguard_tunnel_nodes SET public_key=?, applied_revision=?, latest_handshake_at=?, rx_bytes=?,
			tx_bytes=?, last_reported_at=?, last_error=? WHERE tunnel_id=? AND node_id=?`, report.PublicKey, report.Revision,
			handshake, report.RXBytes, report.TXBytes, when, report.Error, report.TunnelID, nodeID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) CreateWireGuardInstallToken(tunnelID, tokenHash string, expiresAt time.Time) error {
	if tokenHash == "" || !expiresAt.After(now()) {
		return errors.New("invalid WireGuard install token")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRow(`SELECT 1 FROM wireguard_tunnels WHERE id=?`, tunnelID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM wireguard_install_tokens WHERE tunnel_id=? AND used_at IS NULL`, tunnelID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO wireguard_install_tokens(token_hash, tunnel_id, expires_at, created_at) VALUES (?, ?, ?, ?)`,
		tokenHash, tunnelID, stamp(expiresAt), stamp(now())); err != nil {
		return err
	}
	return tx.Commit()
}

// ConfigureWireGuardOrigin records the origin public key. The one-time token
// remains usable while edge public keys are still converging so the installer
// can poll without persisting a long-lived credential.
func (s *Store) ConfigureWireGuardOrigin(tokenHash, publicKey string) (domain.WireGuardTunnel, bool, error) {
	if tokenHash == "" || !domain.ValidWireGuardKey(publicKey) {
		return domain.WireGuardTunnel{}, false, ErrWireGuardInstallToken
	}
	tx, err := s.db.Begin()
	if err != nil {
		return domain.WireGuardTunnel{}, false, err
	}
	defer tx.Rollback()
	var tunnelID, expiresAt string
	var usedAt sql.NullString
	if err := tx.QueryRow(`SELECT tunnel_id, expires_at, used_at FROM wireguard_install_tokens WHERE token_hash=?`, tokenHash).Scan(&tunnelID, &expiresAt, &usedAt); errors.Is(err, sql.ErrNoRows) {
		return domain.WireGuardTunnel{}, false, ErrWireGuardInstallToken
	} else if err != nil {
		return domain.WireGuardTunnel{}, false, err
	}
	expires, err := parseTime(expiresAt)
	if err != nil || usedAt.Valid || !expires.After(now()) {
		return domain.WireGuardTunnel{}, false, ErrWireGuardInstallToken
	}
	var currentKey string
	var revision int64
	if err := tx.QueryRow(`SELECT origin_public_key, revision FROM wireguard_tunnels WHERE id=?`, tunnelID).Scan(&currentKey, &revision); err != nil {
		return domain.WireGuardTunnel{}, false, err
	}
	when := stamp(now())
	if currentKey != publicKey {
		revision++
		if _, err := tx.Exec(`UPDATE wireguard_tunnels SET origin_public_key=?, revision=?, updated_at=? WHERE id=?`, publicKey, revision, when, tunnelID); err != nil {
			return domain.WireGuardTunnel{}, false, err
		}
	}
	var missing int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM wireguard_tunnel_nodes WHERE tunnel_id=? AND public_key=''`, tunnelID).Scan(&missing); err != nil {
		return domain.WireGuardTunnel{}, false, err
	}
	ready := missing == 0
	if ready {
		if _, err := tx.Exec(`UPDATE wireguard_tunnels SET origin_configured_revision=?, origin_configured_at=?, updated_at=? WHERE id=?`, revision, when, when, tunnelID); err != nil {
			return domain.WireGuardTunnel{}, false, err
		}
		if _, err := tx.Exec(`UPDATE wireguard_install_tokens SET used_at=? WHERE token_hash=?`, when, tokenHash); err != nil {
			return domain.WireGuardTunnel{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.WireGuardTunnel{}, false, err
	}
	tunnel, err := s.GetWireGuardTunnel(tunnelID)
	return tunnel, ready, err
}

func (s *Store) CreateWireGuardPerformanceTest(tunnelID, nodeID string, targetMbps, durationSeconds int) (domain.WireGuardPerformanceTest, error) {
	if targetMbps == 0 {
		targetMbps = domain.DefaultWireGuardPerformanceMbps
	}
	if durationSeconds == 0 {
		durationSeconds = domain.DefaultWireGuardPerformanceDuration
	}
	if err := domain.ValidateWireGuardPerformanceRequest(targetMbps, durationSeconds); err != nil {
		return domain.WireGuardPerformanceTest{}, err
	}
	var exists int
	if err := s.db.QueryRow(`SELECT 1 FROM wireguard_tunnel_nodes WHERE tunnel_id=? AND node_id=?`, tunnelID, nodeID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return domain.WireGuardPerformanceTest{}, ErrNotFound
	} else if err != nil {
		return domain.WireGuardPerformanceTest{}, err
	}
	test := domain.WireGuardPerformanceTest{ID: uuid.NewString(), TunnelID: tunnelID, NodeID: nodeID,
		TargetMbps: targetMbps, DurationSeconds: durationSeconds, Status: domain.WireGuardPerformanceQueued, CreatedAt: now()}
	_, err := s.db.Exec(`INSERT INTO wireguard_performance_tests(id, tunnel_id, node_id, target_mbps, duration_seconds, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, test.ID, tunnelID, nodeID, targetMbps, durationSeconds, test.Status, stamp(test.CreatedAt))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return domain.WireGuardPerformanceTest{}, ErrWireGuardPerformanceActive
		}
		return domain.WireGuardPerformanceTest{}, err
	}
	return s.GetWireGuardPerformanceTest(test.ID)
}

func (s *Store) GetWireGuardPerformanceTest(id string) (domain.WireGuardPerformanceTest, error) {
	return scanWireGuardPerformanceTest(s.db.QueryRow(wireGuardPerformanceSelect+` WHERE tests.id=?`, id))
}

func (s *Store) ListWireGuardPerformanceTests(limit int) ([]domain.WireGuardPerformanceTest, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(wireGuardPerformanceSelect+` ORDER BY tests.created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.WireGuardPerformanceTest, 0)
	for rows.Next() {
		test, scanErr := scanWireGuardPerformanceTest(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, test)
	}
	return result, rows.Err()
}

const wireGuardPerformanceSelect = `SELECT tests.id, tests.tunnel_id, tunnels.name, tests.node_id, nodes.name,
	tests.target_mbps, tests.duration_seconds, tests.status, tests.result_json, tests.error,
	tests.created_at, tests.started_at, tests.finished_at
	FROM wireguard_performance_tests tests
	JOIN wireguard_tunnels tunnels ON tunnels.id=tests.tunnel_id
	JOIN nodes ON nodes.id=tests.node_id`

func scanWireGuardPerformanceTest(row scanner) (domain.WireGuardPerformanceTest, error) {
	var test domain.WireGuardPerformanceTest
	var resultJSON, startedAt, finishedAt sql.NullString
	var createdAt string
	err := row.Scan(&test.ID, &test.TunnelID, &test.TunnelName, &test.NodeID, &test.NodeName,
		&test.TargetMbps, &test.DurationSeconds, &test.Status, &resultJSON, &test.Error,
		&createdAt, &startedAt, &finishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WireGuardPerformanceTest{}, ErrNotFound
	}
	if err != nil {
		return domain.WireGuardPerformanceTest{}, err
	}
	test.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return domain.WireGuardPerformanceTest{}, err
	}
	if resultJSON.Valid {
		var result domain.WireGuardPerformanceResult
		if err := json.Unmarshal([]byte(resultJSON.String), &result); err != nil {
			return domain.WireGuardPerformanceTest{}, err
		}
		test.Result = &result
	}
	if startedAt.Valid {
		value, parseErr := parseTime(startedAt.String)
		if parseErr != nil {
			return domain.WireGuardPerformanceTest{}, parseErr
		}
		test.StartedAt = &value
	}
	if finishedAt.Valid {
		value, parseErr := parseTime(finishedAt.String)
		if parseErr != nil {
			return domain.WireGuardPerformanceTest{}, parseErr
		}
		test.FinishedAt = &value
	}
	return test, nil
}

func (s *Store) ClaimWireGuardPerformanceTest(nodeID string) (*domain.WireGuardPerformanceTest, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	staleBefore := stamp(now().Add(-10 * time.Minute))
	if _, err := tx.Exec(`UPDATE wireguard_performance_tests SET status=?, error=?, finished_at=?
		WHERE node_id=? AND status=? AND started_at<?`, domain.WireGuardPerformanceFailed,
		"edge did not report the performance result before the execution deadline", stamp(now()), nodeID,
		domain.WireGuardPerformanceRunning, staleBefore); err != nil {
		return nil, err
	}
	var id string
	err = tx.QueryRow(`SELECT id FROM wireguard_performance_tests WHERE node_id=? AND status=? ORDER BY created_at LIMIT 1`,
		nodeID, domain.WireGuardPerformanceQueued).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	startedAt := now()
	result, err := tx.Exec(`UPDATE wireguard_performance_tests SET status=?, started_at=? WHERE id=? AND status=?`,
		domain.WireGuardPerformanceRunning, stamp(startedAt), id, domain.WireGuardPerformanceQueued)
	if err != nil {
		return nil, err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	test, err := s.GetWireGuardPerformanceTest(id)
	return &test, err
}

func (s *Store) FinishWireGuardPerformanceTest(nodeID, testID string, result *domain.WireGuardPerformanceResult, detail string) error {
	detail = strings.TrimSpace(detail)
	if len(detail) > 1000 {
		return errors.New("WireGuard performance error is too long")
	}
	status := domain.WireGuardPerformanceFailed
	var encoded any
	if result != nil {
		if !domain.ValidWireGuardPerformanceResult(*result) {
			return errors.New("invalid WireGuard performance result")
		}
		contents, err := json.Marshal(result)
		if err != nil {
			return err
		}
		encoded = string(contents)
		if detail == "" {
			status = domain.WireGuardPerformanceSucceeded
		}
	} else if detail == "" {
		detail = "WireGuard performance test failed"
	}
	resultSQL, err := s.db.Exec(`UPDATE wireguard_performance_tests SET status=?, result_json=?, error=?, finished_at=?
		WHERE id=? AND node_id=? AND status=?`, status, encoded, detail, stamp(now()), testID, nodeID, domain.WireGuardPerformanceRunning)
	if err != nil {
		return err
	}
	if changed, err := resultSQL.RowsAffected(); err != nil {
		return err
	} else if changed != 1 {
		return ErrNotFound
	}
	return nil
}

func WireGuardTunnelHasNodes(tunnel domain.WireGuardTunnel, nodeIDs []string) bool {
	assigned := make([]string, 0, len(tunnel.Peers))
	for _, peer := range tunnel.Peers {
		assigned = append(assigned, peer.NodeID)
	}
	for _, nodeID := range nodeIDs {
		if !slices.Contains(assigned, nodeID) {
			return false
		}
	}
	return true
}

func validateSiteWireGuardTunnels(queryer rowQueryer, site domain.Site) error {
	tunnelIDs := make(map[string]string, 2)
	if site.PrimaryOrigin.WireGuardTunnelID != "" {
		tunnelIDs[site.PrimaryOrigin.WireGuardTunnelID] = "primary"
	}
	if site.BackupOrigin != nil && site.BackupOrigin.WireGuardTunnelID != "" {
		role := "backup"
		if _, exists := tunnelIDs[site.BackupOrigin.WireGuardTunnelID]; !exists {
			tunnelIDs[site.BackupOrigin.WireGuardTunnelID] = role
		}
	}
	for tunnelID, role := range tunnelIDs {
		var tunnelName string
		if err := queryer.QueryRow(`SELECT name FROM wireguard_tunnels WHERE id=?`, tunnelID).Scan(&tunnelName); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%s origin WireGuard tunnel %s: %w", role, tunnelID, ErrNotFound)
		} else if err != nil {
			return err
		}
		for _, nodeID := range site.Nodes {
			var assigned int
			if err := queryer.QueryRow(`SELECT COUNT(*) FROM wireguard_tunnel_nodes WHERE tunnel_id=? AND node_id=?`, tunnelID, nodeID).Scan(&assigned); err != nil {
				return err
			}
			if assigned != 1 {
				return fmt.Errorf("WireGuard tunnel %s is not assigned to every edge node selected by the site", tunnelName)
			}
		}
	}
	return nil
}
