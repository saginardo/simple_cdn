package control

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
	"simple_cdn/internal/version"
)

const (
	nodeUpgradeHeartbeatFreshness = 10 * time.Minute
	nodeUpgradeTimeout            = 30 * time.Minute
)

type nodeUpgradeStatusResponse struct {
	domain.Node
	TargetAgentSHA256   string                  `json:"target_agent_sha256,omitempty"`
	TargetAgentVersion  string                  `json:"target_agent_version,omitempty"`
	TargetNginxSHA256   string                  `json:"target_nginx_sha256,omitempty"`
	TargetNginxVersion  string                  `json:"target_nginx_version,omitempty"`
	UpgradeCapable      bool                    `json:"upgrade_capable"`
	NginxUpgradeCapable bool                    `json:"nginx_upgrade_capable"`
	UpgradeUpToDate     bool                    `json:"upgrade_up_to_date"`
	CanUpgrade          bool                    `json:"can_upgrade"`
	UpgradeBlocker      string                  `json:"upgrade_blocker,omitempty"`
	UpgradeTask         *domain.NodeUpgradeTask `json:"upgrade_task,omitempty"`
}

type nodeUpgradeAllResult struct {
	NodeID string                  `json:"node_id"`
	Name   string                  `json:"name"`
	State  string                  `json:"state"`
	Detail string                  `json:"detail,omitempty"`
	Task   *domain.NodeUpgradeTask `json:"task,omitempty"`
}

type nodeUpgradeAllResponse struct {
	Created       int                    `json:"created"`
	AlreadyActive int                    `json:"already_active"`
	UpToDate      int                    `json:"up_to_date"`
	Blocked       int                    `json:"blocked"`
	Results       []nodeUpgradeAllResult `json:"results"`
}

func (s *Server) buildNodeUpgradeStatus(node domain.Node) (nodeUpgradeStatusResponse, error) {
	nginxTarget, nginxTargetErr := s.currentNginxArtifactTarget()
	if nginxTargetErr != nil {
		nginxTarget = s.builtinNginxArtifactTarget()
	}
	result := nodeUpgradeStatusResponse{
		Node:              node,
		TargetAgentSHA256: strings.ToLower(strings.TrimSpace(s.EdgeBinarySHA256)), TargetAgentVersion: version.Version,
		TargetNginxSHA256: nginxTarget.SHA256, TargetNginxVersion: nginxTarget.Version,
	}
	for _, capability := range node.Capabilities {
		if capability == domain.EdgeCapabilityOnlineUpgrade {
			result.UpgradeCapable = true
		}
		if capability == domain.EdgeCapabilityNginxBundle {
			result.NginxUpgradeCapable = true
		}
	}
	if task, err := s.Store.LatestNodeUpgrade(node.ID); err == nil {
		result.UpgradeTask = &task
	} else if !errors.Is(err, store.ErrNotFound) {
		return result, err
	}
	agentUpToDate := validSHA256Digest(node.AgentSHA256) && strings.EqualFold(node.AgentSHA256, result.TargetAgentSHA256)
	nginxUpToDate := validSHA256Digest(node.NginxSHA256) && strings.EqualFold(node.NginxSHA256, result.TargetNginxSHA256)
	result.UpgradeUpToDate = nginxTargetErr == nil && agentUpToDate && nginxUpToDate
	if result.UpgradeTask != nil && (result.UpgradeTask.Status == domain.NodeUpgradeQueued || result.UpgradeTask.Status == domain.NodeUpgradeApplying) {
		result.UpgradeBlocker = "节点升级正在进行"
		return result, nil
	}
	if nginxTargetErr != nil {
		result.UpgradeBlocker = nginxTargetErr.Error()
		return result, nil
	}
	if result.UpgradeUpToDate {
		result.UpgradeBlocker = "节点已是主控当前版本"
		return result, nil
	}
	if node.Status != domain.NodeActive && node.Status != domain.NodeDraining {
		result.UpgradeBlocker = "仅运行中或已暂停的节点可以在线升级"
		return result, nil
	}
	if !result.UpgradeCapable || !validSHA256Digest(node.AgentSHA256) {
		result.UpgradeBlocker = "需要先手动执行一次部署/升级命令以启用在线升级"
		return result, nil
	}
	if agentUpToDate && !nginxUpToDate && !result.NginxUpgradeCapable {
		result.UpgradeBlocker = "需要先手动执行一次部署/升级命令以启用受管 Nginx 在线升级"
		return result, nil
	}
	if node.LastHeartbeatAt == nil || node.LastHeartbeatAt.Before(time.Now().UTC().Add(-nodeUpgradeHeartbeatFreshness)) {
		result.UpgradeBlocker = "节点心跳已过期"
		return result, nil
	}
	if node.ActiveUpgradeID != "" {
		result.UpgradeBlocker = "节点仍在清理上一次本地升级任务"
		return result, nil
	}
	if err := s.validateNodeUpgradeArtifacts(); err != nil {
		result.UpgradeBlocker = err.Error()
		return result, nil
	}
	if active, err := s.Store.HasActiveNodeUninstall(node.ID); err != nil {
		return result, err
	} else if active {
		result.UpgradeBlocker = "节点卸载流程正在进行"
		return result, nil
	}
	if active, err := s.Store.HasActiveNodePublication(node.ID); err != nil {
		return result, err
	} else if active {
		result.UpgradeBlocker = "节点正在确认站点配置"
		return result, nil
	}
	result.CanUpgrade = true
	return result, nil
}

