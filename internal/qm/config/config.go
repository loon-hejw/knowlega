package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type ModelDefinition struct {
	ID               string `yaml:"id" json:"id"`
	Name             string `yaml:"name" json:"name"`
	ContextWindow    int    `yaml:"context_window" json:"contextWindow,omitempty"`
	MaxTokens        int    `yaml:"max_tokens" json:"maxTokens,omitempty"`
	FastMode         bool   `yaml:"fast_mode" json:"fastMode,omitempty"`
	AdaptiveThinking bool   `yaml:"adaptive_thinking" json:"adaptiveThinking,omitempty"`
}

type ModelProviderConfig struct {
	ID               string            `yaml:"id" json:"id"`
	Protocol         string            `yaml:"protocol" json:"protocol"`
	BaseURL          string            `yaml:"base_url" json:"baseUrl"`
	APIKey           string            `yaml:"api_key" json:"-"`
	UserAgent        string            `yaml:"user_agent" json:"userAgent,omitempty"`
	AnthropicVersion string            `yaml:"anthropic_version" json:"anthropicVersion,omitempty"`
	Models           []ModelDefinition `yaml:"models" json:"models"`
}

type ModelRequestConfig struct {
	TimeoutSeconds          int  `yaml:"timeout_seconds" json:"timeoutSeconds"`
	OperationTimeoutSeconds int  `yaml:"operation_timeout_seconds" json:"operationTimeoutSeconds"`
	Retries                 int  `yaml:"retries" json:"retries"`
	MaxInputChars           int  `yaml:"max_input_chars" json:"maxInputChars"`
	MaxOutputTokens         int  `yaml:"max_output_tokens" json:"maxOutputTokens"`
	DisableThinking         bool `yaml:"disable_thinking" json:"disableThinking"`
}

type ModelHarnessConfig struct {
	ID           string               `yaml:"id" json:"id"`
	Provider     string               `yaml:"provider" json:"provider"`
	ModelIDs     []string             `yaml:"model_ids" json:"modelIds"`
	DefaultModel string               `yaml:"default_model" json:"defaultModel"`
	Runtime      HarnessRuntimeConfig `yaml:"runtime" json:"runtime"`
}

type HarnessRuntimeConfig struct {
	BinaryPath                 string   `yaml:"binary_path" json:"binaryPath,omitempty"`
	DetectModel                string   `yaml:"detect_model" json:"detectModel,omitempty"`
	TitleModel                 string   `yaml:"title_model" json:"titleModel,omitempty"`
	JudgeModel                 string   `yaml:"judge_model" json:"judgeModel,omitempty"`
	StartupTimeoutSeconds      int      `yaml:"startup_timeout_seconds" json:"startupTimeoutSeconds,omitempty"`
	TurnWallClockSeconds       int      `yaml:"turn_wall_clock_seconds" json:"turnWallClockSeconds,omitempty"`
	ExecTimeoutSeconds         int      `yaml:"exec_timeout_seconds" json:"execTimeoutSeconds,omitempty"`
	ExecTimeoutCeilingSeconds  int      `yaml:"exec_timeout_ceiling_seconds" json:"execTimeoutCeilingSeconds,omitempty"`
	BackgroundJobTTLSeconds    int      `yaml:"background_job_ttl_seconds" json:"backgroundJobTtlSeconds,omitempty"`
	BackgroundJobTTLMaxSeconds int      `yaml:"background_job_ttl_max_seconds" json:"backgroundJobTtlMaxSeconds,omitempty"`
	ScratchExec                bool     `yaml:"scratch_exec" json:"scratchExec"`
	OwnerAuthExec              bool     `yaml:"owner_auth_exec" json:"ownerAuthExec"`
	ReachExec                  bool     `yaml:"reach_exec" json:"reachExec"`
	ControlTools               bool     `yaml:"control_tools" json:"controlTools"`
	CaptureRequests            bool     `yaml:"capture_requests" json:"captureRequests"`
	SystemCacheSplit           bool     `yaml:"system_cache_split" json:"systemCacheSplit"`
	SoftModelCalls             int      `yaml:"soft_model_calls" json:"softModelCalls,omitempty"`
	MaxModelCalls              int      `yaml:"max_model_calls" json:"maxModelCalls,omitempty"`
	MaxToolCalls               int      `yaml:"max_tool_calls" json:"maxToolCalls,omitempty"`
	FallbackModels             []string `yaml:"fallback_models" json:"fallbackModels,omitempty"`
}

