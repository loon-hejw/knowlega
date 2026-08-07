package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const DefaultPath = "config.yaml"

type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return fmt.Errorf("duration must be a string such as 30s or 3m")
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(node.Value))
	if err != nil {
		return err
	}
	if parsed <= 0 {
		return fmt.Errorf("duration must be positive")
	}
	d.Duration = parsed
	return nil
}

// NonNegativeDuration is used for optional deadlines where zero explicitly
// disables the deadline. Other configured durations continue to require a
// positive value through Duration.
type NonNegativeDuration struct {
	time.Duration
}

func (d *NonNegativeDuration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return fmt.Errorf("duration must be a string such as 0s, 30s, or 3m")
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(node.Value))
	if err != nil {
		return err
	}
	if parsed < 0 {
		return fmt.Errorf("duration must be non-negative")
	}
	d.Duration = parsed
	return nil
}

type Config struct {
	Project   ProjectConfig   `yaml:"project"`
	LLM       LLMConfig       `yaml:"llm"`
	Embedding EmbeddingConfig `yaml:"embedding"`
	Database  DatabaseConfig  `yaml:"database"`
	Server    ServerConfig    `yaml:"server"`
	Research  ResearchConfig  `yaml:"research"`
	Graph     GraphConfig     `yaml:"graph"`
	Query     QueryConfig     `yaml:"query"`

	Path string `yaml:"-"`
}

type ProjectConfig struct {
	Name      string          `yaml:"name"`
	Path      string          `yaml:"path"`
	Bootstrap BootstrapConfig `yaml:"bootstrap"`
}

type BootstrapConfig struct {
	Source               string   `yaml:"source"`
	Agent                string   `yaml:"agent"`
	ReuseExisting        bool     `yaml:"reuse_existing"`
	RetryInitialDelay    Duration `yaml:"retry_initial_delay"`
	RetryMaxDelay        Duration `yaml:"retry_max_delay"`
	Concurrency          int      `yaml:"concurrency"`
	MaxTaskAttempts      int      `yaml:"max_task_attempts"`
	MaxConflictAttempts  int      `yaml:"max_conflict_attempts"`
	MaxImpactAttempts    int      `yaml:"max_impact_attempts"`
	MaxFilesPerTask      int      `yaml:"max_files_per_task"`
	MaxNewPagesPerSource int      `yaml:"max_new_pages_per_source"`
}

type LLMConfig struct {
	Protocol         string   `yaml:"protocol"`
	BaseURL          string   `yaml:"base_url"`
	APIKey           string   `yaml:"api_key"`
	Model            string   `yaml:"model"`
	UserAgent        string   `yaml:"user_agent"`
	AnthropicVersion string   `yaml:"anthropic_version"`
	Timeout          Duration `yaml:"timeout"`
	OperationTimeout Duration `yaml:"operation_timeout"`
	Concurrency      int      `yaml:"concurrency"`
	Retries          int      `yaml:"retries"`
	RetryBaseDelay   Duration `yaml:"retry_base_delay"`
	RetryMaxDelay    Duration `yaml:"retry_max_delay"`
	MaxInputChars    int      `yaml:"max_input_chars"`
	MaxOutputTokens  int      `yaml:"max_output_tokens"`
	DisableThinking  bool     `yaml:"disable_thinking"`
}

type EmbeddingConfig struct {
	BaseURL       string   `yaml:"base_url"`
	APIKey        string   `yaml:"api_key"`
	Model         string   `yaml:"model"`
	Timeout       Duration `yaml:"timeout"`
	MaxInputChars int      `yaml:"max_input_chars"`
}

type DatabaseConfig struct {
	DSN       string `yaml:"dsn"`
	ProjectID string `yaml:"project_id"`
}

type ServerConfig struct {
	Addr            string     `yaml:"addr"`
	Agent           string     `yaml:"agent"`
	Worker          bool       `yaml:"worker"`
	ScanInterval    Duration   `yaml:"scan_interval"`
	APIToken        string     `yaml:"api_token"`
	APIRequireToken bool       `yaml:"api_require_token"`
	GRPC            GRPCConfig `yaml:"grpc"`
}

