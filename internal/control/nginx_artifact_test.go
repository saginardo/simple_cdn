package control

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type testNginxArchiveEntry struct {
	name     string
	mode     int64
	typeflag byte
	contents string
	linkname string
}

func TestResolveNginxBundleValidatesAndHashesArchive(t *testing.T) {
	pathname := filepath.Join(t.TempDir(), "cdn-nginx.tar.gz")
	writeTestNginxArchive(t, pathname, validTestNginxEntries("1.30.4"))

	metadata, err := ResolveNginxBundle(pathname)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Version != "1.30.4" || metadata.SHA256 != fileDigest(t, pathname) {
		t.Fatalf("bundle metadata = %#v", metadata)
	}
}

func TestResolveNginxBundleRejectsUnsafeOrMalformedArchives(t *testing.T) {
	tests := map[string]func([]testNginxArchiveEntry) []testNginxArchiveEntry{
		"outside managed root": func(entries []testNginxArchiveEntry) []testNginxArchiveEntry {
			return append(entries, testNginxArchiveEntry{name: "etc/passwd", mode: 0o644, typeflag: tar.TypeReg, contents: "bad"})
		},
		"duplicate path": func(entries []testNginxArchiveEntry) []testNginxArchiveEntry {
			return append(entries, entries[len(entries)-1])
		},
		"symbolic link": func(entries []testNginxArchiveEntry) []testNginxArchiveEntry {
			return append(entries, testNginxArchiveEntry{name: "nginx/conf/link", mode: 0o777, typeflag: tar.TypeSymlink, linkname: "/etc/passwd"})
		},
		"non-executable binary": func(entries []testNginxArchiveEntry) []testNginxArchiveEntry {
			for index := range entries {
				if entries[index].name == "nginx/sbin/nginx" {
					entries[index].mode = 0o644
				}
			}
			return entries
		},
		"wrong architecture": func(entries []testNginxArchiveEntry) []testNginxArchiveEntry {
			for index := range entries {
				if entries[index].name == "nginx/BUILD.json" {
					entries[index].contents = `{"nginx_version":"1.30.4","architecture":"arm64"}`
				}
			}
			return entries
		},
		"missing license notice": func(entries []testNginxArchiveEntry) []testNginxArchiveEntry {
			filtered := entries[:0]
			for _, entry := range entries {
				if entry.name != "nginx/licenses/nginx.txt" {
					filtered = append(filtered, entry)
				}
			}
			return filtered
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			pathname := filepath.Join(t.TempDir(), "cdn-nginx.tar.gz")
			writeTestNginxArchive(t, pathname, mutate(validTestNginxEntries("1.30.4")))
			if _, err := ResolveNginxBundle(pathname); err == nil {
				t.Fatal("malformed archive was accepted")
			}
		})
	}
}

func TestRenderBootstrapEdgeScriptQuotesManagedNginxArtifacts(t *testing.T) {
	digest := strings.Repeat("A", 64)
	script := renderBootstrapEdgeScript(
		"https://downloads.example.test/nginx'one", digest,
		"https://control.example.test/install-edge-nginx.service", strings.Repeat("b", 64),
	)
	for _, expected := range []string{
		`NGINX_BUNDLE_URL_DEFAULT='https://downloads.example.test/nginx'"'"'one'`,
		`NGINX_BUNDLE_SHA256_DEFAULT='` + strings.ToLower(digest) + `'`,
		`NGINX_SERVICE_URL_DEFAULT='https://control.example.test/install-edge-nginx.service'`,
	} {
		if !strings.Contains(script, expected) {
			t.Fatalf("rendered installer is missing %q", expected)
		}
	}
	command := exec.Command("bash", "-n")
	command.Stdin = strings.NewReader(script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("rendered installer syntax: %v\n%s", err, output)
	}
}

func validTestNginxEntries(version string) []testNginxArchiveEntry {
	return []testNginxArchiveEntry{
		{name: "nginx/", mode: 0o755, typeflag: tar.TypeDir},
		{name: "nginx/sbin/", mode: 0o755, typeflag: tar.TypeDir},
		{name: "nginx/conf/", mode: 0o755, typeflag: tar.TypeDir},
		{name: "nginx/licenses/", mode: 0o755, typeflag: tar.TypeDir},
		{name: "nginx/sbin/nginx", mode: 0o755, typeflag: tar.TypeReg, contents: "managed nginx"},
		{name: "nginx/conf/nginx.conf", mode: 0o644, typeflag: tar.TypeReg, contents: "events {}"},
		{name: "nginx/conf/mime.types", mode: 0o644, typeflag: tar.TypeReg, contents: "types {}"},
		{name: "nginx/licenses/nginx.txt", mode: 0o644, typeflag: tar.TypeReg, contents: "nginx license"},
		{name: "nginx/licenses/ngx_devel_kit.txt", mode: 0o644, typeflag: tar.TypeReg, contents: "ndk license"},
		{name: "nginx/licenses/openresty-luajit.txt", mode: 0o644, typeflag: tar.TypeReg, contents: "luajit license"},
		{name: "nginx/licenses/lua-nginx-module.txt", mode: 0o644, typeflag: tar.TypeReg, contents: "lua nginx license"},
		{name: "nginx/licenses/lua-resty-core.txt", mode: 0o644, typeflag: tar.TypeReg, contents: "resty core license"},
		{name: "nginx/licenses/lua-resty-lrucache.txt", mode: 0o644, typeflag: tar.TypeReg, contents: "lrucache license"},
		{name: "nginx/VERSION", mode: 0o644, typeflag: tar.TypeReg, contents: version + "\n"},
		{name: "nginx/BUILD.json", mode: 0o644, typeflag: tar.TypeReg, contents: fmt.Sprintf(`{"nginx_version":%q,"architecture":"amd64"}`, version)},
	}
}

func writeTestNginxArchive(t *testing.T, pathname string, entries []testNginxArchiveEntry) {
	t.Helper()
	file, err := os.Create(pathname)
	if err != nil {
		t.Fatal(err)
	}
	compressed := gzip.NewWriter(file)
	archive := tar.NewWriter(compressed)
	for _, entry := range entries {
		header := &tar.Header{
			Name: entry.name, Mode: entry.mode, Typeflag: entry.typeflag,
			Size: int64(len(entry.contents)), Linkname: entry.linkname,
		}
		if entry.typeflag == tar.TypeDir || entry.typeflag == tar.TypeSymlink {
			header.Size = 0
		}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := archive.Write([]byte(entry.contents)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