type ModelsConfig struct {
	DefaultHarness  string                `yaml:"default_harness" json:"defaultHarness"`
	LegacyBaseModel string                `yaml:"base_model" json:"-"`
	Request         ModelRequestConfig    `yaml:"request" json:"request"`
	Providers       []ModelProviderConfig `yaml:"providers" json:"providers"`
	Harnesses       []ModelHarnessConfig  `yaml:"harnesses" json:"harnesses"`
}

type SlackConfig struct {
	BotToken string `yaml:"bot_token" json:"-"`
	AppToken string `yaml:"app_token" json:"-"`
	TeamID   string `yaml:"team_id" json:"teamId,omitempty"`
	TeamName string `yaml:"team_name" json:"teamName,omitempty"`
}

type OAuthClientConfig struct {
	Provider          string   `yaml:"provider" json:"provider"`
	ClientID          string   `yaml:"client_id" json:"clientId"`
	ClientSecret      string   `yaml:"client_secret" json:"-"`
	Scopes            []string `yaml:"scopes" json:"scopes,omitempty"`
	RedirectAllowlist []string `yaml:"redirect_allowlist" json:"redirectAllowlist,omitempty"`
	ConsentMode       string   `yaml:"consent_mode" json:"consentMode,omitempty"`
	HostedDomain      string   `yaml:"hosted_domain" json:"hostedDomain,omitempty"`
}

type OAuthConfig struct {
	Clients []OAuthClientConfig `yaml:"clients" json:"clients"`
}

type WorkerPoolConfig struct {
	Concurrency              int `yaml:"concurrency" json:"concurrency"`
	LeaseTTLSeconds          int `yaml:"lease_ttl_seconds" json:"leaseTtlSeconds"`
	HeartbeatIntervalSeconds int `yaml:"heartbeat_interval_seconds" json:"heartbeatIntervalSeconds"`
	PollIntervalMillis       int `yaml:"poll_interval_millis" json:"pollIntervalMillis"`
	MaxClaimBackoffMillis    int `yaml:"max_claim_backoff_millis" json:"maxClaimBackoffMillis"`
}

type WorkersConfig struct {
	ReapIntervalSeconds int                         `yaml:"reap_interval_seconds" json:"reapIntervalSeconds"`
	Pools               map[string]WorkerPoolConfig `yaml:"pools" json:"pools"`
}