type GRPCConfig struct {
	Enabled         bool   `yaml:"enabled"`
	Addr            string `yaml:"addr"`
	AuthToken       string `yaml:"auth_token"`
	RequireAuth     bool   `yaml:"require_auth"`
	ScopeRoot       string `yaml:"scope_root"`
	TLSCertFile     string `yaml:"tls_cert_file"`
	TLSKeyFile      string `yaml:"tls_key_file"`
	TLSClientCAFile string `yaml:"tls_client_ca_file"`
}

type ResearchConfig struct {
	SearXNGURL string   `yaml:"searxng_url"`
	MaxResults int      `yaml:"max_results"`
	Timeout    Duration `yaml:"timeout"`
}

type QueryConfig struct {
	MaxSteps            int                 `yaml:"max_steps"`
	InitialActionBudget int                 `yaml:"initial_action_budget"`
	MaxActionBudget     int                 `yaml:"max_action_budget"`
	VerificationPasses  int                 `yaml:"verification_passes"`
	StagnationRounds    int                 `yaml:"stagnation_rounds"`
	TotalTimeout        NonNegativeDuration `yaml:"total_timeout"`
}

type GraphConfig struct {
	Enabled            bool                 `yaml:"enabled"`
	Worker             bool                 `yaml:"worker"`
	CheckoutRoot       string               `yaml:"checkout_root"`
	MaxParallelJobs    int                  `yaml:"max_parallel_jobs"`
	SemanticEnrichment bool                 `yaml:"semantic_enrichment"`
	Registries         []CodeRegistryConfig `yaml:"registries"`
}

type CodeRegistryConfig struct {
	ID               string                 `yaml:"id"`
	Provider         string                 `yaml:"provider"`
	BaseURL          string                 `yaml:"base_url"`
	APIToken         string                 `yaml:"api_token"`
	APITokenEnv      string                 `yaml:"api_token_env"`
	WebhookSecret    string                 `yaml:"webhook_secret"`
	WebhookSecretEnv string                 `yaml:"webhook_secret_env"`
	Directory        string                 `yaml:"directory"`
	Repositories     []CodeRepositoryConfig `yaml:"repositories"`
}

type CodeRepositoryConfig struct {
	ID       string `yaml:"id"`
	FullName string `yaml:"full_name"`
	CloneURL string `yaml:"clone_url"`
	Branch   string `yaml:"branch"`
	Disabled bool   `yaml:"disabled"`
}

func Defaults() Config {
	return Config{
		Project: ProjectConfig{Bootstrap: BootstrapConfig{
			Agent: "llm", ReuseExisting: true,
			RetryInitialDelay:    Duration{15 * time.Second},
			RetryMaxDelay:        Duration{5 * time.Minute},
			Concurrency:          4,
			MaxTaskAttempts:      4,
			MaxConflictAttempts:  8,
			MaxImpactAttempts:    2,
			MaxFilesPerTask:      12,
			MaxNewPagesPerSource: 3,
		}},
		LLM: LLMConfig{
			Protocol:         "openai",
			BaseURL:          "https://api.openai.com/v1",
			UserAgent:        "knowledge-core/0.1",
			AnthropicVersion: "2023-06-01",
			Timeout:          Duration{180 * time.Second},
			OperationTimeout: Duration{180 * time.Second},
			Retries:          2,
			RetryBaseDelay:   Duration{800 * time.Millisecond},
			RetryMaxDelay:    Duration{5 * time.Second},
			MaxInputChars:    120000,
			MaxOutputTokens:  4096,
			DisableThinking:  true,
		},
		Embedding: EmbeddingConfig{
			Timeout:       Duration{60 * time.Second},
			MaxInputChars: 6000,
		},
		Database: DatabaseConfig{ProjectID: "local"},
		Server: ServerConfig{
			Addr:         "127.0.0.1:19829",
			Agent:        "llm",
			ScanInterval: Duration{30 * time.Second},
			GRPC:         GRPCConfig{Addr: "127.0.0.1:19830"},
		},
		Research: ResearchConfig{
			MaxResults: 10,
			Timeout:    Duration{60 * time.Second},
		},
		Query: QueryConfig{
			MaxSteps:            256,
			InitialActionBudget: 8,
			MaxActionBudget:     32,
			VerificationPasses:  2,
			StagnationRounds:    2,
			TotalTimeout:        NonNegativeDuration{0},
		},
		Graph: GraphConfig{MaxParallelJobs: 2},
	}
}

