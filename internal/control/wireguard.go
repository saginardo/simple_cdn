package control

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"simple_cdn/internal/auth"
	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

//go:embed install-origin-wireguard.sh
var installOriginWireGuardScript string

type wireGuardTunnelRequest struct {
	Name                       string   `json:"name"`
	EndpointHost               string   `json:"endpoint_host"`
	ListenPort                 int      `json:"listen_port"`
	AddressCIDR                string   `json:"address_cidr"`
	MTU                        int      `json:"mtu"`
	PersistentKeepaliveSeconds int      `json:"persistent_keepalive_seconds"`
	PerformancePort            int      `json:"performance_port"`
	NodeIDs                    []string `json:"node_ids"`
}

func (input wireGuardTunnelRequest) tunnel(id string) domain.WireGuardTunnel {
	return domain.WireGuardTunnel{
		ID: id, Name: input.Name, EndpointHost: input.EndpointHost, ListenPort: input.ListenPort,
		AddressCIDR: input.AddressCIDR, MTU: input.MTU,
		PersistentKeepaliveSecs: input.PersistentKeepaliveSeconds, PerformancePort: input.PerformancePort,
	}
}

func (s *Server) listWireGuardTunnels(response http.ResponseWriter, _ *http.Request) {
	tunnels, err := s.Store.ListWireGuardTunnels()
	if err != nil {
		writeStoreError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, tunnels)
}

func (s *Server) suggestedWireGuardCIDR(response http.ResponseWriter, _ *http.Request) {
	value, err := s.Store.SuggestedWireGuardCIDR()
	if err != nil {
		writeStoreError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]string{"address_cidr": value})
}

func (s *Server) createWireGuardTunnel(response http.ResponseWriter, request *http.Request) {
	var input wireGuardTunnelRequest
	if !readJSON(response, request, &input) {
		return
	}
	if err := s.validateWireGuardNodes(input.NodeIDs, false); err != nil {
		writeError(response, http.StatusConflict, err)
		return
	}
	tunnel, err := s.Store.CreateWireGuardTunnel(input.tunnel(""), input.NodeIDs)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	s.audit(request, adminID(request.Context()), "create", "wireguard_tunnel", tunnel.ID, tunnel.Name)
	writeJSON(response, http.StatusCreated, tunnel)
}

func (s *Server) updateWireGuardTunnel(response http.ResponseWriter, request *http.Request) {
	var input wireGuardTunnelRequest
	if !readJSON(response, request, &input) {
		return
	}
	if err := s.validateWireGuardNodes(input.NodeIDs, false); err != nil {
		writeError(response, http.StatusConflict, err)
		return
	}
	tunnel, err := s.Store.UpdateWireGuardTunnel(input.tunnel(request.PathValue("id")), input.NodeIDs)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	s.audit(request, adminID(request.Context()), "update", "wireguard_tunnel", tunnel.ID, tunnel.Name)
	writeJSON(response, http.StatusOK, tunnel)
}

func (s *Server) validateWireGuardNodes(nodeIDs []string, performance bool) error {
	if len(nodeIDs) == 0 {
		return errors.New("select at least one WireGuard-capable edge node")
	}
	seen := make(map[string]bool, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		if seen[nodeID] {
			return errors.New("edge node selection contains duplicates")
		}
		seen[nodeID] = true
		node, err := s.Store.GetNode(nodeID)
		if err != nil {
			return err
		}
		if !slices.Contains(node.Capabilities, domain.EdgeCapabilityWireGuard) {
			return fmt.Errorf("edge node %s must be upgraded or reinstalled with WireGuard support", node.Name)
		}
		if performance && !slices.Contains(node.Capabilities, domain.EdgeCapabilityWireGuardPerformance) {
			return fmt.Errorf("edge node %s does not have iperf3 performance-test support", node.Name)
		}
	}
	return nil
}

