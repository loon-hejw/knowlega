package server

// adminResourceManifest is the compatibility contract returned by Node's
// adminResourceManifest(). Keep resource application in Node until every
// resource has a Go implementation; this file intentionally owns navigation
// metadata only.
func adminResourceManifest() []map[string]any {
	models := func() []map[string]string {
		return []map[string]string{
			{"id": "claude-fable-5", "name": "Claude Fable 5"},
			{"id": "claude-opus-5", "name": "Claude Opus 5"},
			{"id": "claude-opus-4-8", "name": "Claude Opus 4.8"},
			{"id": "claude-sonnet-5", "name": "Claude Sonnet 5"},
			{"id": "claude-haiku-4-5", "name": "Claude Haiku 4.5"},
			{"id": "gpt-5.6-sol", "name": "GPT-5.6 Sol"},
			{"id": "gpt-5.6-terra", "name": "GPT-5.6 Terra"},
			{"id": "gpt-5.6-luna", "name": "GPT-5.6 Luna"},
			{"id": "openrouter/auto", "name": "OpenRouter Auto"},
		}
	}
	return []map[string]any{
		{"id": "security-posture", "kind": "enum", "target": "any", "label": "Harness security posture. The org value is a minimum; narrower scopes may tighten it but cannot weaken it.", "enumValues": []string{"dangerous", "auto", "strict"}},
		{"id": "approval-grant-modes", "kind": "custom", "target": "any", "label": "Which standing HiLO approval options are offered: \"Allow session\" and \"Allow always\". Composes tighten-only with the org value; disabling a mode also suspends existing grants of that mode until re-enabled."},
		{"id": "command-policy", "kind": "custom"},
		{"id": "soul", "kind": "text"},
		{"id": "ambient-policy", "kind": "custom"},
		{"id": "egress", "kind": "custom"},
		{"id": "device-flow-cutover", "kind": "custom", "target": "any", "label": "Credential-file migration by service. legacy restores resident files; prefer_ephemeral uses the isolated adapter with legacy fallback; ephemeral_only quarantines the stored legacy copy without deleting it; inherit clears a scope override."},
		{"id": "unfulfilled-insights", "kind": "boolean"},
		{"id": "external-slack-participants", "kind": "boolean", "target": "org", "label": "Supports external Slack participants: internal members may chat with the agent in Slack rooms whose audience includes an external user (Connect member or guest). Externals themselves still can't interact."},
		{"id": "org-ambient", "kind": "boolean", "target": "org", "label": "Ambient behavior org-wide: off means the agent never acts on overheard messages anywhere, regardless of per-channel settings."},
		{"id": "interactive-fast-mode", "kind": "boolean", "target": "org", "label": "Fast mode for interactive turns org-wide: on means human turns run in fast mode on fast-capable models unless the turn asks otherwise. Requires fast-mode quota with the provider."},
		{"id": "base-model", "kind": "enum", "target": "any", "clearable": true, "enumValues": models()},
		{"id": "runtime", "kind": "custom", "target": "any", "clearable": true},
		{"id": "approved-harnesses", "kind": "string-list", "target": "org", "clearable": true, "enumValues": []string{"pi", "opencode", "codex", "claude", "mock"}},
		{"id": "webui-models", "kind": "string-list", "target": "org", "clearable": true, "label": "Web UI model picker (ordered list of model ids; the org base model is the default selection, else the first). Empty restores the built-in set.", "enumValues": models()},
		{"id": "people-directory-url", "kind": "string", "target": "org", "clearable": true},
		{"id": "branding", "kind": "custom", "target": "org", "clearable": true},
		{"id": "turn-wall-clock", "kind": "string", "target": "org", "clearable": true, "label": "Maximum wall-clock time for a turn in seconds. Zero leaves turns uncapped; empty restores the deployment default."},
		{"id": "browse-model", "kind": "enum", "target": "org", "clearable": true, "label": "The model driving the browser agent in the browse skill, org-wide (empty follows the deployment's base model; fast mode applies only on Opus models).", "enumValues": models()},
		{"id": "browse-max-steps", "kind": "string", "target": "org", "clearable": true, "label": "Max browser steps per browse task (empty restores the default of 50)."},
		{"id": "connectors", "kind": "custom"},
		{"id": "service-credentials", "kind": "custom", "target": "org", "secret": true},
	}
}
