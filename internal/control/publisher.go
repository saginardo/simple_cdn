package control

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
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

func (p Publisher) PublishSite(siteID string) (domain.DeploymentTask, error) {
	desiredStateMu.Lock()
	defer desiredStateMu.Unlock()
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
			for _, nodeID := range publication.Site.Nodes {
				affected[nodeID] = struct{}{}
			}
		}
	}
	if removing {
		delete(materialsByID, siteID)
		for _, nodeID := range targetSite.Nodes {
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
		for _, nodeID := range targetSite.Nodes {
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
	rateLimitPolicies, err := p.Store.ListRateLimitPolicies()
	if err != nil {
		return nil, nil, err
	}
	settings, err := p.Store.ControlSettings()
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
			nodeSites = append(nodeSites, publication.Site)
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
		var nodeSecurityPolicies []domain.SecurityPolicy
		if slices.Contains(node.Capabilities, domain.EdgeCapabilitySecurity) {
			nodeSecurityPolicies = securityPolicies
		}
		nodeRateLimitPolicies := rateLimitPoliciesForCapabilities(rateLimitPolicies, node.Capabilities)
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
			NginxWorkerConnections: node.NginxCapacity.WorkerConnections,
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
			if p.nodeStateMatches(previous, previousCertificates, config, streamConfig, mainConfig, eventsConfig, fragments, ports, udpPorts, originPools, cacheMaxBytes, certificates) {
				continue
			}
			version = previous.Version + 1
		}
		state := domain.DesiredState{
			Version: version, NginxConfig: config, NginxStreamConfig: streamConfig,
			NginxMainConfig: mainConfig, NginxEventsConfig: eventsConfig, NginxFragments: fragments,
			PublicPorts: ports, PublicUDPPorts: udpPorts, OriginPools: originPools,
			CacheMaxBytes: cacheMaxBytes, Certificates: certificates,
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

func (p Publisher) nodeStateMatches(previous domain.DesiredState, encryptedCertificates []byte, config, streamConfig, mainConfig, eventsConfig string, fragments *domain.NginxConfigFragments, ports, udpPorts []int, originPools []domain.OriginPool, cacheMaxBytes int64, certificates map[string]domain.TLSBundle) bool {
	if previous.NginxConfig != config || previous.NginxStreamConfig != streamConfig || previous.NginxMainConfig != mainConfig || previous.NginxEventsConfig != eventsConfig ||
		!nginxConfigFragmentsEqual(previous.NginxFragments, fragments) || !slices.Equal(previous.PublicPorts, ports) || !slices.Equal(previous.PublicUDPPorts, udpPorts) ||
		!originPoolsEqual(previous.OriginPools, originPools) || previous.CacheMaxBytes != cacheMaxBytes {
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

func originPoolsEqual(left, right []domain.OriginPool) bool {
	return slices.EqualFunc(left, right, func(a, b domain.OriginPool) bool {
		return a.ID == b.ID && a.Address == b.Address && a.Scheme == b.Scheme && a.HostHeader == b.HostHeader &&
			a.TLSServerName == b.TLSServerName && a.ConfigPath == b.ConfigPath && a.KeepaliveConnections == b.KeepaliveConnections &&
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
	for _, nodeID := range site.Nodes {
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
