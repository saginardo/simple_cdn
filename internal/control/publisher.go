package control

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/nginx"
	"simple_cdn/internal/store"
)

type Publisher struct {
	Store  *store.Store
	Cipher *Cipher
}

var desiredStateMu sync.Mutex

var (
	errPublishRetryNotReady    = errors.New("latest publish has no failed active edge nodes to retry")
	errPublishRetryUnpublished = errors.New("site has unpublished changes; publish the current configuration instead")
)

func (p Publisher) PublishSite(siteID string) (domain.DeploymentTask, error) {
	desiredStateMu.Lock()
	defer desiredStateMu.Unlock()
	return p.publishSiteLocked(siteID)
}

// RetryFailedPublish creates a new confirmation task for only the nodes that
// failed the latest publish. Their current desired-state version is reused, so
// a retry never republishes an obsolete node target.
func (p Publisher) RetryFailedPublish(siteID string) (domain.DeploymentTask, error) {
	if p.Store == nil {
		return domain.DeploymentTask{}, errors.New("publisher is not configured")
	}
	desiredStateMu.Lock()
	defer desiredStateMu.Unlock()

	site, _, err := p.Store.GetSite(siteID)
	if err != nil {
		return domain.DeploymentTask{}, err
	}
	if site.Deleting {
		return domain.DeploymentTask{}, store.ErrSiteDeleting
	}
	if !site.Published {
		return domain.DeploymentTask{}, errPublishRetryUnpublished
	}
	status, err := p.Store.PublishStatus(siteID)
	if err != nil {
		return domain.DeploymentTask{}, err
	}
	if status.Task == nil || (status.Task.Status != domain.TaskPartial && status.Task.Status != domain.TaskFailed) {
		return domain.DeploymentTask{}, errPublishRetryNotReady
	}

	targets := make([]store.PublishTaskNode, 0, len(status.Nodes))
	nodeIDs := make([]string, 0, len(status.Nodes))
	for _, result := range status.Nodes {
		if result.Status != domain.PublishNodeFailed && result.Status != domain.PublishNodeTimedOut {
			continue
		}
		node, err := p.Store.GetNode(result.NodeID)
		if err != nil {
			return domain.DeploymentTask{}, err
		}
		if node.Status != domain.NodeActive {
			continue
		}
		version, err := p.Store.DesiredVersion(node.ID)
		if err != nil {
			return domain.DeploymentTask{}, err
		}
		if version < 1 || node.AppliedVersion >= version {
			continue
		}
		targets = append(targets, store.PublishTaskNode{NodeID: node.ID, TargetVersion: version})
		nodeIDs = append(nodeIDs, node.ID)
	}
	if len(targets) == 0 {
		return domain.DeploymentTask{}, errPublishRetryNotReady
	}
	if err := p.Store.EnsureNodesNotUpgrading(nodeIDs); err != nil {
		return domain.DeploymentTask{}, err
	}

	deadline := time.Now().UTC().Add(90 * time.Second)
	task, created, err := p.Store.CreateOrGetActivePublishTask(siteID, deadline)
	if err != nil {
		return domain.DeploymentTask{}, err
	}
	if !created {
		return task, nil
	}
	if err := p.Store.UpdateTask(task.ID, domain.TaskApplying, "retrying failed edge confirmations"); err != nil {
		return task, err
	}
	if err := p.Store.CreatePublishTaskNodes(task.ID, targets); err != nil {
		_ = p.Store.UpdateTask(task.ID, domain.TaskFailed, err.Error())
		return task, err
	}
	return p.Store.GetTask(task.ID)
}

func (p Publisher) publishSiteLocked(siteID string) (domain.DeploymentTask, error) {
	site, _, err := p.Store.GetSite(siteID)
	if err != nil {
		return domain.DeploymentTask{}, err
	}
	if site.Deleting {
		return domain.DeploymentTask{}, store.ErrSiteDeleting
	}
	deadline := time.Now().UTC().Add(90 * time.Second)
	task, created, err := p.Store.CreateOrGetActivePublishTask(siteID, deadline)
	if err != nil {
		return domain.DeploymentTask{}, err
	}
	if !created {
		return task, nil
	}
	if err := p.Store.UpdateTask(task.ID, domain.TaskDispatching, "building node configurations"); err != nil {
		return task, err
	}
	if err := p.Store.UpdateTask(task.ID, domain.TaskApplying, "preparing edge configuration confirmation"); err != nil {
		return task, err
	}
	targets, err := p.publishSite(siteID, task.ID, site.ConfigVersion)
	if err != nil {
		_ = p.Store.UpdateTask(task.ID, domain.TaskFailed, err.Error())
		return task, err
	}
	if len(targets) == 0 {
		_ = p.Store.UpdateTask(task.ID, domain.TaskSucceeded, "configuration staged; no active assigned edge nodes to confirm")
	}
	return p.Store.GetTask(task.ID)
}