func Load(path string) (Config, error) {
	if strings.TrimSpace(path) == "" {
		path = defaultConfigPath()
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return Config{}, fmt.Errorf("resolve config path: %w", err)
	}
	file, err := os.Open(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, fmt.Errorf("config file not found: %s", absPath)
		}
		return Config{}, fmt.Errorf("open config file %s: %w", absPath, err)
	}
	defer file.Close()

	cfg := Defaults()
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config %s: %w", absPath, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, fmt.Errorf("decode config %s: multiple YAML documents are not allowed", absPath)
		}
		return Config{}, fmt.Errorf("decode config %s: %w", absPath, err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return Config{}, fmt.Errorf("rewind config %s: %w", absPath, err)
	}
	var explicit struct {
		LLM struct {
			OperationTimeout *Duration `yaml:"operation_timeout"`
		} `yaml:"llm"`
		Query struct {
			MaxSteps        *int `yaml:"max_steps"`
			MaxActionBudget *int `yaml:"max_action_budget"`
		} `yaml:"query"`
	}
	if err := yaml.NewDecoder(file).Decode(&explicit); err != nil {
		return Config{}, fmt.Errorf("inspect config %s: %w", absPath, err)
	}
	if explicit.LLM.OperationTimeout == nil {
		cfg.LLM.OperationTimeout = cfg.LLM.Timeout
	}
	if explicit.Query.MaxSteps == nil && explicit.Query.MaxActionBudget != nil {
		cfg.Query.MaxSteps = *explicit.Query.MaxActionBudget
	}
	cfg.Path = absPath
	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate config %s: %w", absPath, err)
	}
	if err := cfg.ValidateProject(); err != nil {
		return Config{}, fmt.Errorf("validate config %s: %w", absPath, err)
	}
	return cfg, nil
}

func defaultConfigPath() string {
	dir, err := os.Getwd()
	if err != nil {
		return DefaultPath
	}
	for {
		if info, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil && !info.IsDir() {
			return filepath.Join(dir, DefaultPath)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Join(dir, DefaultPath)
		}
		dir = parent
	}
}