func (s *Server) deleteWireGuardTunnel(response http.ResponseWriter, request *http.Request) {
	tunnel, err := s.Store.GetWireGuardTunnel(request.PathValue("id"))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	var input struct {
		Confirmation string `json:"confirmation"`
	}
	if !readJSON(response, request, &input) {
		return
	}
	if input.Confirmation != tunnel.Name {
		writeError(response, http.StatusBadRequest, errors.New("confirmation must exactly match the tunnel name"))
		return
	}
	if err := s.Store.DeleteWireGuardTunnel(tunnel.ID); err != nil {
		writeStoreError(response, err)
		return
	}
	s.audit(request, adminID(request.Context()), "delete", "wireguard_tunnel", tunnel.ID, tunnel.Name)
	writeJSON(response, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) createWireGuardInstallCommand(response http.ResponseWriter, request *http.Request) {
	tunnel, err := s.Store.GetWireGuardTunnel(request.PathValue("id"))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	if !validHTTPSURL(s.ControlURL) {
		writeError(response, http.StatusConflict, errors.New("CONTROL_PUBLIC_URL must be HTTPS before generating a WireGuard install command"))
		return
	}
	token, err := auth.NewOpaqueToken(32)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	expiresAt := time.Now().UTC().Add(15 * time.Minute)
	if err := s.Store.CreateWireGuardInstallToken(tunnel.ID, wireGuardTokenHash(token), expiresAt); err != nil {
		writeStoreError(response, err)
		return
	}
	scriptURL := strings.TrimRight(s.ControlURL, "/") + "/install-origin-wireguard.sh"
	command := fmt.Sprintf("curl -fsSL %q | sudo bash -s -- --control-url %q --token %q --tunnel-id %q", scriptURL, s.ControlURL, token, tunnel.ID)
	s.audit(request, adminID(request.Context()), "create_install_token", "wireguard_tunnel", tunnel.ID, "expires "+expiresAt.Format(time.RFC3339))
	writeJSON(response, http.StatusCreated, map[string]any{"install_command": command, "expires_at": expiresAt})
}

func (s *Server) wireGuardUninstallCommand(response http.ResponseWriter, request *http.Request) {
	tunnel, err := s.Store.GetWireGuardTunnel(request.PathValue("id"))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	if !validHTTPSURL(s.ControlURL) {
		writeError(response, http.StatusConflict, errors.New("CONTROL_PUBLIC_URL must be HTTPS before generating a WireGuard uninstall command"))
		return
	}
	scriptURL := strings.TrimRight(s.ControlURL, "/") + "/install-origin-wireguard.sh"
	command := fmt.Sprintf("curl -fsSL %q | sudo bash -s -- --tunnel-id %q --uninstall", scriptURL, tunnel.ID)
	writeJSON(response, http.StatusOK, map[string]string{"uninstall_command": command})
}

func (s *Server) installOriginWireGuard(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	_, _ = response.Write([]byte(installOriginWireGuardScript))
}

type wireGuardOriginConfigureRequest struct {
	Token     string `json:"token"`
	PublicKey string `json:"public_key"`
}

func (s *Server) configureWireGuardOrigin(response http.ResponseWriter, request *http.Request) {
	var input wireGuardOriginConfigureRequest
	if !readJSON(response, request, &input) {
		return
	}
	if len(input.Token) > 256 || strings.ContainsAny(input.Token, "\x00\r\n") {
		writeError(response, http.StatusForbidden, store.ErrWireGuardInstallToken)
		return
	}
	tunnel, ready, err := s.Store.ConfigureWireGuardOrigin(wireGuardTokenHash(input.Token), strings.TrimSpace(input.PublicKey))
	if err != nil {
		if errors.Is(err, store.ErrWireGuardInstallToken) {
			writeError(response, http.StatusForbidden, err)
		} else {
			writeStoreError(response, err)
		}
		return
	}
	if !ready {
		writeJSON(response, http.StatusConflict, map[string]any{"error": store.ErrWireGuardPeersPending.Error(), "retry_after_seconds": 3})
		return
	}
	_, network, _ := net.ParseCIDR(tunnel.AddressCIDR)
	prefix, _ := network.Mask.Size()
	type originPeer struct {
		PublicKey  string `json:"public_key"`
		AllowedIP  string `json:"allowed_ip"`
		PublicIPv4 string `json:"public_ipv4"`
	}
	peers := make([]originPeer, 0, len(tunnel.Peers))
	for _, peer := range tunnel.Peers {
		if !domain.ValidWireGuardKey(peer.PublicKey) {
			writeError(response, http.StatusConflict, store.ErrWireGuardPeersPending)
			return
		}
		peers = append(peers, originPeer{PublicKey: peer.PublicKey, AllowedIP: peer.Address + "/32", PublicIPv4: peer.NodePublicIPv4})
	}
	s.audit(request, "origin-installer", "configure", "wireguard_tunnel", tunnel.ID, fmt.Sprintf("revision %d", tunnel.Revision))
	writeJSON(response, http.StatusOK, map[string]any{
		"tunnel_id": tunnel.ID, "interface_name": domain.WireGuardInterfaceName(tunnel.ID),
		"origin_address_cidr": fmt.Sprintf("%s/%d", tunnel.OriginAddress, prefix),
		"listen_port":         tunnel.ListenPort, "performance_port": tunnel.PerformancePort,
		"mtu": tunnel.MTU, "revision": tunnel.Revision, "peers": peers,
	})
}

func wireGuardTokenHash(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func (s *Server) edgeWireGuardConfig(response http.ResponseWriter, request *http.Request) {
	configs, err := s.Store.WireGuardEdgeConfigs(edgeNodeID(request.Context()))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	revision := revisionDigest(configs)
	if requestHasRevision(request, revision) {
		writeRevisionNotModified(response, revision)
		return
	}
	response.Header().Set("ETag", revisionETag(revision))
	writeJSON(response, http.StatusOK, map[string]any{"revision": revision, "tunnels": configs})
}

func (s *Server) edgeWireGuardStatus(response http.ResponseWriter, request *http.Request) {
	var input struct {
		Reports []domain.WireGuardPeerReport `json:"reports"`
	}
	if !readJSON(response, request, &input) {
		return
	}
	if err := s.Store.UpdateWireGuardPeerReports(edgeNodeID(request.Context()), input.Reports); err != nil {
		writeStoreError(response, err)
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]bool{"ok": true})
}

func (s *Server) listWireGuardPerformanceTests(response http.ResponseWriter, request *http.Request) {
	limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
	tests, err := s.Store.ListWireGuardPerformanceTests(limit)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, tests)
}