func (p Publisher) RunCacheInvalidation(input store.CacheOperationInput) (domain.CacheOperation, domain.DeploymentTask, error) {
	if p.Store == nil {
		return domain.CacheOperation{}, domain.DeploymentTask{}, errors.New("cache operation publisher is not configured")
	}
	desiredStateMu.Lock()
	defer desiredStateMu.Unlock()
	if err := p.Store.ReconcilePublishTasks(); err != nil {
		return domain.CacheOperation{}, domain.DeploymentTask{}, err
	}
	operation, site, err := p.Store.CreateCacheInvalidationOperation(input)
	if err != nil {
		return domain.CacheOperation{}, domain.DeploymentTask{}, err
	}
	task, publishErr := p.publishSiteLocked(site.ID)
	if publishErr != nil {
		_ = p.Store.FailCacheOperation(operation.ID, task.ID, publishErr.Error())
		return operation, task, publishErr
	}
	if err := p.Store.AttachCacheOperationTask(operation.ID, task.ID); err != nil {
		_ = p.Store.FailCacheOperation(operation.ID, task.ID, err.Error())
		return operation, task, err
	}
	operation, err = p.Store.GetCacheOperation(operation.ID)
	return operation, task, err
}

func (p Publisher) RetryCachePrewarm(operationID, actor, remoteAddr string) (domain.CacheOperation, domain.DeploymentTask, error) {
	if p.Store == nil {
		return domain.CacheOperation{}, domain.DeploymentTask{}, errors.New("cache operation publisher is not configured")
	}
	desiredStateMu.Lock()
	defer desiredStateMu.Unlock()
	if err := p.Store.ReconcilePublishTasks(); err != nil {
		return domain.CacheOperation{}, domain.DeploymentTask{}, err
	}
	operation, site, err := p.Store.CreateCachePrewarmRetryOperation(operationID, actor, remoteAddr)
	if err != nil {
		return domain.CacheOperation{}, domain.DeploymentTask{}, err
	}
	task, publishErr := p.publishSiteLocked(site.ID)
	if publishErr != nil {
		_ = p.Store.FailCacheOperation(operation.ID, task.ID, publishErr.Error())
		return operation, task, publishErr
	}
	if err := p.Store.AttachCacheOperationTask(operation.ID, task.ID); err != nil {
		_ = p.Store.FailCacheOperation(operation.ID, task.ID, err.Error())
		return operation, task, err
	}
	operation, err = p.Store.GetCacheOperation(operation.ID)
	return operation, task, err
}

func (p Publisher) PublishAll() error {
	desiredStateMu.Lock()
	defer desiredStateMu.Unlock()
	if err := p.Store.CheckPublicationMigrationSafety(""); err != nil {
		return err
	}
	publications, err := p.Store.ListSitePublications()
	if err != nil {
		return err
	}
	nodes, err := p.Store.ListNodes()
	if err != nil {
		return err
	}
	affected := make(map[string]struct{}, len(nodes))
	for _, node := range nodes {
		affected[node.ID] = struct{}{}
	}
	updates, _, err := p.renderNodeStateUpdates(publicationMaterials(publications), affected)
	if err != nil {
		return err
	}
	return p.Store.SaveNodeStates(updates)
}

// PublishNode rebuilds just one node's desired state. It is used for
// node-scoped capacity and cache quota changes that do not alter site drafts.
func (p Publisher) PublishNode(nodeID string) error {
	if p.Store == nil || p.Cipher == nil {
		return fmt.Errorf("node publisher is not configured")
	}
	desiredStateMu.Lock()
	defer desiredStateMu.Unlock()
	if err := p.Store.CheckPublicationMigrationSafety(""); err != nil {
		return err
	}
	publications, err := p.Store.ListSitePublications()
	if err != nil {
		return err
	}
	updates, _, err := p.renderNodeStateUpdates(publicationMaterials(publications), map[string]struct{}{nodeID: {}})
	if err != nil {
		return err
	}
	return p.Store.SaveNodeStates(updates)
}

type publicationMaterial struct {
	Site                  domain.Site
	CertificateCiphertext []byte
	KeyCiphertext         []byte
}

type renderedPublication struct {
	Site           domain.Site
	Bundle         domain.TLSBundle
	HasCertificate bool
}

func publicationMaterials(publications []store.SitePublication) []publicationMaterial {
	materials := make([]publicationMaterial, 0, len(publications))
	for _, publication := range publications {
		if publication.Site.Deleting {
			continue
		}
		materials = append(materials, publicationMaterial{
			Site:                  publication.Site,
			CertificateCiphertext: publication.CertificateCiphertext,
			KeyCiphertext:         publication.KeyCiphertext,
		})
	}
	return materials
}