func (c *Config) normalize() {
	dir := filepath.Dir(c.Path)
	c.Project.Name = strings.TrimSpace(c.Project.Name)
	c.Project.Path = resolvePath(dir, c.Project.Path)
	c.Project.Bootstrap.Source = resolvePath(dir, c.Project.Bootstrap.Source)
	c.Project.Bootstrap.Agent = strings.TrimSpace(c.Project.Bootstrap.Agent)
	c.LLM.Protocol = strings.ToLower(strings.TrimSpace(c.LLM.Protocol))
	c.LLM.BaseURL = strings.TrimRight(strings.TrimSpace(c.LLM.BaseURL), "/")
	c.LLM.APIKey = strings.TrimSpace(c.LLM.APIKey)
	c.LLM.Model = strings.TrimSpace(c.LLM.Model)
	c.LLM.UserAgent = strings.TrimSpace(c.LLM.UserAgent)
	c.LLM.AnthropicVersion = strings.TrimSpace(c.LLM.AnthropicVersion)
	c.Embedding.BaseURL = strings.TrimRight(strings.TrimSpace(c.Embedding.BaseURL), "/")
	c.Embedding.APIKey = strings.TrimSpace(c.Embedding.APIKey)
	c.Embedding.Model = strings.TrimSpace(c.Embedding.Model)
	if c.Embedding.Model != "" {
		if c.Embedding.BaseURL == "" {
			c.Embedding.BaseURL = c.LLM.BaseURL
		}
		if c.Embedding.APIKey == "" {
			c.Embedding.APIKey = c.LLM.APIKey
		}
	}
	c.Database.DSN = strings.TrimSpace(c.Database.DSN)
	c.Database.ProjectID = strings.TrimSpace(c.Database.ProjectID)
	c.Server.Addr = strings.TrimSpace(c.Server.Addr)
	c.Server.Agent = strings.TrimSpace(c.Server.Agent)
	c.Server.APIToken = strings.TrimSpace(c.Server.APIToken)
	c.Server.GRPC.Addr = strings.TrimSpace(c.Server.GRPC.Addr)
	c.Server.GRPC.AuthToken = strings.TrimSpace(c.Server.GRPC.AuthToken)
	c.Server.GRPC.ScopeRoot = resolvePath(dir, c.Server.GRPC.ScopeRoot)
	c.Server.GRPC.TLSCertFile = resolvePath(dir, c.Server.GRPC.TLSCertFile)
	c.Server.GRPC.TLSKeyFile = resolvePath(dir, c.Server.GRPC.TLSKeyFile)
	c.Server.GRPC.TLSClientCAFile = resolvePath(dir, c.Server.GRPC.TLSClientCAFile)
	c.Research.SearXNGURL = strings.TrimRight(strings.TrimSpace(c.Research.SearXNGURL), "/")
	c.Graph.CheckoutRoot = strings.TrimSpace(c.Graph.CheckoutRoot)
	if c.Graph.CheckoutRoot == "" {
		c.Graph.CheckoutRoot = filepath.Join(c.Project.Path, ".kbcore", "repos")
	} else if !filepath.IsAbs(c.Graph.CheckoutRoot) {
		c.Graph.CheckoutRoot = filepath.Join(c.Project.Path, c.Graph.CheckoutRoot)
	}
	for registryIndex := range c.Graph.Registries {
		registry := &c.Graph.Registries[registryIndex]
		registry.ID = strings.TrimSpace(registry.ID)
		registry.Provider = strings.ToLower(strings.TrimSpace(registry.Provider))
		registry.BaseURL = strings.TrimRight(strings.TrimSpace(registry.BaseURL), "/")
		registry.APIToken = strings.TrimSpace(registry.APIToken)
		registry.APITokenEnv = strings.TrimSpace(registry.APITokenEnv)
		registry.WebhookSecret = strings.TrimSpace(registry.WebhookSecret)
		registry.WebhookSecretEnv = strings.TrimSpace(registry.WebhookSecretEnv)
		if registry.APIToken == "" && registry.APITokenEnv != "" {
			registry.APIToken = strings.TrimSpace(os.Getenv(registry.APITokenEnv))
		}
		if registry.WebhookSecret == "" && registry.WebhookSecretEnv != "" {
			registry.WebhookSecret = strings.TrimSpace(os.Getenv(registry.WebhookSecretEnv))
		}
		registry.Directory = strings.TrimSpace(registry.Directory)
		if registry.Directory == "" {
			registry.Directory = registry.ID
		}
		for repoIndex := range registry.Repositories {
			repo := &registry.Repositories[repoIndex]
			repo.ID = strings.TrimSpace(repo.ID)
			repo.FullName = strings.Trim(strings.TrimSpace(repo.FullName), "/")
			repo.CloneURL = strings.TrimSpace(repo.CloneURL)
			repo.Branch = strings.TrimSpace(repo.Branch)
			if repo.ID == "" {
				repo.ID = slug(repo.FullName)
			}
			if repo.Branch == "" {
				repo.Branch = "main"
			}
			if repo.CloneURL == "" && registry.BaseURL != "" && repo.FullName != "" {
				repo.CloneURL = registry.BaseURL + "/" + repo.FullName + ".git"
			}
		}
	}
}

