package config

import (
	"errors"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server struct {
		HTTPAddr string `yaml:"http_addr"`
		GRPCAddr string `yaml:"grpc_addr"`
	} `yaml:"server"`
	Database struct {
		URL string `yaml:"url"`
	} `yaml:"database"`
	QM struct {
		OrgID                    string   `yaml:"org_id"`
		NodeCoreURL              string   `yaml:"node_core_url"`
		RouteMode                string   `yaml:"route_mode"`
		SlackEnvironmentState    string   `yaml:"slack_environment_state"`
		SandboxDefaultBackend    string   `yaml:"sandbox_default_backend"`
		SandboxBackends          []string `yaml:"sandbox_backends"`
		OAuthCatalogEnabled      bool     `yaml:"oauth_catalog_enabled"`
		OAuthConfiguredProviders []string `yaml:"oauth_configured_providers"`
		FileStore                struct {
			Mode             string `yaml:"mode"`
			LocalDir         string `yaml:"local_dir"`
			TransferLocalDir string `yaml:"transfer_local_dir"`
		} `yaml:"file_store"`
	} `yaml:"qm"`
	Runner struct {
		LeaseTTLSeconds int `yaml:"lease_ttl_seconds"`
		MaxClaims       int `yaml:"max_claims"`
	} `yaml:"runner"`
	Knowledge struct {
		RootDir string `yaml:"root_dir"`
		LLM     struct {
			BaseURL        string `yaml:"base_url"`
			APIKey         string `yaml:"api_key"`
			Model          string `yaml:"model"`
			TimeoutSeconds int    `yaml:"timeout_seconds"`
		} `yaml:"llm"`
	} `yaml:"knowledge"`
	Auth struct {
		SourceSigningSecret  string `yaml:"source_signing_secret"`
		CapabilitySecret     string `yaml:"capability_secret"`
		PortalIdentitySecret string `yaml:"portal_identity_secret"`
		GRPCInternalToken    string `yaml:"grpc_internal_token"`
		ReplayWindowSeconds  int    `yaml:"replay_window_seconds"`
	} `yaml:"auth"`
}

func Default() Config {
	var c Config
	c.Server.HTTPAddr = ":8080"
	c.Server.GRPCAddr = "127.0.0.1:9090"
	c.QM.OrgID = "local"
	c.QM.NodeCoreURL = "http://127.0.0.1:8081"
	c.QM.RouteMode = "proxy"
	c.QM.SlackEnvironmentState = "unknown"
	c.Runner.LeaseTTLSeconds = 30
	c.Runner.MaxClaims = 8
	c.Knowledge.RootDir = "data/knowledge"
	c.Knowledge.LLM.TimeoutSeconds = 60
	c.Auth.ReplayWindowSeconds = 300
	return c
}