func (p Publisher) publishSite(siteID, publishTaskID string, expectedConfigVersion int64) ([]store.PublishTaskNode, error) {
	updates, targets, err := p.prepareNodeStates(siteID, false)
	if err != nil {
		return nil, err
	}
	if _, err := p.Store.CommitSitePublication(siteID, expectedConfigVersion, publishTaskID, updates, targets); err != nil {
		return nil, err
	}
	return targets, nil
}

func (p Publisher) StageSiteRemoval(taskID, siteID string) error {
	desiredStateMu.Lock()
	defer desiredStateMu.Unlock()
	updates, targets, err := p.prepareNodeStates(siteID, true)
	if err != nil {
		return err
	}
	return p.Store.StageSiteDeletion(taskID, updates, targets)
}

func (p Publisher) prepareNodeStates(siteID string, removing bool) ([]store.NodeStateUpdate, []store.PublishTaskNode, error) {
	migrationExclusion := siteID
	if removing {
		migrationExclusion = ""
	}
	if err := p.Store.CheckPublicationMigrationSafety(migrationExclusion); err != nil {
		return nil, nil, err
	}
	targetSite, _, err := p.Store.GetSite(siteID)
	if err != nil {
		return nil, nil, err
	}
	if !removing && targetSite.Deleting {
		return nil, nil, store.ErrSiteDeleting
	}
	migrationRequired, err := p.Store.PublicationMigrationRequired(siteID)
	if err != nil {
		return nil, nil, err
	}
	publications, err := p.Store.ListSitePublications()
	if err != nil {
		return nil, nil, err
	}
	materialsByID := make(map[string]publicationMaterial, len(publications)+1)
	affected := make(map[string]struct{})
	for _, publication := range publications {
		materialsByID[publication.Site.ID] = publicationMaterial{
			Site:                  publication.Site,
			CertificateCiphertext: publication.CertificateCiphertext,
			KeyCiphertext:         publication.KeyCiphertext,
		}
		if publication.Site.ID == siteID {
			for _, nodeID := range publication.Site.AssignedNodeIDs() {
				affected[nodeID] = struct{}{}
			}
		}
	}
	if removing {
		delete(materialsByID, siteID)
		for _, nodeID := range targetSite.AssignedNodeIDs() {
			affected[nodeID] = struct{}{}
		}
	} else {
		certificate, key, _, certificateErr := p.Store.Certificate(siteID)
		if certificateErr != nil {
			if certificateErr != store.ErrNotFound {
				return nil, nil, certificateErr
			}
			if targetSite.Enabled && domain.SiteNeedsCertificate(targetSite) {
				return nil, nil, fmt.Errorf("site %s needs a certificate before it can be published", targetSite.Name)
			}
		}
		materialsByID[siteID] = publicationMaterial{Site: targetSite, CertificateCiphertext: certificate, KeyCiphertext: key}
		for _, nodeID := range targetSite.AssignedNodeIDs() {
			affected[nodeID] = struct{}{}
		}
	}
	materials := make([]publicationMaterial, 0, len(materialsByID))
	for _, material := range materialsByID {
		if material.Site.Deleting {
			continue
		}
		materials = append(materials, material)
	}
	sort.Slice(materials, func(i, j int) bool { return materials[i].Site.ID < materials[j].Site.ID })
	if migrationRequired {
		nodes, err := p.Store.ListNodes()
		if err != nil {
			return nil, nil, err
		}
		for _, node := range nodes {
			affected[node.ID] = struct{}{}
		}
	}
	return p.renderNodeStateUpdates(materials, affected)
}