func (c Config) Validate() error {
	if c.Project.Bootstrap.Agent != "" && c.Project.Bootstrap.Agent != "llm" {
		return fmt.Errorf("project.bootstrap.agent must be llm")
	}
	if c.Project.Bootstrap.RetryInitialDelay.Duration <= 0 || c.Project.Bootstrap.RetryMaxDelay.Duration <= 0 {
		return fmt.Errorf("project.bootstrap retry delays must be positive")
	}
	if c.Project.Bootstrap.RetryMaxDelay.Duration < c.Project.Bootstrap.RetryInitialDelay.Duration {
		return fmt.Errorf("project.bootstrap.retry_max_delay must be greater than or equal to retry_initial_delay")
	}
	if c.Project.Bootstrap.MaxTaskAttempts < 0 {
		return fmt.Errorf("project.bootstrap.max_task_attempts must be non-negative")
	}
	if c.Project.Bootstrap.MaxConflictAttempts <= 0 {
		return fmt.Errorf("project.bootstrap.max_conflict_attempts must be positive")
	}
	if c.Project.Bootstrap.MaxImpactAttempts < 0 {
		return fmt.Errorf("project.bootstrap.max_impact_attempts must be non-negative")
	}
	if c.Project.Bootstrap.MaxFilesPerTask <= 0 {
		return fmt.Errorf("project.bootstrap.max_files_per_task must be positive")
	}
	if c.Project.Bootstrap.MaxNewPagesPerSource < 0 {
		return fmt.Errorf("project.bootstrap.max_new_pages_per_source must be non-negative")
	}
	if c.Server.Agent != "llm" {
		return fmt.Errorf("server.agent must be llm")
	}
	if err := validateGraphConfig(c.Graph); err != nil {
		return err
	}
	if c.LLM.Protocol != "openai" && c.LLM.Protocol != "anthropic" {
		return fmt.Errorf("llm.protocol must be openai or anthropic")
	}
	if c.LLM.Protocol == "anthropic" && c.LLM.AnthropicVersion == "" {
		return fmt.Errorf("llm.anthropic_version is required when llm.protocol is anthropic")
	}
	if err := validateHTTPURL("llm.base_url", c.LLM.BaseURL, true); err != nil {
		return err
	}
	if c.LLM.Concurrency < 0 {
		return fmt.Errorf("llm.concurrency must be non-negative")
	}
	if c.LLM.Retries < 0 {
		return fmt.Errorf("llm.retries must be non-negative")
	}
	if c.LLM.MaxInputChars <= 0 {
		return fmt.Errorf("llm.max_input_chars must be positive")
	}
	if c.LLM.MaxOutputTokens <= 0 {
		return fmt.Errorf("llm.max_output_tokens must be positive")
	}
	if c.LLM.MaxOutputTokens > 131072 {
		return fmt.Errorf("llm.max_output_tokens must not exceed 131072")
	}
	if c.LLM.Timeout.Duration <= 0 || c.LLM.OperationTimeout.Duration <= 0 || c.LLM.RetryBaseDelay.Duration <= 0 || c.LLM.RetryMaxDelay.Duration <= 0 {
		return fmt.Errorf("llm durations must be positive")
	}
	if c.LLM.RetryMaxDelay.Duration < c.LLM.RetryBaseDelay.Duration {
		return fmt.Errorf("llm.retry_max_delay must be greater than or equal to llm.retry_base_delay")
	}
	if c.Embedding.Model != "" {
		if c.Embedding.APIKey == "" {
			return fmt.Errorf("embedding.api_key is required when embedding.model is configured")
		}
		if err := validateHTTPURL("embedding.base_url", c.Embedding.BaseURL, true); err != nil {
			return err
		}
	}
	if c.Embedding.Timeout.Duration <= 0 || c.Embedding.MaxInputChars <= 0 {
		return fmt.Errorf("embedding timeout and max_input_chars must be positive")
	}
	if c.Database.DSN != "" && c.Database.ProjectID == "" {
		return fmt.Errorf("database.project_id is required when database.dsn is configured")
	}
	if _, _, err := net.SplitHostPort(c.Server.Addr); err != nil {
		return fmt.Errorf("server.addr must be a host:port value: %w", err)
	}
	if c.Server.ScanInterval.Duration <= 0 {
		return fmt.Errorf("server.scan_interval must be positive")
	}
	if c.Server.GRPC.Enabled {
		if _, _, err := net.SplitHostPort(c.Server.GRPC.Addr); err != nil {
			return fmt.Errorf("server.grpc.addr must be a host:port value: %w", err)
		}
		if c.Server.GRPC.RequireAuth && c.Server.GRPC.AuthToken == "" {
			return fmt.Errorf("server.grpc.auth_token is required when server.grpc.require_auth is enabled")
		}
		if c.Server.GRPC.ScopeRoot == "" {
			return fmt.Errorf("server.grpc.scope_root is required when server.grpc.enabled is enabled")
		}
		if (c.Server.GRPC.TLSCertFile == "") != (c.Server.GRPC.TLSKeyFile == "") {
			return fmt.Errorf("server.grpc.tls_cert_file and tls_key_file must be configured together")
		}
		if c.Server.GRPC.TLSClientCAFile != "" && c.Server.GRPC.TLSCertFile == "" {
			return fmt.Errorf("server.grpc.tls_client_ca_file requires server.grpc.tls_cert_file and tls_key_file")
		}
	}
	if err := validateHTTPURL("research.searxng_url", c.Research.SearXNGURL, false); err != nil {
		return err
	}
	if c.Research.MaxResults <= 0 || c.Research.Timeout.Duration <= 0 {
		return fmt.Errorf("research max_results and timeout must be positive")
	}
	if c.Query.MaxSteps <= 0 {
		return fmt.Errorf("query.max_steps must be positive")
	}
	if c.Query.InitialActionBudget <= 0 || c.Query.MaxActionBudget < c.Query.InitialActionBudget {
		return fmt.Errorf("query action budgets must be positive and max_action_budget must be greater than or equal to initial_action_budget")
	}
	if c.Query.VerificationPasses < 0 || c.Query.VerificationPasses > 2 {
		return fmt.Errorf("query.verification_passes must be between 0 and 2")
	}
	if c.Query.StagnationRounds <= 0 || c.Query.TotalTimeout.Duration < 0 {
		return fmt.Errorf("query stagnation_rounds must be positive and total_timeout must be non-negative")
	}
	return nil
}

