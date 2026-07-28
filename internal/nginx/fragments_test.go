package nginx

import (
	"strings"
	"testing"

	"simple_cdn/internal/domain"
)

func TestSplitHTTPConfigSeparatesBaseAndSiteServers(t *testing.T) {
	configuration, err := Render([]domain.Site{
		{ID: "site-a", Name: "A", Domains: []string{"a.example.test"}, PrimaryOrigin: domain.Origin{URL: "https://a-origin.example.test", Enabled: true}, RequestBodyBuffering: true, OriginResponseBuffering: true, Enabled: true},
		{ID: "site-b", Name: "B", Domains: []string{"b.example.test"}, PrimaryOrigin: domain.Origin{URL: "https://b-origin.example.test", Enabled: true}, RequestBodyBuffering: true, OriginResponseBuffering: true, Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	base, sites, err := SplitHTTPConfig(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 2 || sites[0].Name != "site-site-a.conf" || sites[1].Name != "site-site-b.conf" {
		t.Fatalf("HTTP site fragments = %#v", sites)
	}
	if !strings.Contains(base, "proxy_cache_path") || !strings.Contains(base, "$cdn_private_cache_bypass") || !strings.Contains(base, "listen 443 ssl default_server") || strings.Contains(base, "a-origin.example.test") || strings.Contains(base, "b-origin.example.test") {
		t.Fatalf("HTTP base fragment has the wrong ownership:\n%s", base)
	}
	if !strings.Contains(sites[0].Content, "a-origin.example.test") || !strings.Contains(sites[0].Content, "proxy_cache_bypass $cdn_private_cache_bypass;") ||
		!strings.Contains(sites[1].Content, "b-origin.example.test") || !strings.Contains(sites[1].Content, "proxy_no_cache $cdn_private_cache_bypass;") {
		t.Fatalf("site server fragments = %#v", sites)
	}
}

func TestSplitStreamConfigGroupsPortsBySite(t *testing.T) {
	configuration, err := RenderStream([]domain.Site{
		{ID: "mail-a", Name: "Mail A", TCPOnly: true, Enabled: true, TCPForwards: []domain.TCPForward{
			{Name: "smtp", ListenPort: 2525, UpstreamHost: "mail-a.example.test", UpstreamPort: 25},
			{Name: "imaps", ListenPort: 9993, UpstreamHost: "mail-a.example.test", UpstreamPort: 993},
		}},
		{ID: "mail-b", Name: "Mail B", TCPOnly: true, Enabled: true, TCPForwards: []domain.TCPForward{
			{Name: "smtps", ListenPort: 9465, UpstreamHost: "mail-b.example.test", UpstreamPort: 465},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	base, sites, err := SplitStreamConfig(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 2 || !strings.Contains(base, "map $server_port $cdn_tcp_site_id") || strings.Contains(base, "server {\n") {
		t.Fatalf("stream fragments: base=%q sites=%#v", base, sites)
	}
	if sites[0].Name != "site-mail-a.conf" || strings.Count(sites[0].Content, "server {") != 2 ||
		sites[1].Name != "site-mail-b.conf" || strings.Count(sites[1].Content, "server {") != 1 {
		t.Fatalf("grouped stream site fragments = %#v", sites)
	}
}

func TestSplitConfigFragmentsRejectsIncompleteMarkers(t *testing.T) {
	if _, _, err := SplitHTTPConfig("# CDN HTTP site fragment site-a begin\nserver {}\n"); err == nil {
		t.Fatal("incomplete HTTP fragment marker was accepted")
	}
	if _, _, err := SplitStreamConfig("# CDN stream site fragment site-a port 443 begin\nserver {}\n"); err == nil {
		t.Fatal("incomplete stream fragment marker was accepted")
	}
}