func (p Publisher) renderNodeStateUpdates(materials []publicationMaterial, affected map[string]struct{}) ([]store.NodeStateUpdate, []store.PublishTaskNode, error) {
	affectedNodeIDs := make([]string, 0, len(affected))
	for nodeID := range affected {
		affectedNodeIDs = append(affectedNodeIDs, nodeID)
	}
	if err := p.Store.EnsureNodesNotUpgrading(affectedNodeIDs); err != nil {
		return nil, nil, err
	}
	rendered, err := p.decryptPublicationMaterials(materials, affected)
	if err != nil {
		return nil, nil, err
	}
	nodes, err := p.Store.ListNodes()
	if err != nil {
		return nil, nil, err
	}
	securityPolicies, err := p.Store.ListSecurityPolicies()
	if err != nil {
		return nil, nil, err
	}
	powPolicyMaterials, err := p.Store.ListEnabledPOWPolicyMaterials()
	if err != nil {
		return nil, nil, err
	}
	staticAssetReferences, err := p.Store.ListStaticAssetReferences()
	if err != nil {
		return nil, nil, err
	}
	rateLimitPolicies, err := p.Store.ListRateLimitPolicies()
	if err != nil {
		return nil, nil, err
	}
	settings, err := p.Store.ControlSettings()
	if err != nil {
		return nil, nil, err
	}
	wireGuardTunnels, err := p.wireGuardTunnels(materials)
	if err != nil {
		return nil, nil, err
	}
	updates := make([]store.NodeStateUpdate, 0, len(affected))
	targets := make([]store.PublishTaskNode, 0, len(affected))
	for _, node := range nodes {
		if _, found := affected[node.ID]; !found {
			continue
		}
		if node.Status == domain.NodeRevoked || node.Status == domain.NodeUninstalling || node.Status == domain.NodeUninstalled {
			continue
		}
		nodeSites := make([]domain.Site, 0)
		certificates := make(map[string]domain.TLSBundle)
		for _, publication := range rendered {
			if !siteHasNode(publication.Site, node.ID) {
				continue
			}
			nodeSite, err := prepareWireGuardSiteForNode(publication.Site, node, wireGuardTunnels)
			if err != nil {
				return nil, nil, err
			}
			nodeSites = append(nodeSites, nodeSite)
			if publication.HasCertificate {
				certificates[publication.Site.ID] = publication.Bundle
			}
		}
		previous, previousCertificates, stateErr := p.Store.NodeState(node.ID)
		if stateErr != nil && stateErr != store.ErrNotFound {
			return nil, nil, stateErr
		}
		if len(nodeSites) == 0 && stateErr == nil && isTCPOnlyState(previous) {
			nodeSites = append(nodeSites, domain.Site{TCPOnly: true})
		}
		if siteRequiresTCPStream(nodeSites) && !slices.Contains(node.Capabilities, domain.EdgeCapabilityTCPStream) {
			return nil, nil, fmt.Errorf("node %s must be upgraded before publishing TCP forwards", node.Name)
		}
		originHTTP2Capable := slices.Contains(node.Capabilities, domain.EdgeCapabilityOriginHTTP2)
		if sitesRequireOriginHTTP2(nodeSites) && !originHTTP2Capable {
			return nil, nil, fmt.Errorf("node %s must be upgraded before publishing HTTP/2 origin connections", node.Name)
		}
		nodeSecurityPolicies := securityPoliciesForCapabilities(securityPolicies, node.Capabilities)
		nodePOWPolicies, err := p.powPoliciesForNode(powPolicyMaterials, nodeSites, node.Capabilities)
		if err != nil {
			return nil, nil, fmt.Errorf("node %s proof-of-work policies: %w", node.Name, err)
		}
		nodeRateLimitPolicies := rateLimitPoliciesForCapabilities(rateLimitPolicies, node.Capabilities)
		nodeStaticAssets, err := staticAssetsForNode(staticAssetReferences, nodeSites, node.Capabilities)
		if err != nil {
			return nil, nil, fmt.Errorf("node %s static assets: %w", node.Name, err)
		}
		nodeCacheWarmups, err := cacheWarmupsForNode(nodeSites, node.Capabilities)
		if err != nil {
			return nil, nil, fmt.Errorf("node %s cache prewarm jobs: %w", node.Name, err)
		}
		http3Capable := slices.Contains(node.Capabilities, domain.EdgeCapabilityHTTP3)
		managedOriginPools := slices.Contains(node.Capabilities, domain.EdgeCapabilityOriginConnection)
		cacheSizeGB, err := domain.EffectiveNodeCacheMaxSizeGB(node, settings.CacheDefaultSizeGB)
		if err != nil {
			return nil, nil, fmt.Errorf("node %s: %w", node.Name, err)
		}
		renderedConfig, err := nginx.RenderNodeWithRuntimeOptions(nodeSites, nodeSecurityPolicies, nodeRateLimitPolicies, nginx.RenderRuntimeOptions{
			DefaultCacheSizeGB:     cacheSizeGB,
			HTTP3Capable:           http3Capable,
			ManagedOriginPools:     managedOriginPools,
			OriginHTTP2Capable:     originHTTP2Capable,
			NginxWorkerConnections: node.NginxCapacity.WorkerConnections,
			WAFChainCapable:        slices.Contains(node.Capabilities, domain.EdgeCapabilityWAFChain),
			POWCapable:             slices.Contains(node.Capabilities, domain.EdgeCapabilityPOWChallenge),
			POWPolicies:            nodePOWPolicies,
			StaticAssets:           nodeStaticAssets,
			CompressionCapable:     slices.Contains(node.Capabilities, domain.EdgeCapabilityCompression),
		})
		if err != nil {
			return nil, nil, err
		}
		config := renderedConfig.NginxConfig
		originPools := renderedConfig.OriginPools
		cacheMaxBytes := int64(cacheSizeGB) << 30
		streamConfig, err := nginx.RenderStream(nodeSites)
		if err != nil {
			return nil, nil, err
		}
		fragments, err := nginx.SplitConfigFragments(config, streamConfig)
		if err != nil {
			return nil, nil, fmt.Errorf("split Nginx configuration for node %s: %w", node.Name, err)
		}
		ports := requiredPublicPorts(nodeSites)
		udpPorts := requiredPublicUDPPorts(nodeSites, http3Capable)
		mainConfig, eventsConfig, err := nginx.RenderCapacity(node.NginxCapacity)
		if err != nil {
			return nil, nil, fmt.Errorf("node %s capacity: %w", node.Name, err)
		}
		version := int64(1)
		if stateErr == nil {
			if p.nodeStateMatches(previous, previousCertificates, config, streamConfig, mainConfig, eventsConfig, fragments, ports, udpPorts, originPools, nodeStaticAssets, nodeCacheWarmups, cacheMaxBytes, certificates) {
				continue
			}
			version = previous.Version + 1
		}
		state := domain.DesiredState{
			Version: version, NginxConfig: config, NginxStreamConfig: streamConfig,
			NginxMainConfig: mainConfig, NginxEventsConfig: eventsConfig, NginxFragments: fragments,
			PublicPorts: ports, PublicUDPPorts: udpPorts, OriginPools: originPools,
			StaticAssets: nodeStaticAssets, CacheWarmups: nodeCacheWarmups, CacheMaxBytes: cacheMaxBytes, Certificates: certificates,
		}
		serialized, err := json.Marshal(state.Certificates)
		if err != nil {
			return nil, nil, err
		}
		encryptedCertificates, err := p.Cipher.Encrypt(serialized)
		if err != nil {
			return nil, nil, err
		}
		updates = append(updates, store.NodeStateUpdate{NodeID: node.ID, State: state, CertificatesCiphertext: encryptedCertificates})
		if node.Status == domain.NodeActive {
			targets = append(targets, store.PublishTaskNode{NodeID: node.ID, TargetVersion: version})
		}
	}
	return updates, targets, nil
}

