package control

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
)

const (
	nginxBundleLimit             = 128 << 20
	nginxBundleUncompressedLimit = 512 << 20
	nginxBundleEntryLimit        = 4096
)

var (
	nginxVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	gitCommitPattern    = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

type NginxBundleMetadata struct {
	SHA256  string
	Version string
}

type nginxBundleBuild struct {
	NginxVersion    string `json:"nginx_version"`
	Architecture    string `json:"architecture"`
	NGXBrotliCommit string `json:"ngx_brotli_commit"`
	BrotliCommit    string `json:"brotli_commit"`
	ZstdNginxCommit string `json:"zstd_nginx_commit"`
}

func ResolveNginxBundle(pathname string) (NginxBundleMetadata, error) {
	pathname = strings.TrimSpace(pathname)
	if pathname == "" {
		return NginxBundleMetadata{}, nil
	}
	file, err := os.Open(pathname)
	if err != nil {
		return NginxBundleMetadata{}, fmt.Errorf("read NGINX_BUNDLE_PATH: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return NginxBundleMetadata{}, fmt.Errorf("inspect NGINX_BUNDLE_PATH: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > nginxBundleLimit {
		return NginxBundleMetadata{}, errors.New("NGINX_BUNDLE_PATH must be a non-empty regular file no larger than 128 MiB")
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, nginxBundleLimit+1))
	if err != nil || written != info.Size() {
		return NginxBundleMetadata{}, errors.New("could not hash the complete Nginx bundle")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return NginxBundleMetadata{}, fmt.Errorf("rewind Nginx bundle: %w", err)
	}
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return NginxBundleMetadata{}, fmt.Errorf("open Nginx bundle: %w", err)
	}
	archive := tar.NewReader(compressed)
	version := ""
	build := nginxBundleBuild{}
	seen := make(map[string]struct{})
	entryCount := 0
	uncompressedSize := int64(0)
	required := map[string]bool{
		"nginx/sbin/nginx":                      false,
		"nginx/conf/nginx.conf":                 false,
		"nginx/conf/mime.types":                 false,
		"nginx/licenses/nginx.txt":              false,
		"nginx/licenses/ngx_devel_kit.txt":      false,
		"nginx/licenses/openresty-luajit.txt":   false,
		"nginx/licenses/lua-nginx-module.txt":   false,
		"nginx/licenses/lua-resty-core.txt":     false,
		"nginx/licenses/lua-resty-lrucache.txt": false,
		"nginx/licenses/ngx_brotli.txt":         false,
		"nginx/licenses/brotli.txt":             false,
		"nginx/licenses/zstd-nginx-module.txt":  false,
		"nginx/licenses/zstd-library.txt":       false,
		"nginx/VERSION":                         false,
		"nginx/BUILD.json":                      false,
	}
	for {
		header, readErr := archive.Next()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return NginxBundleMetadata{}, fmt.Errorf("read Nginx bundle: %w", readErr)
		}
		entryCount++
		if entryCount > nginxBundleEntryLimit {
			return NginxBundleMetadata{}, fmt.Errorf("Nginx bundle contains more than %d entries", nginxBundleEntryLimit)
		}
		entryName := header.Name
		if header.Typeflag == tar.TypeDir {
			entryName = strings.TrimSuffix(entryName, "/")
		}
		cleaned := path.Clean(entryName)
		if cleaned != entryName || strings.HasPrefix(cleaned, "/") || cleaned == ".." || strings.HasPrefix(cleaned, "../") ||
			(cleaned != "nginx" && !strings.HasPrefix(cleaned, "nginx/")) || strings.ContainsAny(cleaned, " \t\r\n") {
			return NginxBundleMetadata{}, fmt.Errorf("Nginx bundle contains unsafe path %q", header.Name)
		}
		if _, exists := seen[cleaned]; exists {
			return NginxBundleMetadata{}, fmt.Errorf("Nginx bundle contains duplicate entry %q", header.Name)
		}
		seen[cleaned] = struct{}{}
		if header.Typeflag != tar.TypeDir && header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return NginxBundleMetadata{}, fmt.Errorf("Nginx bundle contains unsupported entry %q", header.Name)
		}
		if header.Size < 0 || header.Size > nginxBundleUncompressedLimit-uncompressedSize {
			return NginxBundleMetadata{}, fmt.Errorf("Nginx bundle expands beyond %d bytes", nginxBundleUncompressedLimit)
		}
		uncompressedSize += header.Size
		if header.Name == "nginx/sbin/nginx" && header.FileInfo().Mode()&0o111 == 0 {
			return NginxBundleMetadata{}, errors.New("Nginx bundle binary is not executable")
		}
		if _, found := required[header.Name]; found {
			if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
				return NginxBundleMetadata{}, fmt.Errorf("Nginx bundle required entry %q is not a regular file", header.Name)
			}
			required[header.Name] = true
		}
		if header.Name == "nginx/VERSION" {
			if header.Size > 64 {
				return NginxBundleMetadata{}, errors.New("Nginx bundle VERSION is too large")
			}
			contents, readErr := io.ReadAll(io.LimitReader(archive, 65))
			if readErr != nil {
				return NginxBundleMetadata{}, fmt.Errorf("read Nginx bundle version: %w", readErr)
			}
			version = strings.TrimSpace(string(contents))
		}
		if header.Name == "nginx/BUILD.json" {
			if header.Size > 4096 {
				return NginxBundleMetadata{}, errors.New("Nginx bundle BUILD.json is too large")
			}
			contents, readErr := io.ReadAll(io.LimitReader(archive, 4097))
			if readErr != nil || len(contents) > 4096 || json.Unmarshal(contents, &build) != nil {
				return NginxBundleMetadata{}, errors.New("Nginx bundle BUILD.json is invalid")
			}
		}
	}
	if err := compressed.Close(); err != nil {
		return NginxBundleMetadata{}, fmt.Errorf("close Nginx bundle: %w", err)
	}
	for name, found := range required {
		if !found {
			return NginxBundleMetadata{}, fmt.Errorf("Nginx bundle is missing %s", name)
		}
	}
	if !nginxVersionPattern.MatchString(version) {
		return NginxBundleMetadata{}, errors.New("Nginx bundle VERSION is invalid")
	}
	if build.NginxVersion != version || build.Architecture != "amd64" {
		return NginxBundleMetadata{}, errors.New("Nginx bundle BUILD.json does not match VERSION or amd64 architecture")
	}
	for _, commit := range []string{build.NGXBrotliCommit, build.BrotliCommit, build.ZstdNginxCommit} {
		if !gitCommitPattern.MatchString(strings.ToLower(strings.TrimSpace(commit))) {
			return NginxBundleMetadata{}, errors.New("Nginx bundle BUILD.json has invalid compression module metadata")
		}
	}
	return NginxBundleMetadata{SHA256: hex.EncodeToString(hash.Sum(nil)), Version: version}, nil
}

func renderBootstrapEdgeScript(nginxBundleURL, nginxBundleSHA256, nginxServiceURL, nginxServiceSHA256 string) string {
	replacements := map[string]string{
		`NGINX_BUNDLE_URL_DEFAULT=""`:     `NGINX_BUNDLE_URL_DEFAULT=` + shellSingleQuote(nginxBundleURL),
		`NGINX_BUNDLE_SHA256_DEFAULT=""`:  `NGINX_BUNDLE_SHA256_DEFAULT=` + shellSingleQuote(strings.ToLower(strings.TrimSpace(nginxBundleSHA256))),
		`NGINX_SERVICE_URL_DEFAULT=""`:    `NGINX_SERVICE_URL_DEFAULT=` + shellSingleQuote(nginxServiceURL),
		`NGINX_SERVICE_SHA256_DEFAULT=""`: `NGINX_SERVICE_SHA256_DEFAULT=` + shellSingleQuote(strings.ToLower(strings.TrimSpace(nginxServiceSHA256))),
	}
	result := bootstrapEdgeScript
	for old, replacement := range replacements {
		result = strings.Replace(result, old, replacement, 1)
	}
	return result
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