func (s *Server) validateNodeUpgradeArtifacts() error {
	nginxTarget, err := s.currentNginxArtifactTarget()
	if err != nil {
		return err
	}
	if !validHTTPSURL(s.edgeControlURL()) ||
		!validHTTPSURL(s.EdgeBinaryURL) || !validSHA256Digest(strings.TrimSpace(s.EdgeBinarySHA256)) ||
		!validHTTPSURL(nginxTarget.URL) || !validSHA256Digest(nginxTarget.SHA256) ||
		!nginxVersionPattern.MatchString(nginxTarget.Version) {
		return errors.New("主控尚未配置有效的边缘升级制品")
	}
	return nil
}

func (s *Server) nodeUpgradeInstruction(node domain.Node) domain.NodeUpgradeInstruction {
	baseURL := strings.TrimRight(s.edgeControlURL(), "/")
	nginxTarget, err := s.currentNginxArtifactTarget()
	if err != nil {
		nginxTarget = s.builtinNginxArtifactTarget()
	}
	installer := s.renderEdgeInstallerFor(nginxTarget)
	installerURL := baseURL + "/install-edge.sh"
	if nginxTarget.Path != "" && validSHA256Digest(nginxTarget.SHA256) {
		installerURL = s.versionedEdgeInstallerURL(nginxTarget.SHA256)
	}
	instruction := domain.NodeUpgradeInstruction{
		Binary:         domain.UpgradeArtifact{URL: s.EdgeBinaryURL, SHA256: strings.ToLower(strings.TrimSpace(s.EdgeBinarySHA256))},
		Installer:      domain.UpgradeArtifact{URL: installerURL, SHA256: resourceSHA256(installer)},
		AgentService:   domain.UpgradeArtifact{URL: baseURL + "/install-edge.service", SHA256: resourceSHA256(bootstrapEdgeService)},
		UpdaterService: domain.UpgradeArtifact{URL: baseURL + "/install-edge-updater.service", SHA256: resourceSHA256(bootstrapEdgeUpdaterService)},
	}
	for _, capability := range node.Capabilities {
		if capability == domain.EdgeCapabilityNginxBundle {
			bundle := domain.UpgradeArtifact{URL: nginxTarget.URL, SHA256: nginxTarget.SHA256}
			service := domain.UpgradeArtifact{URL: baseURL + "/install-edge-nginx.service", SHA256: resourceSHA256(bootstrapEdgeNginxService)}
			instruction.NginxBundle = &bundle
			instruction.NginxService = &service
			break
		}
	}
	return instruction
}

func resourceSHA256(value string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func (s *Server) nodeUpgradeStatus(response http.ResponseWriter, request *http.Request) {
	if err := s.Store.ReconcileNodeUpgrades(); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	node, err := s.Store.GetNode(request.PathValue("id"))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	status, err := s.buildNodeUpgradeStatus(node)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, status)
}

func (s *Server) startNodeUpgrade(response http.ResponseWriter, request *http.Request) {
	if err := s.Store.ReconcileNodeUpgrades(); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	node, err := s.Store.GetNode(request.PathValue("id"))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	status, err := s.buildNodeUpgradeStatus(node)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	if status.UpgradeTask != nil && (status.UpgradeTask.Status == domain.NodeUpgradeQueued || status.UpgradeTask.Status == domain.NodeUpgradeApplying) {
		writeJSON(response, http.StatusOK, status)
		return
	}
	if !status.CanUpgrade {
		writeJSON(response, http.StatusConflict, map[string]any{"error": status.UpgradeBlocker, "upgrade": status})
		return
	}
	instruction := s.nodeUpgradeInstruction(node)
	task, created, err := s.Store.CreateOrGetNodeUpgrade(node.ID, instruction, time.Now().UTC().Add(nodeUpgradeTimeout))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	status.UpgradeTask = &task
	status.CanUpgrade = false
	status.UpgradeBlocker = "节点升级正在进行"
	if created {
		s.audit(request, adminID(request.Context()), "start_upgrade", "node", node.ID, "target sha256:"+task.TargetSHA256)
		writeJSON(response, http.StatusCreated, status)
		return
	}
	writeJSON(response, http.StatusOK, status)
}

