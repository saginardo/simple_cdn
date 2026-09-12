package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

const systemHealthInterval = 30 * time.Second
const systemHealthValidity = 90 * time.Second
const systemHealthHeartbeatAge = 90 * time.Second
const systemHealthProbeAge = 3 * time.Minute

// SystemHealthManager only interprets existing observations. It never runs
// network probes, reconciles desired state or starts operational tasks.
type SystemHealthManager struct {
	Server    *Server
	Now       func() time.Time
	collectMu sync.Mutex
	mu        sync.RWMutex
	lastError bool
}

func (m *SystemHealthManager) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

func (m *SystemHealthManager) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := m.Collect(ctx); err != nil && ctx.Err() == nil && m.Server.Logger != nil {
			m.Server.Logger.Warn("system health collection failed", "error", err)
		}
		timer := time.NewTimer(systemHealthInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (m *SystemHealthManager) Collect(ctx context.Context) (err error) {
	m.collectMu.Lock()
	defer m.collectMu.Unlock()
	defer func() { m.mu.Lock(); m.lastError = err != nil; m.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	inputs, err := m.Server.Store.SystemHealthInputs(ctx)
	if err != nil {
		return err
	}
	now := m.now()
	snapshot := evaluateSystemHealth(inputs, now)
	check := healthCheck("control:reconciliation", "control", "健康协调器", "/monitoring", "查看监测")
	check.Impact = "协调器负责服务探测和 DNS 对账，异常可能延迟故障切换。"
	if m.Server.HealthManager == nil {
		check.State, check.Summary = domain.HealthDisabled, "健康协调器未启用"
	} else {
		round := m.Server.HealthManager.LastRound()
		check.ObservedAt = nonzeroHealthTime(round.FinishedAt)
		check.Evidence = []domain.HealthEvidence{{Label: "最近完成时间", Value: healthTime(check.ObservedAt)}, {Label: "检查错误数", Value: strconv.Itoa(round.ErrorCount)}}
		if !healthFresh(check.ObservedAt, now, systemHealthProbeAge) {
			check.State, check.Summary = domain.HealthUnknown, "尚无近期健康协调结果"
		} else if round.TimedOut || round.ErrorCount > 0 || round.Error != "" {
			check.State, check.Summary = domain.HealthWarning, "最近一轮探测或 DNS 对账存在错误"
			check.Evidence = append(check.Evidence, domain.HealthEvidence{Label: "最近错误", Value: healthDetail(round.Error)})
		} else {
			check.Summary = "健康协调器持续运行"
		}
	}
	snapshot.Checks = append(snapshot.Checks, check)
	var backup *BackupHealthStatus
	if m.Server.BackupHealth != nil {
		status := m.Server.BackupHealth.Status()
		backup = &status
	}
	snapshot.Checks = append(snapshot.Checks, backupHealthChecks(backup, now)...)
	finalizeSystemHealth(&snapshot)
	return m.Server.Store.SaveSystemHealthSnapshot(ctx, snapshot)
}

func (s *Server) systemHealth(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Cache-Control", "no-store")
	snapshot, err := s.Store.SystemHealthSnapshot(request.Context())
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeStoreError(response, err)
		return
	}
	now := time.Now().UTC()
	failed := false
	if s.SystemHealth != nil {
		now = s.SystemHealth.now()
		s.SystemHealth.mu.RLock()
		failed = s.SystemHealth.lastError
		s.SystemHealth.mu.RUnlock()
	}
	prepareSystemHealthResponse(&snapshot, now, failed)
	writeJSON(response, http.StatusOK, snapshot)
}

func prepareSystemHealthResponse(snapshot *domain.SystemHealthSnapshot, now time.Time, failed bool) {
	if snapshot.Checks == nil {
		snapshot.Checks = []domain.HealthCheck{}
	}
	if snapshot.Nodes == nil {
		snapshot.Nodes = []domain.HealthResource{}
	}
	if snapshot.Sites == nil {
		snapshot.Sites = []domain.HealthResource{}
	}
	if snapshot.Recoveries == nil {
		snapshot.Recoveries = []domain.HealthRecovery{}
	}
	snapshot.Stale = snapshot.CollectedAt == nil || snapshot.ValidUntil == nil || !now.Before(*snapshot.ValidUntil) || snapshot.CollectedAt.After(now.Add(30*time.Second))
	if snapshot.Stale || failed {
		check := healthCheck("control:collection", "control", "健康数据采集", "/health", "查看状态")
		check.State, check.Summary = domain.HealthUnknown, "健康数据尚未采集或已过期"
		if failed {
			check.Summary = "最近一次健康数据采集失败"
		}
		check.ObservedAt = snapshot.CollectedAt
		check.Impact = "当前检查结果可能不再反映实际运行状态，请等待后台重新采集。"
		check.Evidence = []domain.HealthEvidence{{Label: "最近采集时间", Value: healthTime(snapshot.CollectedAt)}}
		check.Since = now
		if snapshot.ValidUntil != nil && snapshot.ValidUntil.Before(now) {
			check.Since = *snapshot.ValidUntil
		}
		snapshot.Checks = append(snapshot.Checks, check)
		// Never present a cached green result as current. Preserve known failures.
		if snapshot.Stale {
			for i := range snapshot.Checks {
				if snapshot.Checks[i].State == domain.HealthHealthy {
					snapshot.Checks[i].State = domain.HealthUnknown
					snapshot.Checks[i].Summary = "检查结果已过期"
					snapshot.Checks[i].Since = check.Since
				}
			}
		}
	}
	finalizeSystemHealth(snapshot)
}

func healthCheck(id, category, title, action, actionLabel string) domain.HealthCheck {
	return domain.HealthCheck{ID: id, Category: category, State: domain.HealthHealthy, Title: title, ActionURL: action, ActionLabel: actionLabel,
		NodeIDs: []string{}, SiteIDs: []string{}, Evidence: []domain.HealthEvidence{}}
}

func finalizeSystemHealth(snapshot *domain.SystemHealthSnapshot) {
	snapshot.State = domain.HealthDisabled
	for _, check := range snapshot.Checks {
		if domain.HealthStateRank(check.State) < domain.HealthStateRank(snapshot.State) {
			snapshot.State = check.State
		}
	}
	if len(snapshot.Checks) == 0 {
		snapshot.State = domain.HealthUnknown
	}
	sort.SliceStable(snapshot.Checks, func(i, j int) bool {
		a, b := snapshot.Checks[i], snapshot.Checks[j]
		if a.State != b.State {
			return domain.HealthStateRank(a.State) < domain.HealthStateRank(b.State)
		}
		if a.Category != b.Category {
			return a.Category < b.Category
		}
		return a.ID < b.ID
	})
}

func healthFresh(at *time.Time, now time.Time, age time.Duration) bool {
	return at != nil && !at.IsZero() && !at.After(now.Add(30*time.Second)) && now.Sub(*at) <= age
}
func healthTime(at *time.Time) string {
	if at == nil || at.IsZero() {
		return "尚无记录"
	}
	return at.UTC().Format(time.RFC3339)
}
func nonzeroHealthTime(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	return &at
}
func healthDetail(value string) string {
	chars := []rune(value)
	if len(chars) > 512 {
		return string(chars[:512]) + "…"
	}
	return value
}
func healthPath(kind, id string) string { return "/" + kind + "/" + url.PathEscape(id) }

func evaluateSystemHealth(input store.SystemHealthInputs, now time.Time) domain.SystemHealthSnapshot {
	valid := now.Add(systemHealthValidity)
	snapshot := domain.SystemHealthSnapshot{CollectedAt: &now, ValidUntil: &valid, Checks: []domain.HealthCheck{}, Recoveries: []domain.HealthRecovery{}, Nodes: []domain.HealthResource{}, Sites: []domain.HealthResource{}}
	nodes := map[string]domain.Node{}
	nodeSites := map[string][]string{}
	for _, site := range input.Sites {
		snapshot.Sites = append(snapshot.Sites, domain.HealthResource{ID: site.Draft.ID, Name: site.Draft.Name})
		if site.Published != nil && site.Published.Enabled && !site.Draft.Deleting {
			for _, id := range site.Published.AssignedNodeIDs() {
				nodeSites[id] = append(nodeSites[id], site.Draft.ID)
			}
		}
	}
	for _, node := range input.Nodes {
		nodes[node.ID] = node
		snapshot.Nodes = append(snapshot.Nodes, domain.HealthResource{ID: node.ID, Name: node.Name})
		check := healthCheck("node:"+node.ID+":heartbeat", "node", "节点心跳", healthPath("nodes", node.ID), "查看节点")
		check.NodeIDs, check.SiteIDs, check.ObservedAt = []string{node.ID}, append([]string{}, nodeSites[node.ID]...), node.LastHeartbeatAt
		check.Impact = "节点离线可能影响分配站点的可用性和后续配置下发。"
		check.Evidence = []domain.HealthEvidence{{Label: "节点", Value: node.Name}, {Label: "节点状态", Value: string(node.Status)}, {Label: "最近心跳", Value: healthTime(node.LastHeartbeatAt)}}
		switch {
		case node.Status != domain.NodeActive:
			check.State, check.Summary = domain.HealthDisabled, "节点未处于运行状态"
		case node.LastHeartbeatAt == nil || node.LastHeartbeatAt.IsZero() || node.LastHeartbeatAt.After(now.Add(30*time.Second)):
			check.State, check.Summary = domain.HealthUnknown, "节点心跳尚未上报或时间异常"
		case !healthFresh(node.LastHeartbeatAt, now, systemHealthHeartbeatAge):
			check.State, check.Summary = domain.HealthCritical, "节点心跳已过期"
		default:
			check.Summary = "节点心跳正常"
		}
		snapshot.Checks = append(snapshot.Checks, check)
		config := healthCheck("node:"+node.ID+":configuration", "publication", "节点配置一致性", healthPath("nodes", node.ID), "查看节点")
		config.NodeIDs, config.SiteIDs, config.ObservedAt = check.NodeIDs, check.SiteIDs, node.LastHeartbeatAt
		config.Impact = "配置未确认时，新规则可能尚未在该节点生效。"
		desired := input.DesiredVersions[node.ID]
		config.Evidence = []domain.HealthEvidence{{Label: "节点", Value: node.Name}, {Label: "目标版本", Value: strconv.FormatInt(desired, 10)}, {Label: "应用版本", Value: strconv.FormatInt(node.AppliedVersion, 10)}}
		switch {
		case node.Status != domain.NodeActive:
			config.State, config.Summary = domain.HealthDisabled, "节点未处于运行状态"
		case !healthFresh(node.LastHeartbeatAt, now, systemHealthHeartbeatAge):
			config.State, config.Summary = domain.HealthUnknown, "缺少近期节点确认"
		case desired == 0 && len(nodeSites[node.ID]) > 0:
			config.State, config.Summary = domain.HealthUnknown, "尚无目标配置"
		case node.LastError != "":
			config.State, config.Summary = domain.HealthWarning, "节点报告配置或运行错误"
			config.Evidence = append(config.Evidence, domain.HealthEvidence{Label: "最近错误", Value: healthDetail(node.LastError)})
		case node.AppliedVersion < desired:
			config.State, config.Summary = domain.HealthWarning, "节点配置版本落后"
		default:
			config.Summary = "节点配置已同步"
		}
		snapshot.Checks = append(snapshot.Checks, config)
	}
	publicationTasks, certificateTasks := map[string]domain.DeploymentTask{}, map[string]domain.DeploymentTask{}
	for _, task := range input.Tasks {
		if task.Kind == "publish_site" {
			publicationTasks[task.SiteID] = task
		} else {
			certificateTasks[task.SiteID] = task
		}
	}
	probes := map[string]store.HealthProbe{}
	for _, probe := range input.SiteProbes {
		probes[healthProbeKey(probe.SiteID, probe.NodeID, probe.IPv6)] = probe
	}
	for _, site := range input.Sites {
		for _, ipv6 := range []bool{false, true} {
			snapshot.Checks = append(snapshot.Checks, siteAvailabilityHealth(site, nodes, input.NodeProbes, probes, now, ipv6))
		}
		task, hasTask := publicationTasks[site.Draft.ID]
		check := healthCheck("site:"+site.Draft.ID+":publication", "publication", "站点发布", healthPath("sites", site.Draft.ID), "查看站点")
		check.SiteIDs = []string{site.Draft.ID}
		check.Impact = "发布异常可能导致部分节点继续使用之前的配置。"
		check.Evidence = []domain.HealthEvidence{{Label: "站点", Value: site.Draft.Name}}
		if hasTask && publishTaskMatchesSite(task, site.Draft) {
			check.ObservedAt = nonzeroHealthTime(task.UpdatedAt)
			check.NodeIDs = append([]string{}, site.Draft.AssignedNodeIDs()...)
			check.Evidence = append(check.Evidence, domain.HealthEvidence{Label: "任务状态", Value: string(task.Status)}, domain.HealthEvidence{Label: "任务", Value: task.ID}, domain.HealthEvidence{Label: "任务说明", Value: healthDetail(task.Detail)})
			check.State, check.Summary = healthTaskState(task, now, "发布")
		} else {
			check.State, check.Summary = domain.HealthDisabled, "尚无当前发布任务"
		}
		if site.Draft.Deleting {
			check.State, check.Summary = domain.HealthDisabled, "站点正在删除"
		}
		snapshot.Checks = append(snapshot.Checks, check, siteCertificateHealth(site, certificateTasks[site.Draft.ID], now))
	}
	for _, task := range input.Upgrades {
		node, exists := nodes[task.NodeID]
		if !exists {
			continue
		}
		check := healthCheck("node:"+node.ID+":upgrade", "upgrade", "节点升级", healthPath("nodes", node.ID), "查看升级任务")
		check.NodeIDs, check.SiteIDs = []string{node.ID}, append([]string{}, nodeSites[node.ID]...)
		check.ObservedAt = nonzeroHealthTime(task.UpdatedAt)
		check.Impact = "升级失败可能导致版本不一致；当前业务状态需结合节点探测判断。"
		check.Evidence = []domain.HealthEvidence{{Label: "节点", Value: node.Name}, {Label: "任务状态", Value: string(task.Status)}, {Label: "任务", Value: task.ID}, {Label: "任务说明", Value: healthDetail(task.Detail)}}
		switch {
		case node.Status == domain.NodeRevoked || node.Status == domain.NodeUninstalled:
			check.State, check.Summary = domain.HealthDisabled, "节点已退出管理"
		case task.Status == domain.NodeUpgradeFailed:
			check.State, check.Summary = domain.HealthWarning, "最近一次节点升级失败"
		case (task.Status == domain.NodeUpgradeQueued || task.Status == domain.NodeUpgradeApplying) && now.After(task.DeadlineAt):
			check.State, check.Summary = domain.HealthWarning, "节点升级超过任务期限"
		case task.Status == domain.NodeUpgradeQueued || task.Status == domain.NodeUpgradeApplying:
			check.Summary = "节点升级进行中"
		default:
			check.Summary = "最近一次节点升级已完成"
		}
		snapshot.Checks = append(snapshot.Checks, check)
	}
	check := healthCheck("upgrade:rollout", "upgrade", "分批升级", "/nodes", "查看升级批次")
	check.Impact = "暂停的批次不会继续派发后续节点。"
	if rollout := input.Rollout; rollout != nil {
		check.ObservedAt = nonzeroHealthTime(rollout.UpdatedAt)
		for _, member := range rollout.Members {
			check.NodeIDs = append(check.NodeIDs, member.NodeID)
			for _, siteID := range nodeSites[member.NodeID] {
				if !containsHealthID(check.SiteIDs, siteID) {
					check.SiteIDs = append(check.SiteIDs, siteID)
				}
			}
		}
		check.Evidence = []domain.HealthEvidence{{Label: "批次状态", Value: rollout.State}, {Label: "任务说明", Value: healthDetail(rollout.Detail)}}
		switch {
		case rollout.State == "paused":
			check.State, check.Summary = domain.HealthWarning, "分批升级已暂停"
		case rollout.State == "running" && !healthFresh(check.ObservedAt, now, systemHealthProbeAge):
			check.State, check.Summary = domain.HealthUnknown, "分批升级进度长时间未更新"
		case rollout.State == "running":
			check.Summary = "分批升级进行中"
		case rollout.State == "cancelled" || rollout.State == "canceled":
			check.State, check.Summary = domain.HealthDisabled, "分批升级已取消"
		default:
			check.Summary = "分批升级已完成"
		}
	} else {
		check.State, check.Summary = domain.HealthDisabled, "尚无分批升级"
	}
	snapshot.Checks = append(snapshot.Checks, check)
	return snapshot
}

func healthProbeKey(site, node string, ipv6 bool) string {
	return site + ":" + node + ":" + strconv.FormatBool(ipv6)
}

func containsHealthID(ids []string, id string) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

func siteAvailabilityHealth(site store.HealthSite, nodes map[string]domain.Node, nodeProbes map[string]store.HealthProbe, probes map[string]store.HealthProbe, now time.Time, ipv6 bool) domain.HealthCheck {
	family := "IPv4"
	if ipv6 {
		family = "IPv6"
	}
	check := healthCheck("site:"+site.Draft.ID+":"+family, "site", "站点 "+family+" 可用性", healthPath("sites", site.Draft.ID), "查看站点")
	check.SiteIDs = []string{site.Draft.ID}
	check.Impact = "可用节点减少会降低服务冗余；全部路径异常可能影响站点访问。"
	check.Evidence = []domain.HealthEvidence{{Label: "站点", Value: site.Draft.Name}}
	if site.Published == nil || !site.Published.Enabled || site.Draft.Deleting {
		check.State, check.Summary = domain.HealthDisabled, "站点尚未发布、已停用或正在删除"
		return check
	}
	if ipv6 && !site.Published.IPv6Enabled {
		check.State, check.Summary = domain.HealthDisabled, "站点未启用 IPv6"
		return check
	}
	healthy, unknown, eligible := 0, 0, 0
	for _, id := range site.Published.AssignedNodeIDs() {
		check.NodeIDs = append(check.NodeIDs, id)
		node, found := nodes[id]
		if !found || node.Status != domain.NodeActive || (ipv6 && node.PublicIPv6 == "") {
			continue
		}
		eligible++
		probe := probes[healthProbeKey(site.Draft.ID, id, ipv6)]
		nodeProbe := nodeProbes[id]
		fresh := healthFresh(probe.CheckedAt, now, systemHealthProbeAge) && healthFresh(nodeProbe.CheckedAt, now, systemHealthProbeAge) && healthFresh(node.LastHeartbeatAt, now, systemHealthHeartbeatAge)
		if site.PublishedAt != nil && probe.CheckedAt != nil && probe.CheckedAt.Before(*site.PublishedAt) {
			fresh = false
		}
		value := "探测异常"
		if !fresh {
			unknown++
			value = "探测数据过期或缺失"
		} else if probe.Eligible && nodeProbe.Eligible {
			healthy++
			value = "探测通过"
		}
		check.Evidence = append(check.Evidence, domain.HealthEvidence{Label: node.Name, Value: value})
		if probe.Error != "" {
			check.Evidence = append(check.Evidence, domain.HealthEvidence{Label: "最近错误", Value: healthDetail(probe.Error)})
		}
		if probe.CheckedAt != nil && (check.ObservedAt == nil || probe.CheckedAt.Before(*check.ObservedAt)) {
			check.ObservedAt = probe.CheckedAt
		}
	}
	check.Evidence = append(check.Evidence, domain.HealthEvidence{Label: "探测通过 / 运行节点", Value: fmt.Sprintf("%d / %d", healthy, eligible)}, domain.HealthEvidence{Label: "缺失或过期", Value: strconv.Itoa(unknown)})
	switch {
	case eligible == 0:
		check.State, check.Summary = domain.HealthCritical, "没有可参与服务的节点"
	case healthy == 0 && unknown > 0:
		check.State, check.Summary = domain.HealthUnknown, "无法确认站点可用性"
	case healthy == 0:
		check.State, check.Summary = domain.HealthCritical, "全部运行节点的服务探测未通过"
	case healthy < eligible:
		check.State, check.Summary = domain.HealthWarning, "部分节点探测异常或数据不完整"
	default:
		check.Summary = "运行节点的服务探测均已通过"
	}
	return check
}

func healthTaskState(task domain.DeploymentTask, now time.Time, label string) (domain.HealthState, string) {
	if task.Status == domain.TaskFailed || task.Status == domain.TaskPartial || task.Status == domain.TaskRolledBack {
		return domain.HealthWarning, label + "任务未全部成功"
	}
	if task.Status == domain.TaskQueued || task.Status == domain.TaskDispatching || task.Status == domain.TaskApplying {
		if (task.DeadlineAt != nil && now.After(*task.DeadlineAt)) || now.Sub(task.UpdatedAt) > 10*time.Minute {
			return domain.HealthWarning, label + "任务长时间未完成"
		}
		return domain.HealthHealthy, label + "任务进行中"
	}
	return domain.HealthHealthy, label + "任务已完成"
}

func siteCertificateHealth(site store.HealthSite, task domain.DeploymentTask, now time.Time) domain.HealthCheck {
	check := healthCheck("site:"+site.Draft.ID+":certificate", "certificate", "站点证书", healthPath("sites", site.Draft.ID), "查看站点证书")
	check.SiteIDs = []string{site.Draft.ID}
	check.Impact = "服务证书过期会影响 HTTPS 访问；新证书需要完成发布才能在边缘生效。"
	check.Evidence = []domain.HealthEvidence{{Label: "站点", Value: site.Draft.Name}}
	effective := site.Draft
	if site.Published != nil {
		effective = *site.Published
	}
	check.NodeIDs = append([]string{}, effective.AssignedNodeIDs()...)
	if !domain.SiteNeedsCertificate(effective) || !effective.Enabled || site.Draft.Deleting {
		check.State, check.Summary = domain.HealthDisabled, "站点无需 TLS 证书或已停用"
		return check
	}
	notAfter := site.ServingNotAfter
	check.ObservedAt = site.PublishedAt
	if site.Published == nil && site.Certificate != nil {
		notAfter = site.Certificate.NotAfter
		check.ObservedAt = nonzeroHealthTime(site.Certificate.UpdatedAt)
	}
	check.Evidence = append(check.Evidence, domain.HealthEvidence{Label: "服务证书到期时间", Value: healthTime(notAfter)})
	switch {
	case notAfter == nil && site.Published == nil:
		check.State, check.Summary = domain.HealthDisabled, "等待首次证书签发与发布"
	case notAfter == nil:
		check.State, check.Summary = domain.HealthUnknown, "缺少已发布证书有效期"
	case !now.Before(*notAfter):
		check.State, check.Summary = domain.HealthCritical, "服务证书已过期"
	case notAfter.Sub(now) <= 7*24*time.Hour:
		check.State, check.Summary = domain.HealthCritical, "服务证书将在 7 天内到期"
	case notAfter.Sub(now) <= certificateRenewalWindow:
		check.State, check.Summary = domain.HealthWarning, "服务证书已进入续期窗口"
	default:
		check.Summary = "服务证书有效"
	}
	// A later successful certificate import/issuance supersedes an old failure.
	if task.ID != "" && (site.Certificate == nil || !site.Certificate.UpdatedAt.After(task.UpdatedAt)) {
		check.Evidence = append(check.Evidence, domain.HealthEvidence{Label: "证书任务状态", Value: string(task.Status)}, domain.HealthEvidence{Label: "任务说明", Value: healthDetail(task.Detail)})
		state, summary := healthTaskState(task, now, "证书")
		if state == domain.HealthWarning && domain.HealthStateRank(check.State) > domain.HealthStateRank(state) {
			check.State, check.Summary = state, summary
		}
	}
	if site.Certificate != nil {
		check.Evidence = append(check.Evidence, domain.HealthEvidence{Label: "最新签发证书到期时间", Value: healthTime(site.Certificate.NotAfter)})
	}
	return check
}

func backupHealthChecks(status *BackupHealthStatus, now time.Time) []domain.HealthCheck {
	backup := healthCheck("backup:freshness", "backup", "备份时效", "/settings?tab=backup", "查看备份与恢复")
	backup.Impact = "备份过期或失败会扩大潜在的数据恢复缺口。"
	verify := healthCheck("backup:verification", "backup", "恢复验证", "/settings?tab=backup", "查看恢复验证")
	verify.Impact = "缺少近期恢复验证时，无法确认最近备份是否可恢复。"
	if status == nil || status.State == "disabled" {
		backup.State, backup.Summary = domain.HealthDisabled, "备份未配置"
		verify.State, verify.Summary = domain.HealthDisabled, "恢复验证未启用"
		return []domain.HealthCheck{backup, verify}
	}
	backup.ObservedAt = nonzeroHealthTime(status.ObservedAt)
	backup.Summary = status.Summary
	backup.Evidence = []domain.HealthEvidence{{Label: "最近成功备份", Value: healthTime(status.LastSucceededAt)}, {Label: "备份有效期", Value: healthTime(status.ExpiresAt)}}
	switch status.State {
	case "unknown":
		backup.State = domain.HealthUnknown
	case "stale", "stalled", "failed":
		backup.State = domain.HealthWarning
	}
	if backup.State == domain.HealthHealthy && status.LastSucceededAt == nil {
		backup.State, backup.Summary = domain.HealthUnknown, "尚无成功备份记录"
	}
	v := status.Verification
	verify.ObservedAt = nonzeroHealthTime(status.ObservedAt)
	verify.Evidence = []domain.HealthEvidence{{Label: "验证状态", Value: v.State}, {Label: "最近验证成功", Value: healthTime(v.LastVerifiedAt)}, {Label: "可恢复快照时间", Value: healthTime(v.LastVerifiedSnapshotTime)}, {Label: "验证有效期", Value: healthTime(status.VerificationDueAt)}}
	switch {
	case v.State == "unavailable":
		verify.State, verify.Summary = domain.HealthDisabled, "恢复验证未启用"
	case v.State == "unknown":
		verify.State, verify.Summary = domain.HealthUnknown, "恢复验证记录不可用"
	case v.State == OnlineRestoreFailed:
		verify.State, verify.Summary = domain.HealthWarning, "最近一次恢复验证失败"
	case status.VerificationOverdue || v.LastVerifiedAt == nil || (status.VerificationDueAt != nil && !now.Before(*status.VerificationDueAt)):
		verify.State, verify.Summary = domain.HealthWarning, "恢复验证尚未完成或已过期"
	default:
		verify.Summary = "近期恢复验证已通过"
	}
	if v.Error != "" {
		verify.Evidence = append(verify.Evidence, domain.HealthEvidence{Label: "最近错误", Value: healthDetail(v.Error)})
	}
	return []domain.HealthCheck{backup, verify}
}