func securityPoliciesForCapabilities(policies []domain.SecurityPolicy, capabilities []string) []domain.SecurityPolicy {
	if slices.Contains(capabilities, domain.EdgeCapabilityWAFChain) {
		return policies
	}
	if !slices.Contains(capabilities, domain.EdgeCapabilitySecurity) {
		return nil
	}
	result := make([]domain.SecurityPolicy, 0, len(policies))
	for _, policy := range policies {
		if legacy, ok := domain.LegacySecurityPolicy(policy); ok {
			result = append(result, legacy)
		}
	}
	return result
}

func (p Publisher) powPoliciesForNode(materials []store.POWPolicyMaterial, sites []domain.Site, capabilities []string) ([]domain.POWPolicyRuntime, error) {
	if !slices.Contains(capabilities, domain.EdgeCapabilityWAFChain) ||
		!slices.Contains(capabilities, domain.EdgeCapabilityPOWChallenge) {
		return nil, nil
	}
	siteIDs := make(map[string]struct{}, len(sites))
	for _, site := range sites {
		if site.Enabled && !site.TCPOnly {
			siteIDs[site.ID] = struct{}{}
		}
	}
	result := make([]domain.POWPolicyRuntime, 0, len(materials))
	for _, material := range materials {
		relevant := false
		for _, siteID := range material.Policy.SiteIDs {
			if _, found := siteIDs[siteID]; found {
				relevant = true
				break
			}
		}
		if !relevant {
			continue
		}
		if p.Cipher == nil {
			return nil, errors.New("publisher encryption is not configured")
		}
		secret, err := p.Cipher.Decrypt(material.SecretCiphertext)
		if err != nil {
			return nil, fmt.Errorf("decrypt policy %s secret: %w", material.Policy.ID, err)
		}
		if len(secret) != 32 {
			return nil, fmt.Errorf("policy %s secret must be 32 bytes", material.Policy.ID)
		}
		result = append(result, domain.POWPolicyRuntime{
			Policy: material.Policy, Secret: base64.RawStdEncoding.EncodeToString(secret),
		})
	}
	return result, nil
}