func Load(path string) (Config, error) {
	c := Default()
	if path != "" {
		contents, err := os.ReadFile(path)
		if err != nil {
			return Config{}, err
		}
		if err := yaml.Unmarshal(contents, &c); err != nil {
			return Config{}, err
		}
	}
	c.applyLegacyQMEnv(os.LookupEnv)
	if c.Database.URL == "" {
		return Config{}, errors.New("database.url or DATABASE_URL is required")
	}
	if c.QM.OrgID == "" {
		return Config{}, errors.New("qm.org_id or ORG_ID is required")
	}
	if c.QM.RouteMode != "proxy" && c.QM.RouteMode != "shadow_read" && c.QM.RouteMode != "go" {
		return Config{}, errors.New("qm.route_mode must be proxy, shadow_read, or go")
	}
	if c.QM.SlackEnvironmentState != "unknown" && c.QM.SlackEnvironmentState != "absent" && c.QM.SlackEnvironmentState != "configured" && c.QM.SlackEnvironmentState != "partial" {
		return Config{}, errors.New("qm.slack_environment_state must be unknown, absent, configured, or partial")
	}
	if !c.QM.OAuthCatalogEnabled && len(c.QM.OAuthConfiguredProviders) != 0 {
		return Config{}, errors.New("qm.oauth_catalog_enabled is required when qm.oauth_configured_providers is set")
	}
	knownOAuthProviders := map[string]bool{"google": true, "slack": true, "notion": true, "linear": true, "dropbox": true, "github": true, "x": true}
	configuredOAuthProviders := map[string]bool{}
	for _, provider := range c.QM.OAuthConfiguredProviders {
		provider = strings.TrimSpace(provider)
		if !knownOAuthProviders[provider] {
			return Config{}, errors.New("qm.oauth_configured_providers contains an unknown provider: " + provider)
		}
		if configuredOAuthProviders[provider] {
			return Config{}, errors.New("qm.oauth_configured_providers contains a duplicate provider: " + provider)
		}
		configuredOAuthProviders[provider] = true
	}
	if c.QM.SandboxDefaultBackend != "" {
		if !validSandboxBackend(c.QM.SandboxDefaultBackend) {
			return Config{}, errors.New("qm.sandbox_default_backend must be sprites, aws, or local")
		}
		if len(c.QM.SandboxBackends) == 0 {
			return Config{}, errors.New("qm.sandbox_backends is required when qm.sandbox_default_backend is set")
		}
		foundDefault := false
		for _, backend := range c.QM.SandboxBackends {
			if !validSandboxBackend(backend) {
				return Config{}, errors.New("qm.sandbox_backends may contain only sprites, aws, or local")
			}
			if backend == c.QM.SandboxDefaultBackend {
				foundDefault = true
			}
		}
		if !foundDefault {
			return Config{}, errors.New("qm.sandbox_backends must include qm.sandbox_default_backend")
		}
	} else if len(c.QM.SandboxBackends) != 0 {
		return Config{}, errors.New("qm.sandbox_default_backend is required when qm.sandbox_backends is set")
	}
	if c.QM.FileStore.Mode != "" && c.QM.FileStore.Mode != "local" {
		return Config{}, errors.New("qm.file_store.mode must be local when set")
	}
	if c.QM.FileStore.Mode == "local" && strings.TrimSpace(c.QM.FileStore.LocalDir) == "" {
		return Config{}, errors.New("qm.file_store.local_dir is required when qm.file_store.mode is local")
	}
	if c.QM.FileStore.Mode == "" && strings.TrimSpace(c.QM.FileStore.TransferLocalDir) != "" {
		return Config{}, errors.New("qm.file_store.mode is required when qm.file_store.transfer_local_dir is set")
	}
	if c.QM.FileStore.Mode == "" && strings.TrimSpace(c.QM.FileStore.LocalDir) != "" {
		return Config{}, errors.New("qm.file_store.mode is required when qm.file_store.local_dir is set")
	}
	return c, nil
}

func validSandboxBackend(value string) bool {
	return value == "sprites" || value == "aws" || value == "local"
}

func (c *Config) applyLegacyQMEnv(lookup func(string) (string, bool)) {
	set := func(key string, dest *string) {
		if value, ok := lookup(key); ok && strings.TrimSpace(value) != "" {
			*dest = value
		}
	}
	set("DATABASE_URL", &c.Database.URL)
	set("ORG_ID", &c.QM.OrgID)
	set("NODE_CORE_URL", &c.QM.NodeCoreURL)
	set("CORE_SIGNING_SECRET", &c.Auth.SourceSigningSecret)
	set("CAPABILITY_SECRET", &c.Auth.CapabilitySecret)
	set("PORTAL_IDENTITY_SECRET", &c.Auth.PortalIdentitySecret)
	set("QM_BACKEND_GRPC_INTERNAL_TOKEN", &c.Auth.GRPCInternalToken)
	if value, ok := lookup("QM_ROUTE_MODE"); ok && value != "" {
		c.QM.RouteMode = value
	}
	if value, ok := lookup("PORT"); ok && value != "" {
		c.Server.HTTPAddr = ":" + value
	}
	if value, ok := lookup("MAX_CLAIMS"); ok {
		if maxClaims, err := strconv.Atoi(value); err == nil && maxClaims > 0 {
			c.Runner.MaxClaims = maxClaims
		}
	}
	if value, ok := lookup("SOURCE_AUTH_REPLAY_WINDOW_SECONDS"); ok {
		if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
			c.Auth.ReplayWindowSeconds = seconds
		}
	}
}