func (s *Server) createWireGuardPerformanceTest(response http.ResponseWriter, request *http.Request) {
	var input struct {
		TunnelID        string `json:"tunnel_id"`
		NodeID          string `json:"node_id"`
		TargetMbps      int    `json:"target_mbps"`
		DurationSeconds int    `json:"duration_seconds"`
	}
	if !readJSON(response, request, &input) {
		return
	}
	if err := s.validateWireGuardNodes([]string{input.NodeID}, true); err != nil {
		writeError(response, http.StatusConflict, err)
		return
	}
	tunnel, err := s.Store.GetWireGuardTunnel(input.TunnelID)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	if tunnel.OriginPublicKey == "" || tunnel.OriginConfiguredRevision != tunnel.Revision {
		writeError(response, http.StatusConflict, errors.New("apply the latest source install command before running a performance test"))
		return
	}
	peerReady := false
	for _, peer := range tunnel.Peers {
		if peer.NodeID == input.NodeID && peer.PublicKey != "" && peer.AppliedRevision == tunnel.Revision && peer.LastError == "" {
			peerReady = true
		}
	}
	if !peerReady {
		writeError(response, http.StatusConflict, errors.New("the selected edge has not applied the WireGuard tunnel"))
		return
	}
	test, err := s.Store.CreateWireGuardPerformanceTest(input.TunnelID, input.NodeID, input.TargetMbps, input.DurationSeconds)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	s.audit(request, adminID(request.Context()), "start_performance_test", "wireguard_tunnel", tunnel.ID, test.ID+" node="+input.NodeID)
	writeJSON(response, http.StatusAccepted, test)
}

func (s *Server) edgeWireGuardPerformanceTest(response http.ResponseWriter, request *http.Request) {
	nodeID := edgeNodeID(request.Context())
	test, err := s.Store.ClaimWireGuardPerformanceTest(nodeID)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	if test == nil {
		response.WriteHeader(http.StatusNoContent)
		return
	}
	configs, err := s.Store.WireGuardEdgeConfigs(nodeID)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	for _, config := range configs {
		if config.TunnelID == test.TunnelID {
			writeJSON(response, http.StatusOK, map[string]any{"test": test, "config": config})
			return
		}
	}
	_ = s.Store.FinishWireGuardPerformanceTest(nodeID, test.ID, nil, "WireGuard tunnel configuration disappeared before the test started")
	writeError(response, http.StatusConflict, errors.New("WireGuard tunnel configuration is unavailable"))
}

func (s *Server) edgeWireGuardPerformanceResult(response http.ResponseWriter, request *http.Request) {
	var input struct {
		Result *domain.WireGuardPerformanceResult `json:"result"`
		Error  string                             `json:"error"`
	}
	if !readJSON(response, request, &input) {
		return
	}
	if err := s.Store.FinishWireGuardPerformanceTest(edgeNodeID(request.Context()), request.PathValue("id"), input.Result, input.Error); err != nil {
		writeStoreError(response, err)
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]bool{"ok": true})
}
