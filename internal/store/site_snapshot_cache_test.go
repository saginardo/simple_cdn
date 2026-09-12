package store

import (
	"path/filepath"
	"simple_cdn/internal/domain"
	"testing"
)

func TestSiteSnapshotCachesRefreshIndependently(t *testing.T) {
	for _, refreshSummaryFirst := range []bool{false, true} {
		name := "refresh_sites_then_summaries"
		if refreshSummaryFirst {
			name = "refresh_summaries_then_sites"
		}
		t.Run(name, func(t *testing.T) {
			database, err := Open(filepath.Join(t.TempDir(), "control.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			node, err := database.CreateNode("audit-edge", "203.0.113.83")
			if err != nil {
				t.Fatal(err)
			}
			site, err := database.CreateSite(domain.Site{
				Name: "Before", Domains: []string{"audit.example.test"}, Nodes: []string{node.ID},
				PrimaryOrigin: domain.Origin{URL: "https://before-origin.example.test", Enabled: true}, Enabled: true,
			}, "zone")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.ListSites(); err != nil {
				t.Fatal(err)
			}
			if _, err := database.ListSiteSummaries(); err != nil {
				t.Fatal(err)
			}
			site.Name = "After"
			site.PrimaryOrigin.URL = "https://after-origin.example.test"
			site, err = database.UpdateSite(site, "zone")
			if err != nil {
				t.Fatal(err)
			}
			stored, _, err := database.GetSite(site.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Name != "After" {
				t.Fatalf("database update failed: %q", stored.Name)
			}
			if refreshSummaryFirst {
				if _, err := database.ListSiteSummaries(); err != nil {
					t.Fatal(err)
				}
				for attempt := 0; attempt < 2; attempt++ {
					sites, err := database.ListSites()
					if err != nil {
						t.Fatal(err)
					}
					if len(sites) != 1 {
						t.Fatalf("sites = %d", len(sites))
					}
					if sites[0].Name != stored.Name || sites[0].PrimaryOrigin.URL != stored.PrimaryOrigin.URL {
						t.Errorf("read %d: ListSites returns name=%q origin=%q; persisted name=%q origin=%q", attempt+1, sites[0].Name, sites[0].PrimaryOrigin.URL, stored.Name, stored.PrimaryOrigin.URL)
					}
				}
			} else {
				if _, err := database.ListSites(); err != nil {
					t.Fatal(err)
				}
				summaries, err := database.ListSiteSummaries()
				if err != nil {
					t.Fatal(err)
				}
				if len(summaries) != 1 {
					t.Fatalf("summaries = %d", len(summaries))
				}
				if summaries[0].Name != stored.Name {
					t.Errorf("ListSiteSummaries returns name=%q; persisted name=%q", summaries[0].Name, stored.Name)
				}
			}
		})
	}
}
