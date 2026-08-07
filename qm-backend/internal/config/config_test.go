package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOAuthCatalogConfigRequiresExplicitAndKnownProviderDeclaration(t *testing.T) {
	write := func(contents string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	base := "database:\n  url: postgres://example/qm\nqm:\n  org_id: acme\n"

	if _, err := Load(write(base + "  oauth_configured_providers: [github]\n")); err == nil || !strings.Contains(err.Error(), "oauth_catalog_enabled") {
		t.Fatalf("configured provider without explicit catalog enablement err=%v", err)
	}
	if _, err := Load(write(base + "  oauth_catalog_enabled: true\n  oauth_configured_providers: [unknown]\n")); err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("unknown configured provider err=%v", err)
	}
	if _, err := Load(write(base + "  oauth_catalog_enabled: true\n  oauth_configured_providers: [github, github]\n")); err == nil || !strings.Contains(err.Error(), "duplicate provider") {
		t.Fatalf("duplicate configured provider err=%v", err)
	}
	loaded, err := Load(write(base + "  oauth_catalog_enabled: true\n  oauth_configured_providers: [github]\n"))
	if err != nil || !loaded.QM.OAuthCatalogEnabled || len(loaded.QM.OAuthConfiguredProviders) != 1 || loaded.QM.OAuthConfiguredProviders[0] != "github" {
		t.Fatalf("valid OAuth catalog config=%#v err=%v", loaded.QM, err)
	}
}
