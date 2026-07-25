package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/integrations"
	"simple_cdn/internal/store"
)

type blockingCertificateIssuer struct {
	certificate integrations.IssuedCertificate
	started     chan struct{}
	release     chan struct{}
	canceled    chan struct{}
	once        sync.Once
	mu          sync.Mutex
	calls       int
}

func (i *blockingCertificateIssuer) Issue(ctx context.Context, _ string, _ []string) (integrations.IssuedCertificate, error) {
	i.mu.Lock()
	i.calls++
	i.mu.Unlock()
	i.once.Do(func() { close(i.started) })
	select {
	case <-i.release:
		return i.certificate, nil
	case <-ctx.Done():
		select {
		case <-i.canceled:
		default:
			close(i.canceled)
		}
		return integrations.IssuedCertificate{}, ctx.Err()
	}
}

func (i *blockingCertificateIssuer) Calls() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.calls
}

func newCertificateTestManager(t *testing.T) (*store.Store, domain.Site, *CertificateManager, *blockingCertificateIssuer) {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	key, err := NewEncryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("edge-1", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name:          "site",
		Domains:       []string{"cdn.example.test"},
		Nodes:         []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true},
		Enabled:       true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	certificate, privateKey, notAfter := testCertificate(t, site.Domains...)
	issuer := &blockingCertificateIssuer{
		certificate: integrations.IssuedCertificate{CertificatePEM: certificate, PrivateKeyPEM: privateKey, NotAfter: notAfter},
		started:     make(chan struct{}),
		release:     make(chan struct{}),
		canceled:    make(chan struct{}),
	}
	manager := &CertificateManager{Store: database, Publisher: Publisher{Store: database, Cipher: cipher}, Issuer: issuer, IssueTimeout: time.Minute}
	return database, site, manager, issuer
}