type legacyKnowledgeLLMConfig struct {
	BaseURL        string `yaml:"base_url"`
	APIKey         string `yaml:"api_key"`
	Model          string `yaml:"model"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

type Config struct {
	Server struct {
		HTTPAddr string `yaml:"http_addr"`
		GRPCAddr string `yaml:"grpc_addr"`
	} `yaml:"server"`
	Database struct {
		URL string `yaml:"url"`
	} `yaml:"database"`
	QM struct {
		OrgID                    string       `yaml:"org_id"`
		NodeCoreURL              string       `yaml:"node_core_url"`
		RouteMode                string       `yaml:"route_mode"`
		AgentWorkspaceRoot       string       `yaml:"agent_workspace_root"`
		SlackEnvironmentState    string       `yaml:"slack_environment_state"`
		SandboxDefaultBackend    string       `yaml:"sandbox_default_backend"`
		SandboxBackends          []string     `yaml:"sandbox_backends"`
		OAuthCatalogEnabled      bool         `yaml:"oauth_catalog_enabled"`
		OAuthConfiguredProviders []string     `yaml:"oauth_configured_providers"`
		Models                   ModelsConfig `yaml:"models"`
		Slack                    SlackConfig  `yaml:"slack"`
		OAuth                    OAuthConfig  `yaml:"oauth"`
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
	Workers   WorkersConfig `yaml:"workers"`
	Knowledge struct {
		RootDir                 string                   `yaml:"root_dir"`
		Worker                  bool                     `yaml:"worker"`
		AutoProcessProjectFiles bool                     `yaml:"auto_process_project_files"`
		ScanIntervalSeconds     int                      `yaml:"scan_interval_seconds"`
		LLM                     legacyKnowledgeLLMConfig `yaml:"llm"`
	} `yaml:"knowledge"`
	Auth struct {
		SourceSigningSecret      string   `yaml:"source_signing_secret"`
		CapabilitySecret         string   `yaml:"capability_secret"`
		PortalIdentitySecret     string   `yaml:"portal_identity_secret"`
		ConnectorSecretKey       string   `yaml:"connector_secret_key"`
		PreviousConnectorSecrets []string `yaml:"previous_connector_secret_keys"`
		GRPCInternalToken        string   `yaml:"grpc_internal_token"`
		ReplayWindowSeconds      int      `yaml:"replay_window_seconds"`
	} `yaml:"auth"`
}

func Default() Config {
	var c Config
	// Keep the QM Go backend off the legacy modelgate port. Managed
	// dev instances override this with their slot-specific port at bootstrap.
	c.Server.HTTPAddr = ":18083"
	c.Server.GRPCAddr = "127.0.0.1:9090"
	c.QM.OrgID = "local"
	c.QM.NodeCoreURL = "http://127.0.0.1:8081"
	c.QM.RouteMode = "proxy"
	c.QM.AgentWorkspaceRoot = "data/qm-workspaces"
	c.QM.SlackEnvironmentState = "unknown"
	c.Runner.LeaseTTLSeconds = 30
	c.Runner.MaxClaims = 8
	c.Workers = defaultWorkersConfig()
	c.Knowledge.RootDir = "data/knowledge"
	c.Knowledge.AutoProcessProjectFiles = true
	c.Knowledge.ScanIntervalSeconds = 30
	c.QM.Models.Request = defaultModelRequestConfig()
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
		var raw struct {
			Knowledge map[string]any `yaml:"knowledge"`
		}
		if err := yaml.Unmarshal(contents, &raw); err != nil {
			return Config{}, err
		}
		if _, obsolete := raw.Knowledge["query"]; obsolete {
			return Config{}, errors.New("knowledge.query is obsolete; QM Pi now controls project knowledge through the knowledge tool")
		}
		if err := yaml.Unmarshal(contents, &c); err != nil {
			return Config{}, err
		}
	}
	c.applyLegacyQMEnv(os.LookupEnv)
	c.migrateLegacyKnowledgeLLM()
	if c.Database.URL == "" {
		return Config{}, errors.New("database.url or DATABASE_URL is required")
	}
	if c.QM.OrgID == "" {
		return Config{}, errors.New("qm.org_id or ORG_ID is required")
	}
	if c.QM.RouteMode != "proxy" && c.QM.RouteMode != "shadow_read" && c.QM.RouteMode != "go" {
		return Config{}, errors.New("qm.route_mode must be proxy, shadow_read, or go")
	}
	if c.QM.SlackEnvironmentState != "unknown" || c.QM.OAuthCatalogEnabled || len(c.QM.OAuthConfiguredProviders) != 0 {
		return Config{}, errors.New("qm.slack_environment_state, qm.oauth_catalog_enabled, and qm.oauth_configured_providers are deprecated; use qm.slack and qm.oauth")
	}
	if err := validateModels(&c.QM.Models); err != nil {
		return Config{}, err
	}
	if err := validateSlack(c.QM.Slack); err != nil {
		return Config{}, err
	}
	if err := validateOAuth(c.QM.OAuth); err != nil {
		return Config{}, err
	}
	if err := normalizeWorkers(&c.Workers); err != nil {
		return Config{}, err
	}
	if key := c.Auth.ConnectorSecretKey; key != "" && len(key) < 32 {
		return Config{}, errors.New("auth.connector_secret_key must be at least 32 characters")
	}
	for i, key := range c.Auth.PreviousConnectorSecrets {
		if len(key) < 32 {
			return Config{}, fmt.Errorf("auth.previous_connector_secret_keys[%d] must be at least 32 characters", i)
		}
	}
	if (len(c.QM.Models.Providers) != 0 || c.QM.Slack.BotToken != "" || len(c.QM.OAuth.Clients) != 0) && strings.TrimSpace(c.Auth.GRPCInternalToken) == "" {
		return Config{}, errors.New("auth.grpc_internal_token is required when qm.models, qm.slack, or qm.oauth is configured")
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

func defaultModelRequestConfig() ModelRequestConfig {
	return ModelRequestConfig{
		TimeoutSeconds:  60,
		Retries:         2,
		MaxInputChars:   120000,
		MaxOutputTokens: 4096,
	}
}

func (c *Config) migrateLegacyKnowledgeLLM() {
	legacy := c.Knowledge.LLM
	c.Knowledge.LLM = legacyKnowledgeLLMConfig{}
	if legacy == (legacyKnowledgeLLMConfig{}) || modelsConfigured(c.QM.Models) {
		return
	}

	// A request block may already accompany the legacy provider settings. Keep
	// explicitly changed shared request limits authoritative in that case.
	requestConfigured := c.QM.Models.Request != defaultModelRequestConfig()
	if legacy.TimeoutSeconds > 0 && !requestConfigured {
		c.QM.Models.Request.TimeoutSeconds = legacy.TimeoutSeconds
		c.QM.Models.Request.OperationTimeoutSeconds = minimumModelOperationTimeout(c.QM.Models.Request)
	}

	baseURL := strings.TrimSpace(legacy.BaseURL)
	apiKey := strings.TrimSpace(legacy.APIKey)
	model := strings.TrimSpace(legacy.Model)
	if baseURL == "" && apiKey == "" && model == "" {
		return
	}
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}

	const providerID = "legacy-knowledge"
	c.QM.Models.DefaultHarness = "pi"
	c.QM.Models.Providers = []ModelProviderConfig{{
		ID:       providerID,
		Protocol: "openai",
		BaseURL:  baseURL,
		APIKey:   apiKey,
		Models:   []ModelDefinition{{ID: model, Name: model}},
	}}
	c.QM.Models.Harnesses = []ModelHarnessConfig{{
		ID:           "pi",
		Provider:     providerID,
		ModelIDs:     []string{model},
		DefaultModel: model,
	}}
}

func modelsConfigured(models ModelsConfig) bool {
	return strings.TrimSpace(models.DefaultHarness) != "" ||
		strings.TrimSpace(models.LegacyBaseModel) != "" ||
		len(models.Providers) != 0 || len(models.Harnesses) != 0
}

func minimumModelOperationTimeout(request ModelRequestConfig) int {
	if request.TimeoutSeconds <= 0 {
		return 0
	}
	return request.TimeoutSeconds*(request.Retries+1) + 5*request.Retries
}

var providerIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

func validateModels(models *ModelsConfig) error {
	if strings.TrimSpace(models.LegacyBaseModel) != "" {
		return errors.New("qm.models.base_model is no longer supported; configure default_harness and harnesses[].default_model")
	}
	if strings.TrimSpace(models.DefaultHarness) == "" && len(models.Providers) == 0 && len(models.Harnesses) == 0 {
		return nil
	}
	models.DefaultHarness = strings.TrimSpace(models.DefaultHarness)
	if models.DefaultHarness == "" {
		return errors.New("qm.models.default_harness is required when providers or harnesses are configured")
	}
	providerIDs, modelIDs, modelProviders := map[string]bool{}, map[string]bool{}, map[string]string{}
	for i := range models.Providers {
		provider := &models.Providers[i]
		provider.ID = strings.TrimSpace(provider.ID)
		provider.Protocol = strings.TrimSpace(provider.Protocol)
		provider.BaseURL = strings.TrimRight(strings.TrimSpace(provider.BaseURL), "/")
		if !providerIDPattern.MatchString(provider.ID) {
			return fmt.Errorf("qm.models.providers[%d].id must be a lowercase slug", i)
		}
		if providerIDs[provider.ID] {
			return fmt.Errorf("qm.models.providers contains duplicate provider %q", provider.ID)
		}
		providerIDs[provider.ID] = true
		if provider.Protocol != "openai" && provider.Protocol != "anthropic" && provider.Protocol != "mock" {
			return fmt.Errorf("qm.models.providers[%d].protocol must be openai, anthropic, or mock", i)
		}
		if provider.Protocol != "mock" && (provider.BaseURL == "" || strings.TrimSpace(provider.APIKey) == "") {
			return fmt.Errorf("qm.models.providers[%d] requires base_url and api_key", i)
		}
		if len(provider.Models) == 0 {
			return fmt.Errorf("qm.models.providers[%d] requires at least one model", i)
		}
		for j := range provider.Models {
			model := &provider.Models[j]
			model.ID = strings.TrimSpace(model.ID)
			model.Name = strings.TrimSpace(model.Name)
			if model.ID == "" {
				return fmt.Errorf("qm.models.providers[%d].models[%d].id is required", i, j)
			}
			if modelIDs[model.ID] {
				return fmt.Errorf("qm.models contains duplicate model id %q", model.ID)
			}
			modelIDs[model.ID] = true
			modelProviders[model.ID] = provider.ID
			if model.Name == "" {
				model.Name = model.ID
			}
			if model.ContextWindow < 0 || model.MaxTokens < 0 {
				return fmt.Errorf("qm.models.providers[%d].models[%d] token limits must be non-negative", i, j)
			}
		}
	}
	knownHarnesses := map[string]bool{"pi": true, "opencode": true, "codex": true, "claude": true, "mock": true}
	harnessIDs := map[string]bool{}
	for i := range models.Harnesses {
		harness := &models.Harnesses[i]
		harness.ID = strings.TrimSpace(harness.ID)
		harness.Provider = strings.TrimSpace(harness.Provider)
		harness.DefaultModel = strings.TrimSpace(harness.DefaultModel)
		harness.Runtime.BinaryPath = strings.TrimSpace(harness.Runtime.BinaryPath)
		harness.Runtime.DetectModel = strings.TrimSpace(harness.Runtime.DetectModel)
		harness.Runtime.TitleModel = strings.TrimSpace(harness.Runtime.TitleModel)
		harness.Runtime.JudgeModel = strings.TrimSpace(harness.Runtime.JudgeModel)
		if !knownHarnesses[harness.ID] {
			return fmt.Errorf("qm.models.harnesses[%d].id must be pi, opencode, codex, claude, or mock", i)
		}
		if harnessIDs[harness.ID] {
			return fmt.Errorf("qm.models.harnesses contains duplicate harness %q", harness.ID)
		}
		harnessIDs[harness.ID] = true
		if !providerIDs[harness.Provider] {
			return fmt.Errorf("qm.models.harnesses[%d].provider %q is not declared", i, harness.Provider)
		}
		provider, _ := models.Provider(harness.Provider)
		if (harness.ID == "codex" && provider.Protocol != "openai") ||
			(harness.ID == "claude" && provider.Protocol != "anthropic") ||
			(harness.ID == "mock" && provider.Protocol != "mock") ||
			(harness.ID != "mock" && provider.Protocol == "mock") {
			return fmt.Errorf("qm.models.harnesses[%d] harness %q is incompatible with provider protocol %q", i, harness.ID, provider.Protocol)
		}
		if len(harness.ModelIDs) == 0 {
			return fmt.Errorf("qm.models.harnesses[%d] requires at least one model_id", i)
		}
		seenModels := map[string]bool{}
		for j, rawID := range harness.ModelIDs {
			modelID := strings.TrimSpace(rawID)
			harness.ModelIDs[j] = modelID
			if modelID == "" || !modelIDs[modelID] {
				return fmt.Errorf("qm.models.harnesses[%d].model_ids[%d] %q is not declared", i, j, modelID)
			}
			if modelProviders[modelID] != harness.Provider {
				return fmt.Errorf("qm.models.harnesses[%d] model %q belongs to provider %q, not %q", i, modelID, modelProviders[modelID], harness.Provider)
			}
			if seenModels[modelID] {
				return fmt.Errorf("qm.models.harnesses[%d] contains duplicate model %q", i, modelID)
			}
			seenModels[modelID] = true
		}
		if !seenModels[harness.DefaultModel] {
			return fmt.Errorf("qm.models.harnesses[%d].default_model %q must be listed in model_ids", i, harness.DefaultModel)
		}
		if err := validateHarnessRuntime(i, *harness, seenModels); err != nil {
			return err
		}
	}
	if !harnessIDs[models.DefaultHarness] {
		return fmt.Errorf("qm.models.default_harness %q is not declared in harnesses", models.DefaultHarness)
	}
	if models.Request.TimeoutSeconds < 0 || models.Request.OperationTimeoutSeconds < 0 || models.Request.Retries < 0 || models.Request.MaxInputChars < 0 || models.Request.MaxOutputTokens < 0 {
		return errors.New("qm.models.request limits must be non-negative")
	}
	if models.Request.OperationTimeoutSeconds > 0 && models.Request.TimeoutSeconds > 0 {
		minimum := minimumModelOperationTimeout(models.Request)
		if models.Request.OperationTimeoutSeconds < minimum {
			return fmt.Errorf("qm.models.request.operation_timeout_seconds must be at least %d to allow %d retries", minimum, models.Request.Retries)
		}
	}
	return nil
}

func validateHarnessRuntime(index int, harness ModelHarnessConfig, models map[string]bool) error {
	runtime := harness.Runtime
	for field, value := range map[string]int{
		"startup_timeout_seconds":        runtime.StartupTimeoutSeconds,
		"turn_wall_clock_seconds":        runtime.TurnWallClockSeconds,
		"exec_timeout_seconds":           runtime.ExecTimeoutSeconds,
		"exec_timeout_ceiling_seconds":   runtime.ExecTimeoutCeilingSeconds,
		"background_job_ttl_seconds":     runtime.BackgroundJobTTLSeconds,
		"background_job_ttl_max_seconds": runtime.BackgroundJobTTLMaxSeconds,
		"soft_model_calls":               runtime.SoftModelCalls,
		"max_model_calls":                runtime.MaxModelCalls,
		"max_tool_calls":                 runtime.MaxToolCalls,
	} {
		if value < 0 {
			return fmt.Errorf("qm.models.harnesses[%d].runtime.%s must be non-negative", index, field)
		}
	}
	if runtime.SoftModelCalls > 0 && runtime.MaxModelCalls > 0 && runtime.SoftModelCalls > runtime.MaxModelCalls {
		return fmt.Errorf("qm.models.harnesses[%d].runtime.soft_model_calls must not exceed max_model_calls", index)
	}
	seenFallbacks := map[string]bool{}
	for _, model := range runtime.FallbackModels {
		model = strings.TrimSpace(model)
		if model == "" || !models[model] {
			return fmt.Errorf("qm.models.harnesses[%d].runtime.fallback_models contains unknown model %q", index, model)
		}
		if seenFallbacks[model] {
			return fmt.Errorf("qm.models.harnesses[%d].runtime.fallback_models contains duplicate model %q", index, model)
		}
		seenFallbacks[model] = true
	}
	if runtime.ExecTimeoutCeilingSeconds > 0 && runtime.ExecTimeoutSeconds > runtime.ExecTimeoutCeilingSeconds {
		return fmt.Errorf("qm.models.harnesses[%d].runtime.exec_timeout_ceiling_seconds must not be shorter than exec_timeout_seconds", index)
	}
	if runtime.BackgroundJobTTLMaxSeconds > 0 && runtime.BackgroundJobTTLSeconds > runtime.BackgroundJobTTLMaxSeconds {
		return fmt.Errorf("qm.models.harnesses[%d].runtime.background_job_ttl_max_seconds must not be shorter than background_job_ttl_seconds", index)
	}
	for field, model := range map[string]string{"detect_model": runtime.DetectModel, "title_model": runtime.TitleModel, "judge_model": runtime.JudgeModel} {
		if model != "" && !models[model] {
			return fmt.Errorf("qm.models.harnesses[%d].runtime.%s %q must be listed in model_ids", index, field, model)
		}
	}
	if harness.ID != "pi" && (runtime.DetectModel != "" || runtime.TitleModel != "" || runtime.CaptureRequests || runtime.SystemCacheSplit) {
		return fmt.Errorf("qm.models.harnesses[%d] detect_model, title_model, capture_requests, and system_cache_split are pi-only", index)
	}
	if harness.ID == "opencode" && runtime.JudgeModel != "" {
		return fmt.Errorf("qm.models.harnesses[%d].runtime.judge_model is not supported by opencode", index)
	}
	if (harness.ID == "pi" || harness.ID == "mock") && runtime.BinaryPath != "" {
		return fmt.Errorf("qm.models.harnesses[%d].runtime.binary_path is not supported by %s", index, harness.ID)
	}
	if harness.ID != "opencode" && harness.ID != "codex" && runtime.StartupTimeoutSeconds != 0 {
		return fmt.Errorf("qm.models.harnesses[%d].runtime.startup_timeout_seconds is not supported by %s", index, harness.ID)
	}
	return nil
}

func validateSlack(slack SlackConfig) error {
	bot, app := strings.TrimSpace(slack.BotToken), strings.TrimSpace(slack.AppToken)
	if (bot == "") != (app == "") {
		return errors.New("qm.slack.bot_token and qm.slack.app_token must be configured together")
	}
	if bot != "" && (!strings.HasPrefix(bot, "xoxb-") || !strings.HasPrefix(app, "xapp-")) {
		return errors.New("qm.slack tokens must use xoxb- and xapp- prefixes")
	}
	return nil
}

func validateOAuth(oauth OAuthConfig) error {
	known := map[string]bool{"google": true, "slack": true, "notion": true, "linear": true, "dropbox": true, "github": true, "x": true}
	seen := map[string]bool{}
	for i := range oauth.Clients {
		client := &oauth.Clients[i]
		client.Provider = strings.TrimSpace(client.Provider)
		if !known[client.Provider] {
			return fmt.Errorf("qm.oauth.clients[%d] contains unknown provider %q", i, client.Provider)
		}
		if seen[client.Provider] {
			return fmt.Errorf("qm.oauth.clients contains duplicate provider %q", client.Provider)
		}
		seen[client.Provider] = true
		if strings.TrimSpace(client.ClientID) == "" || strings.TrimSpace(client.ClientSecret) == "" {
			return fmt.Errorf("qm.oauth.clients[%d] requires client_id and client_secret", i)
		}
	}
	return nil
}

func (m ModelsConfig) ProviderForModel(modelID string) (ModelProviderConfig, bool) {
	for _, provider := range m.Providers {
		for _, model := range provider.Models {
			if model.ID == modelID {
				return provider, true
			}
		}
	}
	return ModelProviderConfig{}, false
}

func (m ModelsConfig) Model(modelID string) (ModelDefinition, bool) {
	for _, provider := range m.Providers {
		for _, model := range provider.Models {
			if model.ID == modelID {
				return model, true
			}
		}
	}
	return ModelDefinition{}, false
}

func (m ModelsConfig) Provider(providerID string) (ModelProviderConfig, bool) {
	for _, provider := range m.Providers {
		if provider.ID == providerID {
			return provider, true
		}
	}
	return ModelProviderConfig{}, false
}

func (m ModelsConfig) Harness(harnessID string) (ModelHarnessConfig, bool) {
	for _, harness := range m.Harnesses {
		if harness.ID == harnessID {
			return harness, true
		}
	}
	return ModelHarnessConfig{}, false
}

func (m ModelsConfig) DefaultModel() string {
	harness, ok := m.Harness(m.DefaultHarness)
	if !ok {
		return ""
	}
	return harness.DefaultModel
}

func validSandboxBackend(value string) bool {
	return value == "sprites" || value == "aws" || value == "local"
}

var workerPoolNames = []string{
	"turn",
	"slack_ingress",
	"slack_delivery",
	"oauth",
	"deploy",
	"sandbox",
	"skill",
	"maintenance",
}

func defaultWorkerPool(concurrency int) WorkerPoolConfig {
	return WorkerPoolConfig{
		Concurrency:              concurrency,
		LeaseTTLSeconds:          30,
		HeartbeatIntervalSeconds: 10,
		PollIntervalMillis:       250,
		MaxClaimBackoffMillis:    5000,
	}
}

func defaultWorkersConfig() WorkersConfig {
	return WorkersConfig{
		ReapIntervalSeconds: 10,
		Pools: map[string]WorkerPoolConfig{
			"turn":           defaultWorkerPool(8),
			"slack_ingress":  defaultWorkerPool(8),
			"slack_delivery": defaultWorkerPool(4),
			"oauth":          defaultWorkerPool(2),
			"deploy":         defaultWorkerPool(2),
			"sandbox":        defaultWorkerPool(4),
			"skill":          defaultWorkerPool(2),
			"maintenance":    defaultWorkerPool(1),
		},
	}
}

func normalizeWorkers(workers *WorkersConfig) error {
	defaults := defaultWorkersConfig()
	if workers.ReapIntervalSeconds == 0 {
		workers.ReapIntervalSeconds = defaults.ReapIntervalSeconds
	}
	if workers.ReapIntervalSeconds < 1 {
		return errors.New("workers.reap_interval_seconds must be positive")
	}
	if workers.Pools == nil {
		workers.Pools = map[string]WorkerPoolConfig{}
	}
	known := map[string]bool{}
	for _, name := range workerPoolNames {
		known[name] = true
		pool, found := workers.Pools[name]
		if !found {
			workers.Pools[name] = defaults.Pools[name]
			continue
		}
		fallback := defaults.Pools[name]
		if pool.Concurrency == 0 {
			pool.Concurrency = fallback.Concurrency
		}
		if pool.LeaseTTLSeconds == 0 {
			pool.LeaseTTLSeconds = fallback.LeaseTTLSeconds
		}
		if pool.HeartbeatIntervalSeconds == 0 {
			pool.HeartbeatIntervalSeconds = fallback.HeartbeatIntervalSeconds
		}
		if pool.PollIntervalMillis == 0 {
			pool.PollIntervalMillis = fallback.PollIntervalMillis
		}
		if pool.MaxClaimBackoffMillis == 0 {
			pool.MaxClaimBackoffMillis = fallback.MaxClaimBackoffMillis
		}
		workers.Pools[name] = pool
	}
	for name, pool := range workers.Pools {
		if !known[name] {
			return fmt.Errorf("workers.pools contains unknown pool %q", name)
		}
		if pool.Concurrency < 1 {
			return fmt.Errorf("workers.pools.%s.concurrency must be positive", name)
		}
		if pool.LeaseTTLSeconds < 2 {
			return fmt.Errorf("workers.pools.%s.lease_ttl_seconds must be at least 2", name)
		}
		if pool.HeartbeatIntervalSeconds < 1 || pool.HeartbeatIntervalSeconds >= pool.LeaseTTLSeconds {
			return fmt.Errorf("workers.pools.%s.heartbeat_interval_seconds must be positive and shorter than its lease ttl", name)
		}
		if pool.PollIntervalMillis < 10 {
			return fmt.Errorf("workers.pools.%s.poll_interval_millis must be at least 10", name)
		}
		if pool.MaxClaimBackoffMillis < pool.PollIntervalMillis {
			return fmt.Errorf("workers.pools.%s.max_claim_backoff_millis must be at least its poll interval", name)
		}
	}
	return nil
}

func (w WorkersConfig) Pool(name string) (WorkerPoolConfig, bool) {
	pool, ok := w.Pools[name]
	return pool, ok
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
