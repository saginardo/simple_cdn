package version

import "testing"

func TestDevelopmentVersionDefault(t *testing.T) {
	if Version != "dev" {
		t.Fatalf("Version = %q, want development default", Version)
	}
}