func waitFor(t *testing.T, timeout time.Duration, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

func TestIssueTLSContinuesAfterHTTPContextCancellation(t *testing.T) {
	database, site, manager, issuer := newCertificateTestManager(t)
	manager.Start(context.Background())
	t.Cleanup(manager.Stop)
	if err := database.CreateInitialAdmin("hash", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "session-token", "csrf-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database, CertificateManager: manager}
	requestContext, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/api/sites/"+site.ID+"/certificate", nil).WithContext(requestContext)
	request.AddCookie(&http.Cookie{Name: "cdn_session", Value: "session-token"})
	request.Header.Set("X-CSRF-Token", "csrf-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("issue response = %d %s", response.Code, response.Body.String())
	}
	var task domain.DeploymentTask
	if err := json.Unmarshal(response.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-issuer.started:
	case <-time.After(time.Second):
		t.Fatal("issuer was not started")
	}
	close(issuer.release)
	waitFor(t, time.Second, func() bool {
		updated, err := database.GetTask(task.ID)
		return err == nil && updated.Status == domain.TaskSucceeded
	})
	if issuer.Calls() != 1 {
		t.Fatalf("issuer calls = %d, want 1", issuer.Calls())
	}
}

func TestIssueTLSReusesActiveTaskAndExposesLatestTask(t *testing.T) {
	database, site, manager, issuer := newCertificateTestManager(t)
	manager.Start(context.Background())
	t.Cleanup(manager.Stop)
	first, created, err := manager.QueueIssue(site)
	if err != nil || !created {
		t.Fatalf("first task = %#v, created=%t, err=%v", first, created, err)
	}
	select {
	case <-issuer.started:
	case <-time.After(time.Second):
		t.Fatal("issuer was not started")
	}
	second, created, err := manager.QueueIssue(site)
	if err != nil || created || second.ID != first.ID {
		t.Fatalf("second task = %#v, created=%t, err=%v", second, created, err)
	}
	latest, err := database.LatestCertificateTask(site.ID)
	if err != nil || latest.ID != first.ID || !certificateTaskIsActive(latest.Status) {
		t.Fatalf("latest task = %#v, err=%v", latest, err)
	}
	if issuer.Calls() != 1 {
		t.Fatalf("issuer calls = %d, want 1", issuer.Calls())
	}
	close(issuer.release)
	waitFor(t, time.Second, func() bool {
		updated, err := database.GetTask(first.ID)
		return err == nil && updated.Status == domain.TaskSucceeded
	})
}

func TestCreateSiteQueuesCertificateAutomatically(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.CreateInitialAdmin("hash", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "session-token", "csrf-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("edge-auto", "203.0.113.20")
	if err != nil {
		t.Fatal(err)
	}
	key, err := NewEncryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, privateKey, notAfter := testCertificate(t, "auto.example.test")
	issuer := &blockingCertificateIssuer{
		certificate: integrations.IssuedCertificate{CertificatePEM: certificate, PrivateKeyPEM: privateKey, NotAfter: notAfter},
		started:     make(chan struct{}),
		release:     make(chan struct{}),
		canceled:    make(chan struct{}),
	}
	manager := &CertificateManager{
		Store: database, Publisher: Publisher{Store: database, Cipher: cipher}, Issuer: issuer, IssueTimeout: time.Minute,
	}
	manager.Start(context.Background())
	t.Cleanup(manager.Stop)
	server := &Server{Store: database, CertificateManager: manager}

	created := requestSite(t, server, http.MethodPost, "/api/sites", map[string]any{
		"name": "automatic TLS", "zone_id": "zone", "domains": []string{"auto.example.test"}, "node_ids": []string{node.ID},
		"primary_origin": map[string]any{"url": "https://origin.example.test", "enabled": true}, "enabled": true,
	})
	select {
	case <-issuer.started:
	case <-time.After(time.Second):
		t.Fatal("automatic certificate issuer was not started")
	}
	task, err := database.LatestCertificateTask(created.ID)
	if err != nil || task.Kind != manualCertificateTask || !certificateTaskIsActive(task.Status) {
		t.Fatalf("automatic certificate task = %#v, err=%v", task, err)
	}
	close(issuer.release)
	waitFor(t, time.Second, func() bool {
		updated, err := database.GetTask(task.ID)
		return err == nil && updated.Status == domain.TaskSucceeded
	})

	cleartext := requestSite(t, server, http.MethodPost, "/api/sites", map[string]any{
		"name": "cleartext TCP", "zone_id": "zone", "domains": []string{"smtp.example.test"}, "node_ids": []string{node.ID},
		"tcp_only": true, "tcp_forwards": []map[string]any{{
			"name": "SMTP", "listen_port": 25, "upstream_host": "mail.example.test", "upstream_port": 25,
		}}, "enabled": true,
	})
	if _, err := database.LatestCertificateTask(cleartext.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cleartext TCP certificate task error = %v, want not found", err)
	}
	if issuer.Calls() != 1 {
		t.Fatalf("issuer calls = %d, want 1", issuer.Calls())
	}
}

func TestPublishQueuesAndWaitsForCertificate(t *testing.T) {
	database, site, manager, issuer := newCertificateTestManager(t)
	manager.Start(context.Background())
	t.Cleanup(manager.Stop)
	if err := database.CreateInitialAdmin("hash", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "session-token", "csrf-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database, Publisher: manager.Publisher, CertificateManager: manager}

	request := httptest.NewRequest(http.MethodPost, "/api/sites/"+site.ID+"/publish", nil)
	request.AddCookie(&http.Cookie{Name: "cdn_session", Value: "session-token"})
	request.Header.Set("X-CSRF-Token", "csrf-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "TLS certificate issuance is in progress") {
		t.Fatalf("publish before certificate = %d %s", response.Code, response.Body.String())
	}
	if _, err := database.LatestPublishTask(site.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("publish task before certificate error = %v, want not found", err)
	}
	select {
	case <-issuer.started:
	case <-time.After(time.Second):
		t.Fatal("publish preflight did not start certificate issuance")
	}
	certificateTask, err := database.LatestCertificateTask(site.ID)
	if err != nil || !certificateTaskIsActive(certificateTask.Status) {
		t.Fatalf("certificate task = %#v, err=%v", certificateTask, err)
	}
	close(issuer.release)
	waitFor(t, time.Second, func() bool {
		updated, err := database.GetTask(certificateTask.ID)
		return err == nil && updated.Status == domain.TaskSucceeded
	})

	request = httptest.NewRequest(http.MethodPost, "/api/sites/"+site.ID+"/publish", nil)
	request.AddCookie(&http.Cookie{Name: "cdn_session", Value: "session-token"})
	request.Header.Set("X-CSRF-Token", "csrf-token")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("publish after certificate = %d %s", response.Code, response.Body.String())
	}
}

func TestLatestCertificateTaskAPI(t *testing.T) {
	database, site, manager, issuer := newCertificateTestManager(t)
	manager.Start(context.Background())
	t.Cleanup(manager.Stop)
	if err := database.CreateInitialAdmin("hash", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "session-token", "csrf-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	task, created, err := manager.QueueIssue(site)
	if err != nil || !created {
		t.Fatalf("queue task = %#v, created=%t, err=%v", task, created, err)
	}
	select {
	case <-issuer.started:
	case <-time.After(time.Second):
		t.Fatal("issuer was not started")
	}
	server := &Server{Store: database, CertificateManager: manager}
	request := httptest.NewRequest(http.MethodGet, "/api/sites/"+site.ID+"/certificate-task", nil)
	request.AddCookie(&http.Cookie{Name: "cdn_session", Value: "session-token"})
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("latest task response = %d %s", response.Code, response.Body.String())
	}
	var returned domain.DeploymentTask
	if err := json.Unmarshal(response.Body.Bytes(), &returned); err != nil {
		t.Fatal(err)
	}
	if returned.ID != task.ID || !certificateTaskIsActive(returned.Status) {
		t.Fatalf("returned task = %#v", returned)
	}
	close(issuer.release)
}

func TestTLSStatusReportsPublicationAfterCertificate(t *testing.T) {
	database, site, manager, _ := newCertificateTestManager(t)
	if err := database.CreateInitialAdmin("hash", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "session-token", "csrf-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	certificateTask, err := database.CreateTask(manualCertificateTask, site.ID, "certificate stored; publish the site to deploy it")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateTask(certificateTask.ID, domain.TaskSucceeded, certificateTask.Detail); err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database, CertificateManager: manager}
	status := getTLSStatus(t, server, site.ID)
	if status.PublishedAfterCertificate {
		t.Fatalf("TLS status incorrectly reported deployed before publication: %#v", status)
	}
	time.Sleep(time.Millisecond)
	publishTask, err := database.CreateTask("publish_site", site.ID, "configuration available to assigned nodes")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateTask(publishTask.ID, domain.TaskSucceeded, publishTask.Detail); err != nil {
		t.Fatal(err)
	}
	status = getTLSStatus(t, server, site.ID)
	if status.CertificateTask == nil || status.CertificateTask.ID != certificateTask.ID || !status.PublishedAfterCertificate {
		t.Fatalf("TLS status did not report publication after certificate: %#v", status)
	}
}

func TestCertificateOverviewAndManualRenewalAPI(t *testing.T) {
	database, site, manager, issuer := newCertificateTestManager(t)
	manager.Start(context.Background())
	t.Cleanup(manager.Stop)
	if err := database.CreateInitialAdmin("hash", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "session-token", "csrf-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	notAfter := time.Now().UTC().Add(45 * 24 * time.Hour).Round(0)
	if err := database.SaveCertificate(site.ID, []byte("encrypted-certificate"), []byte("encrypted-key"), &notAfter); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	publishTask, err := database.CreateTask("publish_site", site.ID, "certificate published")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateTask(publishTask.ID, domain.TaskSucceeded, publishTask.Detail); err != nil {
		t.Fatal(err)
	}
	withoutTLS, err := database.CreateSite(domain.Site{
		Name:    "tcp cleartext",
		Domains: []string{"tcp.example.test"},
		Nodes:   append([]string(nil), site.Nodes...),
		TCPOnly: true,
		TCPForwards: []domain.TCPForward{{
			Name: "SMTP", ListenPort: 25, UpstreamHost: "mail.example.test", UpstreamPort: 25,
		}},
		Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{Store: database, CertificateManager: manager}
	request := httptest.NewRequest(http.MethodGet, "/api/certificates", nil)
	request.AddCookie(&http.Cookie{Name: "cdn_session", Value: "session-token"})
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("overview response = %d %s", response.Code, response.Body.String())
	}
	var overview certificateOverviewResponse
	if err := json.Unmarshal(response.Body.Bytes(), &overview); err != nil {
		t.Fatal(err)
	}
	if overview.RenewalWindowDays != 30 || overview.ReconcileIntervalSeconds != 12*60*60 || len(overview.Sites) != 2 {
		t.Fatalf("overview metadata = %#v", overview)
	}
	var certificateStatus, noTLSStatus *certificateSiteStatus
	for index := range overview.Sites {
		entry := &overview.Sites[index]
		if entry.SiteID == site.ID {
			certificateStatus = entry
		}
		if entry.SiteID == withoutTLS.ID {
			noTLSStatus = entry
		}
	}
	if certificateStatus == nil || !certificateStatus.CertificatePresent || !certificateStatus.PublishedAfterCertificate || certificateStatus.NotAfter == nil || certificateStatus.RenewalDueAt == nil {
		t.Fatalf("certificate status = %#v", certificateStatus)
	}
	wantRenewalDue := notAfter.Add(-certificateRenewalWindow)
	if !certificateStatus.NotAfter.Equal(notAfter) || !certificateStatus.RenewalDueAt.Equal(wantRenewalDue) {
		t.Fatalf("certificate dates = notAfter %v, renewal %v", certificateStatus.NotAfter, certificateStatus.RenewalDueAt)
	}
	if noTLSStatus == nil || noTLSStatus.NeedsCertificate || noTLSStatus.CertificatePresent {
		t.Fatalf("cleartext TCP status = %#v", noTLSStatus)
	}

	renewRequest := httptest.NewRequest(http.MethodPost, "/api/certificates/"+site.ID+"/renew", nil)
	renewRequest.AddCookie(&http.Cookie{Name: "cdn_session", Value: "session-token"})
	renewRequest.Header.Set("X-CSRF-Token", "csrf-token")
	renewResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(renewResponse, renewRequest)
	if renewResponse.Code != http.StatusAccepted {
		t.Fatalf("renew response = %d %s", renewResponse.Code, renewResponse.Body.String())
	}
	var renewalTask domain.DeploymentTask
	if err := json.Unmarshal(renewResponse.Body.Bytes(), &renewalTask); err != nil {
		t.Fatal(err)
	}
	if renewalTask.Kind != renewCertificateTask || renewalTask.SiteID != site.ID {
		t.Fatalf("renewal task = %#v", renewalTask)
	}
	select {
	case <-issuer.started:
	case <-time.After(time.Second):
		t.Fatal("renewal issuer was not started")
	}
}

func getTLSStatus(t *testing.T, server *Server, siteID string) tlsStatusResponse {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/sites/"+siteID+"/tls-status", nil)
	request.AddCookie(&http.Cookie{Name: "cdn_session", Value: "session-token"})
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("TLS status response = %d %s", response.Code, response.Body.String())
	}
	var status tlsStatusResponse
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestCertificateManagerStopFailsActiveTask(t *testing.T) {
	database, site, manager, issuer := newCertificateTestManager(t)
	manager.Start(context.Background())
	task, created, err := manager.QueueIssue(site)
	if err != nil || !created {
		t.Fatalf("queue task = %#v, created=%t, err=%v", task, created, err)
	}
	select {
	case <-issuer.started:
	case <-time.After(time.Second):
		t.Fatal("issuer was not started")
	}
	manager.Stop()
	select {
	case <-issuer.canceled:
	case <-time.After(time.Second):
		t.Fatal("issuer did not receive cancellation")
	}
	updated, err := database.GetTask(task.ID)
	if err != nil || updated.Status != domain.TaskFailed || !strings.Contains(updated.Detail, "shutdown") {
		t.Fatalf("stopped task = %#v, err=%v", updated, err)
	}
}

func TestCertificateIssueTimeoutFailsTask(t *testing.T) {
	database, site, manager, issuer := newCertificateTestManager(t)
	manager.IssueTimeout = 20 * time.Millisecond
	manager.Start(context.Background())
	t.Cleanup(manager.Stop)
	task, created, err := manager.QueueIssue(site)
	if err != nil || !created {
		t.Fatalf("queue task = %#v, created=%t, err=%v", task, created, err)
	}
	select {
	case <-issuer.started:
	case <-time.After(time.Second):
		t.Fatal("issuer was not started")
	}
	select {
	case <-issuer.canceled:
	case <-time.After(time.Second):
		t.Fatal("issuer did not receive timeout cancellation")
	}
	waitFor(t, time.Second, func() bool {
		updated, err := database.GetTask(task.ID)
		return err == nil && updated.Status == domain.TaskFailed && strings.Contains(updated.Detail, "timed out")
	})
}

func certificateTaskIsActive(status domain.TaskStatus) bool {
	return status == domain.TaskQueued || status == domain.TaskDispatching || status == domain.TaskApplying
}
