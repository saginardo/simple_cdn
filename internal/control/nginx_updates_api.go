package control

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

type nginxArtifactTarget struct {
	SHA256  string
	Version string
	URL     string
	Path    string
	Managed bool
}

type nginxArtifactView struct {
	domain.NginxArtifact
	Managed     bool   `json:"managed"`
	DownloadURL string `json:"download_url"`
}

type nginxArtifactStatusResponse struct {
	NginxUpdateRuntimeStatus
	Current       nginxArtifactView  `json:"current"`
	Candidate     *nginxArtifactView `json:"candidate,omitempty"`
	ArtifactError string             `json:"artifact_error,omitempty"`
}

func (s *Server) currentNginxArtifactTarget() (nginxArtifactTarget, error) {
	if s.NginxUpdates != nil {
		artifact, err := s.Store.CurrentNginxArtifact()
		if err == nil {
			if !s.NginxUpdates.ArtifactReady(artifact) {
				return nginxArtifactTarget{}, errors.New("主控当前 Nginx 升级制品缺失或大小不匹配")
			}
			return s.managedNginxArtifactTarget(artifact), nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return nginxArtifactTarget{}, err
		}
	}
	return s.builtinNginxArtifactTarget(), nil
}

func (s *Server) builtinNginxArtifactTarget() nginxArtifactTarget {
	sha256 := strings.ToLower(strings.TrimSpace(s.NginxBundleSHA256))
	target := nginxArtifactTarget{
		SHA256: sha256, Version: strings.TrimSpace(s.NginxVersion),
		URL: strings.TrimSpace(s.NginxBundleURL), Path: strings.TrimSpace(s.NginxBundlePath),
	}
	if target.Path != "" && validSHA256Digest(target.SHA256) && validHTTPSURL(s.edgeControlURL()) {
		target.URL = s.versionedNginxBundleURL(target.SHA256)
	}
	return target
}

func (s *Server) managedNginxArtifactTarget(artifact domain.NginxArtifact) nginxArtifactTarget {
	return nginxArtifactTarget{
		SHA256: artifact.SHA256, Version: artifact.Version,
		URL: s.versionedNginxBundleURL(artifact.SHA256), Path: s.NginxUpdates.ArtifactPath(artifact.SHA256),
		Managed: true,
	}
}

func (s *Server) nginxArtifactTargetBySHA(sha256 string) (nginxArtifactTarget, error) {
	sha256 = strings.ToLower(strings.TrimSpace(sha256))
	if !validSHA256Digest(sha256) {
		return nginxArtifactTarget{}, store.ErrNotFound
	}
	builtin := s.builtinNginxArtifactTarget()
	if builtin.Path != "" && sha256 == builtin.SHA256 {
		builtin.URL = s.versionedNginxBundleURL(builtin.SHA256)
		return builtin, nil
	}
	if s.NginxUpdates == nil {
		return nginxArtifactTarget{}, store.ErrNotFound
	}
	artifact, err := s.Store.NginxArtifactBySHA(sha256)
	if err != nil {
		return nginxArtifactTarget{}, err
	}
	if !s.NginxUpdates.ArtifactReady(artifact) {
		return nginxArtifactTarget{}, store.ErrNotFound
	}
	return s.managedNginxArtifactTarget(artifact), nil
}

func (s *Server) versionedNginxBundleURL(sha256 string) string {
	return strings.TrimRight(s.edgeControlURL(), "/") + "/downloads/nginx/" + sha256 + "/" + nginxReleaseBundleName
}

func (s *Server) versionedEdgeInstallerURL(sha256 string) string {
	return strings.TrimRight(s.edgeControlURL(), "/") + "/downloads/nginx/" + sha256 + "/install-edge.sh"
}

func (s *Server) renderEdgeInstallerFor(target nginxArtifactTarget) string {
	baseURL := strings.TrimRight(s.edgeControlURL(), "/")
	return renderBootstrapEdgeScript(
		target.URL, target.SHA256,
		baseURL+"/install-edge-nginx.service", resourceSHA256(bootstrapEdgeNginxService),
	)
}

func (s *Server) versionedNginxBundle(response http.ResponseWriter, request *http.Request) {
	target, err := s.nginxArtifactTargetBySHA(request.PathValue("sha256"))
	if err != nil {
		http.NotFound(response, request)
		return
	}
	info, err := os.Stat(target.Path)
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(response, request)
		return
	}
	response.Header().Set("Content-Type", "application/gzip")
	response.Header().Set("Content-Disposition", "attachment; filename="+nginxReleaseBundleName)
	response.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(response, request, target.Path)
}