func validateGraphConfig(graph GraphConfig) error {
	if graph.MaxParallelJobs <= 0 {
		return fmt.Errorf("graph.max_parallel_jobs must be positive")
	}
	if !graph.Enabled && len(graph.Registries) == 0 {
		return nil
	}
	seenRegistries := map[string]bool{}
	for _, registry := range graph.Registries {
		if registry.ID == "" {
			return fmt.Errorf("graph registry id is required")
		}
		if !safePathSegment(registry.ID) {
			return fmt.Errorf("graph registry id %q must be a safe path segment", registry.ID)
		}
		if seenRegistries[registry.ID] {
			return fmt.Errorf("duplicate graph registry id %q", registry.ID)
		}
		seenRegistries[registry.ID] = true
		switch registry.Provider {
		case "github", "gitlab", "gitea":
		default:
			return fmt.Errorf("graph registry %s provider must be github, gitlab, or gitea", registry.ID)
		}
		if err := validateHTTPURL("graph.registries."+registry.ID+".base_url", registry.BaseURL, true); err != nil {
			return err
		}
		if !safeRelativePath(registry.Directory) {
			return fmt.Errorf("graph registry %s directory must be a safe relative path", registry.ID)
		}
		if graph.Enabled && registry.WebhookSecret == "" {
			return fmt.Errorf("graph registry %s webhook secret is required when graph is enabled", registry.ID)
		}
		if len(registry.Repositories) == 0 {
			return fmt.Errorf("graph registry %s must explicitly allow at least one repository", registry.ID)
		}
		seenRepos := map[string]bool{}
		for _, repo := range registry.Repositories {
			if repo.ID == "" || repo.FullName == "" {
				return fmt.Errorf("graph registry %s repository id and full_name are required", registry.ID)
			}
			if !safePathSegment(repo.ID) || !safeRelativePath(repo.FullName) {
				return fmt.Errorf("graph registry %s repository %q has an unsafe id or full_name", registry.ID, repo.ID)
			}
			if seenRepos[repo.ID] {
				return fmt.Errorf("graph registry %s has duplicate repository id %q", registry.ID, repo.ID)
			}
			seenRepos[repo.ID] = true
			if err := validateHTTPURL("graph repository "+registry.ID+"/"+repo.ID+" clone_url", repo.CloneURL, true); err != nil {
				return err
			}
			base, _ := url.Parse(registry.BaseURL)
			clone, _ := url.Parse(repo.CloneURL)
			if base.User != nil || clone.User != nil {
				return fmt.Errorf("graph registry and clone URLs must not contain credentials")
			}
			if !strings.EqualFold(base.Hostname(), clone.Hostname()) {
				return fmt.Errorf("graph repository %s/%s clone_url host must match registry base_url", registry.ID, repo.ID)
			}
			clonePath := strings.TrimSuffix(strings.Trim(clone.Path, "/"), ".git")
			if strings.ToLower(clonePath) != strings.ToLower(repo.FullName) && !strings.HasSuffix(strings.ToLower(clonePath), "/"+strings.ToLower(repo.FullName)) {
				return fmt.Errorf("graph repository %s/%s clone_url must end with full_name", registry.ID, repo.ID)
			}
		}
	}
	return nil
}

