package server

import (
	_ "embed"
	"encoding/json"
	"net/url"
)

//go:embed slack_manifest.json
var slackManifestTemplate []byte

func slackManifestCreationURL() string {
	manifest := map[string]any{}
	if json.Unmarshal(slackManifestTemplate, &manifest) != nil {
		return "https://api.slack.com/apps?new_app=1"
	}
	if display, ok := manifest["display_information"].(map[string]any); ok {
		display["name"], display["description"] = "qm", "qm workspace agent"
	}
	if features, ok := manifest["features"].(map[string]any); ok {
		if bot, ok := features["bot_user"].(map[string]any); ok {
			bot["display_name"] = "qm"
		}
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return "https://api.slack.com/apps?new_app=1"
	}
	u := url.URL{Scheme: "https", Host: "api.slack.com", Path: "/apps"}
	query := url.Values{}
	query.Set("new_app", "1")
	query.Set("manifest_json", string(raw))
	u.RawQuery = query.Encode()
	return u.String()
}