func (s *Server) versionedEdgeInstaller(response http.ResponseWriter, request *http.Request) {
	target, err := s.nginxArtifactTargetBySHA(request.PathValue("sha256"))
	if err != nil {
		http.NotFound(response, request)
		return
	}
	response.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	response.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = response.Write([]byte(s.renderEdgeInstallerFor(target)))
}

func (s *Server) nginxArtifactStatus(response http.ResponseWriter, _ *http.Request) {
	status, err := s.buildNginxArtifactStatus()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, status)
}

func (s *Server) checkNginxArtifacts(response http.ResponseWriter, request *http.Request) {
	if s.NginxUpdates == nil {
		writeError(response, http.StatusConflict, errors.New("主控未配置 Nginx 更新检查器"))
		return
	}
	if err := s.NginxUpdates.Check(request.Context()); err != nil {
		writeError(response, http.StatusBadGateway, err)
		return
	}
	status, err := s.buildNginxArtifactStatus()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	s.audit(request, adminID(request.Context()), "check_updates", "nginx_artifact", "stable", "repository="+status.Repository)
	writeJSON(response, http.StatusOK, status)
}

func (s *Server) promoteNginxArtifact(response http.ResponseWriter, request *http.Request) {
	if s.NginxUpdates == nil {
		writeError(response, http.StatusConflict, errors.New("主控未配置 Nginx 更新检查器"))
		return
	}
	artifact, changed, err := s.NginxUpdates.Promote(request.PathValue("sha256"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(response, http.StatusNotFound, err)
			return
		}
		writeError(response, http.StatusConflict, err)
		return
	}
	if changed {
		s.audit(request, adminID(request.Context()), "promote", "nginx_artifact", artifact.SHA256, "version="+artifact.Version)
		_, _, messageErr := s.Store.CreateMessageOnce(domain.Message{
			Severity: domain.MessageSuccess, Category: "nginx_update",
			Title:      "Nginx 节点升级目标已更新",
			Body:       fmt.Sprintf("Nginx %s 已设为当前目标，节点不会自动升级；请在节点页选择单节点或全部升级。", artifact.Version),
			SourceType: "nginx_artifact", SourceID: artifact.SHA256, SourceStatus: "current",
		})
		if messageErr != nil {
			writeError(response, http.StatusInternalServerError, messageErr)
			return
		}
	}
	status, err := s.buildNginxArtifactStatus()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, status)
}

func (s *Server) buildNginxArtifactStatus() (nginxArtifactStatusResponse, error) {
	runtimeStatus := NginxUpdateRuntimeStatus{}
	if s.NginxUpdates != nil {
		runtimeStatus = s.NginxUpdates.Status()
	}
	target, err := s.currentNginxArtifactTarget()
	if err != nil {
		artifact, currentErr := s.Store.CurrentNginxArtifact()
		if currentErr != nil {
			return nginxArtifactStatusResponse{}, err
		}
		target = s.managedNginxArtifactTarget(artifact)
	}
	current := nginxArtifactView{
		NginxArtifact: domain.NginxArtifact{
			SHA256: target.SHA256, Version: target.Version, State: domain.NginxArtifactCurrent,
		},
		Managed: target.Managed, DownloadURL: target.URL,
	}
	if target.Managed {
		artifact, err := s.Store.CurrentNginxArtifact()
		if err != nil {
			return nginxArtifactStatusResponse{}, err
		}
		current.NginxArtifact = artifact
	}
	result := nginxArtifactStatusResponse{NginxUpdateRuntimeStatus: runtimeStatus, Current: current}
	if err != nil {
		result.ArtifactError = err.Error()
	}
	if s.NginxUpdates == nil {
		return result, nil
	}
	candidate, err := s.Store.CandidateNginxArtifact()
	if errors.Is(err, store.ErrNotFound) {
		return result, nil
	}
	if err != nil {
		return nginxArtifactStatusResponse{}, err
	}
	view := nginxArtifactView{
		NginxArtifact: candidate, Managed: true,
		DownloadURL: s.versionedNginxBundleURL(candidate.SHA256),
	}
	result.Candidate = &view
	return result, nil
}