func safePathSegment(value string) bool {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, `/\\`) {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func safeRelativePath(value string) bool {
	if value == "" || filepath.IsAbs(value) {
		return false
	}
	value = strings.ReplaceAll(value, "\\", "/")
	for _, segment := range strings.Split(value, "/") {
		if !safePathSegment(segment) {
			return false
		}
	}
	return true
}

func slug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func (c Config) ValidateProject() error {
	if c.Project.Name == "" {
		return fmt.Errorf("project.name is required")
	}
	if c.Project.Path == "" {
		return fmt.Errorf("project.path is required")
	}
	if c.Project.Bootstrap.Source == "" {
		return fmt.Errorf("project.bootstrap.source is required")
	}
	info, err := os.Stat(c.Project.Bootstrap.Source)
	if err != nil {
		return fmt.Errorf("project.bootstrap.source is not readable: %w", err)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return fmt.Errorf("project.bootstrap.source must be a regular file or directory")
	}
	return nil
}

func (c Config) ValidateLLM() error {
	if c.LLM.APIKey == "" {
		return fmt.Errorf("llm.api_key is required")
	}
	if c.LLM.Model == "" {
		return fmt.Errorf("llm.model is required")
	}
	return nil
}

func (c Config) ValidateServe() error {
	if err := c.ValidateProject(); err != nil {
		return err
	}
	if c.Project.Bootstrap.Agent != "llm" {
		return fmt.Errorf("project.bootstrap.agent must be llm")
	}
	if c.Project.Bootstrap.Concurrency < 1 {
		return fmt.Errorf("project.bootstrap.concurrency must be at least 1")
	}
	return c.ValidateLLM()
}

func resolvePath(baseDir, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(baseDir, value)
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return filepath.Clean(value)
	}
	return filepath.Clean(abs)
}

func validateHTTPURL(field, value string, required bool) error {
	if value == "" {
		if required {
			return fmt.Errorf("%s is required", field)
		}
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%s must be an absolute http(s) URL", field)
	}
	return nil
}