func staticAssetsForNode(references []domain.StaticAssetReference, sites []domain.Site, capabilities []string) ([]domain.StaticAssetReference, error) {
	if !slices.Contains(capabilities, domain.EdgeCapabilityStaticAssets) {
		return nil, nil
	}
	siteIDs := make(map[string]struct{}, len(sites))
	for _, site := range sites {
		if site.Enabled && !site.TCPOnly {
			siteIDs[site.ID] = struct{}{}
		}
	}
	result := make([]domain.StaticAssetReference, 0)
	for _, reference := range references {
		if _, found := siteIDs[reference.SiteID]; !found {
			continue
		}
		normalized, err := domain.NormalizeStaticAssetReference(reference)
		if err != nil {
			return nil, err
		}
		result = append(result, normalized)
	}
	return result, nil
}

func cacheWarmupsForNode(sites []domain.Site, capabilities []string) ([]domain.CacheWarmup, error) {
	if !slices.Contains(capabilities, domain.EdgeCapabilityCacheControl) {
		return nil, nil
	}
	result := make([]domain.CacheWarmup, 0)
	for _, site := range sites {
		if !site.Enabled || site.TCPOnly {
			continue
		}
		warmups, err := domain.NormalizeCacheWarmups(site.CacheWarmups)
		if err != nil {
			return nil, err
		}
		result = append(result, warmups...)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result, nil
}

func (p Publisher) wireGuardTunnels(materials []publicationMaterial) (map[string]domain.WireGuardTunnel, error) {
	wanted := false
	for _, material := range materials {
		if material.Site.PrimaryOrigin.WireGuardTunnelID != "" ||
			material.Site.BackupOrigin != nil && material.Site.BackupOrigin.WireGuardTunnelID != "" {
			wanted = true
			break
		}
	}
	if !wanted {
		return nil, nil
	}
	tunnels, err := p.Store.ListWireGuardTunnels()
	if err != nil {
		return nil, err
	}
	result := make(map[string]domain.WireGuardTunnel, len(tunnels))
	for _, tunnel := range tunnels {
		result[tunnel.ID] = tunnel
	}
	return result, nil
}

func prepareWireGuardSiteForNode(site domain.Site, node domain.Node, tunnels map[string]domain.WireGuardTunnel) (domain.Site, error) {
	if site.BackupOrigin != nil {
		backup := *site.BackupOrigin
		site.BackupOrigin = &backup
	}
	if !site.Enabled || site.TCPOnly {
		return site, nil
	}
	origins := []struct {
		role   string
		origin *domain.Origin
		active bool
	}{
		{role: "primary", origin: &site.PrimaryOrigin, active: true},
		{role: "backup", origin: site.BackupOrigin, active: site.BackupOrigin != nil && site.BackupOrigin.Enabled},
	}
	for _, selected := range origins {
		if !selected.active || selected.origin == nil || selected.origin.WireGuardTunnelID == "" {
			continue
		}
		if !slices.Contains(node.Capabilities, domain.EdgeCapabilityWireGuard) {
			return domain.Site{}, fmt.Errorf("node %s must be upgraded or reinstalled before publishing WireGuard origins", node.Name)
		}
		tunnel, found := tunnels[selected.origin.WireGuardTunnelID]
		if !found {
			return domain.Site{}, fmt.Errorf("site %s %s origin references a missing WireGuard tunnel", site.Name, selected.role)
		}
		if !domain.ValidWireGuardKey(tunnel.OriginPublicKey) || tunnel.OriginConfiguredRevision != tunnel.Revision {
			return domain.Site{}, fmt.Errorf("WireGuard tunnel %s must be applied on the origin at revision %d before publishing site %s", tunnel.Name, tunnel.Revision, site.Name)
		}
		var peer *domain.WireGuardPeer
		for index := range tunnel.Peers {
			if tunnel.Peers[index].NodeID == node.ID {
				peer = &tunnel.Peers[index]
				break
			}
		}
		if peer == nil {
			return domain.Site{}, fmt.Errorf("WireGuard tunnel %s is not assigned to node %s", tunnel.Name, node.Name)
		}
		if !domain.ValidWireGuardKey(peer.PublicKey) || peer.AppliedRevision != tunnel.Revision || peer.LastError != "" {
			return domain.Site{}, fmt.Errorf("node %s has not applied WireGuard tunnel %s revision %d", node.Name, tunnel.Name, tunnel.Revision)
		}
		if err := rewriteOriginForWireGuard(selected.origin, tunnel); err != nil {
			return domain.Site{}, fmt.Errorf("site %s %s origin: %w", site.Name, selected.role, err)
		}
	}
	return site, nil
}

func rewriteOriginForWireGuard(origin *domain.Origin, tunnel domain.WireGuardTunnel) error {
	parsed, err := url.Parse(origin.URL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return errors.New("invalid origin URL")
	}
	originalHostname := parsed.Hostname()
	if origin.HostHeader == "" {
		origin.HostHeader = originalHostname
	}
	if domain.OriginUsesTLS(parsed.Scheme) && origin.TLSServerName == "" {
		origin.TLSServerName = originalHostname
	}
	if port := parsed.Port(); port != "" {
		parsed.Host = net.JoinHostPort(tunnel.OriginAddress, port)
	} else {
		parsed.Host = tunnel.OriginAddress
	}
	origin.URL = parsed.String()
	return nil
}

func sitesRequireOriginHTTP2(sites []domain.Site) bool {
	for _, site := range sites {
		if !site.Enabled || site.TCPOnly {
			continue
		}
		for _, origin := range []*domain.Origin{&site.PrimaryOrigin, site.BackupOrigin} {
			if origin == nil || !origin.Enabled {
				continue
			}
			version := domain.EffectiveOriginHTTPVersion(*origin)
			if version == domain.OriginHTTPVersionHTTP2 || version == domain.OriginHTTPVersionH2C {
				return true
			}
		}
	}
	return false
}

func rateLimitPoliciesForCapabilities(policies []domain.RateLimitPolicy, capabilities []string) []domain.RateLimitPolicy {
	if !slices.Contains(capabilities, domain.EdgeCapabilityRateLimit) {
		return nil
	}
	if slices.Contains(capabilities, domain.EdgeCapabilitySecurity) {
		return policies
	}
	result := append([]domain.RateLimitPolicy(nil), policies...)
	for index := range result {
		result[index].BanEnabled = false
	}
	return result
}

func (p Publisher) decryptPublicationMaterials(materials []publicationMaterial, affected map[string]struct{}) ([]renderedPublication, error) {
	rendered := make([]renderedPublication, 0, len(materials))
	for _, material := range materials {
		if !siteTouchesNodes(material.Site, affected) {
			continue
		}
		publication := renderedPublication{Site: material.Site}
		if !material.Site.Enabled || !domain.SiteNeedsCertificate(material.Site) {
			rendered = append(rendered, publication)
			continue
		}
		if len(material.CertificateCiphertext) == 0 || len(material.KeyCiphertext) == 0 {
			return nil, fmt.Errorf("site %s needs a certificate before its node configuration can be rebuilt", material.Site.Name)
		}
		certificatePEM, err := p.Cipher.Decrypt(material.CertificateCiphertext)
		if err != nil {
			return nil, fmt.Errorf("decrypt certificate for %s: %w", material.Site.Name, err)
		}
		if err := validateCertificateDomains(certificatePEM, material.Site.Domains, time.Now().UTC()); err != nil {
			return nil, fmt.Errorf("site %s certificate: %w", material.Site.Name, err)
		}
		privateKeyPEM, err := p.Cipher.Decrypt(material.KeyCiphertext)
		if err != nil {
			return nil, fmt.Errorf("decrypt private key for %s: %w", material.Site.Name, err)
		}
		if err := validateCertificatePrivateKey(certificatePEM, privateKeyPEM); err != nil {
			return nil, fmt.Errorf("site %s certificate private key: %w", material.Site.Name, err)
		}
		publication.Bundle = domain.TLSBundle{
			CertificatePEM: string(certificatePEM),
			PrivateKeyPEM:  string(privateKeyPEM),
		}
		publication.HasCertificate = true
		rendered = append(rendered, publication)
	}
	return rendered, nil
}

func (p Publisher) nodeStateMatches(previous domain.DesiredState, encryptedCertificates []byte, config, streamConfig, mainConfig, eventsConfig string, fragments *domain.NginxConfigFragments, ports, udpPorts []int, originPools []domain.OriginPool, staticAssets []domain.StaticAssetReference, cacheWarmups []domain.CacheWarmup, cacheMaxBytes int64, certificates map[string]domain.TLSBundle) bool {
	if previous.NginxConfig != config || previous.NginxStreamConfig != streamConfig || previous.NginxMainConfig != mainConfig || previous.NginxEventsConfig != eventsConfig ||
		!nginxConfigFragmentsEqual(previous.NginxFragments, fragments) || !slices.Equal(previous.PublicPorts, ports) || !slices.Equal(previous.PublicUDPPorts, udpPorts) ||
		!originPoolsEqual(previous.OriginPools, originPools) || !slices.Equal(previous.StaticAssets, staticAssets) || !cacheWarmupsEqual(previous.CacheWarmups, cacheWarmups) || previous.CacheMaxBytes != cacheMaxBytes {
		return false
	}
	previousBundles := make(map[string]domain.TLSBundle)
	if len(encryptedCertificates) != 0 {
		encoded, err := p.Cipher.Decrypt(encryptedCertificates)
		if err != nil || json.Unmarshal(encoded, &previousBundles) != nil {
			return false
		}
	}
	return maps.Equal(previousBundles, certificates)
}

func cacheWarmupsEqual(left, right []domain.CacheWarmup) bool {
	return slices.EqualFunc(left, right, func(a, b domain.CacheWarmup) bool {
		return a.ID == b.ID && a.SiteID == b.SiteID && a.Host == b.Host && a.CreatedAt.Equal(b.CreatedAt) && slices.Equal(a.Paths, b.Paths)
	})
}

func originPoolsEqual(left, right []domain.OriginPool) bool {
	return slices.EqualFunc(left, right, func(a, b domain.OriginPool) bool {
		return a.ID == b.ID && a.Address == b.Address && a.Scheme == b.Scheme && a.HTTPVersion == b.HTTPVersion && a.HostHeader == b.HostHeader &&
			a.HealthCheckMethod == b.HealthCheckMethod && a.HealthCheckPath == b.HealthCheckPath && a.TLSServerName == b.TLSServerName && a.ConfigPath == b.ConfigPath && a.KeepaliveConnections == b.KeepaliveConnections &&
			slices.Equal(a.References, b.References)
	})
}

func nginxConfigFragmentsEqual(left, right *domain.NginxConfigFragments) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.HTTPBase == right.HTTPBase && left.StreamBase == right.StreamBase &&
		slices.Equal(left.HTTPSites, right.HTTPSites) && slices.Equal(left.StreamSites, right.StreamSites)
}