func (s *Server) startAllNodeUpgrades(response http.ResponseWriter, request *http.Request) {
	if err := s.Store.ReconcileNodeUpgrades(); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	nodes, err := s.Store.ListNodes()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	result := nodeUpgradeAllResponse{Results: make([]nodeUpgradeAllResult, 0, len(nodes))}
	for _, node := range nodes {
		status, err := s.buildNodeUpgradeStatus(node)
		if err != nil {
			writeError(response, http.StatusInternalServerError, err)
			return
		}
		item := nodeUpgradeAllResult{NodeID: node.ID, Name: node.Name}
		switch {
		case status.UpgradeTask != nil && (status.UpgradeTask.Status == domain.NodeUpgradeQueued || status.UpgradeTask.Status == domain.NodeUpgradeApplying):
			item.State, item.Detail, item.Task = "already_active", status.UpgradeBlocker, status.UpgradeTask
			result.AlreadyActive++
		case status.UpgradeUpToDate:
			item.State, item.Detail = "up_to_date", status.UpgradeBlocker
			result.UpToDate++
		case !status.CanUpgrade:
			item.State, item.Detail = "blocked", status.UpgradeBlocker
			result.Blocked++
		default:
			task, _, err := s.Store.CreateOrGetNodeUpgrade(node.ID, s.nodeUpgradeInstruction(node), time.Now().UTC().Add(nodeUpgradeTimeout))
			if err != nil {
				if errors.Is(err, store.ErrNodeOperationActive) || errors.Is(err, store.ErrNodeUpgradeActive) || errors.Is(err, store.ErrUpgradeRetryNotReady) {
					item.State, item.Detail = "blocked", err.Error()
					result.Blocked++
					break
				}
				writeStoreError(response, err)
				return
			}
			item.State, item.Detail, item.Task = "created", "节点升级已排队", &task
			result.Created++
			s.audit(request, adminID(request.Context()), "start_upgrade", "node", node.ID, "bulk target sha256:"+task.TargetSHA256)
		}
		result.Results = append(result.Results, item)
	}
	statusCode := http.StatusOK
	if result.Created != 0 {
		statusCode = http.StatusAccepted
	}
	writeJSON(response, statusCode, result)
}

func (s *Server) edgeUpgradeInstruction(response http.ResponseWriter, request *http.Request) {
	instruction, err := s.Store.NodeUpgradeInstruction(edgeNodeID(request.Context()))
	if errors.Is(err, store.ErrNotFound) {
		response.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, instruction)
}

func (s *Server) edgeUpgradeReport(response http.ResponseWriter, request *http.Request) {
	var report domain.NodeUpgradeReport
	if !readJSON(response, request, &report) {
		return
	}
	report.TaskID = strings.TrimSpace(report.TaskID)
	report.ErrorCode = strings.TrimSpace(report.ErrorCode)
	report.Detail = strings.TrimSpace(report.Detail)
	report.InstalledSHA256 = strings.ToLower(strings.TrimSpace(report.InstalledSHA256))
	report.InstalledNginxSHA256 = strings.ToLower(strings.TrimSpace(report.InstalledNginxSHA256))
	if !validNodeUpgradeTaskID(report.TaskID) || len(report.ErrorCode) > 64 || len(report.Detail) > 4096 ||
		(report.InstalledSHA256 != "" && !validSHA256Digest(report.InstalledSHA256)) ||
		(report.InstalledNginxSHA256 != "" && !validSHA256Digest(report.InstalledNginxSHA256)) {
		writeError(response, http.StatusBadRequest, errors.New("invalid node upgrade report"))
		return
	}
	nodeID := edgeNodeID(request.Context())
	task, err := s.Store.RecordNodeUpgradeReport(nodeID, report)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	if task.Status == domain.NodeUpgradeApplying || task.Status == domain.NodeUpgradeSucceeded || task.Status == domain.NodeUpgradeFailed {
		s.audit(request, "edge:"+nodeID, "upgrade_"+string(task.Status), "node", nodeID, task.Detail)
	}
	writeJSON(response, http.StatusOK, task)
}

func validNodeUpgradeTaskID(value string) bool {
	_, err := uuid.Parse(value)
	return err == nil
}
