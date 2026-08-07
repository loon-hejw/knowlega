package server

import (
	"reflect"
	"testing"

	"github.com/loon-hejw/knowlega/internal/qm/data"
)

func TestBestOAuthConnectorStatusMatchesNodeAccountSlotPreference(t *testing.T) {
	expiresAt := int64(1234)
	statuses := []data.ConnectorTokenStatus{
		{OwnerID: "U1", Host: "api.github.com", AccountType: "", Connected: true, NeedsReconnect: true},
		{OwnerID: "U1", Host: "api.github.com", AccountType: "personal", Connected: true, ExpiresAt: &expiresAt},
		{OwnerID: "U1", Host: "api.github.com", AccountType: "company", Connected: true},
	}
	best := bestOAuthConnectorStatus(statuses, "U1", "API.GITHUB.COM")
	if best.AccountType != "personal" || !best.Connected || best.NeedsReconnect || best.ExpiresAt == nil || *best.ExpiresAt != expiresAt {
		t.Fatalf("best status=%#v", best)
	}
	for index, provider := range oauthCatalogProviders {
		if !reflect.DeepEqual(oauthProviderHosts[provider.Name], provider.Hosts) {
			t.Fatalf("provider host catalog drift for %s: static=%v revoke=%v", provider.Name, provider.Hosts, oauthProviderHosts[provider.Name])
		}
		if provider.Name == "github" && provider.ConsentMode != "github_app" {
			t.Fatalf("github catalog provider=%#v", provider)
		}
		if len(provider.Hosts) == 0 || provider.SetupGuide["url"] == "" {
			t.Fatalf("incomplete catalog provider at index %d: %#v", index, provider)
		}
	}
}