func siteTouchesNodes(site domain.Site, nodeIDs map[string]struct{}) bool {
	for _, nodeID := range site.AssignedNodeIDs() {
		if _, found := nodeIDs[nodeID]; found {
			return true
		}
	}
	return false
}

func requiredPublicPorts(sites []domain.Site) []int {
	ports := make(map[int]struct{})
	dedicatedTCP := false
	for _, site := range sites {
		if site.TCPOnly {
			dedicatedTCP = true
		}
		if !site.Enabled {
			continue
		}
		if !site.TCPOnly {
			ports[80] = struct{}{}
			ports[443] = struct{}{}
		}
		for _, forward := range site.TCPForwards {
			ports[forward.ListenPort] = struct{}{}
		}
	}
	if len(ports) == 0 && !dedicatedTCP {
		ports[80] = struct{}{}
	}
	result := make([]int, 0, len(ports))
	for port := range ports {
		result = append(result, port)
	}
	sort.Ints(result)
	return result
}

func requiredPublicUDPPorts(sites []domain.Site, http3Capable bool) []int {
	if !http3Capable {
		return nil
	}
	for _, site := range sites {
		if site.Enabled && !site.TCPOnly && site.HTTP3Enabled {
			return []int{443}
		}
	}
	return nil
}

func siteRequiresTCPStream(sites []domain.Site) bool {
	for _, site := range sites {
		if site.TCPOnly || (site.Enabled && len(site.TCPForwards) > 0) {
			return true
		}
	}
	return false
}

func isTCPOnlyState(state domain.DesiredState) bool {
	if state.PublicPorts == nil || state.NginxStreamConfig == "" || strings.Contains(state.NginxConfig, "listen 80") || strings.Contains(state.NginxConfig, "listen 443") {
		return false
	}
	for _, port := range state.PublicPorts {
		if port == 80 || port == 443 {
			return false
		}
	}
	return true
}

func validateCertificateDomains(certificatePEM []byte, domains []string, now time.Time) error {
	block, _ := pem.Decode(certificatePEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return fmt.Errorf("invalid PEM certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}
	if !certificate.NotAfter.After(now) {
		return fmt.Errorf("expired at %s", certificate.NotAfter.UTC().Format(time.RFC3339))
	}
	for _, domainName := range domains {
		if err := certificate.VerifyHostname(domainName); err != nil {
			return fmt.Errorf("does not cover %s", domainName)
		}
	}
	return nil
}

func validateCertificatePrivateKey(certificatePEM, privateKeyPEM []byte) error {
	_, err := tls.X509KeyPair(certificatePEM, privateKeyPEM)
	return err
}

func (p Publisher) StoreCertificate(siteID string, certificatePEM, privateKeyPEM []byte, notAfter time.Time) error {
	if err := validateCertificatePrivateKey(certificatePEM, privateKeyPEM); err != nil {
		return fmt.Errorf("validate certificate private key: %w", err)
	}
	certificate, err := p.Cipher.Encrypt(certificatePEM)
	if err != nil {
		return err
	}
	key, err := p.Cipher.Encrypt(privateKeyPEM)
	if err != nil {
		return err
	}
	return p.Store.SaveCertificate(siteID, certificate, key, &notAfter)
}
