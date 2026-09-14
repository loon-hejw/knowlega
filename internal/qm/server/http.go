package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	khttp "github.com/go-kratos/kratos/v2/transport/http"
	knowlega "github.com/loon-hejw/knowlega/internal/agent/knowlega"
	agentservice "github.com/loon-hejw/knowlega/internal/agent/knowlega/service"
	"github.com/loon-hejw/knowlega/internal/qm/auth"
	"github.com/loon-hejw/knowlega/internal/qm/biz"
	"github.com/loon-hejw/knowlega/internal/qm/config"
	"github.com/loon-hejw/knowlega/internal/qm/data"
	"github.com/loon-hejw/knowlega/internal/qm/secretbox"
)

type HTTPServer struct {
	config            config.Config
	projects          *biz.ProjectUsecase
	projectRepo       *data.ProjectRepository
	knowledgeAgent    *knowlega.Agent
	directory         *data.DirectoryRepository
	acl               *data.ACLRepository
	environments      *data.EnvironmentRepository
	channelPolicy     *data.ChannelPolicyRepository
	surfaceCache      *data.SurfaceCacheRepository
	ambientJudgments  *data.AmbientJudgmentRepository
	ackEmojiPicks     *data.AckEmojiPickRepository
	egress            *data.EgressRepository
	errors            *data.ErrorEventRepository
	runs              *data.RunRepository
	runtimeTasks      *data.RuntimeTaskRepository
	agentApprovals    *data.AgentApprovalRepository
	deliveries        *data.DeliveryRepository
	sessions          *data.SessionRepository
	replay            *data.ReplayRepository
	metrics           *data.MetricsRepository
	retention         *data.RetentionRepository
	crons             *data.CronRepository
	files             *data.FileArtifactRepository
	projectFiles      *data.ProjectFileMembershipRepository
	knowledgeScopes   *data.KnowledgeScopeRepository
	deployments       *data.DeploymentRepository
	memory            *data.MemoryRepository
	skills            *data.SkillRepository
	souls             *data.SoulRepository
	userConfig        *data.UserConfigRepository
	contextRequests   *data.ContextRequestRepository
	connectorSecrets  *secretbox.Box
	keychain          *data.KeychainStatusRepository
	customProviders   *data.CustomProviderRepository
	slackInstallation *data.SlackInstallationRepository
	sandboxRoutes     *data.SandboxRouteRepository
	runtimeConfig     *data.RuntimeConfigRepository
	secretDrops       *data.SecretDropRepository
	audit             *data.Auditor
	auth              auth.Verifier
	readiness         func(context.Context) error
	proxy             http.Handler
	logger            *slog.Logger
}

var (
	wakeFireKeyPattern       = regexp.MustCompile(`^([a-z][a-z0-9_-]*):([^:]+)(?::(.+))?$`)
	conversationColorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	onboardingMarkerPattern  = regexp.MustCompile(`(?im)^[ \t]*[-*]?[ \t]*(?:\(\d{4}-\d\d-\d\d\)[ \t]*)?Onboarding:[ \t]*(?:completed|dismissed|pending)[ \t]+v2\b.*$`)
	trailingLineSpacePattern = regexp.MustCompile(`(?m)[ \t]+$`)
	threeNewlinesPattern     = regexp.MustCompile(`\n{3,}`)
	blobTransferIDPattern    = regexp.MustCompile(`^[0-9a-f]{32}$`)
	sha256HexPattern         = regexp.MustCompile(`^[0-9a-f]{64}$`)
	safeSkillNamePattern     = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_.-]{0,126}[A-Za-z0-9_-])?$`)
)

func NewHTTPServer(cfg config.Config, pg *data.Postgres, knowledgeAgent *knowlega.Agent, logger *slog.Logger) (*khttp.Server, error) {
	upstream, err := url.Parse(cfg.QM.NodeCoreURL)
	if err != nil {
		return nil, err
	}
	repo := data.NewProjectRepository(pg, cfg.QM.OrgID)
	var connectorSecrets *secretbox.Box
	if cfg.Auth.ConnectorSecretKey != "" {
		connectorSecrets, err = secretbox.New(cfg.Auth.ConnectorSecretKey, cfg.Auth.PreviousConnectorSecrets, "connector-secrets")
		if err != nil {
			return nil, err
		}
	}
	h := &HTTPServer{
		config:            cfg,
		projects:          biz.NewProjectUsecase(repo),
		projectRepo:       repo,
		knowledgeAgent:    knowledgeAgent,
		audit:             data.NewAuditor(pg),
		directory:         data.NewDirectoryRepository(pg, cfg.QM.OrgID),
		acl:               data.NewACLRepository(pg),
		environments:      data.NewEnvironmentRepository(pg, cfg.QM.OrgID),
		channelPolicy:     data.NewChannelPolicyRepository(pg, cfg.QM.OrgID),
		surfaceCache:      data.NewSurfaceCacheRepository(pg, cfg.QM.OrgID),
		ambientJudgments:  data.NewAmbientJudgmentRepository(pg, cfg.QM.OrgID),
		ackEmojiPicks:     data.NewAckEmojiPickRepository(pg, cfg.QM.OrgID),
		egress:            data.NewEgressRepository(pg),
		errors:            data.NewErrorEventRepository(pg),
		runs:              data.NewRunRepository(pg),
		runtimeTasks:      data.NewRuntimeTaskRepository(pg),
		agentApprovals:    data.NewAgentApprovalRepository(pg),
		deliveries:        data.NewDeliveryRepository(pg),
		sessions:          data.NewSessionRepository(pg),
		replay:            data.NewReplayRepository(pg),
		metrics:           data.NewMetricsRepository(pg),
		retention:         data.NewRetentionRepository(pg),
		crons:             data.NewCronRepository(pg),
		files:             data.NewFileArtifactRepository(pg),
		projectFiles:      data.NewProjectFileMembershipRepository(pg),
		knowledgeScopes:   data.NewKnowledgeScopeRepository(pg),
		deployments:       data.NewDeploymentRepository(pg),
		memory:            data.NewMemoryRepository(pg),
		skills:            data.NewSkillRepository(pg),
		souls:             data.NewSoulRepository(pg),
		userConfig:        data.NewUserConfigRepository(pg),
		contextRequests:   data.NewContextRequestRepository(pg),
		connectorSecrets:  connectorSecrets,
		keychain:          data.NewKeychainStatusRepository(pg),
		customProviders:   data.NewCustomProviderRepository(pg),
		slackInstallation: data.NewSlackInstallationRepository(pg),
		sandboxRoutes:     data.NewSandboxRouteRepository(pg),
		runtimeConfig:     data.NewRuntimeConfigRepository(pg),
		auth:              auth.Verifier{SourceSecret: cfg.Auth.SourceSigningSecret, CapabilitySecret: cfg.Auth.CapabilitySecret, ReplayWindow: time.Duration(cfg.Auth.ReplayWindowSeconds) * time.Second, DB: pg.Pool},
		secretDrops:       data.NewSecretDropRepository(pg),
		readiness: func(ctx context.Context) error {
			probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			return pg.Pool.Ping(probeCtx)
		},
		proxy:  httputil.NewSingleHostReverseProxy(upstream),
		logger: logger,
	}
	// The browser-facing runtime exposes long-lived SSE streams (notably
	// /v1/session-state/events). Kratos defaults to a one-second request
	// context timeout, which silently tears those streams down while the
	// reverse proxy is copying the body. Route handlers and upstream clients
	// own their finite operation deadlines, so keep the transport context
	// alive for streaming requests.
	srv := khttp.NewServer(khttp.Address(cfg.Server.HTTPAddr), khttp.Timeout(0))
	srv.HandlePrefix("/", h)
	return srv, nil
}

func (h *HTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if r.URL.Path == "/readyz" {
		if h.readiness == nil || h.readiness(r.Context()) != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]bool{"ok": false})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	// Onboarding configuration is YAML-owned by the Go control plane. These
	// routes must never fall through to the legacy Node proxy, regardless of
	// the broader migration route mode.
	if h.yamlOnboardingRoute(r.Method, r.URL.Path) {
		h.serveOwned(w, r)
		return
	}
	if h.agentRuntimeRoute(r.Method, r.URL.Path) {
		h.serveOwned(w, r)
		return
	}
	// The Web UI forwards file reads with Node-compatible source authentication.
	// When both runtimes share the configured local store, Go must serve those
	// bytes even in proxy mode; the legacy Node docstore may point elsewhere.
	if h.localFileContentRoute(r) {
		h.serveOwned(w, r)
		return
	}
	// Project file knowledge state is Go-owned even while the remaining surface
	// runs in proxy mode. Node's FileListItem has no projectFile projection, so
	// proxying this read would hide queued/ready/failed state from the Web UI.
	if projectScopeResourcesRoute(r) {
		h.serveOwned(w, r)
		return
	}
	if r.Method == http.MethodGet && projectKnowledgeDocumentID(r.URL.Path) != "" {
		h.serveOwned(w, r)
		return
	}
	if h.config.QM.RouteMode == "shadow_read" && r.Method == http.MethodGet && r.URL.Path == "/v1/projects" && r.Header.Get(auth.CapabilityHeader) != "" {
		h.shadowProjects(w, r)
		return
	}
	// Raw Blob transfer must stay streaming: its Node-compatible source
	// signature covers the declared digest, not the request body. Route it
	// before serveOwned, which deliberately buffers ordinary JSON requests.
	if h.config.QM.RouteMode == "go" && h.localBlobRoute(r) {
		h.serveLocalBlob(w, r)
		return
	}
	if h.config.QM.RouteMode != "go" || !h.owns(r.Method, r.URL.Path) || h.proxyCronRoute(r) || h.proxyViewerDeploymentRoute(r) || h.proxyDeploymentMutationRoute(r) || h.proxyCapabilityMemoryRoute(r) || h.proxyScopeResourcesRoute(r) || h.proxyFileListRoute(r) || h.proxyFileContentRoute(r) || h.proxyFileUploadRoute(r) || h.proxyAdminFileRoute(r) || h.proxySlackInstallationRoute(r) || h.proxySandboxRoutes(r) || h.proxyOAuthCatalogRoute(r) || h.proxySurfaceContextPendingRoute(r) {
		h.proxy.ServeHTTP(w, r)
		return
	}
	h.serveOwned(w, r)
}

func (h *HTTPServer) agentRuntimeRoute(method, path string) bool {
	if len(h.config.QM.Models.Harnesses) == 0 {
		return false
	}
	if path == "/v1/turns" || path == "/v1/runs" {
		return method == http.MethodPost && path == "/v1/turns" || method == http.MethodGet && path == "/v1/runs"
	}
	if path == "/v1/approvals/pending" || approvalID(path) != "" {
		return method == http.MethodGet
	}
	return method == http.MethodGet && runRecordID(path) != "" || method == http.MethodPost && (runSignalID(path) != "" || runDeliveryStateID(path) != "" || turnMetricsRunID(path) != "")
}

func projectScopeResourcesRoute(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Path == "/v1/scope-resources" && strings.HasPrefix(strings.TrimSpace(r.URL.Query().Get("scope")), "group:web-project-")
}

func (h *HTTPServer) shadowProjects(w http.ResponseWriter, r *http.Request) {
	goResponse := httptest.NewRecorder()
	h.serveOwned(goResponse, r.Clone(r.Context()))
	nodeResponse := httptest.NewRecorder()
	h.proxy.ServeHTTP(nodeResponse, r)
	if goResponse.Code != nodeResponse.Code || !bytes.Equal(goResponse.Body.Bytes(), nodeResponse.Body.Bytes()) {
		h.logger.Warn("shadow response mismatch", "method", r.Method, "path", r.URL.Path, "go_status", goResponse.Code, "node_status", nodeResponse.Code, "go_bytes", goResponse.Body.Len(), "node_bytes", nodeResponse.Body.Len())
	}
	for key, values := range nodeResponse.Result().Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(nodeResponse.Code)
	_, _ = w.Write(nodeResponse.Body.Bytes())
}

func (h *HTTPServer) owns(method, path string) bool {
	if h.yamlOnboardingRoute(method, path) {
		return true
	}
	if path == "/v1/apis" {
		return method == http.MethodGet
	}
	if path == "/v1/share" {
		return method == http.MethodPost
	}
	if path == "/v1/keychain/grants" {
		return method == http.MethodGet || method == http.MethodPost
	}
	if keychainReadPath(path) {
		return method == http.MethodGet
	}
	if keychainCredentialDeleteID(path) != "" {
		return method == http.MethodDelete
	}
	if keychainGrantRevokeID(path) != "" {
		return method == http.MethodPost
	}
	if keychainAskDeclineID(path) != "" {
		return method == http.MethodPost
	}
	if path == "/v1/connectors/oauth/revoke" {
		return method == http.MethodPost
	}
	if path == "/v1/connectors/catalog" || path == "/v1/connectors/oauth/status" {
		return method == http.MethodGet
	}
	if path == "/v1/projects" {
		return method == http.MethodGet || method == http.MethodPost
	}
	if projectRenameID(path) != "" {
		return method == http.MethodPatch
	}
	if projectMemberCollectionID(path) != "" {
		return method == http.MethodPost
	}
	if _, memberID := projectMemberID(path); memberID != "" {
		return method == http.MethodDelete
	}
	if projectFileCollectionID(path) != "" {
		return method == http.MethodPost
	}
	if projectKnowledgeDocumentID(path) != "" {
		return method == http.MethodGet
	}
	if _, fileID := projectFileID(path); fileID != "" {
		return method == http.MethodDelete
	}
	if _, fileID := projectFileRetryID(path); fileID != "" {
		return method == http.MethodPost
	}
	if path == "/v1/runs" {
		return method == http.MethodGet
	}
	if path == "/v1/turns" {
		return method == http.MethodPost
	}
	if runRecordID(path) != "" {
		return method == http.MethodGet
	}
	if path == "/v1/deployments" {
		return method == http.MethodGet
	}
	if deploymentID(path) != "" {
		return method == http.MethodGet
	}
	if deploymentNameID(path) != "" || deploymentDisplayNameID(path) != "" {
		return method == http.MethodPost
	}
	if deploymentShareID(path) != "" {
		return method == http.MethodPost
	}
	if path == "/v1/approvals/pending" {
		return method == http.MethodGet
	}
	if approvalID(path) != "" {
		return method == http.MethodGet
	}
	if path == "/v1/session-cap" {
		return method == http.MethodPost
	}
	if path == "/v1/soul" {
		return method == http.MethodGet || method == http.MethodPost
	}
	if path == "/v1/memory" {
		return method == http.MethodGet || method == http.MethodPut
	}
	if path == "/v1/memory/history" {
		return method == http.MethodGet
	}
	if path == "/v1/memory/restore" {
		return method == http.MethodPost
	}
	if path == "/v1/runtime-config" {
		return method == http.MethodGet || method == http.MethodPut
	}
	if path == "/v1/surface-config" {
		return method == http.MethodGet
	}
	if path == "/v1/session-state/events" {
		return method == http.MethodGet
	}
	if path == "/v1/keychain/drops" {
		return method == http.MethodPost
	}
	if strings.HasPrefix(path, "/v1/keychain/drops/") && strings.HasSuffix(path, "/form") {
		return method == http.MethodGet
	}
	if strings.HasPrefix(path, "/v1/keychain/drops/") && !strings.Contains(path[len("/v1/keychain/drops/"):], "/") {
		return method == http.MethodPost
	}
	if path == "/v1/sessions" {
		return method == http.MethodGet
	}
	if path == "/v1/conversations" {
		return method == http.MethodGet
	}
	if sessionBackgroundID(path) != "" {
		return method == http.MethodGet
	}
	if sessionApprovalsID(path) != "" {
		return method == http.MethodGet
	}
	if path == "/v1/contexts" {
		return method == http.MethodGet
	}
	if path == "/v1/scope-resources" {
		return method == http.MethodGet
	}
	if path == "/v1/files" {
		return method == http.MethodGet
	}
	if fileContentID(path) != "" {
		return method == http.MethodGet
	}
	if path == "/v1/files/upload" {
		return method == http.MethodPost
	}
	if path == "/v1/blobs" {
		return method == http.MethodPost
	}
	if path == "/v1/skills" {
		return method == http.MethodGet
	}
	if skillID(path) != "" {
		return method == http.MethodGet || method == http.MethodDelete
	}
	if blobTransferID(path) != "" {
		return method == http.MethodGet
	}
	if path == "/v1/grants" || path == "/v1/grants/revoke" {
		return method == http.MethodPost
	}
	if conversationID(path) != "" {
		return method == http.MethodGet || method == http.MethodPost
	}
	if sessionEntryPath(path) || sessionID(path) != "" {
		return method == http.MethodGet || sessionID(path) != "" && method == http.MethodPost
	}
	if runDeliveryStateID(path) != "" {
		return method == http.MethodPost
	}
	if runSignalID(path) != "" {
		return method == http.MethodPost
	}
	if path == "/v1/deliveries/ack-by-key" {
		return method == http.MethodPost
	}
	if path == "/v1/deliveries" {
		return method == http.MethodGet
	}
	if deliveryAckID(path) != "" {
		return method == http.MethodPost
	}
	if path == "/v1/egress-audit" {
		return method == http.MethodPost
	}
	if path == "/v1/auth/broker/claim" {
		return method == http.MethodPost
	}
	if turnMetricsRunID(path) != "" {
		return method == http.MethodPost
	}
	if path == "/v1/directory" {
		return method == http.MethodPost
	}
	if path == "/v1/environments" {
		return method == http.MethodGet || method == http.MethodPost
	}
	if path == "/v1/environments/attach" {
		return method == http.MethodPost
	}
	if path == "/v1/surface-cache/policy" {
		return method == http.MethodGet || method == http.MethodPost
	}
	if path == "/v1/contexts/policy" {
		return method == http.MethodGet || method == http.MethodPut
	}
	if path == "/v1/surface-context" || path == "/v1/surface-file" {
		return method == http.MethodPost
	}
	if path == "/v1/surface-context/pending" {
		return method == http.MethodGet
	}
	if surfaceContextResultID(path) != "" {
		return method == http.MethodPost
	}
	if path == "/v1/directory/meta" {
		return method == http.MethodGet
	}
	if path == "/v1/directory/resolve" {
		return method == http.MethodGet
	}
	if path == "/v1/admin/grants" {
		return method == http.MethodPost
	}
	if path == "/v1/crons" {
		return method == http.MethodGet || method == http.MethodPost
	}
	if triggerConsentID(path) != "" {
		return method == http.MethodPost
	}
	if strings.HasPrefix(path, "/v1/crons/") {
		segments := strings.Split(strings.Trim(path, "/"), "/")
		return len(segments) == 3 && (method == http.MethodGet || method == http.MethodPatch || method == http.MethodDelete) || len(segments) == 4 && segments[3] == "disable" && method == http.MethodPost || len(segments) == 4 && segments[3] == "runs" && method == http.MethodGet || len(segments) == 4 && segments[3] == "destination" && method == http.MethodPost
	}
	if path == "/v1/admin/whoami" {
		return method == http.MethodGet
	}
	if path == "/v1/admin/scopes" {
		return method == http.MethodGet
	}
	if path == "/v1/admin/resources" {
		return method == http.MethodGet
	}
	if path == "/v1/admin/files" {
		return method == http.MethodGet
	}
	if path == "/v1/admin/files/read" || path == "/v1/admin/files/download" {
		return method == http.MethodGet
	}
	if path == "/v1/admin/files/upload" {
		return method == http.MethodPost
	}
	if path == "/v1/admin/deployments" {
		return method == http.MethodGet
	}
	if path == "/v1/admin/crons" {
		return method == http.MethodGet
	}
	if adminCronDestinationID(path) != "" {
		return method == http.MethodPut
	}
	if path == "/v1/admin/skills" {
		return method == http.MethodGet
	}
	if path == "/v1/admin/skill-packs" {
		return method == http.MethodGet
	}
	if adminSkillPackID(path) != "" {
		return method == http.MethodPatch || method == http.MethodDelete
	}
	if path == "/v1/admin/keychain" {
		return method == http.MethodGet
	}
	if path == "/v1/admin/custom-providers" {
		return method == http.MethodGet
	}
	if path == "/v1/admin/slack-installation" {
		return method == http.MethodGet
	}
	if path == "/v1/admin/sandbox-routes" {
		return method == http.MethodGet
	}
	if path == "/v1/admin/memory" {
		return method == http.MethodGet || method == http.MethodPut
	}
	if path == "/v1/admin/memory/scopes" {
		return method == http.MethodGet
	}
	if path == "/v1/admin/impersonate" || path == "/v1/admin/impersonate/stop" {
		return method == http.MethodPost
	}
	if path == "/v1/admin/directory" || path == "/v1/admin/slack-mirror" || path == "/v1/admin/slack-mirror/messages" || path == "/v1/admin/ambient-judgments" || path == "/v1/admin/ack-emoji-picks" {
		return method == http.MethodGet
	}
	if path == "/v1/admin/metrics" {
		return method == http.MethodGet
	}
	if path == "/v1/admin/sessions" {
		return method == http.MethodGet
	}
	if path == "/v1/admin/users" {
		return method == http.MethodGet
	}
	if adminUserOnboardingID(path) != "" {
		return method == http.MethodPut
	}
	if adminUserResetID(path) != "" {
		return method == http.MethodPost
	}
	if adminUserDetailID(path) != "" {
		return method == http.MethodGet
	}
	if adminSessionLLMID(path) != "" {
		return method == http.MethodGet
	}
	if adminSessionDetailID(path) != "" {
		return method == http.MethodGet
	}
	if adminSkillID(path) != "" {
		return method == http.MethodGet || method == http.MethodDelete
	}
	if path == "/v1/admin/audit" || path == "/v1/admin/egress" || path == "/v1/admin/errors" || path == "/v1/admin/runs" || path == "/v1/admin/retention" {
		return method == http.MethodGet
	}
	if path == "/v1/admin/deliveries/shadow" {
		return method == http.MethodGet
	}
	if strings.HasPrefix(path, "/v1/admin/grants/") {
		return method == http.MethodDelete
	}
	if strings.HasPrefix(path, "/v1/principals/") && (strings.HasSuffix(path, "/deactivate") || strings.HasSuffix(path, "/reactivate")) {
		return method == http.MethodPost
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 3 && parts[0] == "v1" && parts[1] == "projects" {
		return method == http.MethodPatch
	}
	return len(parts) == 4 && parts[0] == "v1" && parts[1] == "projects" && parts[3] == "members" && method == http.MethodPost ||
		len(parts) == 5 && parts[0] == "v1" && parts[1] == "projects" && parts[3] == "members" && method == http.MethodDelete
}

func (h *HTTPServer) proxyCronRoute(r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/v1/crons") {
		return false
	}
	// A capability pause is a durable state transition. Go writes the same
	// shared cron row and queues the edit notice for Node's delivery worker.
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/disable") && r.Header.Get(auth.CapabilityHeader) != "" {
		return false
	}
	if r.Method == http.MethodDelete && cronIDForMutation(r.URL.Path) != "" && r.Header.Get(auth.CapabilityHeader) != "" {
		return false
	}
	if r.Method == http.MethodPatch && cronIDForMutation(r.URL.Path) != "" && r.Header.Get(auth.CapabilityHeader) != "" {
		return false
	}
	if r.Method == http.MethodPost && cronDestinationID(r.URL.Path) != "" && r.Header.Get(auth.CapabilityHeader) != "" {
		return false
	}
	// The capability read projection is entirely durable state. Keep viewer and
	// principalId variants on Node: those are portal identities rather than the
	// caller identity carried by the signed capability.
	if r.Method == http.MethodGet && r.Header.Get(auth.CapabilityHeader) != "" && r.URL.Query().Get("viewer") == "" && r.URL.Query().Get("principalId") == "" {
		return false
	}
	return r.Header.Get(auth.CapabilityHeader) != "" || r.URL.Query().Get("viewer") != "" || r.URL.Query().Get("principalId") != ""
}

// Identified deployment reads include ACL-derived permissions and short-lived
// Git access URLs. Node remains authoritative for that live authorization
// path while Go serves the anonymous durable metadata projection.
func (h *HTTPServer) proxyViewerDeploymentRoute(r *http.Request) bool {
	if r.Method != http.MethodGet || (r.URL.Path != "/v1/deployments" && deploymentID(r.URL.Path) == "") {
		return false
	}
	return r.Header.Get(auth.CapabilityHeader) != "" || r.URL.Query().Get("principalId") != "" || r.Header.Get("x-as-principal") != ""
}

// Deployment name edits are safe durable-map mutations for source callers.
// Capability callers retain Node while Go lacks its complete team-scope
// identity projection; this mirrors the staged cron ownership boundary.
func (h *HTTPServer) proxyDeploymentMutationRoute(r *http.Request) bool {
	if r.Method != http.MethodPost || deploymentNameID(r.URL.Path) == "" && deploymentDisplayNameID(r.URL.Path) == "" {
		return false
	}
	return r.Header.Get(auth.CapabilityHeader) != ""
}

// The legacy /v1/memory self endpoint is source-only. Capability history and
// restore are served below from the shared revision table after selecting the
// claim's explicit personal or org write scope.
func (h *HTTPServer) proxyCapabilityMemoryRoute(r *http.Request) bool {
	return r.URL.Path == "/v1/memory" && r.Header.Get(auth.CapabilityHeader) != ""
}

// Team membership is still an identity-runtime concern in Node. Do not turn a
// missing team projection into a narrower result while progressively moving
// scope-resources to Go.
func (h *HTTPServer) proxyScopeResourcesRoute(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Path == "/v1/scope-resources" && strings.HasPrefix(r.URL.Query().Get("scope"), "team:")
}

// Portal identity and shared capability scopes still depend on Node's live
// identity evaluator. The Go path below is the durable metadata projection for
// capability callers whose scope authorization is already synchronized here.
func (h *HTTPServer) proxyFileListRoute(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Path == "/v1/files" && r.Header.Get(auth.CapabilityHeader) == "" && r.Header.Get("x-signature") == ""
}

func (h *HTTPServer) proxyFileListScope(identity auth.Identity, r *http.Request) bool {
	if r.Method != http.MethodGet || r.URL.Path != "/v1/files" {
		return false
	}
	return proxySharedFileCapabilityScope(identity)
}

func proxySharedFileCapabilityScope(identity auth.Identity) bool {
	kind, ref := splitScopeID(identity.ScopeID)
	return kind == "channel" || kind == "team" || kind == "group" && !strings.HasPrefix(ref, "web-project-")
}

// File byte reads can move to Go only when both processes share Node's local
// docstore. S3 and portal identity requests stay on Node until their storage
// and identity contracts are implemented here too.
func (h *HTTPServer) proxyFileContentRoute(r *http.Request) bool {
	if r.Method != http.MethodGet || fileContentID(r.URL.Path) == "" {
		return false
	}
	return h.config.QM.FileStore.Mode != "local" || strings.TrimSpace(h.config.QM.FileStore.LocalDir) == "" || r.Header.Get(auth.CapabilityHeader) == "" && r.Header.Get("x-signature") == ""
}

func (h *HTTPServer) localFileContentRoute(r *http.Request) bool {
	return r.Method == http.MethodGet && fileContentID(r.URL.Path) != "" &&
		h.config.QM.FileStore.Mode == "local" && strings.TrimSpace(h.config.QM.FileStore.LocalDir) != "" &&
		(r.Header.Get(auth.CapabilityHeader) != "" || r.Header.Get("x-signature") != "")
}

func (h *HTTPServer) proxyFileContentScope(identity auth.Identity, r *http.Request) bool {
	return r.Method == http.MethodGet && fileContentID(r.URL.Path) != "" && proxySharedFileCapabilityScope(identity)
}

// Node continues to stage raw Blob-transfer uploads, while Go can consume the
// staged local file and create the durable artifact when both local roots are
// explicitly shared. S3 staging remains Node-owned.
func (h *HTTPServer) proxyFileUploadRoute(r *http.Request) bool {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/files/upload" {
		return false
	}
	store := h.config.QM.FileStore
	return store.Mode != "local" || strings.TrimSpace(store.LocalDir) == "" || strings.TrimSpace(store.TransferLocalDir) == ""
}

func (h *HTTPServer) proxyFileUploadScope(r *http.Request, raw []byte) bool {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/files/upload" {
		return false
	}
	var input struct {
		ScopeID string `json:"scopeId"`
	}
	return json.Unmarshal(raw, &input) == nil && strings.HasPrefix(strings.TrimSpace(input.ScopeID), "team:")
}

// Admin file content can be served only when Go shares Node's configured local
// byte stores. Keep S3 (and an undeclared local topology) on Node so a Go
// cutover never changes the durable-byte source behind an artifact row.
func (h *HTTPServer) proxyAdminFileRoute(r *http.Request) bool {
	store := h.config.QM.FileStore
	switch {
	case r.Method == http.MethodGet && (r.URL.Path == "/v1/admin/files/read" || r.URL.Path == "/v1/admin/files/download"):
		return store.Mode != "local" || strings.TrimSpace(store.LocalDir) == ""
	case r.Method == http.MethodPost && r.URL.Path == "/v1/admin/files/upload":
		return store.Mode != "local" || strings.TrimSpace(store.LocalDir) == "" || strings.TrimSpace(store.TransferLocalDir) == ""
	default:
		return false
	}
}

// The Go implementation is intentionally limited to the operator-declared
// local transfer root. S3 and other Node transfer stores continue through the
// reverse proxy untouched.
func (h *HTTPServer) localBlobRoute(r *http.Request) bool {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		return false
	}
	if r.Method == http.MethodPost && r.URL.Path != "/v1/blobs" {
		return false
	}
	if r.Method == http.MethodGet && blobTransferID(r.URL.Path) == "" {
		return false
	}
	store := h.config.QM.FileStore
	return store.Mode == "local" && strings.TrimSpace(store.TransferLocalDir) != ""
}

// Slack environment credentials remain Node-owned. An operator must explicitly
// declare their state in config before Go owns the read endpoint; the default
// unknown value preserves the existing Node response without guessing.
func (h *HTTPServer) proxySlackInstallationRoute(r *http.Request) bool {
	return false
}

func (h *HTTPServer) proxySandboxRoutes(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Path == "/v1/admin/sandbox-routes" && h.config.QM.SandboxDefaultBackend == ""
}

func (h *HTTPServer) proxySurfaceContextPendingRoute(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Path == "/v1/surface-context/pending"
}

func (h *HTTPServer) proxyCronPatch(r *http.Request, body []byte) bool {
	if r.Method != http.MethodPatch || !strings.HasPrefix(r.URL.Path, "/v1/crons/") {
		return false
	}
	var patch map[string]json.RawMessage
	if json.Unmarshal(body, &patch) != nil {
		return false
	}
	// Calendar schedules rely on the established Node Croner implementation
	// for its complete five-field and IANA/DST semantics. Interval and one-shot
	// schedules are normalized below from durable state without that runtime.
	var schedule map[string]json.RawMessage
	return json.Unmarshal(patch["schedule"], &schedule) == nil && schedule["cron"] != nil
}

// Approving an ask resumes a waiting Node turn, so only the direct
// persistence-only grant branch belongs to Go at this stage.
func (h *HTTPServer) proxyKeychainGrant(r *http.Request, body []byte) bool {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/keychain/grants" {
		return false
	}
	var input map[string]json.RawMessage
	if json.Unmarshal(body, &input) != nil {
		return false
	}
	var ask string
	return json.Unmarshal(input["ask"], &ask) == nil
}

// Team mode, text delivery, and calendar schedule changes need capability
// claims or Croner semantics that remain Node-owned. The remaining owner-mode
// PATCH fields are durable cron state and are handled below.
func (h *HTTPServer) proxyCapabilityCronPatch(r *http.Request, body []byte) bool {
	if r.Method != http.MethodPatch || cronIDForMutation(r.URL.Path) == "" || r.Header.Get(auth.CapabilityHeader) == "" {
		return false
	}
	var input map[string]json.RawMessage
	if json.Unmarshal(body, &input) != nil {
		return false
	}
	if input["runAs"] != nil || input["text"] != nil {
		return true
	}
	var schedule map[string]json.RawMessage
	return json.Unmarshal(input["schedule"], &schedule) == nil && schedule["cron"] != nil
}

func (h *HTTPServer) serveOwned(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "payload_too_large"})
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if h.proxyCronPatch(r, body) {
		h.proxy.ServeHTTP(w, r)
		return
	}
	if h.proxyKeychainGrant(r, body) {
		h.proxy.ServeHTTP(w, r)
		return
	}
	if h.proxyArtifactShare(r, body) {
		h.proxy.ServeHTTP(w, r)
		return
	}
	if h.proxyCapabilityCronPatch(r, body) {
		h.proxy.ServeHTTP(w, r)
		return
	}
	routeAuth := "either"
	if r.URL.Path == "/v1/directory" || r.URL.Path == "/v1/directory/meta" || r.URL.Path == "/v1/runs" || r.URL.Path == "/v1/turns" || runRecordID(r.URL.Path) != "" || r.URL.Path == "/v1/approvals/pending" || approvalID(r.URL.Path) != "" || r.URL.Path == "/v1/contexts" || r.URL.Path == "/v1/scope-resources" || r.URL.Path == "/v1/skills" || r.URL.Path == "/v1/grants" || r.URL.Path == "/v1/grants/revoke" || r.URL.Path == "/v1/session-cap" || r.URL.Path == "/v1/memory" || r.URL.Path == "/v1/sessions" || sessionBackgroundID(r.URL.Path) != "" || sessionApprovalsID(r.URL.Path) != "" || sessionEntryPath(r.URL.Path) || sessionID(r.URL.Path) != "" || runDeliveryStateID(r.URL.Path) != "" || runSignalID(r.URL.Path) != "" || r.URL.Path == "/v1/deliveries" || r.URL.Path == "/v1/deliveries/ack-by-key" || deliveryAckID(r.URL.Path) != "" || r.URL.Path == "/v1/egress-audit" || r.URL.Path == "/v1/auth/broker/claim" || turnMetricsRunID(r.URL.Path) != "" || surfaceContextResultID(r.URL.Path) != "" || r.URL.Path == "/v1/surface-context/pending" || r.URL.Path == "/v1/surface-config" || r.URL.Path == "/v1/session-state/events" || r.URL.Path == "/v1/files/upload" || r.URL.Path == "/v1/connectors/catalog" || r.URL.Path == "/v1/connectors/oauth/status" || projectKnowledgeDocumentID(r.URL.Path) != "" || r.Method == http.MethodPost && r.URL.Path == "/v1/crons" || strings.HasPrefix(r.URL.Path, "/v1/principals/") || strings.HasPrefix(r.URL.Path, "/v1/surface-cache/") || strings.HasPrefix(r.URL.Path, "/v1/contexts/") || strings.HasPrefix(r.URL.Path, "/v1/keychain/drops/") {
		routeAuth = "source"
	}
	identity, err := h.auth.Authenticate(r.Context(), r, body, routeAuth)
	if err != nil {
		status := auth.HTTPStatus(err)
		errorCode := "unauthorized"
		if status == http.StatusForbidden {
			errorCode = "forbidden"
		}
		writeJSON(w, status, map[string]string{"error": errorCode, "message": err.Error()})
		return
	}
	if h.proxyFileListScope(identity, r) || h.proxyFileContentScope(identity, r) || h.proxyFileUploadScope(r, body) {
		h.proxy.ServeHTTP(w, r)
		return
	}
	if identity.ActorID != "" && !h.authorizeScope(r.Context(), identity) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "capability scope membership has been revoked"})
		return
	}
	if message := h.capabilityAdminDenied(r, identity); message != "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": message})
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/onboarding":
		h.adminOnboarding(w, r, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/model-providers":
		h.adminModelProviders(w, r, identity)
	case (r.Method == http.MethodPut || r.Method == http.MethodDelete) && strings.HasPrefix(r.URL.Path, "/v1/admin/model-providers/"):
		h.yamlManaged(w, r, identity, "qm.models")
	case (r.Method == http.MethodPut || r.Method == http.MethodDelete) && strings.HasPrefix(r.URL.Path, "/v1/admin/custom-providers/"):
		h.yamlManaged(w, r, identity, "qm.models.providers")
	case (r.Method == http.MethodPut || r.Method == http.MethodDelete) && r.URL.Path == "/v1/admin/slack-installation":
		h.yamlManaged(w, r, identity, "qm.slack")
	case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/base-model") && strings.HasPrefix(r.URL.Path, "/v1/admin/scopes/"):
		h.yamlManaged(w, r, identity, "qm.models.harnesses")
	case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/connectors") && strings.HasPrefix(r.URL.Path, "/v1/admin/scopes/"):
		h.yamlManaged(w, r, identity, "qm.oauth.clients")
	case r.Method == http.MethodGet && r.URL.Path == "/v1/apis":
		h.listAgentAPIs(w, r, identity)
	case r.Method == http.MethodGet && keychainReadPath(r.URL.Path):
		h.readKeychain(w, r, identity)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/keychain/grants":
		h.createKeychainGrant(w, r, body, identity)
	case r.Method == http.MethodDelete && keychainCredentialDeleteID(r.URL.Path) != "":
		h.deleteKeychainCredential(w, r, identity)
	case r.Method == http.MethodPost && keychainGrantRevokeID(r.URL.Path) != "":
		h.revokeKeychainGrant(w, r, identity)
	case r.Method == http.MethodPost && keychainAskDeclineID(r.URL.Path) != "":
		h.declineKeychainAsk(w, r, body, identity)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/connectors/oauth/revoke":
		h.revokeOAuthConnector(w, r, body, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/connectors/catalog":
		h.oauthCatalog(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/connectors/oauth/status":
		h.oauthStatus(w, r, identity)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/session-cap":
		h.sessionCapability(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/soul":
		h.getSoul(w, r, identity)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/soul":
		h.postSoul(w, r, body, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/memory":
		h.getMemory(w, r)
	case r.Method == http.MethodPut && r.URL.Path == "/v1/memory":
		h.putMemory(w, r, body)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/memory/history":
		h.memoryHistory(w, r, identity)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/memory/restore":
		h.restoreMemory(w, r, body, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/runtime-config":
		h.getRuntimeConfig(w, r, identity)
	case r.Method == http.MethodPut && r.URL.Path == "/v1/runtime-config":
		h.putRuntimeConfig(w, r, body, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/surface-config":
		h.getSurfaceConfig(w, r, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/session-state/events":
		h.sessionStateEvents(w, r, identity)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/keychain/drops":
		h.mintSecretDrop(w, r, body, identity)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/keychain/drops/") && strings.HasSuffix(r.URL.Path, "/form"):
		h.secretDropForm(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/keychain/drops/") && !strings.Contains(r.URL.Path[len("/v1/keychain/drops/"):], "/"):
		h.redeemSecretDrop(w, r, body)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/deployments":
		h.listDeployments(w, r)
	case r.Method == http.MethodGet && deploymentID(r.URL.Path) != "":
		h.getDeployment(w, r)
	case r.Method == http.MethodPost && deploymentNameID(r.URL.Path) != "":
		h.renameDeployment(w, r, body, identity)
	case r.Method == http.MethodPost && deploymentDisplayNameID(r.URL.Path) != "":
		h.setDeploymentDisplayName(w, r, body, identity)
	case r.Method == http.MethodPost && deploymentShareID(r.URL.Path) != "":
		h.shareDeployment(w, r, body, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/approvals/pending":
		h.pendingApprovalForThread(w, r)
	case r.Method == http.MethodGet && approvalID(r.URL.Path) != "":
		h.getApproval(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
		h.listSessions(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/conversations":
		h.listAgentConversations(w, r, identity)
	case r.Method == http.MethodGet && sessionBackgroundID(r.URL.Path) != "":
		h.sessionBackground(w, r)
	case r.Method == http.MethodGet && sessionApprovalsID(r.URL.Path) != "":
		h.sessionApprovals(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/projects":
		h.listProjects(w, r, identity)
	case r.Method == http.MethodGet && projectKnowledgeDocumentID(r.URL.Path) != "":
		h.openProjectKnowledgeDocument(w, r, identity)
	case r.Method == http.MethodPost && projectFileCollectionID(r.URL.Path) != "":
		h.attachProjectFile(w, r, body, identity)
	case r.Method == http.MethodDelete && func() bool { _, fileID := projectFileID(r.URL.Path); return fileID != "" }():
		h.removeProjectFile(w, r, body, identity)
	case r.Method == http.MethodPost && func() bool { _, fileID := projectFileRetryID(r.URL.Path); return fileID != "" }():
		h.retryProjectFile(w, r, body, identity)
	case r.Method == http.MethodGet && conversationID(r.URL.Path) != "":
		h.agentConversation(w, r, identity)
	case r.Method == http.MethodPost && conversationID(r.URL.Path) != "":
		h.patchAgentConversation(w, r, body, identity)
	case r.Method == http.MethodGet && sessionEntryPath(r.URL.Path):
		h.viewerSessionEntry(w, r)
	case r.Method == http.MethodGet && sessionID(r.URL.Path) != "":
		h.viewerSession(w, r)
	case r.Method == http.MethodPost && sessionID(r.URL.Path) != "":
		h.patchViewerSession(w, r, body)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/runs":
		h.activeRunForThread(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/turns":
		h.postAgentTurn(w, r, body, identity)
	case r.Method == http.MethodGet && runRecordID(r.URL.Path) != "":
		h.getAgentRun(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/contexts":
		h.listContexts(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/scope-resources":
		h.listScopeResources(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/skills":
		h.listSkills(w, r)
	case r.Method == http.MethodGet && skillID(r.URL.Path) != "":
		h.getSkillDetail(w, r, identity)
	case r.Method == http.MethodDelete && skillID(r.URL.Path) != "":
		h.archiveOwnedSkill(w, r, body, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/files":
		h.listFilesForCapability(w, r, identity)
	case r.Method == http.MethodGet && fileContentID(r.URL.Path) != "":
		h.openFileForCapability(w, r, identity)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/files/upload":
		h.uploadFileFromLocalTransfer(w, r, body)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/grants":
		h.createGrant(w, r, body)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/grants/revoke":
		h.revokeGrant(w, r, body)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/share":
		h.shareArtifact(w, r, body, identity)
	case r.Method == http.MethodPost && runDeliveryStateID(r.URL.Path) != "":
		h.setRunDeliveryState(w, r, body)
	case r.Method == http.MethodPost && runSignalID(r.URL.Path) != "":
		h.signalRun(w, r, body)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/deliveries/ack-by-key":
		h.ackDeliveryByKey(w, r, body)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/deliveries":
		h.pendingDeliveries(w, r)
	case r.Method == http.MethodPost && deliveryAckID(r.URL.Path) != "":
		h.ackDelivery(w, r, body)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/egress-audit":
		h.ingestEgressAudit(w, r, body)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/auth/broker/claim":
		h.claimBrokerNonce(w, r, body)
	case r.Method == http.MethodPost && turnMetricsRunID(r.URL.Path) != "":
		h.patchTurnMetrics(w, r, body)
	case r.Method == http.MethodPost && triggerConsentID(r.URL.Path) != "":
		h.decideTriggerConsent(w, r, body, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/crons":
		if identity.ActorID != "" {
			h.listCapabilityCrons(w, r, identity)
		} else {
			h.listCrons(w, r)
		}
	case r.Method == http.MethodPost && r.URL.Path == "/v1/crons":
		h.createSourceCron(w, r, body)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/crons/") && strings.HasSuffix(r.URL.Path, "/runs"):
		if identity.ActorID != "" {
			h.capabilityCronRuns(w, r, identity)
		} else {
			h.cronRuns(w, r)
		}
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/crons/"):
		if identity.ActorID != "" {
			h.getCapabilityCron(w, r, identity)
		} else {
			h.getCron(w, r)
		}
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/crons/"):
		if identity.ActorID != "" {
			h.patchCapabilityCron(w, r, body, identity)
		} else {
			h.patchCron(w, r, body)
		}
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/crons/"):
		if identity.ActorID != "" {
			h.deleteCapabilityCron(w, r, identity)
		} else {
			h.deleteCron(w, r)
		}
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/crons/") && strings.HasSuffix(r.URL.Path, "/disable"):
		if identity.ActorID != "" {
			h.disableCapabilityCron(w, r, identity)
		} else {
			h.disableCron(w, r)
		}
	case r.Method == http.MethodPost && cronDestinationID(r.URL.Path) != "":
		h.retargetCapabilityCron(w, r, body, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/environments":
		h.listEnvironments(w, r, identity)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/environments":
		h.createEnvironment(w, r, body, identity)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/environments/attach":
		h.attachEnvironment(w, r, body, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/surface-cache/policy":
		h.getSurfaceCachePolicy(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/surface-cache/policy":
		h.setSurfaceCachePolicy(w, r, body)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/contexts/policy":
		h.getContextPolicy(w, r)
	case r.Method == http.MethodPut && r.URL.Path == "/v1/contexts/policy":
		h.setContextPolicy(w, r, body)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/surface-context":
		h.createSurfaceContext(w, r, body, identity)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/surface-file":
		h.createSurfaceFile(w, r, body, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/surface-context/pending":
		h.pendingSurfaceContext(w, r)
	case r.Method == http.MethodPost && surfaceContextResultID(r.URL.Path) != "":
		h.fulfillSurfaceContext(w, r, body)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/directory":
		h.syncDirectory(w, r, body, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/directory/meta":
		h.directoryMeta(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/directory/resolve":
		h.resolveDirectory(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/admin/grants":
		h.createAdminGrant(w, r, body, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/whoami":
		h.adminWhoAmI(w, r, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/scopes":
		h.adminScopes(w, r, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/resources":
		h.adminResources(w, r, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/files":
		h.adminFiles(w, r, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/files/read":
		h.readAdminFile(w, r, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/files/download":
		h.downloadAdminFile(w, r, identity)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/admin/files/upload":
		h.uploadAdminFile(w, r, body, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/deployments":
		h.adminDeployments(w, r, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/crons":
		h.adminCrons(w, r, identity)
	case r.Method == http.MethodPut && adminCronDestinationID(r.URL.Path) != "":
		h.setAdminCronDestination(w, r, body, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/skills":
		h.adminSkills(w, r, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/skill-packs":
		h.adminSkillPacks(w, r, identity)
	case r.Method == http.MethodDelete && adminSkillPackID(r.URL.Path) != "":
		h.removeAdminSkillPack(w, r, identity)
	case r.Method == http.MethodPatch && adminSkillPackID(r.URL.Path) != "":
		h.patchAdminSkillPack(w, r, body, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/memory":
		h.adminMemory(w, r, identity)
	case r.Method == http.MethodPut && r.URL.Path == "/v1/admin/memory":
		h.putAdminMemory(w, r, body, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/memory/scopes":
		h.adminMemoryScopes(w, r, identity)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/admin/impersonate":
		h.startImpersonation(w, r, body, identity)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/admin/impersonate/stop":
		h.stopImpersonation(w, r, body, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/directory":
		h.adminDirectory(w, r, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/slack-mirror":
		h.adminSlackMirror(w, r, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/slack-mirror/messages":
		h.adminSlackMirrorMessages(w, r, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/audit":
		h.adminAudit(w, r, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/ambient-judgments":
		h.adminAmbientJudgments(w, r, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/ack-emoji-picks":
		h.adminAckEmojiPicks(w, r, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/egress":
		h.adminEgress(w, r, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/errors":
		h.adminErrors(w, r, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/runs":
		h.adminRuns(w, r, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/retention":
		h.adminRetention(w, r, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/metrics":
		h.adminMetrics(w, r, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/keychain":
		h.adminKeychain(w, r, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/custom-providers":
		h.adminCustomProviders(w, r, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/slack-installation":
		h.adminSlackInstallation(w, r, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/sandbox-routes":
		h.adminSandboxRoutes(w, r, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/sessions":
		h.adminSessions(w, r, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/users":
		h.adminUsers(w, r, identity)
	case r.Method == http.MethodPut && adminUserOnboardingID(r.URL.Path) != "":
		h.setAdminUserOnboarding(w, r, body, identity)
	case r.Method == http.MethodPost && adminUserResetID(r.URL.Path) != "":
		h.resetAdminUser(w, r, identity)
	case r.Method == http.MethodGet && adminUserDetailID(r.URL.Path) != "":
		h.adminUserDetail(w, r, identity)
	case r.Method == http.MethodGet && adminSessionLLMID(r.URL.Path) != "":
		h.adminSessionLLM(w, r, identity)
	case r.Method == http.MethodGet && adminSessionDetailID(r.URL.Path) != "":
		h.adminSessionDetail(w, r, identity)
	case r.Method == http.MethodGet && adminSkillID(r.URL.Path) != "":
		h.adminSkill(w, r, identity)
	case r.Method == http.MethodDelete && adminSkillID(r.URL.Path) != "":
		h.archiveAdminSkill(w, r, identity)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/deliveries/shadow":
		h.adminShadowDeliveries(w, r, identity)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/admin/grants/"):
		h.revokeAdminGrant(w, r, identity)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/deactivate"):
		h.setPrincipalActive(w, r, false)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reactivate"):
		h.setPrincipalActive(w, r, true)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/projects":
		h.createProject(w, r, body, identity)
	case r.Method == http.MethodPatch && projectRenameID(r.URL.Path) != "":
		h.renameProject(w, r, body, identity)
	case r.Method == http.MethodPost && projectMemberCollectionID(r.URL.Path) != "":
		h.addProjectMember(w, r, body, identity)
	case r.Method == http.MethodDelete && func() bool { _, memberID := projectMemberID(r.URL.Path); return memberID != "" }():
		h.removeProjectMember(w, r, body, identity)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	}
}

func keychainReadPath(path string) bool {
	return path == "/v1/keychain/credentials" || path == "/v1/keychain/overview" || path == "/v1/keychain/grants" || path == "/v1/keychain/asks"
}

func keychainCredentialDeleteID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "keychain" || parts[2] != "credentials" || strings.TrimSpace(parts[3]) == "" {
		return ""
	}
	return parts[3]
}

func keychainGrantRevokeID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 5 || parts[0] != "v1" || parts[1] != "keychain" || parts[2] != "grants" || strings.TrimSpace(parts[3]) == "" || parts[4] != "revoke" {
		return ""
	}
	return parts[3]
}

func keychainAskDeclineID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 5 || parts[0] != "v1" || parts[1] != "keychain" || parts[2] != "asks" || strings.TrimSpace(parts[3]) == "" || parts[4] != "decline" {
		return ""
	}
	return parts[3]
}

func cronIDForMutation(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 3 || parts[0] != "v1" || parts[1] != "crons" || strings.TrimSpace(parts[2]) == "" {
		return ""
	}
	return parts[2]
}

func cronDestinationID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "crons" || strings.TrimSpace(parts[2]) == "" || parts[3] != "destination" {
		return ""
	}
	return parts[2]
}

var oauthProviderHosts = map[string][]string{
	"google": {
		"gmail.googleapis.com",
		"www.googleapis.com",
		"sheets.googleapis.com",
		"docs.googleapis.com",
		"slides.googleapis.com",
	},
	"slack":   {"slack.com"},
	"notion":  {"api.notion.com"},
	"linear":  {"api.linear.app"},
	"dropbox": {"api.dropboxapi.com", "content.dropboxapi.com"},
	"github":  {"api.github.com"},
	"x":       {"api.x.com"},
}

// readKeychain owns the three side-effect-free agent reads. It uses the same
// metadata-only projection as the Go admin view; encryption, OAuth connector
// handling, grant mutation, notifications, and secret materialization remain
// on Node.
func (h *HTTPServer) readKeychain(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	if identity.ActorID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized", "message": "keychain routes require an agent capability token"})
		return
	}
	status, err := h.keychain.List(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	switch r.URL.Path {
	case "/v1/keychain/credentials":
		credentials := make([]map[string]any, 0)
		for _, credential := range status.Credentials {
			if samePrincipal(stringValue(credential["ownerId"]), identity.ActorID) {
				credentials = append(credentials, credential)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"credentials": credentials})
	case "/v1/keychain/overview":
		h.keychainOverview(w, r, identity, status)
	case "/v1/keychain/grants":
		grants, known := make([]map[string]any, 0), map[string]bool{}
		for _, grant := range status.Grants {
			if samePrincipal(stringValue(grant["ownerId"]), identity.ActorID) || stringValue(grant["audienceScopeId"]) == identity.ScopeID {
				id := stringValue(grant["id"])
				if !known[id] {
					grants, known[id] = append(grants, grant), true
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"grants": grants})
	case "/v1/keychain/asks":
		asks := make([]map[string]any, 0)
		for _, ask := range status.Asks {
			if samePrincipal(stringValue(ask["requesterId"]), identity.ActorID) || samePrincipal(stringValue(ask["ownerId"]), identity.ActorID) || stringValue(ask["requesterScopeId"]) == identity.ScopeID {
				asks = append(asks, ask)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"asks": asks})
	}
}

func (h *HTTPServer) deleteKeychainCredential(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	if identity.ActorID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized", "message": "keychain routes require an agent capability token"})
		return
	}
	id := keychainCredentialDeleteID(r.URL.Path)
	ok, err := h.keychain.DeleteOwnedCredential(r.Context(), identity.ActorID, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: identity.ActorID, Action: "keychain.delete", Resource: id, ScopeLabel: identity.ScopeID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *HTTPServer) revokeKeychainGrant(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	if identity.ActorID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized", "message": "keychain routes require an agent capability token"})
		return
	}
	id := keychainGrantRevokeID(r.URL.Path)
	ok, err := h.keychain.RevokeOwnedGrant(r.Context(), identity.ActorID, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: identity.ActorID, Action: "keychain.revoke", Resource: id, ScopeLabel: identity.ScopeID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *HTTPServer) declineKeychainAsk(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	if identity.ActorID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized", "message": "keychain routes require an agent capability token"})
		return
	}
	if identity.Triggered {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "consent can only be recorded on a turn its owner themself sent — this turn was fired by a trigger, not a person"})
		return
	}
	var input map[string]json.RawMessage
	_ = json.Unmarshal(raw, &input)
	var note string
	_ = json.Unmarshal(input["note"], &note)
	ask, err := h.keychain.DeclineAsk(r.Context(), identity.ActorID, keychainAskDeclineID(r.URL.Path), note)
	if err != nil {
		var keychainErr *data.KeychainGrantError
		if errors.As(err, &keychainErr) {
			writeJSON(w, keychainErr.Status, map[string]string{"error": "keychain", "message": keychainErr.Message})
			return
		}
		h.fail(w, err)
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: identity.ActorID, Action: "keychain.ask.decline", Resource: stringValue(ask["id"]), ScopeLabel: identity.ScopeID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ask": ask})
}

func (h *HTTPServer) createKeychainGrant(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	if identity.ActorID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized", "message": "keychain routes require an agent capability token"})
		return
	}
	if identity.Triggered {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "consent can only be recorded on a turn its owner themself sent — this turn was fired by a trigger, not a person"})
		return
	}
	var input struct {
		Credential json.RawMessage `json:"credential"`
		Ask        json.RawMessage `json:"ask"`
		Mode       json.RawMessage `json:"mode"`
		Purpose    json.RawMessage `json:"purpose"`
		ExpiresAt  json.RawMessage `json:"expiresAt"`
	}
	if json.Unmarshal(raw, &input) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "expected { credential | ask, mode: \"once\"|\"standing\", purpose }"})
		return
	}
	var credential, mode, purpose string
	if json.Unmarshal(input.Credential, &credential) != nil || json.Unmarshal(input.Mode, &mode) != nil || json.Unmarshal(input.Purpose, &purpose) != nil || (mode != "once" && mode != "standing") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "expected { credential | ask, mode: \"once\"|\"standing\", purpose }"})
		return
	}
	expiresAt, err := inboundKeychainExpiry(input.ExpiresAt)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": err.Error()})
		return
	}
	result, err := h.keychain.CreateDirectGrant(r.Context(), data.KeychainGrantInput{CredentialID: credential, OwnerID: identity.ActorID, AudienceScopeID: identity.ScopeID, Mode: mode, Purpose: purpose, ExpiresAt: expiresAt})
	if err != nil {
		var keychainErr *data.KeychainGrantError
		if errors.As(err, &keychainErr) {
			writeJSON(w, keychainErr.Status, map[string]string{"error": "keychain", "message": keychainErr.Message})
			return
		}
		h.fail(w, err)
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: identity.ActorID, Action: "keychain.grant." + mode, Resource: credential + "→" + identity.ScopeID, ScopeLabel: identity.ScopeID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	for _, ask := range result.AdoptedAsks {
		if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: identity.ActorID, Action: "keychain.ask.resolve", Resource: stringValue(ask["id"]) + " (grant " + stringValue(result.Grant["id"]) + ")", ScopeLabel: identity.ScopeID}); err != nil {
			h.fail(w, err)
			return
		}
	}
	grantID := stringValue(result.Grant["id"])
	writeJSON(w, http.StatusOK, map[string]any{"grant": result.Grant, "use": map[string]string{
		"command": `curl -fsS -X POST "$AGENT_API_URL/v1/keychain/use" -H "x-agent-capability: $AGENT_API_TOKEN" -H 'content-type: application/json' -d '{"grant":"` + grantID + `"}' -o /tmp/keychain.env && . /tmp/keychain.env`,
		"note":    "Run the task in that same shell. The secret never appears in output — do not cat the file.",
	}})
}

func inboundKeychainExpiry(raw json.RawMessage) (*int64, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	const message = "expiresAt must be an epoch timestamp in seconds or milliseconds, or an ISO date string"
	var value float64
	if err := json.Unmarshal(raw, &value); err == nil {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, errors.New(message)
		}
		if value < 1_000_000_000_000 {
			value *= 1000
		}
		value = math.Trunc(value)
		if value < float64(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()) || value > float64(time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()) {
			return nil, errors.New(message)
		}
		result := int64(value)
		return &result, nil
	}
	var text string
	if json.Unmarshal(raw, &text) != nil || strings.TrimSpace(text) == "" {
		return nil, errors.New(message)
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		parsed, err = time.Parse("2006-01-02", text)
	}
	if err != nil || parsed.UnixMilli() < time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli() || parsed.UnixMilli() > time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli() {
		return nil, errors.New(message)
	}
	result := parsed.UnixMilli()
	return &result, nil
}

// revokeOAuthConnector owns the local-persistence half of connector revocation.
// Token exchange, status refresh, consent, and provider configuration remain in
// Node; revocation itself only removes the known managed credential slots and
// their active grants from the shared durable maps.
func (h *HTTPServer) revokeOAuthConnector(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	var input struct {
		PrincipalID string `json:"principalId"`
		Provider    string `json:"provider"`
		Host        string `json:"host"`
	}
	if !decodeJSON(w, raw, &input) {
		return
	}
	principalID := input.PrincipalID
	if identity.ActorID != "" {
		principalID = principalKey(principalID)
		if principalID == "" {
			principalID = identity.ActorID
		}
		if !samePrincipal(principalID, identity.ActorID) {
			member := false
			for _, candidate := range identity.KeychainMembers {
				if samePrincipal(candidate.ID, principalID) {
					member = true
					break
				}
			}
			if !member {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "principalId must be a member of this conversation"})
				return
			}
		}
	}
	if principalID == "" || (input.Provider == "" && input.Host == "") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "principalId and provider or host required"})
		return
	}
	if input.Provider != "" {
		hosts, ok := oauthProviderHosts[input.Provider]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found", "message": "unknown OAuth provider: " + input.Provider})
			return
		}
		if err := h.keychain.DeleteConnectorTokens(r.Context(), principalID, hosts); err != nil {
			h.fail(w, err)
			return
		}
		if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: principalID, Action: "connector.oauth.revoked", Resource: input.Provider, ScopeLabel: principalID, IdempotencyKey: requestID(r)}); err != nil {
			h.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "principalId": principalID, "provider": input.Provider, "hosts": hosts})
		return
	}
	if err := h.keychain.DeleteConnectorTokens(r.Context(), principalID, []string{input.Host}); err != nil {
		h.fail(w, err)
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: principalID, Action: "connector.oauth.revoked", Resource: input.Host, ScopeLabel: principalID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "principalId": principalID, "host": input.Host})
}

func (h *HTTPServer) keychainOverview(w http.ResponseWriter, r *http.Request, identity auth.Identity, status data.KeychainStatus) {
	credentials := make([]map[string]any, 0)
	for _, credential := range status.Credentials {
		if samePrincipal(stringValue(credential["ownerId"]), identity.ActorID) {
			credentials = append(credentials, credential)
		}
	}
	connectors, err := h.keychain.ConnectorMetadata(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	connectorCredentials := make([]map[string]any, 0)
	for _, connector := range connectors {
		if samePrincipal(stringValue(connector["ownerId"]), identity.ActorID) {
			connectorCredentials = append(connectorCredentials, connector)
		}
	}
	grants := make([]map[string]any, 0)
	for _, grant := range status.Grants {
		if samePrincipal(stringValue(grant["ownerId"]), identity.ActorID) {
			grants = append(grants, grant)
		}
	}
	asks := make([]map[string]any, 0)
	for _, ask := range status.Asks {
		if samePrincipal(stringValue(ask["ownerId"]), identity.ActorID) && stringValue(ask["status"]) == "pending" {
			asks = append(asks, ask)
		}
	}
	usage := make([]map[string]any, 0)
	for _, credential := range credentials {
		service, id := stringValue(credential["service"]), stringValue(credential["id"])
		if service == "" || id == "" {
			continue
		}
		rows, err := h.egress.CredentialUsage(r.Context(), "keychain:"+service+":"+id, 20)
		if err != nil {
			h.fail(w, err)
			return
		}
		for _, row := range rows {
			item := map[string]any{"ts": row.TS, "slug": row.Slug, "host": row.Host, "status": row.Status, "scopeLabel": row.ScopeLabel, "principalId": row.PrincipalID, "credentialId": id}
			if row.UpstreamStatus != nil {
				item["upstreamStatus"] = *row.UpstreamStatus
			}
			usage = append(usage, item)
		}
	}
	sort.SliceStable(usage, func(i, j int) bool {
		left, _ := usage[i]["ts"].(int64)
		right, _ := usage[j]["ts"].(int64)
		return left > right
	})
	if len(usage) > 50 {
		usage = usage[:50]
	}
	scopeIDs := make([]string, 0, len(grants)+len(asks)+len(usage))
	for _, grant := range grants {
		scopeIDs = append(scopeIDs, stringValue(grant["audienceScopeId"]))
	}
	for _, ask := range asks {
		scopeIDs = append(scopeIDs, stringValue(ask["requesterScopeId"]))
	}
	for _, row := range usage {
		scopeIDs = append(scopeIDs, stringValue(row["scopeLabel"]))
	}
	labels, err := h.discoveredScopeLabels(r.Context(), "org:"+h.config.QM.OrgID, scopeIDs...)
	if err != nil {
		h.fail(w, err)
		return
	}
	scopeNames := map[string]string{}
	for _, scopeID := range scopeIDs {
		if label := labels[scopeID]; label != "" {
			scopeNames[scopeID] = label
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": credentials, "connectorCredentials": connectorCredentials, "grants": grants, "asks": asks, "usage": usage, "scopeNames": scopeNames})
}

func (h *HTTPServer) listAgentAPIs(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	if identity.ActorID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized", "message": "agent capability token required"})
		return
	}
	isAdmin, role := false, ""
	grants, err := h.acl.ListAdminGrants(r.Context(), identity.ActorID)
	if err != nil {
		h.logger.Error("load admin grants for API discovery", "error", err)
	} else {
		for _, grant := range grants {
			if grant.Role == "org_admin" && samePrincipal(grant.PrincipalID, identity.ActorID) {
				isAdmin, role = true, grant.Role
				break
			}
		}
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: identity.ActorID, Action: "apis.list", Resource: "apis", ScopeLabel: identity.ScopeID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, renderAgentAPIs(identity, isAdmin, role))
}

func (h *HTTPServer) capabilityAdminDenied(r *http.Request, identity auth.Identity) string {
	if identity.ActorID == "" || !strings.HasPrefix(r.URL.Path, "/v1/admin/") || r.URL.Path == "/v1/admin/whoami" {
		return ""
	}
	if !identity.LiveActor {
		return "admin actions through the agent require a turn the admin started themselves — autonomous turns (crons) cannot act as an admin"
	}
	if strings.HasPrefix(r.URL.Path, "/v1/admin/grants") {
		return "admin grant changes (promote/revoke) are portal-only — the agent cannot manage who governs the org"
	}
	if strings.HasPrefix(r.URL.Path, "/v1/admin/impersonate") {
		return "impersonating a user is portal-only — the agent cannot act as another person"
	}
	if r.Method == http.MethodGet && adminContentRead(r.URL.Path) && !strings.HasPrefix(identity.ScopeID, "personal:") {
		return "this admin read returns private content — ask the agent in a DM"
	}
	return ""
}

func adminContentRead(path string) bool {
	if path == "/v1/admin/runs" || path == "/v1/admin/audit" || path == "/v1/admin/errors" || path == "/v1/admin/egress" || path == "/v1/admin/metrics" || path == "/v1/admin/users" || strings.HasPrefix(path, "/v1/admin/users/") || strings.HasPrefix(path, "/v1/admin/files") || path == "/v1/admin/memory" || path == "/v1/admin/deployments" || path == "/v1/admin/crons" || path == "/v1/admin/keychain" || strings.HasPrefix(path, "/v1/admin/sessions") || strings.HasPrefix(path, "/v1/admin/skills") {
		return true
	}
	if path == "/v1/admin/deliveries/shadow" || strings.HasPrefix(path, "/v1/admin/slack-mirror") || strings.HasPrefix(path, "/v1/admin/ambient-judgments") || strings.HasPrefix(path, "/v1/admin/ack-emoji-picks") {
		return true
	}
	return false
}

func (h *HTTPServer) adminActor(ctx context.Context, r *http.Request, identity auth.Identity) (string, bool) {
	actor := h.requestActor(r, identity)
	if actor == "" {
		return "", false
	}
	grants, err := h.acl.ListAdminGrants(ctx, actor)
	if err != nil {
		h.logger.Error("load admin grants", "error", err)
		return "", false
	}
	for _, grant := range grants {
		if grant.Role == "org_admin" && grant.ScopeID == "org:"+h.config.QM.OrgID {
			return actor, true
		}
	}
	return "", false
}

func (h *HTTPServer) requestActor(r *http.Request, identity auth.Identity) string {
	if identity.ActorID != "" {
		return identity.ActorID
	}
	value := r.Header.Get("x-admin-actor")
	if at := strings.LastIndex(value, "@"); at > 0 && value[at+1:] == h.config.QM.OrgID {
		return value[:at]
	}
	return ""
}

func (h *HTTPServer) adminWhoAmI(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor := h.requestActor(r, identity)
	if actor == "" {
		writeJSON(w, http.StatusOK, map[string]any{"isAdmin": false, "permissions": []string{}})
		return
	}
	grants, err := h.acl.ListAdminGrants(r.Context(), actor)
	if err != nil {
		h.fail(w, err)
		return
	}
	response := map[string]any{"isAdmin": false, "permissions": []string{}}
	for _, grant := range grants {
		if grant.Role != "org_admin" {
			continue
		}
		response["isAdmin"] = true
		response["role"] = grant.Role
		response["scopeId"] = grant.ScopeID
		response["permissions"] = []string{"admin"}
		break
	}
	scope := actor
	if value, ok := response["scopeId"].(string); ok {
		scope = value
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "admin.whoami", Resource: "whoami", ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *HTTPServer) adminResources(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, ok := h.requireOrgAdmin(w, r, identity)
	if !ok {
		return
	}
	scope := "org:" + h.config.QM.OrgID
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "resources.read", Resource: "resources", ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"resources": adminResourceManifest()})
}

func (h *HTTPServer) startImpersonation(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	actor, ok := h.requireOrgAdmin(w, r, identity)
	if !ok {
		return
	}
	var input struct {
		Target string `json:"target"`
	}
	if !decodeJSON(w, raw, &input) {
		return
	}
	target := strings.TrimSpace(input.Target)
	if target == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "target principal required"})
		return
	}
	if target == actor {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "cannot impersonate yourself"})
		return
	}
	member, err := h.directory.Get(r.Context(), target)
	if err != nil {
		h.fail(w, err)
		return
	}
	scope := "org:" + h.config.QM.OrgID
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "impersonate.start", Resource: target, ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	displayName := target
	if member != nil {
		displayName = member.DisplayName
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "target": target, "displayName": displayName})
}

func (h *HTTPServer) stopImpersonation(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	actor, ok := h.requireOrgAdmin(w, r, identity)
	if !ok {
		return
	}
	var input struct {
		Target string `json:"target"`
	}
	if !decodeJSON(w, raw, &input) {
		return
	}
	target := strings.TrimSpace(input.Target)
	if target == "" {
		target = "-"
	}
	scope := "org:" + h.config.QM.OrgID
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "impersonate.stop", Resource: target, ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *HTTPServer) createAdminGrant(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	actor, ok := h.adminActor(r.Context(), r, identity)
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "admin grant required for this scope"})
		return
	}
	var input struct {
		PrincipalID string `json:"principalId"`
		Role        string `json:"role"`
		ScopeID     string `json:"scopeId"`
	}
	if !decodeJSON(w, raw, &input) {
		return
	}
	if strings.TrimSpace(input.PrincipalID) == "" || input.Role != "org_admin" || input.ScopeID != "org:"+h.config.QM.OrgID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "grant_failed", "message": "principalId and org_admin scope required"})
		return
	}
	grant := data.AdminGrant{PrincipalID: strings.TrimSpace(input.PrincipalID), Role: input.Role, ScopeID: input.ScopeID, GrantedBy: actor, CreatedAt: time.Now().UnixMilli()}
	if err := h.acl.PutAdminGrant(r.Context(), grant); err != nil {
		h.fail(w, err)
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "grant.create", Resource: grant.PrincipalID + "/" + grant.Role, ScopeLabel: grant.ScopeID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "grant": grant})
}

func (h *HTTPServer) revokeAdminGrant(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, ok := h.adminActor(r.Context(), r, identity)
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "admin grant required for this scope"})
		return
	}
	principalID := strings.TrimPrefix(r.URL.Path, "/v1/admin/grants/")
	scopeID, role := r.URL.Query().Get("scope"), r.URL.Query().Get("role")
	if principalID == "" || scopeID == "" || role != "org_admin" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "principalId (path), and scope + role=org_admin (query) required"})
		return
	}
	grants, err := h.acl.ListAdminGrants(r.Context(), "")
	if err != nil {
		h.fail(w, err)
		return
	}
	adminCount := 0
	for _, grant := range grants {
		if grant.Role == "org_admin" && grant.ScopeID == "org:"+h.config.QM.OrgID {
			adminCount++
		}
	}
	if adminCount <= 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "revoke_failed", "message": "cannot revoke the last org admin"})
		return
	}
	if err := h.acl.DeleteAdminGrant(r.Context(), principalID, scopeID, role); err != nil {
		h.fail(w, err)
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "grant.revoke", Resource: principalID + "/" + role, ScopeLabel: scopeID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *HTTPServer) requireOrgAdmin(w http.ResponseWriter, r *http.Request, identity auth.Identity) (string, bool) {
	actor, ok := h.adminActor(r.Context(), r, identity)
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "admin grant required for this scope"})
		return "", false
	}
	return actor, true
}

func (h *HTTPServer) adminDirectory(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	if _, ok := h.requireOrgAdmin(w, r, identity); !ok {
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeJSON(w, http.StatusOK, map[string]any{"members": []any{}})
		return
	}
	matches, err := h.directory.Resolve(r.Context(), query)
	if err != nil {
		h.fail(w, err)
		return
	}
	members := make([]map[string]string, 0, len(matches))
	for _, member := range matches {
		members = append(members, map[string]string{"principalId": member.PrincipalID, "displayName": member.DisplayName})
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": members})
}

func (h *HTTPServer) adminFiles(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, scope, ok := h.requireScopedAdmin(w, r, identity)
	if !ok {
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "files.read", Resource: "files", ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	files, err := h.files.List(r.Context(), scope, strings.HasPrefix(scope, "org:"), 2000)
	if err != nil {
		h.fail(w, err)
		return
	}
	views := make([]map[string]any, 0, len(files))
	for _, file := range files {
		views = append(views, map[string]any{"id": file.ID, "scopeId": file.OwnerScopeID, "name": file.Name, "path": file.Path, "mimetype": file.Mimetype, "size": file.SizeBytes, "direction": file.Direction, "createdAt": file.CreatedAt, "openable": file.BlobKey != nil})
	}
	writeJSON(w, http.StatusOK, map[string]any{"scopeId": scope, "files": views})
}

const adminFilePreviewMaxBytes int64 = 256 * 1024

// readAdminFile mirrors Node's bounded UTF-8 preview for a local artifact.
// It is intentionally distinct from a download: previewing an unavailable
// local byte key is a 404 even when the metadata row still exists.
func (h *HTTPServer) readAdminFile(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	file, actor, ok := h.authorizeAdminFile(w, r, identity)
	if !ok {
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "file.read", Resource: file.Path, ScopeLabel: file.OwnerScopeID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	content, _, err := data.OpenLocalFileBlob(h.config.QM.FileStore.LocalDir, *file.BlobKey)
	if err != nil {
		h.fail(w, err)
		return
	}
	if content == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	defer content.Close()
	preview, err := io.ReadAll(io.LimitReader(content, adminFilePreviewMaxBytes))
	if err != nil {
		h.fail(w, err)
		return
	}
	// Node marks a preview as truncated when it exactly reaches the limit.
	truncated := int64(len(preview)) >= adminFilePreviewMaxBytes
	writeJSON(w, http.StatusOK, map[string]any{"id": file.ID, "scopeId": file.OwnerScopeID, "path": file.Path, "name": file.Name, "mimetype": file.Mimetype, "content": string(preview), "truncated": truncated})
}

func (h *HTTPServer) downloadAdminFile(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	file, actor, ok := h.authorizeAdminFile(w, r, identity)
	if !ok {
		return
	}
	mimetype := strings.ToLower(strings.TrimSpace(strings.Split(file.Mimetype, ";")[0]))
	inline := adminInlineFileMimetype(mimetype)
	action := "file.download"
	if inline {
		action = "file.view"
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: action, Resource: file.Path, ScopeLabel: file.OwnerScopeID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	content, size, err := data.OpenLocalFileBlob(h.config.QM.FileStore.LocalDir, *file.BlobKey)
	if err != nil {
		h.fail(w, err)
		return
	}
	if content == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	defer content.Close()
	if mimetype == "" {
		mimetype = "application/octet-stream"
	}
	if !inline {
		mimetype = "application/octet-stream"
	}
	w.Header().Set("content-type", browserFileContentType(mimetype))
	w.Header().Set("content-length", strconv.FormatInt(size, 10))
	w.Header().Set("content-disposition", adminContentDisposition(file.Name, inline))
	w.Header().Set("x-content-type-options", "nosniff")
	if inline {
		w.Header().Set("cache-control", "private, max-age=300")
		w.Header().Set("content-security-policy", "sandbox")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, content)
}

func (h *HTTPServer) uploadAdminFile(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	actor, scope, ok := h.requireScopedAdmin(w, r, identity)
	if !ok {
		return
	}
	var input struct {
		BlobID   string `json:"blobId"`
		Name     string `json:"name"`
		Mimetype string `json:"mimetype"`
	}
	if !decodeJSON(w, raw, &input) {
		return
	}
	blobID := strings.TrimSpace(input.BlobID)
	if blobID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "blobId required"})
		return
	}
	staged, _, err := data.OpenLocalTransferBlob(h.config.QM.FileStore.TransferLocalDir, blobID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if staged == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found", "message": "staged blob not found"})
		return
	}
	defer staged.Close()
	defer func() { _ = data.DeleteLocalTransferBlob(h.config.QM.FileStore.TransferLocalDir, blobID) }()
	name := safeUploadFileName(input.Name)
	mimetype := strings.ToLower(strings.TrimSpace(strings.Split(input.Mimetype, ";")[0]))
	if mimetype == "" {
		mimetype = uploadFileMimetype(name)
	}
	blobKey, size, err := data.PutLocalFileBlob(h.config.QM.FileStore.LocalDir, staged, maxBlobTransferBytes)
	if errors.Is(err, data.ErrLocalFileTooLarge) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "payload_too_large", "message": err.Error()})
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	id, err := data.NewFileArtifactID()
	if err != nil {
		h.fail(w, err)
		return
	}
	createdAt := time.Now().UnixMilli()
	createdInScope := scope
	file, err := h.files.Create(r.Context(), data.FileArtifact{ID: id, OwnerScopeID: scope, CreatedBy: actor, Name: name, Path: "artifacts/" + id + "/" + name, Mimetype: mimetype, SizeBytes: size, BlobKey: &blobKey, Direction: "in", CreatedInScope: &createdInScope, CreatedAt: createdAt, UpdatedAt: createdAt})
	if err != nil {
		h.fail(w, err)
		return
	}
	_ = h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "file.upload", Resource: file.Path, ScopeLabel: scope, IdempotencyKey: requestID(r)})
	h.enqueueKnowledgeFile(r.Context(), *file, scope, actor)
	writeJSON(w, http.StatusOK, map[string]any{"file": adminFileView(*file)})
}

func (h *HTTPServer) authorizeAdminFile(w http.ResponseWriter, r *http.Request, identity auth.Identity) (*data.FileArtifact, string, bool) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "id required"})
		return nil, "", false
	}
	file, err := h.files.Get(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return nil, "", false
	}
	if file == nil || file.BlobKey == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return nil, "", false
	}
	actor, ok := h.requireOrgAdmin(w, r, identity)
	if !ok {
		return nil, "", false
	}
	return file, actor, true
}

func adminFileView(file data.FileArtifact) map[string]any {
	return map[string]any{"id": file.ID, "scopeId": file.OwnerScopeID, "name": file.Name, "path": file.Path, "mimetype": file.Mimetype, "size": file.SizeBytes, "direction": file.Direction, "createdAt": file.CreatedAt, "openable": file.BlobKey != nil}
}

func adminInlineFileMimetype(mimetype string) bool {
	return mimetype == "application/pdf" || mimetype == "text/plain" || mimetype == "image/png" || mimetype == "image/jpeg" || mimetype == "image/gif" || mimetype == "image/webp" || mimetype == "image/avif" || mimetype == "image/bmp"
}

func adminContentDisposition(name string, inline bool) string {
	kind := "attachment"
	if inline {
		kind = "inline"
	}
	fallback := strings.NewReplacer("\r", "_", "\n", "_", "\"", "_", "\\", "_").Replace(name)
	fallback = strings.Map(func(value rune) rune {
		if value < 0x20 || value > 0x7e {
			return '_'
		}
		return value
	}, fallback)
	if fallback == "" {
		fallback = "download"
	}
	return kind + `; filename="` + fallback + `"; filename*=UTF-8''` + url.PathEscape(name)
}

func (h *HTTPServer) adminMemory(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, scope, ok := h.requireScopedAdmin(w, r, identity)
	if !ok {
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "memory.read", Resource: "memory", ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	head, err := h.memory.Head(r.Context(), scope)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scopeId": scope, "content": head.Content})
}

func (h *HTTPServer) adminDeployments(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, scope, ok := h.requireScopedAdmin(w, r, identity)
	if !ok {
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "deployments.read", Resource: "deployments", ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	deployments, err := h.deployments.List(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	orgWide := strings.HasPrefix(scope, "org:")
	views := make([]map[string]any, 0, len(deployments))
	for _, deployment := range deployments {
		if !orgWide && deployment.OwnerScopeID != scope {
			continue
		}
		view := map[string]any{
			"id":             deployment.ID,
			"ownerScopeId":   deployment.OwnerScopeID,
			"name":           firstNonEmpty(deployment.DisplayName, deployment.Name),
			"status":         deployment.Status,
			"currentVersion": deployment.CurrentVersion,
			"versions":       len(deployment.Versions),
			"createdBy":      deployment.CreatedBy,
		}
		if len(deployment.Versions) != 0 {
			view["createdAt"] = deployment.Versions[0].CreatedAt
		}
		if deployment.LastAccessAt != nil {
			view["lastAccessAt"] = *deployment.LastAccessAt
		}
		if publicURL := deploymentPublicURL(deployment.Endpoint); publicURL != "" {
			view["publicUrl"] = publicURL
		}
		views = append(views, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"scopeId": scope, "deployments": views})
}

func (h *HTTPServer) adminCrons(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, scope, ok := h.requireScopedAdmin(w, r, identity)
	if !ok {
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "crons.read", Resource: "crons", ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	records, err := h.crons.List(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	orgWide := strings.HasPrefix(scope, "org:")
	crons := make([]map[string]any, 0, len(records))
	for _, record := range records {
		var document map[string]any
		if err := json.Unmarshal(record.JSON, &document); err != nil || document == nil {
			h.fail(w, errors.New("cron JSON must be an object"))
			return
		}
		ownerScopeID, _ := document["ownerScopeId"].(string)
		if ownerScopeID == "" || !orgWide && ownerScopeID != scope {
			continue
		}
		view := map[string]any{"id": record.ID, "ownerScopeId": ownerScopeID}
		for _, key := range []string{"title", "action", "message", "owner", "createdBy", "enabled", "archived", "schedule", "destination", "createdAt", "lastFiredAt"} {
			if value, exists := document[key]; exists {
				view[key] = value
			}
		}
		crons = append(crons, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"scopeId": scope, "crons": crons})
}

// setAdminCronDestination ports the durable half of Node's admin retarget
// route. It changes only the shared cron document and (for shared crons)
// appends a notification to the delivery queue. Node's worker still performs
// the eventual Slack/provider delivery.
func (h *HTTPServer) setAdminCronDestination(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	actor, scope, ok := h.requireScopedAdmin(w, r, identity)
	if !ok {
		return
	}
	id := adminCronDestinationID(r.URL.Path)
	record, err := h.crons.Get(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if record == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	var before map[string]any
	if err := json.Unmarshal(record.JSON, &before); err != nil || before == nil {
		h.fail(w, errors.New("cron JSON must be an object"))
		return
	}
	ownerScopeID := stringValue(before["ownerScopeId"])
	if !strings.HasPrefix(scope, "org:") && ownerScopeID != scope {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "cron is outside the requested scope"})
		return
	}
	var input map[string]json.RawMessage
	if !decodeJSON(w, raw, &input) {
		return
	}
	destination, exists := input["destination"]
	if !exists {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "destination is required; use null to clear"})
		return
	}
	clear := string(bytes.TrimSpace(destination)) == "null"
	if !clear && !adminCronDestination(destination) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "destination must be a principal or slack destination with a target"})
		return
	}
	patch, _ := json.Marshal(map[string]json.RawMessage{"destination": destination})
	remove := []string(nil)
	if clear {
		remove = []string{"destination"}
		patch = []byte("{}")
	}
	updated, err := h.crons.Merge(r.Context(), id, patch, remove)
	if err != nil {
		h.fail(w, err)
		return
	}
	if updated == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: stringValue(before["owner"]), Action: "cron_retarget", Resource: id, ScopeLabel: ownerScopeID}); err != nil {
		h.fail(w, err)
		return
	}
	if err := h.enqueueCronDestinationNotice(r.Context(), *record, before, actor, destination, clear); err != nil {
		h.logger.Warn("cron destination edit notice failed", "cron_id", id, "error", err)
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "cron.destination.update", Resource: id, ScopeLabel: ownerScopeID}); err != nil {
		h.fail(w, err)
		return
	}
	var cron map[string]any
	if err := json.Unmarshal(updated.JSON, &cron); err != nil || cron == nil {
		h.fail(w, errors.New("cron JSON must be an object"))
		return
	}
	if _, exists := cron["id"]; !exists {
		cron["id"] = updated.ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"cron": cron})
}

func adminCronDestination(raw json.RawMessage) bool {
	var value map[string]json.RawMessage
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return false
	}
	for key := range value {
		if key != "type" && key != "target" && key != "audienceScopeId" && key != "onBehalfOf" {
			return false
		}
	}
	var kind, target string
	if json.Unmarshal(value["type"], &kind) != nil || (kind != "principal" && kind != "slack") || json.Unmarshal(value["target"], &target) != nil || strings.TrimSpace(target) == "" {
		return false
	}
	for _, key := range []string{"audienceScopeId", "onBehalfOf"} {
		if raw, exists := value[key]; exists {
			var text string
			if json.Unmarshal(raw, &text) != nil {
				return false
			}
		}
	}
	return true
}

func (h *HTTPServer) enqueueCronDestinationNotice(ctx context.Context, record data.CronRecord, cron map[string]any, editorID string, destination json.RawMessage, cleared bool) error {
	if stringValue(cron["runAs"]) != "scopeShared" || h.deliveries == nil {
		return nil
	}
	ownerID := stringValue(cron["owner"])
	if ownerID == "" || h.sameCronPerson(ctx, editorID, ownerID) {
		return nil
	}
	editorName := editorID
	if editor, err := h.directory.Get(ctx, editorID); err == nil && editor != nil && editor.DisplayName != "" {
		editorName = editor.DisplayName
	}
	ref := "shared"
	if title := stringValue(cron["title"]); title != "" {
		ref = `"` + title + `"`
	}
	place := ""
	if kind, scopeRef := splitScopeID(stringValue(cron["ownerScopeId"])); kind == "channel" {
		if channel, err := h.directory.Channel(ctx, scopeRef); err == nil && channel != nil && channel.Name != "" {
			place = " in " + channel.Name
		}
	}
	label := "to post somewhere new"
	if cleared {
		label = "to post somewhere new"
	}
	text := "Heads up: " + editorName + " changed your " + ref + " cron" + place + " " + label + "."
	target := "cleared"
	if !cleared {
		var next map[string]any
		if json.Unmarshal(destination, &next) == nil {
			target = stringValue(next["target"])
		}
	}
	sum := sha256.Sum256([]byte(record.ID + ":" + editorID + ":destination:" + target))
	key := "cron-edit-notice:" + record.ID + ":" + hex.EncodeToString(sum[:])[:16]
	payload, _ := json.Marshal(map[string]string{"type": "principal", "target": ownerID, "audienceScopeId": "personal:" + ownerID, "onBehalfOf": editorID})
	return h.deliveries.Enqueue(ctx, data.DeliveryEnqueueInput{Destination: payload, Text: text, IdempotencyKey: key})
}

func (h *HTTPServer) sameCronPerson(ctx context.Context, left, right string) bool {
	if sameSoulPerson(left, right) {
		return true
	}
	leftMember, leftErr := h.directory.Get(ctx, left)
	rightMember, rightErr := h.directory.Get(ctx, right)
	if leftErr != nil || rightErr != nil || leftMember == nil || rightMember == nil {
		return false
	}
	return sameSoulPerson(leftMember.PrincipalID, rightMember.PrincipalID) || leftMember.SlackID != "" && sameSoulPerson(leftMember.SlackID, rightMember.PrincipalID) || rightMember.SlackID != "" && sameSoulPerson(leftMember.PrincipalID, rightMember.SlackID) || leftMember.SlackID != "" && rightMember.SlackID != "" && sameSoulPerson(leftMember.SlackID, rightMember.SlackID)
}

func (h *HTTPServer) adminSkills(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, scope, ok := h.requireScopedAdmin(w, r, identity)
	if !ok {
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "skills.read", Resource: "skills", ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	skills, err := h.skills.List(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	packs, err := h.skills.Packs(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	orgWide := strings.HasPrefix(scope, "org:")
	views := make([]map[string]any, 0, len(skills))
	for _, skill := range skills {
		if !orgWide && skill.ScopeID != scope {
			continue
		}
		views = append(views, skillAdminSummary(skill, packs))
	}
	writeJSON(w, http.StatusOK, map[string]any{"scopeId": scope, "skills": views})
}

// listSkills mirrors Node's source-authenticated skill catalog. The skill
// durable map is deliberately shared with Node, while write, review, publish,
// pack synchronization, and runtime materialization remain Node-owned.
func (h *HTTPServer) listSkills(w http.ResponseWriter, r *http.Request) {
	principalID := r.URL.Query().Get("principalId")
	if principalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "principalId required"})
		return
	}
	skills, err := h.skills.List(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	orderedScopes, err := h.skillScopesForPrincipal(r.Context(), principalID, skills)
	if err != nil {
		h.fail(w, err)
		return
	}
	visible := resolveVisibleSkills(skills, orderedScopes)
	views := make([]map[string]any, 0, len(visible))
	for _, row := range visible {
		editable, err := h.managesArtifactHome(r.Context(), row.skill.ScopeID, row.skill.CreatedBy, principalID)
		if err != nil {
			h.fail(w, err)
			return
		}
		views = append(views, skillListView(row.skill, len(row.shadowed) > 0, editable))
		if r.URL.Query().Get("includeShadowed") == "1" {
			for _, shadowed := range row.shadowed {
				editable, err := h.managesArtifactHome(r.Context(), shadowed.ScopeID, shadowed.CreatedBy, principalID)
				if err != nil {
					h.fail(w, err)
					return
				}
				views = append(views, skillListView(shadowed, false, editable))
			}
		}
	}
	// Node also returns archived skills, but only to a manager of their home.
	for _, skill := range skills {
		if skill.Status != "archived" {
			continue
		}
		editable, err := h.managesArtifactHome(r.Context(), skill.ScopeID, skill.CreatedBy, principalID)
		if err != nil {
			h.fail(w, err)
			return
		}
		if editable {
			views = append(views, skillListView(skill, false, true))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": views})
}

// getSkillDetail preserves Node's narrower body-bearing projection. A source
// signature alone is not enough in a configured deployment: the caller must
// also present a portal identity, while an agent capability uses its signed
// actor directly.
func (h *HTTPServer) getSkillDetail(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	principalID, ok := h.skillDetailPrincipal(w, r, identity)
	if !ok {
		return
	}
	skill, err := h.skills.Get(r.Context(), skillID(r.URL.Path))
	if err != nil {
		h.fail(w, err)
		return
	}
	if skill == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	manageable, err := h.managesArtifactHome(r.Context(), skill.ScopeID, skill.CreatedBy, principalID)
	if err != nil {
		h.fail(w, err)
		return
	}
	visible := false
	if !manageable {
		all, err := h.skills.List(r.Context())
		if err != nil {
			h.fail(w, err)
			return
		}
		scopes, err := h.skillScopesForPrincipal(r.Context(), principalID, all)
		if err != nil {
			h.fail(w, err)
			return
		}
		for _, row := range resolveVisibleSkills(all, scopes) {
			if row.skill.ID == skill.ID {
				visible = true
				break
			}
		}
	}
	if !manageable && !visible {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	kind, _ := splitScopeID(skill.ScopeID)
	scope := skill.ScopeID
	if kind != "" {
		scope = kind
	}
	files := make([]map[string]any, 0, len(skill.Manifest.Files))
	for _, file := range skill.Manifest.Files {
		files = append(files, map[string]any{"path": file.Path, "executable": file.Executable})
	}
	view := map[string]any{
		"id":                   skill.ID,
		"name":                 skill.Manifest.Name,
		"description":          skill.Manifest.Description,
		"body":                 skill.Manifest.Body,
		"scope":                scope,
		"scopeId":              skill.ScopeID,
		"status":               skill.Status,
		"version":              skill.Version,
		"createdBy":            skill.CreatedBy,
		"files":                files,
		"requiredCapabilities": skill.Manifest.RequiredCapabilities,
		"grantedCapabilities":  skill.GrantedCapabilities,
		"editable":             manageable,
	}
	if skill.Pack != nil {
		view["pack"] = skill.Pack
	}
	if skill.CreatedAt != nil {
		view["createdAt"] = *skill.CreatedAt
	}
	if skill.UpdatedAt != nil {
		view["updatedAt"] = *skill.UpdatedAt
	}
	writeJSON(w, http.StatusOK, map[string]any{"skill": view})
}

func (h *HTTPServer) skillDetailPrincipal(w http.ResponseWriter, r *http.Request, identity auth.Identity) (string, bool) {
	if identity.ActorID != "" {
		internal, err := h.directory.IsInternal(r.Context(), identity.ActorID)
		if err != nil {
			h.fail(w, err)
			return "", false
		}
		if !internal {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized", "message": "principal is no longer active"})
			return "", false
		}
		return identity.ActorID, true
	}
	secret := h.config.Auth.PortalIdentitySecret
	if secret == "" {
		secret = h.config.Auth.SourceSigningSecret
	}
	portal, err := auth.VerifyPortalIdentity(r.Header.Get("x-portal-identity"), secret, time.Now())
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "capability_required"})
		return "", false
	}
	internal, err := h.directory.IsInternal(r.Context(), portal.PrincipalID)
	if err != nil {
		h.fail(w, err)
		return "", false
	}
	if !internal {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "capability_required"})
		return "", false
	}
	return portal.PrincipalID, true
}

type visibleSkill struct {
	skill    data.Skill
	shadowed []data.Skill
}

// skillScopesForPrincipal implements the ordering specific to Node's
// listVisibleSkills: personal skill wins, then visible channel/group homes in
// durable skill-id order, then the organization skill. Team scopes are not
// introduced here because this source route has only principalId and Node's
// classify(principalId) path has no team assertion either.
func (h *HTTPServer) skillScopesForPrincipal(ctx context.Context, principalID string, skills []data.Skill) ([]string, error) {
	internal, err := h.directory.IsInternal(ctx, principalID)
	if err != nil {
		return nil, err
	}
	if !internal {
		return []string{}, nil
	}
	accessible, err := h.resourceScopesForPrincipal(ctx, principalID)
	if err != nil {
		return nil, err
	}
	accessibleSet := make(map[string]bool, len(accessible))
	for _, scope := range accessible {
		accessibleSet[scope] = true
	}
	sharedHomes := []string{}
	seenHomes := map[string]bool{}
	for _, skill := range skills {
		kind, _ := splitScopeID(skill.ScopeID)
		if (kind != "channel" && kind != "group") || seenHomes[skill.ScopeID] {
			continue
		}
		seenHomes[skill.ScopeID] = true
		if accessibleSet[skill.ScopeID] {
			sharedHomes = append(sharedHomes, skill.ScopeID)
		}
	}
	return append(append([]string{"personal:" + principalID}, sharedHomes...), "org:"+h.config.QM.OrgID), nil
}

func resolveVisibleSkills(skills []data.Skill, orderedScopes []string) []visibleSkill {
	byScopeAndName := map[string]data.Skill{}
	for _, skill := range skills {
		if skill.Status != "published" {
			continue
		}
		key := skill.ScopeID + "\x00" + skill.Manifest.Name
		if _, exists := byScopeAndName[key]; !exists {
			byScopeAndName[key] = skill
		}
	}
	inScope := map[string]bool{}
	for _, scope := range orderedScopes {
		inScope[scope] = true
	}
	names := []string{}
	seenNames := map[string]bool{}
	for _, skill := range skills {
		if skill.Status != "published" || !inScope[skill.ScopeID] || !safeSkillNamePattern.MatchString(skill.Manifest.Name) || seenNames[skill.Manifest.Name] {
			continue
		}
		seenNames[skill.Manifest.Name] = true
		names = append(names, skill.Manifest.Name)
	}
	result := make([]visibleSkill, 0, len(names))
	for _, name := range names {
		matches := []data.Skill{}
		for _, scope := range orderedScopes {
			if skill, ok := byScopeAndName[scope+"\x00"+name]; ok {
				matches = append(matches, skill)
			}
		}
		if len(matches) > 0 {
			result = append(result, visibleSkill{skill: matches[0], shadowed: matches[1:]})
		}
	}
	return result
}

func skillListView(skill data.Skill, shadowed, editable bool) map[string]any {
	kind, _ := splitScopeID(skill.ScopeID)
	scope := skill.ScopeID
	if kind != "" {
		scope = kind
	}
	source := "native"
	if skill.Pack != nil {
		source = "pack"
	}
	view := map[string]any{
		"id":                   skill.ID,
		"name":                 skill.Manifest.Name,
		"description":          skill.Manifest.Description,
		"scope":                scope,
		"scopeId":              skill.ScopeID,
		"shadowed":             shadowed,
		"status":               skill.Status,
		"version":              skill.Version,
		"source":               source,
		"assetCount":           len(skill.Manifest.Files),
		"requiredCapabilities": skill.Manifest.RequiredCapabilities,
		"editable":             editable,
	}
	if skill.Pack != nil {
		view["pack"] = skill.Pack
	}
	return view
}

func (h *HTTPServer) adminSkillPacks(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, ok := h.requireOrgAdmin(w, r, identity)
	if !ok {
		return
	}
	packs, err := h.skills.ListPacks(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	skills, err := h.skills.List(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	imported := map[string]map[string]bool{}
	for _, skill := range skills {
		if skill.Status != "published" || skill.Pack == nil {
			continue
		}
		if imported[skill.Pack.PackID] == nil {
			imported[skill.Pack.PackID] = map[string]bool{}
		}
		imported[skill.Pack.PackID][skill.Pack.UpstreamName] = true
	}
	views := make([]map[string]any, 0, len(packs))
	for _, pack := range packs {
		encoded, err := json.Marshal(pack)
		if err != nil {
			h.fail(w, err)
			return
		}
		view := map[string]any{}
		if err := json.Unmarshal(encoded, &view); err != nil {
			h.fail(w, err)
			return
		}
		view["importedCount"] = len(imported[pack.ID])
		views = append(views, view)
	}
	scope := "org:" + h.config.QM.OrgID
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "skill_packs.read", Resource: "skill_packs", ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"packs": views})
}

func (h *HTTPServer) removeAdminSkillPack(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, ok := h.requireOrgAdmin(w, r, identity)
	if !ok {
		return
	}
	id := adminSkillPackID(r.URL.Path)
	removed, err := h.skills.RemovePack(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "skill_pack.remove", Resource: id, ScopeLabel: "org:" + h.config.QM.OrgID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"removed": removed})
}

func (h *HTTPServer) patchAdminSkillPack(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	actor, ok := h.requireOrgAdmin(w, r, identity)
	if !ok {
		return
	}
	input := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(raw)) != 0 {
		var generic any
		if err := json.Unmarshal(raw, &generic); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
			return
		}
		if _, ok := generic.(map[string]any); ok {
			if err := json.Unmarshal(raw, &input); err != nil {
				h.fail(w, err)
				return
			}
		}
	}
	patch, remove, message := skillPackPatch(input)
	if message != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": message})
		return
	}
	id := adminSkillPackID(r.URL.Path)
	updated, found, err := h.skills.PatchPack(r.Context(), id, patch, remove)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !found {
		h.fail(w, errors.New("unknown skill pack: "+id))
		return
	}
	var pack map[string]any
	if err := json.Unmarshal(updated, &pack); err != nil || pack == nil {
		h.fail(w, errors.New("skill pack JSON must be an object"))
		return
	}
	if _, exists := pack["id"]; !exists {
		pack["id"] = id
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "skill_pack.update", Resource: id, ScopeLabel: stringValue(pack["targetScopeId"]), IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pack": pack})
}

func skillPackPatch(input map[string]json.RawMessage) (map[string]json.RawMessage, []string, string) {
	patch := map[string]json.RawMessage{}
	remove := []string{}
	for _, field := range []string{"ref", "url"} {
		if raw, exists := input[field]; exists {
			var value string
			if json.Unmarshal(raw, &value) == nil && strings.TrimSpace(value) != "" {
				patch[field], _ = json.Marshal(strings.TrimSpace(value))
			}
		}
	}
	if raw, exists := input["trustTier"]; exists {
		var value string
		if json.Unmarshal(raw, &value) == nil && (value == "internal" || value == "third-party") {
			patch["trustTier"] = raw
		}
	}
	if raw, exists := input["syncMode"]; exists {
		var value string
		if json.Unmarshal(raw, &value) == nil && (value == "pinned" || value == "tracked") {
			patch["syncMode"] = raw
		}
	}
	if raw, exists := input["subset"]; exists {
		var value any
		if json.Unmarshal(raw, &value) != nil {
			return nil, nil, "subset must be 'all' or string[]"
		}
		if value == "all" {
			patch["subset"] = raw
		} else if values, ok := value.([]any); ok {
			items := make([]string, 0, len(values))
			for _, item := range values {
				text, ok := item.(string)
				if !ok {
					return nil, nil, "subset must be 'all' or string[]"
				}
				items = append(items, text)
			}
			patch["subset"], _ = json.Marshal(items)
		} else {
			return nil, nil, "subset must be 'all' or string[]"
		}
	}
	if raw, exists := input["config"]; exists {
		if config, keep := normalizeSkillPackConfig(raw); keep {
			patch["config"] = config
		} else {
			remove = append(remove, "config")
		}
	}
	return patch, remove, ""
}

func normalizeSkillPackConfig(raw json.RawMessage) (json.RawMessage, bool) {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil, false
	}
	if value == nil {
		return nil, false
	}
	if _, array := value.([]any); array {
		encoded, _ := json.Marshal(map[string]any{})
		return encoded, true
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	config := map[string]any{}
	for _, key := range []string{"skillGlobs", "exclude"} {
		if values, ok := object[key].([]any); ok {
			items := make([]string, 0, len(values))
			for _, value := range values {
				if text, ok := value.(string); ok {
					items = append(items, text)
				}
			}
			config[key] = items
		}
	}
	if value, exists := object["fieldOverrides"]; exists && value != nil {
		if _, object := value.(map[string]any); object {
			config["fieldOverrides"] = value
		} else if _, array := value.([]any); array {
			config["fieldOverrides"] = value
		}
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

func (h *HTTPServer) adminSkill(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, scope, ok := h.requireScopedAdmin(w, r, identity)
	if !ok {
		return
	}
	skill, err := h.skills.Get(r.Context(), adminSkillID(r.URL.Path))
	if err != nil {
		h.fail(w, err)
		return
	}
	if skill == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	if !strings.HasPrefix(scope, "org:") && skill.ScopeID != scope {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "skill is outside the requested scope"})
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "skill.read", Resource: skill.ID, ScopeLabel: skill.ScopeID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	packs, err := h.skills.Packs(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	view := map[string]any{
		"id":                   skill.ID,
		"ownerScopeId":         skill.ScopeID,
		"name":                 skill.Manifest.Name,
		"description":          skill.Manifest.Description,
		"body":                 skill.Manifest.Body,
		"requiredCapabilities": skill.Manifest.RequiredCapabilities,
		"grantedCapabilities":  skill.GrantedCapabilities,
		"approvals":            skill.Approvals,
		"status":               skill.Status,
		"version":              skill.Version,
		"createdBy":            skill.CreatedBy,
	}
	files := make([]map[string]any, 0, len(skill.Manifest.Files))
	for _, file := range skill.Manifest.Files {
		files = append(files, map[string]any{"path": file.Path, "executable": file.Executable})
	}
	view["files"] = files
	addSkillPackView(view, *skill, packs)
	writeJSON(w, http.StatusOK, view)
}

func (h *HTTPServer) archiveAdminSkill(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, scope, ok := h.requireScopedAdmin(w, r, identity)
	if !ok {
		return
	}
	id := adminSkillID(r.URL.Path)
	skill, err := h.skills.Get(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if skill == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	if !strings.HasPrefix(scope, "org:") && skill.ScopeID != scope {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "skill is outside the requested scope"})
		return
	}
	archived, err := h.skills.Archive(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !archived {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "skill.archive", Resource: id, ScopeLabel: skill.ScopeID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func skillAdminSummary(skill data.Skill, packs map[string]data.SkillPack) map[string]any {
	view := map[string]any{
		"id":           skill.ID,
		"ownerScopeId": skill.ScopeID,
		"name":         skill.Manifest.Name,
		"description":  skill.Manifest.Description,
		"status":       skill.Status,
		"version":      skill.Version,
		"createdBy":    skill.CreatedBy,
	}
	if skill.CreatedAt != nil {
		view["createdAt"] = *skill.CreatedAt
	}
	if skill.UpdatedAt != nil {
		view["updatedAt"] = *skill.UpdatedAt
	}
	if skill.LastUsedAt != nil {
		view["lastUsedAt"] = *skill.LastUsedAt
	}
	addSkillPackView(view, skill, packs)
	return view
}

func addSkillPackView(view map[string]any, skill data.Skill, packs map[string]data.SkillPack) {
	if skill.Pack == nil {
		return
	}
	if pack, ok := packs[skill.Pack.PackID]; ok {
		view["pack"] = map[string]any{"id": pack.ID, "url": pack.URL}
	}
}

func deploymentPublicURL(endpoint *data.DeploymentEndpoint) string {
	if endpoint == nil || endpoint.PublicURL == nil || *endpoint.PublicURL == "" {
		return ""
	}
	raw := *endpoint.PublicURL
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	query := parsed.Query()
	query.Del("access")
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func (h *HTTPServer) putAdminMemory(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	actor, scope, ok := h.requireScopedAdmin(w, r, identity)
	if !ok {
		return
	}
	var input struct {
		Content *string `json:"content"`
	}
	if !decodeJSON(w, raw, &input) {
		return
	}
	if input.Content == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "memory requires { content: string }"})
		return
	}
	if err := h.memory.Replace(r.Context(), scope, *input.Content, actor); err != nil {
		h.fail(w, err)
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "memory.update", Resource: "memory", ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "scopeId": scope})
}

func (h *HTTPServer) adminMemoryScopes(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, ok := h.requireOrgAdmin(w, r, identity)
	if !ok {
		return
	}
	orgScope := "org:" + h.config.QM.OrgID
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "memory.scopes.read", Resource: "memory", ScopeLabel: orgScope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	labels, err := h.discoveredScopeLabels(r.Context(), orgScope)
	if err != nil {
		h.fail(w, err)
		return
	}
	metadata, err := h.memory.Metadata(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	byScope := make(map[string]data.MemoryScopeMetadata, len(metadata))
	for _, item := range metadata {
		byScope[item.ScopeID] = item
	}
	scopes := make([]map[string]any, 0, len(labels))
	for scopeID, label := range labels {
		meta, exists := byScope[scopeID]
		bytes := int64(0)
		if exists {
			bytes = meta.Bytes
		}
		view := map[string]any{"scopeId": scopeID, "hasMemory": bytes > 0, "bytes": bytes}
		if label != "" {
			view["label"] = label
		}
		if exists && meta.UpdatedAt != nil && *meta.UpdatedAt != 0 {
			view["updatedAt"] = *meta.UpdatedAt
		}
		scopes = append(scopes, view)
	}
	sort.SliceStable(scopes, func(i, j int) bool {
		leftMemory, _ := scopes[i]["hasMemory"].(bool)
		rightMemory, _ := scopes[j]["hasMemory"].(bool)
		if leftMemory != rightMemory {
			return leftMemory
		}
		leftUpdated, _ := scopes[i]["updatedAt"].(int64)
		rightUpdated, _ := scopes[j]["updatedAt"].(int64)
		if leftUpdated != rightUpdated {
			return leftUpdated > rightUpdated
		}
		return scopes[i]["scopeId"].(string) < scopes[j]["scopeId"].(string)
	})
	writeJSON(w, http.StatusOK, map[string]any{"scopeId": orgScope, "scopes": scopes})
}

func (h *HTTPServer) discoveredScopeLabels(ctx context.Context, orgScope string, extraScopeIDs ...string) (map[string]string, error) {
	labels := map[string]string{orgScope: "org-wide"}
	scopes, err := h.sessions.DistinctScopes(ctx)
	if err != nil {
		return nil, err
	}
	for _, scope := range scopes {
		if _, exists := labels[scope.ScopeID]; !exists || labels[scope.ScopeID] == "" && scope.ChannelName != nil {
			if scope.ChannelName != nil {
				labels[scope.ScopeID] = "#" + *scope.ChannelName
			} else if !exists {
				labels[scope.ScopeID] = ""
			}
		}
	}
	principals, err := h.sessions.ParticipantPrincipalIDs(ctx)
	if err != nil {
		return nil, err
	}
	grants, err := h.acl.ListAdminGrants(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, grant := range grants {
		principals = append(principals, grant.PrincipalID)
	}
	for _, principalID := range principals {
		if principalID != "" {
			if _, exists := labels["personal:"+principalID]; !exists {
				labels["personal:"+principalID] = ""
			}
		}
	}
	for _, scopeID := range extraScopeIDs {
		if scopeID != "" {
			if _, exists := labels[scopeID]; !exists {
				labels[scopeID] = ""
			}
		}
	}
	members, err := h.directory.List(ctx)
	if err != nil {
		return nil, err
	}
	memberNames := make(map[string]string, len(members))
	for _, member := range members {
		memberNames[member.PrincipalID] = member.DisplayName
	}
	for scopeID, label := range labels {
		if label != "" || !strings.HasPrefix(scopeID, "personal:") {
			continue
		}
		labels[scopeID] = memberNames[strings.TrimPrefix(scopeID, "personal:")]
	}
	channels, err := h.directory.ListChannels(ctx)
	if err != nil {
		return nil, err
	}
	channelNames := make(map[string]string, len(channels))
	for _, channel := range channels {
		channelNames[channel.ChannelID] = channel.Name
	}
	for scopeID, label := range labels {
		if label != "" || !strings.HasPrefix(scopeID, "channel:") {
			continue
		}
		if name := channelNames[strings.TrimPrefix(scopeID, "channel:")]; name != "" {
			labels[scopeID] = "#" + name
		}
	}
	return labels, nil
}

func (h *HTTPServer) adminScopes(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, ok := h.requireOrgAdmin(w, r, identity)
	if !ok {
		return
	}
	orgScope := "org:" + h.config.QM.OrgID
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "scopes.read", Resource: "scopes", ScopeLabel: orgScope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	cronRecords, err := h.crons.List(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	cronOwners := make([]string, 0, len(cronRecords))
	for _, record := range cronRecords {
		var document map[string]any
		if json.Unmarshal(record.JSON, &document) != nil {
			h.fail(w, errors.New("cron JSON must be an object"))
			return
		}
		if owner, _ := document["ownerScopeId"].(string); owner != "" {
			cronOwners = append(cronOwners, owner)
		}
	}
	deployments, err := h.deployments.List(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	skills, err := h.skills.List(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	extraOwners := append([]string{}, cronOwners...)
	for _, deployment := range deployments {
		extraOwners = append(extraOwners, deployment.OwnerScopeID)
	}
	for _, skill := range skills {
		extraOwners = append(extraOwners, skill.ScopeID)
	}
	labels, err := h.discoveredScopeLabels(r.Context(), orgScope, extraOwners...)
	if err != nil {
		h.fail(w, err)
		return
	}
	// A very high limit keeps this administrative aggregate unpaged, matching
	// Node's scopeSessionSummaries call used by its scope overview.
	summaries, err := h.sessions.ListSummaries(r.Context(), orgScope, true, "", "", "", int(^uint(0)>>1), 0, 0, "")
	if err != nil {
		h.fail(w, err)
		return
	}
	countByScope := func(ids []string) map[string]int {
		result := map[string]int{}
		for _, id := range ids {
			result[id]++
		}
		return result
	}
	conversationCount, backgroundCount := map[string]int{}, map[string]int{}
	lastActivity, lastConversation, lastMessageAt := map[string]int64{}, map[string]int64{}, map[string]int64{}
	lastMessageSet := map[string]bool{}
	lastMessage := map[string]string{}
	for _, summary := range summaries {
		if summary.LastActivity > lastActivity[summary.ScopeID] {
			lastActivity[summary.ScopeID] = summary.LastActivity
		}
		if adminSessionCategory(summary.ThreadRef) != "conversation" {
			backgroundCount[summary.ScopeID]++
			continue
		}
		conversationCount[summary.ScopeID]++
		if summary.LastActivity >= lastConversation[summary.ScopeID] {
			lastConversation[summary.ScopeID] = summary.LastActivity
		}
		if summary.Turns > 0 && (!lastMessageSet[summary.ScopeID] || summary.LastActivity > lastMessageAt[summary.ScopeID]) {
			lastMessageSet[summary.ScopeID] = true
			lastMessageAt[summary.ScopeID] = summary.LastActivity
			lastMessage[summary.ScopeID] = summary.LastMessage
		}
	}
	cronCounts := countByScope(cronOwners)
	deploymentOwners := make([]string, 0, len(deployments))
	for _, deployment := range deployments {
		deploymentOwners = append(deploymentOwners, deployment.OwnerScopeID)
	}
	skillOwners := make([]string, 0, len(skills))
	for _, skill := range skills {
		skillOwners = append(skillOwners, skill.ScopeID)
	}
	deploymentCounts, skillCounts := countByScope(deploymentOwners), countByScope(skillOwners)
	views := make([]map[string]any, 0, len(labels))
	for scopeID, label := range labels {
		view := map[string]any{
			"scopeId":                  scopeID,
			"sessions":                 conversationCount[scopeID],
			"backgroundSessions":       backgroundCount[scopeID],
			"lastActivity":             lastActivity[scopeID],
			"lastConversationActivity": lastConversation[scopeID],
			"lastMessage":              lastMessage[scopeID],
			"crons":                    cronCounts[scopeID],
			"deployments":              deploymentCounts[scopeID],
			"skills":                   skillCounts[scopeID],
		}
		if label != "" {
			view["label"] = label
		}
		views = append(views, view)
	}
	sort.SliceStable(views, func(i, j int) bool {
		leftActivity, _ := views[i]["lastActivity"].(int64)
		rightActivity, _ := views[j]["lastActivity"].(int64)
		if leftActivity != rightActivity {
			return leftActivity > rightActivity
		}
		leftSessions, _ := views[i]["sessions"].(int)
		rightSessions, _ := views[j]["sessions"].(int)
		if leftSessions != rightSessions {
			return leftSessions > rightSessions
		}
		leftBackground, _ := views[i]["backgroundSessions"].(int)
		rightBackground, _ := views[j]["backgroundSessions"].(int)
		if leftBackground != rightBackground {
			return leftBackground > rightBackground
		}
		return views[i]["scopeId"].(string) < views[j]["scopeId"].(string)
	})
	writeJSON(w, http.StatusOK, map[string]any{"scopeId": orgScope, "scopes": views})
}

func (h *HTTPServer) adminAudit(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, ok := h.requireOrgAdmin(w, r, identity)
	if !ok {
		return
	}
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	if scope == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "scope required"})
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "audit.read", Resource: "audit", ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	filter := scope
	if strings.HasPrefix(scope, "org:") {
		filter = ""
	}
	records, err := h.audit.Tail(r.Context(), filter, 200)
	if err != nil {
		h.fail(w, err)
		return
	}
	events := make([]map[string]any, 0, len(records))
	for _, record := range records {
		event := map[string]any{"ts": record.At, "principalId": record.PrincipalID, "action": record.Action, "scopeLabel": record.ScopeLabel, "resource": record.Resource}
		if record.Status != "" {
			event["status"] = record.Status
		}
		events = append(events, event)
	}
	writeJSON(w, http.StatusOK, map[string]any{"scopeId": scope, "events": events})
}

func (h *HTTPServer) requireScopedAdmin(w http.ResponseWriter, r *http.Request, identity auth.Identity) (string, string, bool) {
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	if scope == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "scope required"})
		return "", "", false
	}
	actor, ok := h.requireOrgAdmin(w, r, identity)
	return actor, scope, ok
}

func (h *HTTPServer) adminEgress(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, scope, ok := h.requireScopedAdmin(w, r, identity)
	if !ok {
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "egress.read", Resource: "egress", ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	result, err := h.egress.Summary(r.Context(), scope, strings.HasPrefix(scope, "org:"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scopeId": scope, "records": result.Records, "total": result.Total, "denied": result.Denied, "hosts": result.Hosts, "bySource": result.BySource})
}

func (h *HTTPServer) adminErrors(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, scope, ok := h.requireScopedAdmin(w, r, identity)
	if !ok {
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "errors.read", Resource: "errors", ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	query := data.ErrorEventQuery{SessionID: r.URL.Query().Get("sessionId")}
	if !strings.HasPrefix(scope, "org:") {
		query.ScopeID = scope
	}
	if r.URL.Query().Get("count") != "" {
		total, err := h.errors.Count(r.Context(), query)
		if err != nil {
			h.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"scopeId": scope, "total": total})
		return
	}
	errors, err := h.errors.List(r.Context(), query)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scopeId": scope, "errors": errors})
}

func (h *HTTPServer) adminRuns(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, scope, ok := h.requireScopedAdmin(w, r, identity)
	if !ok {
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "runs.read", Resource: "runs", ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	runs, err := h.runs.ListAdmin(r.Context(), scope, strings.HasPrefix(scope, "org:"))
	if err != nil {
		h.fail(w, err)
		return
	}
	active := 0
	for _, run := range runs {
		if run.Status == "pending" || run.Status == "running" {
			active++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"scopeId": scope, "active": active, "runs": runs})
}

func latencySummary(values []float64) map[string]any {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	pct := func(p float64) any {
		if len(sorted) == 0 {
			return nil
		}
		i := int(math.Ceil(p/100*float64(len(sorted)))) - 1
		if i < 0 {
			i = 0
		}
		if i >= len(sorted) {
			i = len(sorted) - 1
		}
		return sorted[i]
	}
	return map[string]any{"count": len(sorted), "p50": pct(50), "p95": pct(95), "p99": pct(99)}
}

func metricInts(samples []data.TurnMetric, get func(data.TurnMetric) *int) []float64 {
	values := []float64{}
	for _, sample := range samples {
		if value := get(sample); value != nil {
			values = append(values, float64(*value))
		}
	}
	return values
}

func metricStat(values []float64) map[string]any {
	result := latencySummary(values)
	if len(values) == 0 {
		result["avg"] = nil
		return result
	}
	total := 0.0
	for _, value := range values {
		total += value
	}
	result["avg"] = total / float64(len(values))
	return result
}

func (h *HTTPServer) adminMetrics(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, scope, ok := h.requireScopedAdmin(w, r, identity)
	if !ok {
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "metrics.read", Resource: "metrics", ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	orgWide := strings.HasPrefix(scope, "org:")
	samples, err := h.metrics.List(r.Context(), firstIf(!orgWide, scope), 10000)
	if err != nil {
		h.fail(w, err)
		return
	}
	capture, turns := []data.TurnMetric{}, []data.TurnMetric{}
	for _, sample := range samples {
		if sample.Status == "capture" {
			capture = append(capture, sample)
		} else {
			turns = append(turns, sample)
		}
	}
	total := make([]float64, 0, len(turns))
	ttft := metricInts(turns, func(s data.TurnMetric) *int { return s.TTFTMS })
	for _, sample := range turns {
		total = append(total, float64(sample.TotalMS))
	}
	byDay := map[string][]data.TurnMetric{}
	for _, sample := range turns {
		day := time.UnixMilli(sample.TS).UTC().Format("2006-01-02")
		byDay[day] = append(byDay[day], sample)
	}
	days := make([]string, 0, len(byDay))
	for day := range byDay {
		days = append(days, day)
	}
	sort.Strings(days)
	series := make([]map[string]any, 0, len(days))
	for _, day := range days {
		daily := latencySummary(metricInts(byDay[day], func(s data.TurnMetric) *int { return s.TTFTMS }))
		series = append(series, map[string]any{"day": day, "turns": len(byDay[day]), "ttftP50": daily["p50"], "ttftP95": daily["p95"]})
	}
	runs, err := h.runs.ListAdminMetrics(r.Context(), scope, orgWide)
	if err != nil {
		h.fail(w, err)
		return
	}
	queue, runLatency := []float64{}, []float64{}
	done, failed := 0, 0
	for _, run := range runs {
		if run.StartedAt != nil {
			queue = append(queue, float64(*run.StartedAt-run.CreatedAt))
			if run.FinishedAt != nil {
				runLatency = append(runLatency, float64(*run.FinishedAt-*run.StartedAt))
			}
		}
		if run.Status == "done" {
			done++
		}
		if run.Status == "failed" {
			failed++
		}
	}
	traceA, traceB, cold, warm := []data.TurnMetric{}, []data.TurnMetric{}, []data.TurnMetric{}, []data.TurnMetric{}
	for _, sample := range turns {
		if sample.Provisioned == nil {
			continue
		}
		if *sample.Provisioned {
			traceB = append(traceB, sample)
			if sample.ColdStart != nil && *sample.ColdStart {
				cold = append(cold, sample)
			} else if sample.ColdStart != nil {
				warm = append(warm, sample)
			}
		} else {
			traceA = append(traceA, sample)
		}
	}
	phase := func(name string, set []data.TurnMetric, get func(data.TurnMetric) *int) map[string]any {
		values := metricInts(set, get)
		result := latencySummary(values)
		buckets := []int{10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000}
		counts := make([]int, len(buckets)+1)
		dayValues := map[string][]float64{}
		worst := []map[string]any{}
		for _, sample := range set {
			value := get(sample)
			if value == nil {
				continue
			}
			v := float64(*value)
			day := time.UnixMilli(sample.TS).UTC().Format("2006-01-02")
			dayValues[day] = append(dayValues[day], v)
			index := len(buckets)
			for i, edge := range buckets {
				if v <= float64(edge) {
					index = i
					break
				}
			}
			counts[index]++
			worst = append(worst, map[string]any{"ms": v, "ts": sample.TS, "totalMs": sample.TotalMS, "sessionId": sample.SessionID, "turnSeq": sample.TurnSeq, "scopeLabel": sample.ScopeLabel, "provisioned": sample.Provisioned, "coldStart": sample.ColdStart})
		}
		phaseDays := make([]string, 0, len(dayValues))
		for day := range dayValues {
			phaseDays = append(phaseDays, day)
		}
		sort.Strings(phaseDays)
		daily := make([]map[string]any, 0, len(phaseDays))
		for _, day := range phaseDays {
			entry := latencySummary(dayValues[day])
			entry["day"] = day
			daily = append(daily, entry)
		}
		sort.Slice(worst, func(i, j int) bool { return worst[i]["ms"].(float64) > worst[j]["ms"].(float64) })
		if len(worst) > 8 {
			worst = worst[:8]
		}
		dist := make([]map[string]any, 0, len(counts))
		for i, count := range counts {
			var edge any
			if i < len(buckets) {
				edge = buckets[i]
			}
			dist = append(dist, map[string]any{"le": edge, "count": count})
		}
		result["phase"] = name
		result["daily"] = daily
		result["dist"] = dist
		result["worst"] = worst
		return result
	}
	phaseDefs := []struct {
		name    string
		get     func(data.TurnMetric) *int
		capture bool
	}{
		{"total", func(s data.TurnMetric) *int { return &s.TotalMS }, false}, {"ttft", func(s data.TurnMetric) *int { return s.TTFTMS }, false}, {"slackInflight", func(s data.TurnMetric) *int { return s.SlackInflightMS }, false}, {"intakePreamble", func(s data.TurnMetric) *int { return s.IntakePreambleMS }, false}, {"queue", func(s data.TurnMetric) *int { return s.QueueMS }, false}, {"dispatch", func(s data.TurnMetric) *int { return s.DispatchMS }, false}, {"ingress", func(s data.TurnMetric) *int { return s.IngressMS }, false}, {"detect", func(s data.TurnMetric) *int { return s.DetectMS }, false}, {"compact", func(s data.TurnMetric) *int { return s.CompactMS }, false}, {"provision", func(s data.TurnMetric) *int { return s.ProvisionMS }, false}, {"materialize", func(s data.TurnMetric) *int { return s.MaterializeMS }, false}, {"creds", func(s data.TurnMetric) *int { return s.CredsMS }, false}, {"layers", func(s data.TurnMetric) *int { return s.LayersMS }, false}, {"compile", func(s data.TurnMetric) *int { return s.CompileMS }, false}, {"recall", func(s data.TurnMetric) *int { return s.RecallMS }, false}, {"lease", func(s data.TurnMetric) *int { return s.LeaseMS }, false}, {"exec", func(s data.TurnMetric) *int { return s.ExecMS }, false}, {"stream", func(s data.TurnMetric) *int { return s.StreamMS }, false}, {"deliver", func(s data.TurnMetric) *int { return s.DeliverMS }, false}, {"capture", func(s data.TurnMetric) *int { return s.CaptureMS }, true},
	}
	phases := make([]map[string]any, 0, len(phaseDefs))
	for _, def := range phaseDefs {
		set := turns
		if def.capture {
			set = capture
		}
		phases = append(phases, phase(def.name, set, def.get))
	}
	classified := append(append([]data.TurnMetric{}, traceA...), traceB...)
	cacheRead, cacheWrite, uncached := int64(0), int64(0), int64(0)
	ratios := []float64{}
	misses := 0
	missSamples := 0
	for _, sample := range turns {
		if sample.CacheRead != nil {
			cacheRead += *sample.CacheRead
		}
		if sample.CacheWrite != nil {
			cacheWrite += *sample.CacheWrite
		}
		if sample.UncachedInput != nil {
			uncached += *sample.UncachedInput
		}
		if sample.CacheRead != nil || sample.CacheWrite != nil || sample.UncachedInput != nil {
			read, write, input := int64(0), int64(0), int64(0)
			if sample.CacheRead != nil {
				read = *sample.CacheRead
			}
			if sample.CacheWrite != nil {
				write = *sample.CacheWrite
			}
			if sample.UncachedInput != nil {
				input = *sample.UncachedInput
			}
			denominator := read + write + input
			if denominator > 0 {
				ratio := float64(read) / float64(denominator)
				ratios = append(ratios, ratio)
				if write >= 1024 && ratio < 0.1 {
					misses++
				}
				missSamples++
			}
		}
	}
	avgRatio := any(nil)
	if len(ratios) > 0 {
		sum := 0.0
		for _, v := range ratios {
			sum += v
		}
		avgRatio = sum / float64(len(ratios))
	}
	pooled := any(nil)
	if cacheRead+cacheWrite+uncached > 0 {
		pooled = float64(cacheRead) / float64(cacheRead+cacheWrite+uncached)
	}
	missRate := any(nil)
	if missSamples > 0 {
		missRate = float64(misses) / float64(missSamples)
	}
	finished := done + failed
	failureRate := 0.0
	if finished > 0 {
		failureRate = float64(failed) / float64(finished)
	}
	writeJSON(w, http.StatusOK, map[string]any{"scopeId": scope, "ttft": latencySummary(ttft), "turnLatency": latencySummary(total), "runLatency": latencySummary(runLatency), "queueWait": latencySummary(queue), "throughput": map[string]any{"total": len(runs), "done": done, "failed": failed, "failureRate": failureRate}, "series": series, "anatomy": map[string]any{"traceA": map[string]any{"samples": len(traceA), "total": metricStat(metricInts(traceA, func(s data.TurnMetric) *int { return &s.TotalMS })), "ttft": metricStat(metricInts(traceA, func(s data.TurnMetric) *int { return s.TTFTMS })), "stream": metricStat(metricInts(traceA, func(s data.TurnMetric) *int { return s.StreamMS })), "lease": metricStat(metricInts(traceA, func(s data.TurnMetric) *int { return s.LeaseMS })), "recall": metricStat(metricInts(traceA, func(s data.TurnMetric) *int { return s.RecallMS })), "modelCalls": metricStat(metricInts(traceA, func(s data.TurnMetric) *int { return s.ModelCalls })), "toolCalls": metricStat(metricInts(traceA, func(s data.TurnMetric) *int { return s.ToolCalls }))}, "traceB": map[string]any{"samples": len(traceB), "cold": len(cold), "warm": len(warm), "total": metricStat(metricInts(traceB, func(s data.TurnMetric) *int { return &s.TotalMS })), "ttft": metricStat(metricInts(traceB, func(s data.TurnMetric) *int { return s.TTFTMS })), "provision": metricStat(metricInts(traceB, func(s data.TurnMetric) *int { return s.ProvisionMS })), "provisionCold": metricStat(metricInts(cold, func(s data.TurnMetric) *int { return s.ProvisionMS })), "provisionWarm": metricStat(metricInts(warm, func(s data.TurnMetric) *int { return s.ProvisionMS })), "materialize": metricStat(metricInts(traceB, func(s data.TurnMetric) *int { return s.MaterializeMS })), "exec": metricStat(metricInts(traceB, func(s data.TurnMetric) *int { return s.ExecMS })), "recall": metricStat(metricInts(traceB, func(s data.TurnMetric) *int { return s.RecallMS })), "modelCalls": metricStat(metricInts(traceB, func(s data.TurnMetric) *int { return s.ModelCalls })), "toolCalls": metricStat(metricInts(traceB, func(s data.TurnMetric) *int { return s.ToolCalls }))}, "composite": map[string]any{"totalTurns": len(classified), "provisionedTurns": len(traceB), "provisionRate": ratio(len(traceB), len(classified)), "coldRate": ratio(len(cold), len(traceB)), "total": metricStat(metricInts(classified, func(s data.TurnMetric) *int { return &s.TotalMS })), "modelCalls": metricStat(metricInts(classified, func(s data.TurnMetric) *int { return s.ModelCalls })), "toolCalls": metricStat(metricInts(classified, func(s data.TurnMetric) *int { return s.ToolCalls }))}, "capture": metricStat(metricInts(capture, func(s data.TurnMetric) *int { return s.CaptureMS }))}, "cache": map[string]any{"samples": len(ratios), "avgHitRatio": avgRatio, "pooledHitRatio": pooled, "missTurns": misses, "missRate": missRate, "cacheReadTotal": cacheRead, "cacheWriteTotal": cacheWrite, "uncachedInputTotal": uncached}, "phases": phases})
}

func firstIf(condition bool, value string) string {
	if condition {
		return value
	}
	return ""
}
func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

func adminSessionCategory(threadRef string) string {
	if dataSessionOrigin(threadRef) == "conversation" {
		return "conversation"
	}
	return "background"
}

func dataSessionOrigin(threadRef string) string {
	parts := strings.Split(threadRef, ":")
	if len(parts) == 4 && parts[0] == "agent" && parts[1] == "main" && (parts[2] == "cron" || parts[2] == "webhook" || parts[2] == "monitor") && parts[3] != "" {
		return parts[2]
	}
	if len(parts) >= 2 && (parts[0] == "cron" || parts[0] == "webhook" || parts[0] == "monitor") && parts[1] != "" {
		return parts[0]
	}
	return "conversation"
}

func adminSessionOrigin(threadRef, sourceScope, requestedScope string, crons *data.CronRepository) any {
	trigger := dataSessionOrigin(threadRef)
	if trigger == "conversation" {
		return nil
	}
	parts := strings.Split(threadRef, ":")
	sourceID := ""
	slot := any(nil)
	monologue := false
	if len(parts) == 4 && parts[0] == "agent" && parts[1] == "main" {
		sourceID = parts[3]
		monologue = true
	} else {
		sourceID = parts[1]
		if len(parts) > 2 {
			slot = strings.Join(parts[2:], ":")
		}
	}
	label := map[string]string{"cron": "Cron", "monitor": "Monitor wake", "webhook": "Webhook wake"}[trigger]
	if monologue {
		label = map[string]string{"cron": "Cron monologue", "monitor": "Monitor monologue", "webhook": "Webhook monologue"}[trigger]
	}
	if trigger != "cron" {
		result := map[string]any{"kind": "background_wake", "label": label, "trigger": trigger, "fireKey": threadRef, "fireSlot": slot, "sourceId": sourceID}
		if monologue {
			result["monologue"] = true
		}
		return result
	}
	result := map[string]any{"kind": "cron", "label": label, "cronId": sourceID, "fireKey": threadRef, "fireSlot": slot, "cron": nil}
	if monologue {
		result["monologue"] = true
	}
	if record, err := crons.Get(context.Background(), sourceID); err == nil && record != nil {
		var cron map[string]any
		if json.Unmarshal(record.JSON, &cron) == nil && (strings.HasPrefix(requestedScope, "org:") || cron["ownerScopeId"] == sourceScope) {
			result["cron"] = adminCronSummary(cron)
		}
	}
	return result
}

func sessionSummaryView(item data.SessionSummary, requestedScope string, crons *data.CronRepository, delivered map[string]int) map[string]any {
	origin := dataSessionOrigin(item.ThreadRef)
	kind := item.Type
	if origin != "conversation" {
		if len(strings.Split(item.ThreadRef, ":")) == 4 && strings.HasPrefix(item.ThreadRef, "agent:main:") {
			kind = origin + "_monologue"
		} else {
			kind = origin + "_wake"
		}
	}
	result := map[string]any{"id": item.ID, "type": item.Type, "origin": adminSessionOrigin(item.ThreadRef, item.ScopeID, requestedScope, crons), "scopeId": item.ScopeID, "threadRef": item.ThreadRef, "turns": item.Turns, "messages": item.Messages, "lastActivity": item.LastActivity, "createdAt": item.CreatedAt, "firstMessage": item.FirstMessage, "lastMessage": item.LastMessage, "category": adminSessionCategory(item.ThreadRef), "kind": kind}
	if origin != "conversation" {
		result["delivered"] = delivered[item.ID]
	}
	return result
}

func (h *HTTPServer) adminSessions(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, scope, ok := h.requireScopedAdmin(w, r, identity)
	if !ok {
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "sessions.read", Resource: "sessions", ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	category := r.URL.Query().Get("category")
	if category != "background" && category != "all" {
		category = "conversation"
	}
	filterCategory := category
	if category == "all" {
		filterCategory = ""
	}
	origin := r.URL.Query().Get("origin")
	if origin != "cron" && origin != "other_background" {
		origin = ""
	}
	cronID := strings.TrimSpace(r.URL.Query().Get("cron"))
	orgWide := strings.HasPrefix(scope, "org:")
	stats, err := h.sessions.Stats(r.Context(), scope, orgWide, filterCategory, origin, cronID)
	if err != nil {
		h.fail(w, err)
		return
	}
	limit := queryLimit(r.URL.Query().Get("limit"), 50, 200)
	if origin == "cron" && cronID == "" {
		groups, err := h.sessions.CronGroups(r.Context(), scope, orgWide)
		if err != nil {
			h.fail(w, err)
			return
		}
		cronIDs := make([]string, 0, len(groups))
		for _, group := range groups {
			cronIDs = append(cronIDs, group["cronId"].(string))
		}
		deliveredRuns, err := h.deliveries.SentRunCountsByCron(r.Context(), cronIDs)
		if err != nil {
			h.fail(w, err)
			return
		}
		for _, group := range groups {
			group["deliveredRuns"] = deliveredRuns[group["cronId"].(string)]
			group["origin"] = adminSessionOrigin("agent:main:cron:"+group["cronId"].(string), group["scopeId"].(string), scope, h.crons)
		}
		writeJSON(w, http.StatusOK, map[string]any{"scopeId": scope, "crons": groups, "total": len(groups), "totalByCategory": stats.TotalByCategory, "totalByType": stats.ByTypeAll, "distinctCrons": stats.Crons, "category": category, "turns": stats.Turns, "byType": stats.ByType, "limit": limit, "offset": 0})
		return
	}
	cursor := r.URL.Query().Get("cursor")
	beforeActivity := int64(0)
	beforeID := ""
	if parts := strings.SplitN(cursor, "~", 2); len(parts) == 2 {
		if parsed, err := strconv.ParseInt(parts[0], 10, 64); err == nil && parts[1] != "" {
			beforeActivity = parsed
			beforeID = parts[1]
		}
	}
	offset := 0
	if beforeID == "" {
		if parsed, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && parsed > 0 {
			offset = parsed
		}
		if stats.Total > 0 && offset > stats.Total-1 {
			offset = ((stats.Total - 1) / limit) * limit
		}
	}
	summaries, err := h.sessions.ListSummaries(r.Context(), scope, orgWide, filterCategory, origin, cronID, limit, offset, beforeActivity, beforeID)
	if err != nil {
		h.fail(w, err)
		return
	}
	delivered, err := h.deliveries.SentCountsBySourceSessions(r.Context(), summaries)
	if err != nil {
		h.fail(w, err)
		return
	}
	sessions := make([]map[string]any, 0, len(summaries))
	for _, item := range summaries {
		sessions = append(sessions, sessionSummaryView(item, scope, h.crons, delivered))
	}
	response := map[string]any{"scopeId": scope, "sessions": sessions, "total": stats.Total, "totalByCategory": stats.TotalByCategory, "totalByType": stats.ByTypeAll, "distinctCrons": stats.Crons, "category": category, "turns": stats.Turns, "byType": stats.ByType, "limit": limit, "offset": offset}
	if len(summaries) == limit {
		last := summaries[len(summaries)-1]
		response["nextCursor"] = strconv.FormatInt(last.LastActivity, 10) + "~" + last.ID
	}
	if cronID != "" {
		response["cron"] = adminSessionOrigin("agent:main:cron:"+cronID, scope, scope, h.crons)
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *HTTPServer) adminUsers(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, ok := h.requireOrgAdmin(w, r, identity)
	if !ok {
		return
	}
	scope := "org:" + h.config.QM.OrgID
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "users.read", Resource: "users", ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	windows, err := h.sessions.ParticipantWindows(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	turns, err := h.sessions.AttributedTurns(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	grants, err := h.acl.ListAdminGrants(r.Context(), "")
	if err != nil {
		h.fail(w, err)
		return
	}
	bySession := map[string][]data.ParticipantWindow{}
	for _, window := range windows {
		bySession[window.SessionID] = append(bySession[window.SessionID], window)
	}
	sessionsByUser := map[string]map[string]bool{}
	turnsByUser := map[string]int{}
	lastSeen := map[string]int64{}
	mark := func(principal string, at int64) {
		if at > lastSeen[principal] {
			lastSeen[principal] = at
		}
	}
	for _, window := range windows {
		if sessionsByUser[window.PrincipalID] == nil {
			sessionsByUser[window.PrincipalID] = map[string]bool{}
		}
		sessionsByUser[window.PrincipalID][window.SessionID] = true
		mark(window.PrincipalID, window.ValidFrom)
	}
	for _, turn := range turns {
		for _, window := range bySession[turn.SessionID] {
			if samePrincipal(window.PrincipalID, turn.PrincipalID) {
				turnsByUser[turn.PrincipalID] += turn.Turns
				mark(turn.PrincipalID, turn.LastAt)
				break
			}
		}
	}
	ids := map[string]bool{}
	for id := range sessionsByUser {
		ids[id] = true
	}
	for _, grant := range grants {
		ids[grant.PrincipalID] = true
	}
	users := make([]map[string]any, 0, len(ids))
	for id := range ids {
		admin := map[string]any{"isAdmin": false}
		for _, grant := range grants {
			if samePrincipal(grant.PrincipalID, id) && grant.Role == "org_admin" {
				admin["isAdmin"] = true
				admin["role"] = "org_admin"
				admin["scopeId"] = grant.ScopeID
				break
			}
		}
		var seen any
		if lastSeen[id] > 0 {
			seen = lastSeen[id]
		}
		sessions := 0
		if values := sessionsByUser[id]; values != nil {
			sessions = len(values)
		}
		users = append(users, map[string]any{"principalId": id, "sessionCount": sessions, "turnCount": turnsByUser[id], "lastSeenAt": seen, "admin": admin})
	}
	sort.Slice(users, func(i, j int) bool {
		a, b := users[i], users[j]
		aa := a["admin"].(map[string]any)["isAdmin"].(bool)
		bb := b["admin"].(map[string]any)["isAdmin"].(bool)
		if aa != bb {
			return aa
		}
		at, _ := a["lastSeenAt"].(int64)
		bt, _ := b["lastSeenAt"].(int64)
		return at > bt
	})
	writeJSON(w, http.StatusOK, map[string]any{"scopeId": scope, "users": users, "grants": grants})
}

// adminKeychain is deliberately metadata-only. The durable documents may
// contain encrypted values, but the Node admin endpoint strips secretEnc and
// never decrypts credentials for this view; Go follows that same boundary.
func (h *HTTPServer) adminKeychain(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, ok := h.requireOrgAdmin(w, r, identity)
	if !ok {
		return
	}
	scope := "org:" + h.config.QM.OrgID
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "keychain.read", Resource: "keychain", ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	status, err := h.keychain.List(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	if !status.Enabled {
		writeJSON(w, http.StatusOK, map[string]any{"scopeId": scope, "people": []any{}, "credentials": []any{}, "grants": []any{}, "asks": []any{}, "enabled": false})
		return
	}
	participants, err := h.sessions.ParticipantPrincipalIDs(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	adminGrants, err := h.acl.ListAdminGrants(r.Context(), "")
	if err != nil {
		h.fail(w, err)
		return
	}
	members, err := h.directory.List(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	names, rosterIDs := map[string]string{}, map[string]string{}
	for _, member := range members {
		key := principalKey(member.PrincipalID)
		names[key], rosterIDs[key] = member.DisplayName, member.PrincipalID
	}
	ids := map[string]string{}
	add := func(value string) {
		key := principalKey(value)
		if key == "" {
			return
		}
		if _, present := ids[key]; !present {
			if roster := rosterIDs[key]; roster != "" {
				ids[key] = roster
			} else {
				ids[key] = value
			}
		}
	}
	for _, credential := range status.Credentials {
		add(anyString(credential["ownerId"]))
	}
	for _, grant := range status.Grants {
		add(anyString(grant["ownerId"]))
		add(anyString(grant["usedBy"]))
	}
	for _, ask := range status.Asks {
		add(anyString(ask["ownerId"]))
		add(anyString(ask["requesterId"]))
	}
	for _, principalID := range participants {
		add(principalID)
	}
	for _, grant := range adminGrants {
		add(grant.PrincipalID)
	}
	keys := make([]string, 0, len(ids))
	for key := range ids {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	people := make([]map[string]any, 0, len(keys))
	now := time.Now().UnixMilli()
	for _, key := range keys {
		principalID := ids[key]
		credentialCount, activeGrantCount, pendingAskCount := 0, 0, 0
		for _, credential := range status.Credentials {
			if samePrincipal(anyString(credential["ownerId"]), principalID) {
				credentialCount++
			}
		}
		for _, grant := range status.Grants {
			if samePrincipal(anyString(grant["ownerId"]), principalID) && anyString(grant["status"]) == "active" {
				expiresAt := anyInt64(grant["expiresAt"])
				if expiresAt == 0 || expiresAt > now {
					activeGrantCount++
				}
			}
		}
		for _, ask := range status.Asks {
			if samePrincipal(anyString(ask["ownerId"]), principalID) && anyString(ask["status"]) == "pending" {
				pendingAskCount++
			}
		}
		person := map[string]any{"principalId": principalID, "displayName": nil, "credentialCount": credentialCount, "activeGrantCount": activeGrantCount, "pendingAskCount": pendingAskCount}
		if displayName := names[key]; displayName != "" {
			person["displayName"] = displayName
		}
		people = append(people, person)
	}
	writeJSON(w, http.StatusOK, map[string]any{"scopeId": scope, "people": people, "credentials": status.Credentials, "grants": status.Grants, "asks": status.Asks, "enabled": true})
}

func (h *HTTPServer) adminCustomProviders(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, ok := h.requireOrgAdmin(w, r, identity)
	if !ok {
		return
	}
	providers := make([]map[string]any, 0)
	for _, provider := range safeModelProviders(h.config.QM.Models) {
		if provider["protocol"] == "mock" {
			continue
		}
		provider["hasKey"] = provider["configured"]
		providers = append(providers, provider)
	}
	scope := "org:" + h.config.QM.OrgID
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "custom-providers.read", Resource: "custom-providers", ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": providers})
}

func (h *HTTPServer) adminSlackInstallation(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, ok := h.requireOrgAdmin(w, r, identity)
	if !ok {
		return
	}
	scope := "org:" + h.config.QM.OrgID
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "slack-installation.read", Resource: "slack-installation", ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	configured := strings.TrimSpace(h.config.QM.Slack.BotToken) != "" && strings.TrimSpace(h.config.QM.Slack.AppToken) != ""
	response := map[string]any{
		"createUrl": slackManifestCreationURL(), "configured": configured, "managed": true,
		"source": "yaml", "configPath": "qm.slack",
	}
	if h.config.QM.Slack.TeamID != "" {
		response["teamId"] = h.config.QM.Slack.TeamID
	}
	if h.config.QM.Slack.TeamName != "" {
		response["teamName"] = h.config.QM.Slack.TeamName
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *HTTPServer) adminSandboxRoutes(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, ok := h.requireOrgAdmin(w, r, identity)
	if !ok {
		return
	}
	routes, err := h.sandboxRoutes.List(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	scope := "org:" + h.config.QM.OrgID
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "sandbox_routes.read", Resource: "sandbox_routes", ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"defaultBackend": h.config.QM.SandboxDefaultBackend, "availableBackends": h.config.QM.SandboxBackends, "routes": routes})
}

func (h *HTTPServer) setAdminUserOnboarding(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	actor, ok := h.requireOrgAdmin(w, r, identity)
	if !ok {
		return
	}
	var input struct {
		Status string `json:"status"`
	}
	if !decodeJSON(w, raw, &input) {
		return
	}
	if input.Status != "not_started" && input.Status != "pending" && input.Status != "completed" && input.Status != "dismissed" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "onboarding requires { status: not_started|pending|completed|dismissed }"})
		return
	}
	principalID := adminUserOnboardingID(r.URL.Path)
	if principalID == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	scope := "personal:" + principalID
	head, err := h.memory.Head(r.Context(), scope)
	if err != nil {
		h.fail(w, err)
		return
	}
	content := setOnboardingStatus(head.Content, input.Status, time.Now().UTC().Format("2006-01-02"))
	if err := h.memory.Replace(r.Context(), scope, content, actor); err != nil {
		h.fail(w, err)
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "user.onboarding.set", Resource: principalID + "/" + input.Status, ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "scopeId": scope, "status": input.Status})
}

func (h *HTTPServer) adminUserDetail(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, ok := h.requireOrgAdmin(w, r, identity)
	if !ok {
		return
	}
	principalID := adminUserDetailID(r.URL.Path)
	if principalID == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	orgScope, personalScope := "org:"+h.config.QM.OrgID, "personal:"+principalID
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "user.read", Resource: principalID, ScopeLabel: orgScope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	member, err := h.directory.Get(r.Context(), principalID)
	if err != nil {
		h.fail(w, err)
		return
	}
	grants, err := h.acl.ListAdminGrants(r.Context(), "")
	if err != nil {
		h.fail(w, err)
		return
	}
	windows, err := h.sessions.ParticipantWindows(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	turnRows, err := h.sessions.AttributedTurns(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	sessionIDs, turnsBySession := map[string]bool{}, map[string]int{}
	var firstSeenAt, lastSeenAt *int64
	markSeen := func(at int64) {
		if firstSeenAt == nil || at < *firstSeenAt {
			value := at
			firstSeenAt = &value
		}
		if lastSeenAt == nil || at > *lastSeenAt {
			value := at
			lastSeenAt = &value
		}
	}
	for _, window := range windows {
		if !samePrincipal(window.PrincipalID, principalID) {
			continue
		}
		sessionIDs[window.SessionID] = true
		markSeen(window.ValidFrom)
	}
	turns := 0
	for _, turn := range turnRows {
		if !samePrincipal(turn.PrincipalID, principalID) || !sessionIDs[turn.SessionID] {
			continue
		}
		turnsBySession[turn.SessionID] += turn.Turns
		turns += turn.Turns
		markSeen(turn.FirstAt)
		markSeen(turn.LastAt)
	}
	ids := make([]string, 0, len(sessionIDs))
	for id := range sessionIDs {
		ids = append(ids, id)
	}
	summaries, err := h.sessions.SummariesForIDs(r.Context(), ids)
	if err != nil {
		h.fail(w, err)
		return
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].LastActivity > summaries[j].LastActivity })
	if len(summaries) > 100 {
		summaries = summaries[:100]
	}
	conversations := make([]map[string]any, 0, len(summaries))
	for _, summary := range summaries {
		conversations = append(conversations, map[string]any{"id": summary.ID, "type": summary.Type, "scopeId": summary.ScopeID, "turns": summary.Turns, "messages": summary.Messages, "userTurns": turnsBySession[summary.ID], "lastActivity": summary.LastActivity, "createdAt": summary.CreatedAt, "firstMessage": summary.FirstMessage, "lastMessage": summary.LastMessage})
	}
	files, err := h.files.ListOwned(r.Context(), personalScope, 200)
	if err != nil {
		h.fail(w, err)
		return
	}
	fileViews := make([]map[string]any, 0, len(files))
	for _, file := range files {
		fileViews = append(fileViews, map[string]any{"id": file.ID, "name": file.Name, "path": file.Path, "mimetype": file.Mimetype, "size": file.SizeBytes, "direction": file.Direction, "createdAt": file.CreatedAt, "openable": file.BlobKey != nil})
	}
	cronRecords, err := h.crons.List(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	cronViews := []map[string]any{}
	for _, record := range cronRecords {
		var document map[string]any
		if json.Unmarshal(record.JSON, &document) != nil || document["ownerScopeId"] != personalScope {
			continue
		}
		view := map[string]any{"id": record.ID}
		for _, key := range []string{"title", "action", "message", "owner", "createdBy", "enabled", "archived", "schedule", "createdAt", "lastFiredAt"} {
			if value, exists := document[key]; exists {
				view[key] = value
			}
		}
		cronViews = append(cronViews, view)
	}
	deployments, err := h.deployments.List(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	deploymentViews := []map[string]any{}
	for _, deployment := range deployments {
		if deployment.OwnerScopeID != personalScope {
			continue
		}
		view := map[string]any{"id": deployment.ID, "name": firstNonEmpty(deployment.DisplayName, deployment.Name), "status": deployment.Status, "currentVersion": deployment.CurrentVersion, "versions": len(deployment.Versions), "createdBy": deployment.CreatedBy}
		if deployment.LastAccessAt != nil {
			view["lastAccessAt"] = *deployment.LastAccessAt
		}
		if publicURL := deploymentPublicURL(deployment.Endpoint); publicURL != "" {
			view["publicUrl"] = publicURL
		}
		deploymentViews = append(deploymentViews, view)
	}
	soul, err := h.souls.Get(r.Context(), personalScope)
	if err != nil {
		h.fail(w, err)
		return
	}
	configSnapshot, err := h.userConfig.Snapshot(r.Context(), orgScope, personalScope)
	if err != nil {
		h.fail(w, err)
		return
	}
	memory, err := h.memory.Head(r.Context(), personalScope)
	if err != nil {
		h.fail(w, err)
		return
	}
	admin := map[string]any{"isAdmin": false}
	for _, grant := range grants {
		if samePrincipal(grant.PrincipalID, principalID) && grant.Role == "org_admin" {
			admin["isAdmin"], admin["role"], admin["scopeId"] = true, "org_admin", grant.ScopeID
			break
		}
	}
	response := map[string]any{
		"principalId":   principalID,
		"scopeId":       personalScope,
		"admin":         admin,
		"stats":         map[string]any{"sessions": len(sessionIDs), "turns": turns, "firstSeenAt": firstSeenAt, "lastSeenAt": lastSeenAt},
		"conversations": conversations,
		"files":         fileViews,
		"deployments":   deploymentViews,
		"crons":         cronViews,
		"config": map[string]any{
			"hasSoul":         soul != nil,
			"soulVersion":     soulVersion(soul),
			"securityPosture": configSnapshot.SecurityPosture,
			"commandPolicy":   jsonValue(configSnapshot.CommandPolicy),
			"egress":          jsonValue(configSnapshot.Egress),
			"baseModel":       configSnapshot.BaseModel,
			"connectors":      configSnapshot.Connectors,
		},
		"onboarding": onboardingStatus(memory.Content),
	}
	if member != nil && member.DisplayName != "" {
		response["displayName"] = member.DisplayName
	}
	writeJSON(w, http.StatusOK, response)
}

func soulVersion(soul *data.Soul) int {
	if soul == nil {
		return 0
	}
	return soul.Version
}

func onboardingStatus(memory string) string {
	for _, status := range []string{"completed", "dismissed", "pending"} {
		pattern := regexp.MustCompile(`(?im)(?:^|\n)\s*[-*]?\s*(?:\(\d{4}-\d\d-\d\d\)\s*)?Onboarding:\s*` + status + `\s+v2\b`)
		if pattern.MatchString(memory) {
			return status
		}
	}
	return "not_started"
}

func (h *HTTPServer) resetAdminUser(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, ok := h.requireOrgAdmin(w, r, identity)
	if !ok {
		return
	}
	principalID := adminUserResetID(r.URL.Path)
	if principalID == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	scope := "personal:" + principalID
	head, err := h.memory.Head(r.Context(), scope)
	if err != nil {
		h.fail(w, err)
		return
	}
	if err := h.memory.Replace(r.Context(), scope, setOnboardingStatus(head.Content, "not_started", time.Now().UTC().Format("2006-01-02")), actor); err != nil {
		h.fail(w, err)
		return
	}
	sessions, err := h.sessions.ListByParticipant(r.Context(), principalID)
	if err != nil {
		h.fail(w, err)
		return
	}
	deleted := 0
	for _, session := range sessions {
		if session.ScopeID != scope {
			continue
		}
		if err := h.sessions.Delete(r.Context(), session.ID); err != nil {
			h.fail(w, err)
			return
		}
		deleted++
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "user.reset", Resource: principalID + "/sessions=" + strconv.Itoa(deleted), ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "scopeId": scope, "deletedSessions": deleted})
}

// setOnboardingStatus maintains the v2 marker format written by Node while
// leaving the rest of the user's durable memory untouched.
func setOnboardingStatus(memory, status, today string) string {
	base := onboardingMarkerPattern.ReplaceAllString(memory, "")
	base = trailingLineSpacePattern.ReplaceAllString(base, "")
	base = threeNewlinesPattern.ReplaceAllString(base, "\n\n")
	base = strings.TrimSpace(base)
	if status == "not_started" {
		if base == "" {
			return ""
		}
		return base + "\n"
	}
	line := "- Onboarding: " + status + " v2 on " + today + "."
	if status == "pending" {
		line = "- Onboarding: pending v2 since " + today + "."
	}
	if base == "" {
		return line + "\n"
	}
	return base + "\n" + line + "\n"
}

func (h *HTTPServer) adminSessionLLM(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, scope, ok := h.requireScopedAdmin(w, r, identity)
	if !ok {
		return
	}
	id := adminSessionLLMID(r.URL.Path)
	session, err := h.sessions.Get(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if session == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	if !strings.HasPrefix(scope, "org:") && session.ScopeID != scope {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "session is outside the requested scope"})
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "session.llm.read", Resource: id, ScopeLabel: session.ScopeID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	turn := r.URL.Query().Get("turnSeq")
	var turnSeq *int
	orphans := false
	if turn != "" {
		if turn == "orphan" {
			orphans = true
		} else if value, parseErr := strconv.Atoi(turn); parseErr == nil {
			turnSeq = &value
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "turnSeq must be an integer or \"orphan\""})
			return
		}
	}
	requests, err := h.sessions.ListLLMRequests(r.Context(), id, turnSeq, orphans)
	if err != nil {
		h.fail(w, err)
		return
	}
	items := make([]map[string]any, 0, len(requests))
	for _, item := range requests {
		record := map[string]any{"id": item.ID, "sessionId": item.SessionID, "turnSeq": item.TurnSeq, "step": item.Step, "model": item.Model, "scopeLabel": item.ScopeLabel, "createdAt": item.CreatedAt, "truncated": item.Truncated, "ttftMs": item.TTFTMS, "durationMs": item.DurationMS, "stepGapMs": item.StepGapMS, "toolWallMs": jsonValue(item.ToolWallMS), "usage": jsonValue(item.Usage), "transport": jsonValue(item.Transport), "gapPhases": jsonValue(item.GapPhases)}
		if turnSeq != nil || orphans {
			record["request"] = jsonValue(item.Request)
		}
		items = append(items, record)
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": sessionRecordView(session), "requests": items})
}

func jsonValue(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return value
}

func sessionRecordView(session *data.SessionRecord) map[string]any {
	result := map[string]any{"id": session.ID, "type": session.Type, "scopeId": session.ScopeID, "threadRef": session.ThreadRef, "createdAt": session.CreatedAt}
	if session.Surface != nil {
		result["surface"] = *session.Surface
	}
	if session.Title != nil {
		result["title"] = *session.Title
	}
	if session.ChannelName != nil {
		result["channelName"] = *session.ChannelName
	}
	return result
}

func llmRequestView(item data.LLMRequest, includeRequest bool) map[string]any {
	record := map[string]any{"id": item.ID, "sessionId": item.SessionID, "turnSeq": item.TurnSeq, "step": item.Step, "model": item.Model, "scopeLabel": item.ScopeLabel, "createdAt": item.CreatedAt, "truncated": item.Truncated, "ttftMs": item.TTFTMS, "durationMs": item.DurationMS, "stepGapMs": item.StepGapMS, "toolWallMs": jsonValue(item.ToolWallMS), "usage": jsonValue(item.Usage), "transport": jsonValue(item.Transport), "gapPhases": jsonValue(item.GapPhases)}
	if includeRequest {
		record["request"] = jsonValue(item.Request)
	}
	return record
}

func jsonArray(raw json.RawMessage) any {
	if len(raw) == 0 {
		return []any{}
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return []any{}
	}
	if value == nil {
		return []any{}
	}
	return value
}

type deliveryProvenance struct {
	Trigger, FireKey, SourceSessionID, SourceThreadRef, SourceScopeID string
	SourceUserSeq, SourceAssistantEntrySeq                            *int
}

func parseDeliveryProvenance(raw json.RawMessage) deliveryProvenance {
	var value deliveryProvenance
	_ = json.Unmarshal(raw, &value)
	return value
}

func (h *HTTPServer) deliveryEventOrigin(raw json.RawMessage, idempotencyKey, requestedScope string) any {
	p := parseDeliveryProvenance(raw)
	fireKey := firstNonEmpty(p.FireKey, idempotencyKey)
	trigger := p.Trigger
	if trigger == "" {
		trigger = dataSessionOrigin(fireKey)
	}
	if trigger == "" || trigger == "conversation" {
		return nil
	}
	if trigger == "cron" {
		return adminSessionOrigin(fireKey, firstNonEmpty(p.SourceScopeID, requestedScope), requestedScope, h.crons)
	}
	parts := strings.Split(fireKey, ":")
	sourceID := any(nil)
	slot := any(nil)
	if len(parts) >= 2 {
		sourceID = parts[1]
		if len(parts) > 2 {
			slot = strings.Join(parts[2:], ":")
		}
	}
	label := map[string]string{"monitor": "Monitor wake", "webhook": "Webhook wake", "cron": "Cron"}[trigger]
	result := map[string]any{"kind": "background_wake", "label": label, "trigger": trigger, "fireKey": fireKey, "fireSlot": slot, "sourceId": sourceID}
	if p.SourceScopeID != "" {
		result["sourceScopeId"] = p.SourceScopeID
	}
	if p.SourceThreadRef != "" {
		result["sourceThreadRef"] = p.SourceThreadRef
	}
	return result
}

func (h *HTTPServer) transcriptEntries(ctx context.Context, session *data.SessionRecord, limit int) ([]map[string]any, bool, error) {
	all := limit >= 50000
	if all {
		limit = 50000
	}
	raw, err := h.sessions.Entries(ctx, session.ID, func() int {
		if all {
			return 0
		}
		return limit + 1
	}(), 0)
	if err != nil {
		return nil, false, err
	}
	hasMore := !all && len(raw) > limit
	if hasMore {
		raw = raw[len(raw)-limit:]
	}
	participants, err := h.sessions.ActiveParticipantIDs(ctx, session.ID)
	if err != nil {
		return nil, false, err
	}
	name := ""
	if len(participants) == 1 {
		members, err := h.directory.List(ctx)
		if err != nil {
			return nil, false, err
		}
		for _, member := range members {
			if member.PrincipalID == participants[0] {
				name = strings.TrimSpace(member.DisplayName)
				break
			}
		}
		if name == "" {
			name = participants[0]
		}
	}
	entries := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		if entry.Type == "soul" {
			continue
		}
		payload := jsonValue(entry.Payload)
		if entry.Type == "user" && name != "" {
			if object, ok := payload.(map[string]any); ok {
				if existing, ok := object["name"].(string); !ok || strings.TrimSpace(existing) == "" {
					copy := map[string]any{}
					for k, v := range object {
						copy[k] = v
					}
					copy["name"] = name
					payload = copy
				}
			}
		}
		entries = append(entries, map[string]any{"sessionId": entry.SessionID, "seq": entry.Sequence, "parentSeq": entry.ParentSequence, "type": entry.Type, "payload": payload, "scopeLabel": entry.ScopeLabel, "createdAt": entry.CreatedAt})
	}
	return entries, hasMore, nil
}

func (h *HTTPServer) recipientSidecar(ctx context.Context, p deliveryProvenance, requestedScope string) (map[string]any, error) {
	if p.SourceSessionID == "" && p.SourceThreadRef == "" {
		return nil, nil
	}
	var source *data.SessionRecord
	var err error
	if p.SourceSessionID != "" {
		source, err = h.sessions.Get(ctx, p.SourceSessionID)
	} else {
		source, err = h.sessions.GetByThread(ctx, p.SourceThreadRef)
	}
	if err != nil {
		return nil, err
	}
	if source == nil || (!strings.HasPrefix(requestedScope, "org:") && source.ScopeID != requestedScope) {
		return nil, nil
	}
	result := map[string]any{"sourceSession": sessionRecordView(source)}
	if p.SourceUserSeq == nil {
		return result, nil
	}
	entries, err := h.sessions.Entries(ctx, source.ID, 0, *p.SourceUserSeq)
	if err != nil {
		return nil, err
	}
	turns := map[int]bool{*p.SourceUserSeq: true}
	for _, entry := range entries {
		if entry.Type == "user" && (p.SourceAssistantEntrySeq == nil || entry.Sequence <= *p.SourceAssistantEntrySeq) {
			turns[entry.Sequence] = true
		}
	}
	ids := make([]int, 0, len(turns))
	for id := range turns {
		ids = append(ids, id)
	}
	requests, err := h.sessions.ListLLMRequestsForTurns(ctx, source.ID, ids)
	if err != nil {
		return nil, err
	}
	if len(requests) > 0 {
		views := make([]map[string]any, 0, len(requests))
		for _, request := range requests {
			views = append(views, llmRequestView(request, true))
		}
		result["llmRequests"] = views
	}
	return result, nil
}

func (h *HTTPServer) adminSessionDetail(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, scope, ok := h.requireScopedAdmin(w, r, identity)
	if !ok {
		return
	}
	id := adminSessionDetailID(r.URL.Path)
	session, err := h.sessions.Get(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if session == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	if !strings.HasPrefix(scope, "org:") && session.ScopeID != scope {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "session is outside the requested scope"})
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "session.read", Resource: id, ScopeLabel: session.ScopeID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	want := queryLimit(r.URL.Query().Get("limit"), 500, 50000)
	entries, hasMore, err := h.transcriptEntries(r.Context(), session, want)
	if err != nil {
		h.fail(w, err)
		return
	}
	recipient, err := h.deliveries.ListByRecipientThread(r.Context(), session.ThreadRef, 100)
	if err != nil {
		h.fail(w, err)
		return
	}
	source, err := h.deliveries.ListBySourceSession(r.Context(), session.ID, session.ThreadRef, 100)
	if err != nil {
		h.fail(w, err)
		return
	}
	events := make([]map[string]any, 0, len(recipient)+len(source))
	for _, delivery := range recipient {
		p := parseDeliveryProvenance(delivery.Provenance)
		event := map[string]any{"type": "principal_delivery", "deliveryId": delivery.ID, "text": delivery.Text, "attachments": jsonArray(delivery.Attachments), "destination": jsonValue(delivery.Destination), "createdAt": firstDeliveryTime(delivery), "idempotencyKey": delivery.IdempotencyKey, "provenance": jsonValue(delivery.Provenance), "origin": h.deliveryEventOrigin(delivery.Provenance, delivery.IdempotencyKey, scope)}
		sidecar, err := h.recipientSidecar(r.Context(), p, scope)
		if err != nil {
			h.fail(w, err)
			return
		}
		for key, value := range sidecar {
			event[key] = value
		}
		events = append(events, event)
	}
	for _, delivery := range source {
		event := map[string]any{"type": "outbound_delivery", "deliveryId": delivery.ID, "text": delivery.Text, "attachments": jsonArray(delivery.Attachments), "destination": jsonValue(delivery.Destination), "createdAt": firstDeliveryTime(delivery), "idempotencyKey": delivery.IdempotencyKey, "provenance": jsonValue(delivery.Provenance), "origin": h.deliveryEventOrigin(delivery.Provenance, delivery.IdempotencyKey, scope)}
		if delivery.RecipientThread != nil {
			event["recipientThreadRef"] = *delivery.RecipientThread
		}
		if delivery.DeliveredAt != nil {
			event["deliveredAt"] = *delivery.DeliveredAt
		}
		if delivery.Shadow {
			event["shadow"] = true
		}
		events = append(events, event)
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": sessionRecordView(session), "entries": entries, "deliveryEvents": events, "hasMore": hasMore, "limit": want, "origin": adminSessionOrigin(session.ThreadRef, session.ScopeID, scope, h.crons)})
}

func firstDeliveryTime(delivery data.PendingDelivery) int64 {
	if delivery.DeliveredAt != nil {
		return *delivery.DeliveredAt
	}
	return delivery.CreatedAt
}

func (h *HTTPServer) adminShadowDeliveries(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, scope, ok := h.requireScopedAdmin(w, r, identity)
	if !ok {
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "deliveries.shadow.read", Resource: "deliveries/shadow", ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	rows, err := h.deliveries.ListShadow(r.Context(), 200)
	if err != nil {
		h.fail(w, err)
		return
	}
	orgWide := strings.HasPrefix(scope, "org:")
	shadow := make([]map[string]any, 0, len(rows))
	for _, delivery := range rows {
		var provenance struct {
			Trigger         string `json:"trigger"`
			FireKey         string `json:"fireKey"`
			SourceScopeID   string `json:"sourceScopeId"`
			SourceThreadRef string `json:"sourceThreadRef"`
		}
		if len(delivery.Provenance) == 0 || json.Unmarshal(delivery.Provenance, &provenance) != nil || (!orgWide && provenance.SourceScopeID != scope) {
			continue
		}
		shadow = append(shadow, map[string]any{
			"deliveryId":     delivery.ID,
			"text":           delivery.Text,
			"destination":    delivery.Destination,
			"createdAt":      delivery.CreatedAt,
			"idempotencyKey": delivery.IdempotencyKey,
			"provenance":     delivery.Provenance,
			"shadow":         true,
			"origin":         h.deliveryOrigin(r.Context(), delivery, provenance, scope, orgWide),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"scopeId": scope, "shadow": shadow})
}

func (h *HTTPServer) deliveryOrigin(ctx context.Context, delivery data.ShadowDelivery, provenance struct {
	Trigger         string `json:"trigger"`
	FireKey         string `json:"fireKey"`
	SourceScopeID   string `json:"sourceScopeId"`
	SourceThreadRef string `json:"sourceThreadRef"`
}, scope string, orgWide bool) any {
	fireKey := provenance.FireKey
	if fireKey == "" {
		fireKey = delivery.IdempotencyKey
	}
	match := wakeFireKeyPattern.FindStringSubmatch(fireKey)
	trigger := provenance.Trigger
	if len(match) > 0 {
		trigger = match[1]
	}
	if trigger == "" || trigger == "conversation" {
		return nil
	}
	if trigger == "cron" && len(match) > 0 && match[1] == "cron" {
		origin := map[string]any{"kind": "cron", "label": "Cron", "cronId": match[2], "fireKey": fireKey, "fireSlot": nil, "cron": nil}
		if len(match) == 4 && match[3] != "" {
			origin["fireSlot"] = match[3]
		}
		if record, err := h.crons.Get(ctx, match[2]); err == nil && record != nil {
			var cron map[string]any
			if json.Unmarshal(record.JSON, &cron) == nil && (orgWide || cron["ownerScopeId"] == scope) {
				origin["cron"] = adminCronSummary(cron)
			}
		}
		return origin
	}
	label := "Background wake"
	switch strings.ToLower(trigger) {
	case "cron":
		label = "Cron"
	case "monitor":
		label = "Monitor wake"
	case "webhook":
		label = "Webhook wake"
	}
	origin := map[string]any{"kind": "background_wake", "label": label, "trigger": trigger, "fireKey": fireKey, "fireSlot": nil, "sourceId": nil}
	if len(match) > 0 {
		origin["sourceId"] = match[2]
		if len(match) == 4 && match[3] != "" {
			origin["fireSlot"] = match[3]
		}
	}
	if provenance.SourceScopeID != "" {
		origin["sourceScopeId"] = provenance.SourceScopeID
	}
	if provenance.SourceThreadRef != "" {
		origin["sourceThreadRef"] = provenance.SourceThreadRef
	}
	return origin
}

func adminCronSummary(cron map[string]any) map[string]any {
	result := map[string]any{}
	for _, key := range []string{"id", "ownerScopeId", "title", "action", "message", "owner", "createdBy", "enabled", "archived", "schedule", "createdAt", "lastFiredAt"} {
		if value, ok := cron[key]; ok {
			result[key] = value
		}
	}
	return result
}

func (h *HTTPServer) adminRetention(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "org:" + h.config.QM.OrgID
	}
	if !strings.HasPrefix(scope, "org:") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "retention is org-wide; request an org scope"})
		return
	}
	actor, ok := h.requireOrgAdmin(w, r, identity)
	if !ok {
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "retention.read", Resource: "retention", ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	report, err := h.retention.Report(r.Context(), time.Now().UnixMilli())
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scopeId": scope, "generatedAt": report.GeneratedAt, "active": report.Active, "newVsReturning": report.NewVsReturning, "cohorts": report.Cohorts, "perUser": report.PerUser, "totals": report.Totals, "attribution": report.Attribution, "note": report.Note})
}

func (h *HTTPServer) adminAmbientJudgments(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, ok := h.requireOrgAdmin(w, r, identity)
	if !ok {
		return
	}
	container := strings.TrimSpace(r.URL.Query().Get("container"))
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "ambient_judgments.read", Resource: firstNonEmpty(container, "all"), ScopeLabel: "org:" + h.config.QM.OrgID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	if id := positiveInt64(r.URL.Query().Get("id")); id != nil {
		judgment, err := h.ambientJudgments.Get(r.Context(), *id)
		if err != nil {
			h.fail(w, err)
			return
		}
		if judgment == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		meta, err := h.directory.Meta(r.Context())
		if err != nil {
			h.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"scopeId": "org:" + h.config.QM.OrgID, "judgment": judgment, "workspaceUrl": meta["workspaceUrl"]})
		return
	}
	limit := queryLimit(r.URL.Query().Get("limit"), 100, 200)
	judgments, err := h.ambientJudgments.List(r.Context(), data.AmbientJudgmentQuery{Container: container, Decisions: selectedValues(r.URL.Query().Get("decision"), "act", "ignore", "fastlane"), Before: positiveInt64(r.URL.Query().Get("before")), BeforeID: positiveInt64(r.URL.Query().Get("beforeId")), Limit: limit + 1})
	if err != nil {
		h.fail(w, err)
		return
	}
	counts, err := h.ambientJudgments.Counts(r.Context(), container)
	if err != nil {
		h.fail(w, err)
		return
	}
	hasMore := len(judgments) > limit
	if hasMore {
		judgments = judgments[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"scopeId": "org:" + h.config.QM.OrgID, "judgments": judgments, "counts": counts, "hasMore": hasMore, "limit": limit})
}

func (h *HTTPServer) adminAckEmojiPicks(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, ok := h.requireOrgAdmin(w, r, identity)
	if !ok {
		return
	}
	channel := strings.TrimSpace(r.URL.Query().Get("container"))
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "ack_emoji_picks.read", Resource: firstNonEmpty(channel, "all"), ScopeLabel: "org:" + h.config.QM.OrgID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	if id := positiveInt64(r.URL.Query().Get("id")); id != nil {
		pick, err := h.ackEmojiPicks.Get(r.Context(), *id)
		if err != nil {
			h.fail(w, err)
			return
		}
		if pick == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		meta, err := h.directory.Meta(r.Context())
		if err != nil {
			h.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"scopeId": "org:" + h.config.QM.OrgID, "pick": pick, "workspaceUrl": meta["workspaceUrl"]})
		return
	}
	limit := queryLimit(r.URL.Query().Get("limit"), 100, 200)
	picks, err := h.ackEmojiPicks.List(r.Context(), data.AckEmojiPickQuery{Channel: channel, Outcomes: selectedValues(r.URL.Query().Get("outcome"), "picked", "declined"), Before: positiveInt64(r.URL.Query().Get("before")), BeforeID: positiveInt64(r.URL.Query().Get("beforeId")), Limit: limit + 1})
	if err != nil {
		h.fail(w, err)
		return
	}
	counts, err := h.ackEmojiPicks.Counts(r.Context(), channel)
	if err != nil {
		h.fail(w, err)
		return
	}
	hasMore := len(picks) > limit
	if hasMore {
		picks = picks[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"scopeId": "org:" + h.config.QM.OrgID, "picks": picks, "counts": counts, "hasMore": hasMore, "limit": limit})
}

func positiveInt64(raw string) *int64 {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return nil
	}
	return &value
}

func queryLimit(raw string, fallback, maximum int) int {
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return fallback
	}
	return min(maximum, value)
}

func selectedValues(raw string, allowed ...string) []string {
	allowedSet := make(map[string]bool, len(allowed))
	for _, value := range allowed {
		allowedSet[value] = true
	}
	selected := []string{}
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); allowedSet[value] {
			selected = append(selected, value)
		}
	}
	return selected
}

func (h *HTTPServer) adminSlackMirror(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, ok := h.requireOrgAdmin(w, r, identity)
	if !ok {
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "slack_mirror.read", Resource: "slack-mirror", ScopeLabel: "org:" + h.config.QM.OrgID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	containers, err := h.surfaceCache.ListContainers(r.Context(), 500)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scopeId": "org:" + h.config.QM.OrgID, "containers": containers})
}

func (h *HTTPServer) adminSlackMirrorMessages(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, ok := h.requireOrgAdmin(w, r, identity)
	if !ok {
		return
	}
	container, query := strings.TrimSpace(r.URL.Query().Get("container")), strings.TrimSpace(r.URL.Query().Get("q"))
	if container == "" && query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "container or q required"})
		return
	}
	limit := 200
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil {
			limit = min(400, max(1, value))
		}
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "slack_mirror.messages.read", Resource: firstNonEmpty(container, "search:"+query), ScopeLabel: "org:" + h.config.QM.OrgID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	var messages []data.SurfaceMessage
	var err error
	mode := "timeline"
	if query != "" {
		mode = "search"
		messages, err = h.surfaceCache.Search(r.Context(), query, container, limit)
	} else {
		messages, err = h.surfaceCache.Timeline(r.Context(), container, r.URL.Query().Get("before"), limit+1)
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	messages, err = h.surfaceMessagesWithDirectoryNames(r.Context(), messages)
	if err != nil {
		h.fail(w, err)
		return
	}
	result := map[string]any{"scopeId": "org:" + h.config.QM.OrgID, "mode": mode, "messages": messages}
	if mode == "search" {
		result["query"] = query
	} else {
		hasMore := len(messages) > limit
		if hasMore {
			messages = messages[len(messages)-limit:]
			result["messages"] = messages
		}
		result["container"] = container
		result["hasMore"] = hasMore
		result["limit"] = limit
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *HTTPServer) surfaceMessagesWithDirectoryNames(ctx context.Context, messages []data.SurfaceMessage) ([]data.SurfaceMessage, error) {
	members, err := h.directory.List(ctx)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, member := range members {
		names[member.PrincipalID] = member.DisplayName
		if member.SlackID != "" {
			names[member.SlackID] = member.DisplayName
		}
	}
	for index := range messages {
		if messages[index].AuthorName == nil && messages[index].AuthorID != nil {
			if name := names[*messages[index].AuthorID]; name != "" {
				messages[index].AuthorName = &name
			}
		}
	}
	return messages, nil
}

func (h *HTTPServer) syncDirectory(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	var input struct {
		Members          *[]data.DirectoryMember  `json:"members"`
		Channels         *[]data.DirectoryChannel `json:"channels"`
		ChannelMembers   *[]data.DirectoryPair    `json:"channelMembers"`
		GroupMembers     *[]data.DirectoryPair    `json:"groupMembers"`
		WorkspaceURL     string                   `json:"workspaceUrl"`
		MembersSyncedAt  *int64                   `json:"membersSyncedAt"`
		ChannelsSyncedAt *int64                   `json:"channelsSyncedAt"`
		GroupsSyncedAt   *int64                   `json:"groupsSyncedAt"`
	}
	if !decodeJSON(w, raw, &input) {
		return
	}
	if input.Members == nil && input.Channels == nil && input.GroupMembers == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "members[], channels[], and/or groupMembers[] required"})
		return
	}
	if input.WorkspaceURL != "" {
		value := strings.TrimRight(input.WorkspaceURL, "/")
		if regexp.MustCompile(`^https://[^\s/]+$`).MatchString(value) {
			if err := h.directory.SetWorkspaceURL(r.Context(), value); err != nil {
				h.fail(w, err)
				return
			}
		}
	}
	err := h.directory.Sync(r.Context(), data.DirectoryUpdate{Members: input.Members, Channels: input.Channels, ChannelMembers: input.ChannelMembers, GroupMembers: input.GroupMembers, MembersSyncedAt: input.MembersSyncedAt, ChannelsSyncedAt: input.ChannelsSyncedAt, GroupsSyncedAt: input.GroupsSyncedAt})
	if err != nil {
		h.fail(w, err)
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: "source", Action: "directory.sync", Resource: "directory", ScopeLabel: "org:" + h.config.QM.OrgID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	result := map[string]any{"ok": true}
	if input.Members != nil {
		result["members"] = len(*input.Members)
	}
	if input.Channels != nil {
		result["channels"] = len(*input.Channels)
	}
	if input.GroupMembers != nil {
		result["groupMembers"] = len(*input.GroupMembers)
	}
	writeJSON(w, http.StatusOK, result)
	_ = identity
}

func (h *HTTPServer) directoryMeta(w http.ResponseWriter, r *http.Request) {
	result, err := h.directory.Meta(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *HTTPServer) resolveDirectory(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "q (a name to resolve) required"})
		return
	}
	matches, err := h.directory.Resolve(r.Context(), query)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"matches": matches})
}

func (h *HTTPServer) setPrincipalActive(w http.ResponseWriter, r *http.Request, active bool) {
	segments := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(segments) != 4 || segments[2] == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	id := segments[2]
	if err := h.directory.SetActive(r.Context(), id, active); err != nil {
		h.fail(w, err)
		return
	}
	action := "principal.deactivate"
	if active {
		action = "principal.reactivate"
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: id, Action: action, Resource: "principal", ScopeLabel: "org:" + h.config.QM.OrgID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "principalId": id, "active": active})
}

func (h *HTTPServer) authorizeScope(ctx context.Context, identity auth.Identity) bool {
	if identity.ScopeID == "" || identity.ScopeID == "personal:"+identity.ActorID || identity.ScopeID == "org:"+h.config.QM.OrgID {
		return true
	}
	// Node's authorizesCapabilityScope only revokes project-group capabilities.
	// Channel and ordinary group capabilities are issued after Node's own
	// membership checks and must not be rejected here merely because they are
	// not represented as web projects in the shared database.
	kind, ref := splitScopeID(identity.ScopeID)
	if kind != "group" || !strings.HasPrefix(ref, "web-project-") {
		return true
	}
	ok, err := h.projectRepo.HasScopeMembership(ctx, identity.ScopeID, identity.ActorID)
	return err == nil && ok
}

func (h *HTTPServer) listProjects(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	requested := strings.TrimSpace(r.URL.Query().Get("principalId"))
	principal, ok := capabilityPrincipal(w, identity, requested)
	if !ok {
		return
	}
	if principal == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "principalId required"})
		return
	}
	projects, err := h.projects.List(r.Context(), principal)
	if err != nil {
		h.fail(w, err)
		return
	}
	views := make([]map[string]any, 0, len(projects))
	for _, project := range projects {
		view, err := h.projectView(r.Context(), project)
		if err != nil {
			h.fail(w, err)
			return
		}
		views = append(views, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": views})
}

// projectView is the public Node-compatible project shape. The durable
// project record keeps principal ids; display names and current membership are
// resolved at response time from the synchronized directory and roster.
func (h *HTTPServer) projectView(ctx context.Context, project biz.Project) (map[string]any, error) {
	scopeID := biz.ProjectScopeID(project.ID)
	members := make([]map[string]string, 0, len(project.MemberIDs))
	memberIDs := make([]string, 0, len(project.MemberIDs))
	for _, principalID := range project.MemberIDs {
		member, err := h.directory.Get(ctx, principalID)
		if err != nil {
			return nil, err
		}
		current, err := h.projectRepo.HasScopeMembership(ctx, scopeID, principalID)
		if err != nil {
			return nil, err
		}
		if member == nil || member.Type != "internal" || !current {
			continue
		}
		memberIDs = append(memberIDs, principalID)
		members = append(members, map[string]string{"principalId": principalID, "displayName": firstNonEmpty(strings.TrimSpace(member.DisplayName), principalID)})
	}
	return map[string]any{"id": project.ID, "orgId": project.OrgID, "name": project.Name, "ownerId": project.OwnerID, "memberIds": memberIDs, "createdAt": project.CreatedAt, "updatedAt": project.UpdatedAt, "scopeId": scopeID, "members": members}, nil
}

func (h *HTTPServer) listContexts(w http.ResponseWriter, r *http.Request) {
	principalID := strings.TrimSpace(r.URL.Query().Get("principalId"))
	if principalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "principalId required"})
		return
	}
	type contextView struct {
		scopeID, kind, name string
		isPrivate           *bool
		sessionCount        int
		lastActivity        *int64
		project             any
	}
	personal := "personal:" + principalID
	byScope := map[string]*contextView{personal: {scopeID: personal, kind: "personal"}}
	internal, err := h.directory.IsInternal(r.Context(), principalID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if internal {
		channels, err := h.directory.ChannelsFor(r.Context(), principalID)
		if err != nil {
			h.fail(w, err)
			return
		}
		for _, channel := range channels {
			private := channel.IsPrivate
			byScope["channel:"+channel.ChannelID] = &contextView{scopeID: "channel:" + channel.ChannelID, kind: "channel", name: channel.Name, isPrivate: &private}
		}
		projects, err := h.projects.List(r.Context(), principalID)
		if err != nil {
			h.fail(w, err)
			return
		}
		for _, project := range projects {
			view, err := h.projectView(r.Context(), project)
			if err != nil {
				h.fail(w, err)
				return
			}
			byScope[biz.ProjectScopeID(project.ID)] = &contextView{scopeID: biz.ProjectScopeID(project.ID), kind: "group", name: project.Name, project: view}
		}
	}
	groups, err := h.directory.GroupIDsFor(r.Context(), principalID)
	if err != nil {
		h.fail(w, err)
		return
	}
	groupSet := map[string]bool{}
	for _, group := range groups {
		groupSet[group] = true
	}
	sessions, err := h.sessions.ListViewerSessions(r.Context(), principalID)
	if err != nil {
		h.fail(w, err)
		return
	}
	for _, session := range sessions {
		if strings.HasPrefix(session.ScopeID, "group:web-project-") && !h.viewerCanAccessSession(r.Context(), &session, principalID) {
			continue
		}
		kind, ref, found := strings.Cut(session.ScopeID, ":")
		if !found || (session.ScopeID != personal && kind != "channel" && kind != "group") {
			continue
		}
		ctx := byScope[session.ScopeID]
		if ctx == nil && kind == "group" && groupSet[ref] {
			ctx = &contextView{scopeID: session.ScopeID, kind: "group"}
			byScope[session.ScopeID] = ctx
		}
		if ctx == nil {
			continue
		}
		if ctx.name == "" && session.ChannelName != nil {
			ctx.name = *session.ChannelName
		}
		hasTitle := session.ParticipantTitle != nil && strings.TrimSpace(*session.ParticipantTitle) != "" || session.ParticipantTitle == nil && session.Title != nil && strings.TrimSpace(*session.Title) != ""
		if session.HasEntries || hasTitle {
			ctx.sessionCount++
			activity := session.LastActivity
			if ctx.lastActivity == nil || activity > *ctx.lastActivity {
				ctx.lastActivity = &activity
			}
		}
	}
	contexts := make([]*contextView, 0, len(byScope))
	for _, item := range byScope {
		contexts = append(contexts, item)
	}
	sort.SliceStable(contexts, func(i, j int) bool {
		if contexts[i].kind == "personal" || contexts[j].kind == "personal" {
			return contexts[i].kind == "personal"
		}
		var left, right int64
		if contexts[i].lastActivity != nil {
			left = *contexts[i].lastActivity
		}
		if contexts[j].lastActivity != nil {
			right = *contexts[j].lastActivity
		}
		return left > right
	})
	result := make([]map[string]any, 0, len(contexts))
	for _, item := range contexts {
		view := map[string]any{"scopeId": item.scopeID, "kind": item.kind, "name": any(nil), "sessionCount": item.sessionCount, "lastActivityAt": item.lastActivity}
		if item.name != "" {
			view["name"] = item.name
		}
		if item.isPrivate != nil {
			view["isPrivate"] = *item.isPrivate
		}
		if item.project != nil {
			view["project"] = item.project
		}
		result = append(result, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"contexts": result})
}

// listScopeResources mirrors Node's application-level scope projection. It is
// deliberately source-authenticated: the surface owns the principal selector,
// while this service reconstructs visibility from the synchronized directory,
// projects and resource ACLs.
func (h *HTTPServer) listScopeResources(w http.ResponseWriter, r *http.Request) {
	principalID := strings.TrimSpace(r.URL.Query().Get("principalId"))
	scopeID := strings.TrimSpace(r.URL.Query().Get("scope"))
	if principalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "principalId required"})
		return
	}
	if scopeID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "scope required"})
		return
	}
	projectMember, projectOwner, err := h.projectResourceAccess(r.Context(), principalID, scopeID)
	if err != nil {
		h.fail(w, err)
		return
	}
	visible := projectMember
	if !projectMember {
		visible, err = h.canAccessResourceScope(r.Context(), principalID, scopeID)
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	if !visible {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found", "message": "not a context you can see"})
		return
	}
	resourceScopes, err := h.resourceScopesForPrincipal(r.Context(), principalID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if projectMember {
		resourceScopes = appendUniqueStrings(resourceScopes, "personal:"+principalID, scopeID)
	}
	owned, err := h.files.ListOwnedInScope(r.Context(), resourceScopes, scopeID)
	if err != nil {
		h.fail(w, err)
		return
	}
	handles, err := h.acl.FileHandlesFor(r.Context(), resourceScopes)
	if err != nil {
		h.fail(w, err)
		return
	}
	shared, err := h.files.ResolveByOwnerPaths(r.Context(), handles)
	if err != nil {
		h.fail(w, err)
		return
	}
	ownedScopes := make(map[string]bool, len(resourceScopes))
	for _, candidate := range resourceScopes {
		ownedScopes[candidate] = true
	}
	files := append([]data.FileArtifact{}, owned...)
	for _, file := range shared {
		if !ownedScopes[file.OwnerScopeID] && file.CreatedInScope != nil && *file.CreatedInScope == scopeID {
			files = append(files, file)
		}
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].CreatedAt > files[j].CreatedAt })

	crons, err := h.crons.List(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	cronViews := make([]any, 0)
	for _, cron := range crons {
		var owner struct {
			OwnerScopeID string `json:"ownerScopeId"`
		}
		if json.Unmarshal(cron.JSON, &owner) == nil && owner.OwnerScopeID == scopeID {
			cronViews = append(cronViews, jsonValue(cron.JSON))
		}
	}
	deployments, err := h.deployments.List(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	deploymentViews := make([]map[string]any, 0)
	for _, deployment := range deployments {
		if deployment.OwnerScopeID != scopeID && (deployment.CreatedInScope == nil || *deployment.CreatedInScope != scopeID) {
			continue
		}
		permission, err := h.deploymentGitPermission(r.Context(), deployment, principalID)
		if err != nil {
			h.fail(w, err)
			return
		}
		if permission != "" {
			deploymentViews = append(deploymentViews, map[string]any{"id": deployment.ID, "name": firstNonEmpty(firstNonEmpty(deployment.DisplayName, deployment.Name), deployment.ID), "status": deployment.Status, "permission": permission, "currentVersion": deployment.CurrentVersion})
		}
	}
	skills, err := h.skills.List(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	skillViews := make([]map[string]any, 0)
	for _, skill := range skills {
		if skill.ScopeID == scopeID {
			skillViews = append(skillViews, map[string]any{"id": skill.ID, "name": skill.Manifest.Name, "description": skill.Manifest.Description, "status": skill.Status})
		}
	}
	manageable := projectOwner
	if !projectMember {
		manageable, err = h.canManageResourceScope(r.Context(), principalID, scopeID)
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	fileViews := make([]map[string]any, 0, len(files))
	memberships := h.projectFileMemberships(r.Context(), scopeID)
	for _, file := range files {
		view := fileListView(file)
		if membership, ok := memberships[file.ID]; ok {
			view["projectFile"] = membership
		}
		fileViews = append(fileViews, view)
	}
	response := map[string]any{"files": fileViews, "crons": cronViews, "deployments": deploymentViews, "skills": skillViews, "manageable": manageable}
	if projectID := projectIDFromScope(scopeID); projectID != "" && h.knowledgeAgent != nil && h.projectRepo != nil {
		project, lookupErr := h.projectRepo.Get(r.Context(), projectID)
		if lookupErr != nil {
			h.fail(w, lookupErr)
			return
		}
		if project != nil {
			status, statusErr := h.knowledgeAgent.Status(knowlega.ScopeRef{OrgID: project.OrgID, ExternalScopeID: scopeID, Kind: "project", Name: project.Name})
			if statusErr == nil {
				response["knowledge"] = projectKnowledgeStatusView(status)
			} else {
				response["knowledge"] = map[string]any{"status": "failed", "ready": false, "lastError": statusErr.Error()}
			}
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func projectKnowledgeStatusView(status knowlega.ScopeStatus) map[string]any {
	state := strings.TrimSpace(status.State)
	if state == "" {
		if status.Ready {
			state = agentservice.KnowledgeWorkspaceReady
		} else {
			state = agentservice.KnowledgeWorkspaceEmpty
		}
	}
	view := map[string]any{
		"status": state, "ready": status.Ready, "sourceCount": status.SourceCount, "wikiPageCount": status.WikiPageCount,
		"queue": map[string]int{"pending": status.Queue.Pending, "processing": status.Queue.Processing, "done": status.Queue.Done, "failed": status.Queue.Failed, "total": status.Queue.Total},
	}
	if strings.TrimSpace(status.LastError) != "" {
		view["lastError"] = status.LastError
	}
	if status.LastSuccessfulAt != nil {
		view["lastSuccessfulAt"] = status.LastSuccessfulAt.UTC().Format(time.RFC3339Nano)
	}
	return view
}

func (h *HTTPServer) projectResourceAccess(ctx context.Context, principalID, scopeID string) (member, owner bool, err error) {
	const prefix = "group:web-project-"
	if h.projectRepo == nil || !strings.HasPrefix(scopeID, prefix) {
		return false, false, nil
	}
	project, err := h.projectRepo.Get(ctx, strings.TrimPrefix(scopeID, prefix))
	if err != nil || project == nil {
		return false, false, err
	}
	for _, candidate := range project.MemberIDs {
		if sameSoulPerson(candidate, principalID) {
			member = true
			break
		}
	}
	return member, member && sameSoulPerson(project.OwnerID, principalID), nil
}

func (h *HTTPServer) openProjectKnowledgeDocument(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	projectID := projectKnowledgeDocumentID(r.URL.Path)
	principalID, ok := capabilityPrincipal(w, identity, strings.TrimSpace(r.URL.Query().Get("viewer")))
	if !ok {
		return
	}
	if principalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "viewer required"})
		return
	}
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "path required"})
		return
	}
	if h.knowledgeAgent == nil || h.projectRepo == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "knowledge_unavailable"})
		return
	}
	project, err := h.projectRepo.Get(r.Context(), projectID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if project == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	member, _, err := h.projectResourceAccess(r.Context(), principalID, biz.ProjectScopeID(projectID))
	if err != nil {
		h.fail(w, err)
		return
	}
	if !member {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	document, err := h.knowledgeAgent.Read(knowlega.ScopeRef{OrgID: project.OrgID, ExternalScopeID: biz.ProjectScopeID(project.ID), Kind: "project", Name: project.Name}, path)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	response := map[string]any{"path": document.Path, "title": document.Title, "kind": document.Kind, "content": document.Content}
	if h.projectFiles != nil {
		memberships, listErr := h.projectFiles.ListByProject(r.Context(), projectID)
		if listErr != nil {
			h.fail(w, listErr)
			return
		}
		for _, membership := range memberships {
			if membership.RawPath != nil && *membership.RawPath == document.Path {
				response["qmFileId"] = membership.FileID
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func appendUniqueStrings(values []string, additions ...string) []string {
	seen := make(map[string]bool, len(values)+len(additions))
	for _, value := range values {
		seen[value] = true
	}
	for _, value := range additions {
		if value != "" && !seen[value] {
			values = append(values, value)
			seen[value] = true
		}
	}
	return values
}

// listFilesForCapability is the Go projection of Node's GET /v1/files route.
// It returns metadata only; the configured Node durable-byte-store continues to
// own byte streaming at /v1/files/:id/content and upload handling.
func (h *HTTPServer) listFilesForCapability(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	principalID := firstNonEmpty(strings.TrimSpace(identity.ActorID), strings.TrimSpace(r.URL.Query().Get("viewer")))
	if principalID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "capability_required"})
		return
	}
	createdInScope := strings.TrimSpace(r.URL.Query().Get("scope"))
	if projectID := projectIDFromScope(createdInScope); projectID != "" {
		allowed, err := h.canAccessResourceScope(r.Context(), principalID, createdInScope)
		if err != nil {
			h.fail(w, err)
			return
		}
		if !allowed {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		files, err := h.files.ListForProject(r.Context(), projectID, fileListLimit(r.URL.Query().Get("limit")))
		if err != nil {
			h.fail(w, err)
			return
		}
		memberships := h.projectFileMemberships(r.Context(), createdInScope)
		views := make([]map[string]any, 0, len(files))
		for _, file := range files {
			view := fileListView(file)
			if membership, ok := memberships[file.ID]; ok {
				view["projectFile"] = membership
			}
			views = append(views, view)
		}
		writeJSON(w, http.StatusOK, map[string]any{"owned": views, "shared": []any{}})
		return
	}
	scopes, err := h.resourceScopesForPrincipal(r.Context(), principalID)
	if err != nil {
		h.fail(w, err)
		return
	}
	page, err := h.files.ListOwnedByScopes(
		r.Context(),
		scopes,
		fileListLimit(r.URL.Query().Get("limit")),
		r.URL.Query().Get("cursor"),
		strings.TrimSpace(r.URL.Query().Get("scope")),
	)
	if err != nil {
		h.fail(w, err)
		return
	}
	handles, err := h.acl.FileHandlesFor(r.Context(), scopes)
	if err != nil {
		h.fail(w, err)
		return
	}
	sharedRows, err := h.files.ResolveByOwnerPaths(r.Context(), handles)
	if err != nil {
		h.fail(w, err)
		return
	}
	ownedScopes := make(map[string]bool, len(scopes))
	for _, scope := range scopes {
		ownedScopes[scope] = true
	}
	memberships := h.projectFileMemberships(r.Context(), createdInScope)
	shared := make([]data.FileArtifact, 0, len(sharedRows))
	for _, file := range sharedRows {
		if ownedScopes[file.OwnerScopeID] || createdInScope != "" && (file.CreatedInScope == nil || *file.CreatedInScope != createdInScope) {
			continue
		}
		shared = append(shared, file)
	}
	sort.SliceStable(shared, func(i, j int) bool { return shared[i].UpdatedAt > shared[j].UpdatedAt })
	ownedViews := make([]map[string]any, 0, len(page.Files))
	for _, file := range page.Files {
		view := fileListView(file)
		if membership, ok := memberships[file.ID]; ok {
			view["projectFile"] = membership
		}
		ownedViews = append(ownedViews, view)
	}
	sharedViews := make([]map[string]any, 0, len(shared))
	for _, file := range shared {
		view := fileListView(file)
		if membership, ok := memberships[file.ID]; ok {
			view["projectFile"] = membership
		}
		sharedViews = append(sharedViews, view)
	}
	response := map[string]any{"owned": ownedViews, "shared": sharedViews}
	if page.NextCursor != "" {
		response["nextCursor"] = page.NextCursor
	}
	writeJSON(w, http.StatusOK, response)
}

// openFileForCapability serves the configured local docstore after applying an
// owner-scope or ACL-handle check. Source-authenticated Web UI requests may use
// the QM project membership persisted in the project document while the legacy
// directory projection is still being retired.
func (h *HTTPServer) openFileForCapability(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	principalID := firstNonEmpty(strings.TrimSpace(identity.ActorID), strings.TrimSpace(r.URL.Query().Get("viewer")))
	if principalID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "capability_required"})
		return
	}
	file, err := h.files.Get(r.Context(), fileContentID(r.URL.Path))
	if err != nil {
		h.fail(w, err)
		return
	}
	if file == nil || file.BlobKey == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	scopes, err := h.resourceScopesForPrincipal(r.Context(), principalID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if file.CreatedInScope != nil {
		projectScope := strings.TrimSpace(*file.CreatedInScope)
		member, _, err := h.projectResourceAccess(r.Context(), principalID, projectScope)
		if err != nil {
			h.fail(w, err)
			return
		}
		if member {
			scopes = appendUniqueStrings(scopes, "personal:"+principalID, projectScope)
		}
	}
	allowed := false
	for _, scope := range scopes {
		if scope == file.OwnerScopeID {
			allowed = true
			break
		}
	}
	if !allowed {
		grants, err := h.acl.List(r.Context(), file.OwnerScopeID, file.Path)
		if err != nil {
			h.fail(w, err)
			return
		}
		for _, grant := range grants {
			for _, scope := range scopes {
				if scope == grant.GranteeScopeID {
					allowed = true
					break
				}
			}
			if allowed {
				break
			}
		}
	}
	if !allowed {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	content, size, err := data.OpenLocalFileBlob(h.config.QM.FileStore.LocalDir, *file.BlobKey)
	if err != nil {
		h.fail(w, err)
		return
	}
	if content == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	defer content.Close()
	w.Header().Set("content-type", browserFileContentType(file.Mimetype))
	w.Header().Set("content-length", strconv.FormatInt(size, 10))
	w.Header().Set("content-disposition", "inline; filename*=UTF-8''"+url.PathEscape(file.Name))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, content)
}

func browserFileContentType(value string) string {
	contentType := strings.TrimSpace(value)
	if contentType == "" {
		return "application/octet-stream"
	}
	baseType := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	if strings.HasPrefix(baseType, "text/") && !strings.Contains(strings.ToLower(contentType), "charset=") {
		return contentType + "; charset=utf-8"
	}
	return contentType
}

const maxBlobTransferBytes int64 = 1_000_000_000

// serveLocalBlob implements Node's local Blob-transfer boundary without
// buffering uploads. File artifact metadata is deliberately a later concern:
// this endpoint only creates or reads an opaque, short-lived staged blob.
func (h *HTTPServer) serveLocalBlob(w http.ResponseWriter, r *http.Request) {
	direction, blobID := "write", ""
	if r.Method == http.MethodGet {
		direction, blobID = "read", blobTransferID(r.URL.Path)
	}
	if token := r.Header.Get(auth.CapabilityHeader); token != "" {
		claims, err := h.auth.VerifyBlobTransferCapability(token, direction, blobID)
		if err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "blob-transfer capability token not valid for this transfer"})
			return
		}
		identity := auth.Identity{ActorID: claims.ActorID, ScopeID: claims.ScopeID, Audience: claims.Audience, LiveActor: claims.LiveActor}
		// Channel/team and non-web-project group membership remains evaluated by
		// Node while that identity projection is being migrated. Do not consume
		// the streaming upload before handing it to the proxy.
		if proxySharedFileCapabilityScope(identity) {
			h.proxy.ServeHTTP(w, r)
			return
		}
		if !h.authorizeScope(r.Context(), identity) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "capability scope membership has been revoked"})
			return
		}
	} else {
		declaredSHA := ""
		if r.Method == http.MethodPost {
			declaredSHA = r.Header.Get("x-content-sha256")
			if h.auth.SourceSecret != "" && !sha256HexPattern.MatchString(declaredSHA) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "x-content-sha256 (hex sha-256) required"})
				return
			}
		}
		canonical := r.Method + "\n" + r.URL.RequestURI() + "\n" + declaredSHA
		if err := h.auth.VerifySourceCanonical(r.Context(), r, canonical, false); err != nil {
			status := auth.HTTPStatus(err)
			code := "unauthorized"
			if status == http.StatusForbidden {
				code = "forbidden"
			}
			writeJSON(w, status, map[string]string{"error": code, "message": err.Error()})
			return
		}
	}

	if r.Method == http.MethodGet {
		content, size, err := data.OpenLocalTransferBlob(h.config.QM.FileStore.TransferLocalDir, blobID)
		if err != nil {
			h.fail(w, err)
			return
		}
		if content == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		defer content.Close()
		w.Header().Set("content-type", "application/octet-stream")
		w.Header().Set("content-length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, content)
		return
	}

	declaredSHA := r.Header.Get("x-content-sha256")
	blobID, size, err := data.PutLocalTransferBlob(h.config.QM.FileStore.TransferLocalDir, r.Body, declaredSHA, maxBlobTransferBytes)
	if errors.Is(err, data.ErrLocalBlobTooLarge) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "payload_too_large", "message": err.Error()})
		return
	}
	if errors.Is(err, data.ErrLocalBlobHashMismatch) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "hash_mismatch", "message": err.Error()})
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"blobId": blobID, "sizeBytes": size})
}

// uploadFileFromLocalTransfer is the local-storage half of Node's file upload
// flow. Node can continue to stage the raw Blob-transfer upload while Go owns
// the durable docstore write, metadata, and optional shared-scope ACL grant.
func (h *HTTPServer) uploadFileFromLocalTransfer(w http.ResponseWriter, r *http.Request, raw []byte) {
	var input struct {
		PrincipalID string `json:"principalId"`
		BlobID      string `json:"blobId"`
		Name        string `json:"name"`
		Mimetype    string `json:"mimetype"`
		ScopeID     string `json:"scopeId"`
	}
	if !decodeJSON(w, raw, &input) {
		return
	}
	principalID, blobID := strings.TrimSpace(input.PrincipalID), strings.TrimSpace(input.BlobID)
	if principalID == "" || blobID == "" || input.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "principalId, blobId, and name required"})
		return
	}
	ownerScope := "personal:" + principalID
	createdScope := strings.TrimSpace(input.ScopeID)
	if createdScope == "" {
		createdScope = ownerScope
	}
	staged, _, err := data.OpenLocalTransferBlob(h.config.QM.FileStore.TransferLocalDir, blobID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if staged == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found", "message": "staged blob not found"})
		return
	}
	defer staged.Close()
	defer func() { _ = data.DeleteLocalTransferBlob(h.config.QM.FileStore.TransferLocalDir, blobID) }()
	allowed, err := h.canAccessResourceScope(r.Context(), principalID, createdScope)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !allowed {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "you can only upload to your own contexts"})
		return
	}
	name := safeUploadFileName(input.Name)
	blobKey, size, err := data.PutLocalFileBlob(h.config.QM.FileStore.LocalDir, staged, 1_000_000_000)
	if errors.Is(err, data.ErrLocalFileTooLarge) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "payload_too_large", "message": err.Error()})
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	id, err := data.NewFileArtifactID()
	if err != nil {
		h.fail(w, err)
		return
	}
	mimetype := strings.ToLower(strings.TrimSpace(strings.Split(input.Mimetype, ";")[0]))
	if mimetype == "" {
		mimetype = uploadFileMimetype(name)
	}
	createdAt := time.Now().UnixMilli()
	createdInScope := createdScope
	file, err := h.files.Create(r.Context(), data.FileArtifact{
		ID:             id,
		OwnerScopeID:   ownerScope,
		CreatedBy:      principalID,
		Name:           name,
		Path:           "artifacts/" + id + "/" + name,
		Mimetype:       mimetype,
		SizeBytes:      size,
		BlobKey:        &blobKey,
		SHA256:         strings.TrimPrefix(blobKey, "files/"),
		Direction:      "in",
		CreatedInScope: &createdInScope,
		CreatedAt:      createdAt,
		UpdatedAt:      createdAt,
	})
	if err != nil {
		h.fail(w, err)
		return
	}
	if createdScope != ownerScope {
		if err := h.acl.Put(r.Context(), data.Grant{OwnerScopeID: ownerScope, Path: file.Path, GranteeScopeID: createdScope, Permission: "read", GrantedBy: principalID}); err != nil {
			h.fail(w, err)
			return
		}
	}
	// Node records this audit event best-effort after the durable artifact write.
	_ = h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: principalID, Action: "file.upload", Resource: file.Path, ScopeLabel: createdScope, IdempotencyKey: requestID(r)})
	membership := h.enqueueKnowledgeFile(r.Context(), *file, createdScope, principalID)
	response := map[string]any{"file": fileListView(*file)}
	if membership != nil {
		response["projectFile"] = membership
	}
	writeJSON(w, http.StatusOK, response)
}

const knowledgeFileMaxBytes = 32 << 20

func (h *HTTPServer) enqueueKnowledgeFile(ctx context.Context, file data.FileArtifact, scopeID, actor string) *data.ProjectFileMembership {
	const projectScopePrefix = "group:web-project-"
	if h.knowledgeAgent == nil || h.projectFiles == nil || file.BlobKey == nil || !strings.HasPrefix(scopeID, projectScopePrefix) {
		return nil
	}
	projectID := strings.TrimPrefix(scopeID, projectScopePrefix)
	projectName := ""
	if h.projectRepo != nil {
		project, lookupErr := h.projectRepo.Get(ctx, projectID)
		if lookupErr != nil {
			_ = h.audit.Record(ctx, data.AuditEvent{PrincipalID: actor, Action: "project.file.enqueue_failed", Resource: file.Path, ScopeLabel: scopeID, Detail: lookupErr.Error()})
			return nil
		}
		if project != nil {
			projectName = project.Name
		}
	}
	ref := knowlega.ScopeRef{OrgID: h.config.QM.OrgID, ExternalScopeID: scopeID, Kind: "project", Name: projectName}
	status, err := h.knowledgeAgent.EnsureScope(ctx, ref)
	if err != nil {
		_ = h.audit.Record(ctx, data.AuditEvent{PrincipalID: actor, Action: "project.file.enqueue_failed", Resource: file.Path, ScopeLabel: scopeID, Detail: err.Error()})
		return nil
	}
	if err := h.persistKnowledgeScope(ctx, ref, status); err != nil {
		_ = h.audit.Record(ctx, data.AuditEvent{PrincipalID: actor, Action: "project.file.enqueue_failed", Resource: file.Path, ScopeLabel: scopeID, Detail: err.Error()})
		return nil
	}
	defer func() {
		if err := h.refreshKnowledgeScope(ctx, ref); err != nil {
			_ = h.audit.Record(ctx, data.AuditEvent{PrincipalID: actor, Action: "project.knowledge.status_refresh_failed", Resource: file.Path, ScopeLabel: scopeID, Detail: err.Error()})
		}
	}()
	sha := strings.TrimSpace(file.SHA256)
	if sha == "" {
		sha = strings.TrimPrefix(*file.BlobKey, "files/")
	}
	membership, err := h.projectFiles.PutQueued(ctx, data.ProjectFileMembership{ProjectID: projectID, ProjectScopeID: scopeID, FileID: file.ID, KnowledgeProjectID: status.ProjectID, SourceSHA256: sha})
	if err != nil {
		_ = h.audit.Record(ctx, data.AuditEvent{PrincipalID: actor, Action: "project.file.enqueue_failed", Resource: file.Path, ScopeLabel: scopeID, Detail: err.Error()})
		return nil
	}
	if !supportedKnowledgeFile(file.Name) {
		detail := "supported knowledge file extensions are .txt, .md, .pdf, and .docx"
		_ = h.projectFiles.SetState(ctx, projectID, file.ID, "unsupported", nil, nil, 0, &detail)
		membership.Status = "unsupported"
		membership.LastError = &detail
		return &membership
	}
	content, size, err := data.OpenLocalFileBlob(h.config.QM.FileStore.LocalDir, *file.BlobKey)
	if err != nil || content == nil {
		if err == nil {
			err = errors.New("file blob not found")
		}
		_ = h.projectFiles.SetState(ctx, projectID, file.ID, "failed", nil, nil, 0, stringPointer(err.Error()))
		_ = h.audit.Record(ctx, data.AuditEvent{PrincipalID: actor, Action: "project.file.enqueue_failed", Resource: file.Path, ScopeLabel: scopeID, Detail: err.Error()})
		membership.Status = "failed"
		membership.LastError = stringPointer(err.Error())
		return &membership
	}
	defer content.Close()
	if size > knowledgeFileMaxBytes {
		detail := "file exceeds knowledge ingestion limit"
		_ = h.projectFiles.SetState(ctx, projectID, file.ID, "failed", nil, nil, 0, &detail)
		_ = h.audit.Record(ctx, data.AuditEvent{PrincipalID: actor, Action: "project.file.enqueue_failed", Resource: file.Path, ScopeLabel: scopeID, Detail: detail})
		membership.Status = "failed"
		membership.LastError = &detail
		return &membership
	}
	dataBytes, err := io.ReadAll(io.LimitReader(content, knowledgeFileMaxBytes+1))
	if err != nil {
		_ = h.projectFiles.SetState(ctx, projectID, file.ID, "failed", nil, nil, 0, stringPointer(err.Error()))
		_ = h.audit.Record(ctx, data.AuditEvent{PrincipalID: actor, Action: "project.file.enqueue_failed", Resource: file.Path, ScopeLabel: scopeID, Detail: err.Error()})
		membership.Status = "failed"
		membership.LastError = stringPointer(err.Error())
		return &membership
	}
	if int64(len(dataBytes)) > knowledgeFileMaxBytes {
		detail := "file exceeds knowledge ingestion limit"
		_ = h.projectFiles.SetState(ctx, projectID, file.ID, "failed", nil, nil, 0, &detail)
		_ = h.audit.Record(ctx, data.AuditEvent{PrincipalID: actor, Action: "project.file.enqueue_failed", Resource: file.Path, ScopeLabel: scopeID, Detail: detail})
		membership.Status = "failed"
		membership.LastError = &detail
		return &membership
	}
	task, err := h.knowledgeAgent.EnqueueQMFile(ref, file.ID, file.Name, sha, dataBytes)
	if err != nil {
		_ = h.projectFiles.SetState(ctx, projectID, file.ID, "failed", nil, nil, 0, stringPointer(err.Error()))
		_ = h.audit.Record(ctx, data.AuditEvent{PrincipalID: actor, Action: "project.file.enqueue_failed", Resource: file.Path, ScopeLabel: scopeID, Detail: err.Error()})
		membership.Status = "failed"
		membership.LastError = stringPointer(err.Error())
		return &membership
	}
	rawPath, relErr := filepath.Rel(status.ProjectPath, filepath.FromSlash(task.SourcePath))
	if relErr != nil {
		rawPath = task.SourcePath
	}
	rawPath = filepath.ToSlash(rawPath)
	_ = h.projectFiles.SetState(ctx, projectID, file.ID, "queued", &rawPath, &task.ID, 0, nil)
	membership.RawPath = &rawPath
	membership.QueueTaskID = &task.ID
	return &membership
}

func supportedKnowledgeFile(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".txt", ".md", ".pdf", ".docx":
		return true
	default:
		return false
	}
}

func stringPointer(value string) *string { return &value }

func (h *HTTPServer) attachProjectFile(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	projectID := projectFileCollectionID(r.URL.Path)
	var input struct {
		PrincipalID string `json:"principalId"`
		FileID      string `json:"fileId"`
	}
	if !decodeJSON(w, raw, &input) {
		return
	}
	principalID := firstNonEmpty(strings.TrimSpace(identity.ActorID), strings.TrimSpace(input.PrincipalID))
	if principalID == "" || strings.TrimSpace(input.FileID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "principalId and fileId required"})
		return
	}
	scopeID := biz.ProjectScopeID(projectID)
	allowed, err := h.canManageResourceScope(r.Context(), principalID, scopeID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !allowed {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	file, err := h.files.Get(r.Context(), strings.TrimSpace(input.FileID))
	if err != nil {
		h.fail(w, err)
		return
	}
	if file == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	readable, err := h.canReadFile(r.Context(), principalID, *file)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !readable {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	if file.OwnerScopeID != scopeID {
		if err := h.acl.Put(r.Context(), data.Grant{OwnerScopeID: file.OwnerScopeID, Path: file.Path, GranteeScopeID: scopeID, Permission: "read", GrantedBy: principalID}); err != nil {
			h.fail(w, err)
			return
		}
	}
	membership := h.enqueueKnowledgeFile(r.Context(), *file, scopeID, principalID)
	if membership == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "not_configured", "message": "project file ingestion is unavailable"})
		return
	}
	_ = h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: principalID, Action: "project.file.attach", Resource: file.Path, ScopeLabel: scopeID, IdempotencyKey: requestID(r)})
	writeJSON(w, http.StatusAccepted, map[string]any{"file": fileListView(*file), "projectFile": membership})
}

func (h *HTTPServer) retryProjectFile(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	projectID, fileID := projectFileRetryID(r.URL.Path)
	principalID := projectFilePrincipal(raw, identity)
	scopeID := biz.ProjectScopeID(projectID)
	allowed, err := h.canManageResourceScope(r.Context(), principalID, scopeID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if principalID == "" || !allowed {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	membership, err := h.projectFiles.Find(r.Context(), projectID, fileID)
	if err != nil {
		h.fail(w, err)
		return
	}
	file, err := h.files.Get(r.Context(), fileID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if membership == nil || file == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	updated := h.enqueueKnowledgeFile(r.Context(), *file, scopeID, principalID)
	if updated == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "not_configured"})
		return
	}
	_ = h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: principalID, Action: "project.file.retry", Resource: file.Path, ScopeLabel: scopeID, IdempotencyKey: requestID(r)})
	writeJSON(w, http.StatusAccepted, map[string]any{"file": fileListView(*file), "projectFile": updated})
}

func (h *HTTPServer) removeProjectFile(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	projectID, fileID := projectFileID(r.URL.Path)
	principalID := projectFilePrincipal(raw, identity)
	scopeID := biz.ProjectScopeID(projectID)
	allowed, err := h.canManageResourceScope(r.Context(), principalID, scopeID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if principalID == "" || !allowed {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	membership, err := h.projectFiles.Find(r.Context(), projectID, fileID)
	if err != nil {
		h.fail(w, err)
		return
	}
	file, err := h.files.Get(r.Context(), fileID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if membership == nil || file == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	projectName := ""
	if h.projectRepo != nil {
		project, lookupErr := h.projectRepo.Get(r.Context(), projectID)
		if lookupErr != nil {
			h.fail(w, lookupErr)
			return
		}
		if project != nil {
			projectName = project.Name
		}
	}
	ref := knowlega.ScopeRef{OrgID: h.config.QM.OrgID, ExternalScopeID: scopeID, Kind: "project", Name: projectName}
	if err := h.projectFiles.SetState(r.Context(), projectID, fileID, "removing", nil, membership.QueueTaskID, membership.GeneratedPageCount, nil); err != nil {
		h.fail(w, err)
		return
	}
	var cleanup any = map[string]any{}
	if membership.RawPath != nil && strings.TrimSpace(*membership.RawPath) != "" {
		taskID := ""
		if membership.QueueTaskID != nil {
			taskID = *membership.QueueTaskID
		}
		result, err := h.knowledgeAgent.RemoveQMFile(ref, *membership.RawPath, taskID)
		if err != nil {
			message := err.Error()
			_ = h.projectFiles.SetState(r.Context(), projectID, fileID, "failed", membership.RawPath, membership.QueueTaskID, membership.GeneratedPageCount, &message)
			h.fail(w, err)
			return
		}
		if err := h.knowledgeAgent.SyncWiki(r.Context(), ref); err != nil {
			message := err.Error()
			_ = h.projectFiles.SetState(r.Context(), projectID, fileID, "failed", membership.RawPath, membership.QueueTaskID, membership.GeneratedPageCount, &message)
			h.fail(w, err)
			return
		}
		cleanup = result
	}
	if err := h.acl.Revoke(r.Context(), file.OwnerScopeID, file.Path, scopeID); err != nil {
		message := err.Error()
		_ = h.projectFiles.SetState(r.Context(), projectID, fileID, "failed", membership.RawPath, membership.QueueTaskID, membership.GeneratedPageCount, &message)
		h.fail(w, err)
		return
	}
	if err := h.projectFiles.Delete(r.Context(), projectID, fileID); err != nil {
		h.fail(w, err)
		return
	}
	remaining, err := h.projectFiles.ListByFile(r.Context(), fileID)
	if err != nil {
		h.fail(w, err)
		return
	}
	fileDeleted := false
	if len(remaining) == 0 {
		if err := h.acl.DeleteResource(r.Context(), file.OwnerScopeID, file.Path); err != nil {
			h.fail(w, err)
			return
		}
		blobKey := ""
		if file.BlobKey != nil {
			blobKey = *file.BlobKey
		}
		if err := h.files.Delete(r.Context(), file.ID); err != nil {
			h.fail(w, err)
			return
		}
		if blobKey != "" {
			references, err := h.files.CountBlobReferences(r.Context(), blobKey)
			if err != nil {
				h.fail(w, err)
				return
			}
			if references == 0 {
				if err := data.DeleteLocalFileBlob(h.config.QM.FileStore.LocalDir, blobKey); err != nil {
					h.fail(w, err)
					return
				}
			}
		}
		fileDeleted = true
	}
	if err := h.refreshKnowledgeScope(r.Context(), ref); err != nil {
		_ = h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: principalID, Action: "project.knowledge.status_refresh_failed", Resource: file.Path, ScopeLabel: scopeID, Detail: err.Error()})
	}
	_ = h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: principalID, Action: "project.file.remove", Resource: file.Path, ScopeLabel: scopeID, IdempotencyKey: requestID(r)})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "fileDeleted": fileDeleted, "cleanup": cleanup})
}

func projectFilePrincipal(raw []byte, identity auth.Identity) string {
	if strings.TrimSpace(identity.ActorID) != "" {
		return strings.TrimSpace(identity.ActorID)
	}
	var input struct {
		PrincipalID string `json:"principalId"`
	}
	_ = json.Unmarshal(raw, &input)
	return strings.TrimSpace(input.PrincipalID)
}

func (h *HTTPServer) canReadFile(ctx context.Context, principalID string, file data.FileArtifact) (bool, error) {
	scopes, err := h.resourceScopesForPrincipal(ctx, principalID)
	if err != nil {
		return false, err
	}
	for _, scopeID := range scopes {
		if scopeID == file.OwnerScopeID {
			return true, nil
		}
	}
	grants, err := h.acl.List(ctx, file.OwnerScopeID, file.Path)
	if err != nil {
		return false, err
	}
	for _, grant := range grants {
		for _, scopeID := range scopes {
			if grant.GranteeScopeID == scopeID {
				return true, nil
			}
		}
	}
	return false, nil
}

func projectIDFromScope(scopeID string) string {
	const prefix = "group:web-project-"
	if !strings.HasPrefix(scopeID, prefix) {
		return ""
	}
	return strings.TrimPrefix(scopeID, prefix)
}

func safeUploadFileName(name string) string {
	base := strings.TrimSpace(path.Base(strings.ReplaceAll(name, "\\", "/")))
	if base == "" || strings.Trim(base, ".") == "" {
		return "file"
	}
	return base
}

func uploadFileMimetype(name string) string {
	extension := strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
	if mimetype, ok := map[string]string{
		"txt": "text/plain", "md": "text/markdown", "csv": "text/csv", "json": "application/json", "html": "text/html", "xml": "application/xml", "yaml": "application/yaml", "yml": "application/yaml", "pdf": "application/pdf", "png": "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg", "gif": "image/gif", "svg": "image/svg+xml", "zip": "application/zip",
	}[extension]; ok {
		return mimetype
	}
	return "application/octet-stream"
}

func fileListLimit(raw string) int {
	if raw == "" {
		return 50
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return 50
	}
	value = math.Floor(value)
	if value > 200 {
		return 200
	}
	return int(value)
}

func fileListView(file data.FileArtifact) map[string]any {
	view := map[string]any{
		"id":           file.ID,
		"ownerScopeId": file.OwnerScopeID,
		"name":         file.Name,
		"mimetype":     file.Mimetype,
		"sizeBytes":    file.SizeBytes,
		"direction":    file.Direction,
		"createdAt":    file.CreatedAt,
		"openable":     file.BlobKey != nil,
	}
	if file.CreatedInScope != nil {
		view["createdInScope"] = *file.CreatedInScope
	}
	return view
}

func (h *HTTPServer) projectFileMemberships(ctx context.Context, scopeID string) map[string]data.ProjectFileMembership {
	const prefix = "group:web-project-"
	result := map[string]data.ProjectFileMembership{}
	if h.projectFiles == nil || !strings.HasPrefix(scopeID, prefix) {
		return result
	}
	items, err := h.projectFiles.ListByProject(ctx, strings.TrimPrefix(scopeID, prefix))
	if err != nil {
		return result
	}
	for _, item := range items {
		result[item.FileID] = item
	}
	return result
}

func (h *HTTPServer) resourceScopesForPrincipal(ctx context.Context, principalID string) ([]string, error) {
	internal, err := h.directory.IsInternal(ctx, principalID)
	if err != nil || !internal {
		return []string{}, err
	}
	scopes := map[string]bool{"personal:" + principalID: true, "org:" + h.config.QM.OrgID: true}
	sessions, err := h.sessions.ListViewerSessions(ctx, principalID)
	if err != nil {
		return nil, err
	}
	historical := map[string]bool{}
	for _, session := range sessions {
		historical[session.ScopeID] = true
	}
	channels, err := h.directory.ChannelsFor(ctx, principalID)
	if err != nil {
		return nil, err
	}
	for _, channel := range channels {
		scope := "channel:" + channel.ChannelID
		if channel.IsPrivate || historical[scope] {
			scopes[scope] = true
		}
	}
	groups, err := h.directory.GroupIDsFor(ctx, principalID)
	if err != nil {
		return nil, err
	}
	for _, groupID := range groups {
		scopes["group:"+groupID] = true
	}
	projects, err := h.projects.List(ctx, principalID)
	if err != nil {
		return nil, err
	}
	for _, project := range projects {
		scopes[biz.ProjectScopeID(project.ID)] = true
	}
	result := make([]string, 0, len(scopes))
	for scope := range scopes {
		result = append(result, scope)
	}
	return result, nil
}

func (h *HTTPServer) canAccessResourceScope(ctx context.Context, principalID, scopeID string) (bool, error) {
	internal, err := h.directory.IsInternal(ctx, principalID)
	if err != nil || !internal {
		return false, err
	}
	kind, ref := splitScopeID(scopeID)
	switch kind {
	case "personal":
		return sameSoulPerson(ref, principalID), nil
	case "org":
		return true, nil
	case "group":
		if strings.HasPrefix(ref, "web-project-") {
			return h.projectRepo.HasScopeMembership(ctx, scopeID, principalID)
		}
		return h.directory.IsScopeMember(ctx, kind, ref, principalID)
	case "channel":
		channel, err := h.directory.Channel(ctx, ref)
		if err != nil || channel == nil {
			return false, err
		}
		if !channel.IsPrivate {
			return true, nil
		}
		return h.directory.IsScopeMember(ctx, kind, ref, principalID)
	default:
		return false, nil
	}
}

func (h *HTTPServer) canWriteResourceScope(ctx context.Context, principalID, scopeID string) (bool, error) {
	kind, ref := splitScopeID(scopeID)
	if kind == "org" {
		return h.directory.IsInternal(ctx, principalID)
	}
	if kind == "personal" {
		return sameSoulPerson(ref, principalID), nil
	}
	if kind == "group" {
		return h.canAccessResourceScope(ctx, principalID, scopeID)
	}
	if kind != "channel" {
		return false, nil
	}
	channel, err := h.directory.Channel(ctx, ref)
	if err != nil || channel == nil || !channel.IsPrivate {
		return false, err
	}
	return h.directory.IsScopeMember(ctx, kind, ref, principalID)
}

func (h *HTTPServer) canManageResourceScope(ctx context.Context, principalID, scopeID string) (bool, error) {
	kind, _ := splitScopeID(scopeID)
	if kind != "personal" && kind != "channel" && kind != "group" {
		return false, nil
	}
	return h.canWriteResourceScope(ctx, principalID, scopeID)
}

func (h *HTTPServer) managesArtifactHome(ctx context.Context, homeScopeID, createdBy, principalID string) (bool, error) {
	manageable, err := h.canManageResourceScope(ctx, principalID, homeScopeID)
	if err != nil || manageable {
		return manageable, err
	}
	if !sameSoulPerson(createdBy, principalID) {
		return false, nil
	}
	kind, ref := splitScopeID(homeScopeID)
	if kind == "personal" {
		return true, nil
	}
	if kind != "channel" {
		return false, nil
	}
	channel, err := h.directory.Channel(ctx, ref)
	return err == nil && channel != nil && !channel.IsPrivate, err
}

func (h *HTTPServer) deploymentGitPermission(ctx context.Context, deployment data.Deployment, principalID string) (string, error) {
	managed, err := h.managesArtifactHome(ctx, deployment.OwnerScopeID, deployment.CreatedBy, principalID)
	if err != nil {
		return "", err
	}
	if managed {
		return "write", nil
	}
	if deployment.CreatedInScope != nil {
		kind, _ := splitScopeID(*deployment.CreatedInScope)
		if kind == "channel" || kind == "team" {
			writable, err := h.canWriteResourceScope(ctx, principalID, *deployment.CreatedInScope)
			if err != nil {
				return "", err
			}
			if writable {
				return "write", nil
			}
		}
	}
	canRead := false
	if kind, _ := splitScopeID(deployment.OwnerScopeID); kind == "org" {
		canRead, err = h.canAccessResourceScope(ctx, principalID, deployment.OwnerScopeID)
		if err != nil {
			return "", err
		}
	}
	grants, err := h.acl.List(ctx, deployment.OwnerScopeID, "deployment:"+deployment.ID)
	if err != nil {
		return "", err
	}
	for _, grant := range grants {
		if grant.Permission != "read" && grant.Permission != "write" {
			continue
		}
		allowed, err := h.canAccessResourceScope(ctx, principalID, grant.GranteeScopeID)
		if err != nil {
			return "", err
		}
		if !allowed {
			continue
		}
		if grant.Permission == "write" {
			writable, err := h.canWriteResourceScope(ctx, principalID, grant.GranteeScopeID)
			if err != nil {
				return "", err
			}
			if writable || strings.HasPrefix(grant.GranteeScopeID, "channel:") {
				return "write", nil
			}
		}
		canRead = true
	}
	if canRead {
		return "read", nil
	}
	allowed, err := h.canAccessResourceScope(ctx, principalID, deployment.OwnerScopeID)
	if err != nil || !allowed {
		return "", err
	}
	return "read", nil
}

func (h *HTTPServer) createGrant(w http.ResponseWriter, r *http.Request, raw []byte) {
	var input struct {
		OwnerScopeID   *string `json:"ownerScopeId"`
		Ref            *string `json:"ref"`
		GranteeScopeID *string `json:"granteeScopeId"`
		Permission     *string `json:"permission"`
		GrantedBy      *string `json:"grantedBy"`
	}
	if !decodeJSON(w, raw, &input) {
		return
	}
	if input.OwnerScopeID == nil || input.Ref == nil || input.GranteeScopeID == nil || input.GrantedBy == nil || input.Permission == nil || (*input.Permission != "read" && *input.Permission != "write") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "expected a Grant"})
		return
	}
	if err := h.authorizeGrantMutation(r.Context(), *input.OwnerScopeID, *input.Ref, *input.GrantedBy); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "grant_failed", "message": err.Error()})
		return
	}
	grant := data.Grant{OwnerScopeID: *input.OwnerScopeID, Path: *input.Ref, GranteeScopeID: *input.GranteeScopeID, Permission: *input.Permission, GrantedBy: *input.GrantedBy}
	if err := h.acl.Put(r.Context(), grant); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "grant_failed", "message": err.Error()})
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: grant.GrantedBy, Action: "grant", Resource: grant.Path, ScopeLabel: grant.GranteeScopeID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *HTTPServer) revokeGrant(w http.ResponseWriter, r *http.Request, raw []byte) {
	var input struct {
		OwnerScopeID   *string `json:"ownerScopeId"`
		Ref            *string `json:"ref"`
		GranteeScopeID *string `json:"granteeScopeId"`
		RevokedBy      *string `json:"revokedBy"`
	}
	if !decodeJSON(w, raw, &input) {
		return
	}
	if input.OwnerScopeID == nil || *input.OwnerScopeID == "" || input.Ref == nil || *input.Ref == "" || input.GranteeScopeID == nil || *input.GranteeScopeID == "" || input.RevokedBy == nil || *input.RevokedBy == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "ownerScopeId, ref, granteeScopeId, revokedBy required"})
		return
	}
	if err := h.authorizeGrantMutation(r.Context(), *input.OwnerScopeID, *input.Ref, *input.RevokedBy); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "revoke_failed", "message": "only a manager of this scope may revoke access"})
		return
	}
	if err := h.acl.Revoke(r.Context(), *input.OwnerScopeID, *input.Ref, *input.GranteeScopeID); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "revoke_failed", "message": err.Error()})
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: *input.RevokedBy, Action: "revoke", Resource: *input.Ref, ScopeLabel: *input.GranteeScopeID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *HTTPServer) authorizeGrantMutation(ctx context.Context, ownerScopeID, ref, actorID string) error {
	kind, scopeOwner := splitScopeID(ownerScopeID)
	if kind == "personal" {
		if sameSoulPerson(scopeOwner, actorID) {
			return nil
		}
		return errors.New("only a manager of this scope may grant access (no transitive re-share)")
	}
	if kind != "channel" && kind != "group" {
		return nil
	}
	author, err := h.grantArtifactAuthor(ctx, ownerScopeID, ref)
	if err != nil {
		return err
	}
	managed, err := h.managesArtifactHome(ctx, ownerScopeID, author, actorID)
	if err != nil {
		return err
	}
	if !managed {
		return errors.New("only a manager of this scope may grant access (no transitive re-share)")
	}
	return nil
}

func (h *HTTPServer) grantArtifactAuthor(ctx context.Context, ownerScopeID, ref string) (string, error) {
	switch {
	case strings.HasPrefix(ref, "skill:"):
		skill, err := h.skills.Get(ctx, strings.TrimPrefix(ref, "skill:"))
		if err != nil || skill == nil {
			return "", err
		}
		return skill.CreatedBy, nil
	case strings.HasPrefix(ref, "cron:"):
		cron, err := h.crons.Get(ctx, strings.TrimPrefix(ref, "cron:"))
		if err != nil || cron == nil {
			return "", err
		}
		var value struct {
			CreatedBy string `json:"createdBy"`
		}
		if json.Unmarshal(cron.JSON, &value) != nil {
			return "", nil
		}
		return value.CreatedBy, nil
	case strings.HasPrefix(ref, "deployment:"):
		deployment, err := h.deployments.Get(ctx, strings.TrimPrefix(ref, "deployment:"))
		if err != nil || deployment == nil {
			return "", err
		}
		return deployment.CreatedBy, nil
	default:
		files, err := h.files.ResolveByOwnerPaths(ctx, []data.FileHandle{{OwnerScopeID: ownerScopeID, Path: ref}})
		if err != nil || len(files) == 0 {
			return "", err
		}
		return files[0].CreatedBy, nil
	}
}

func (h *HTTPServer) sessionCapability(w http.ResponseWriter, r *http.Request) {
	secret := h.config.Auth.PortalIdentitySecret
	if secret == "" {
		secret = h.config.Auth.SourceSigningSecret
	}
	portal, err := auth.VerifyPortalIdentity(r.Header.Get("x-portal-identity"), secret, time.Now())
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized", "message": "portal identity required"})
		return
	}
	internal, err := h.directory.IsInternal(r.Context(), portal.PrincipalID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !internal {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized", "message": "portal identity required"})
		return
	}
	token, err := auth.MintCapability(auth.Claims{ActorID: portal.PrincipalID, ScopeID: "personal:" + portal.PrincipalID, ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, h.config.Auth.CapabilitySecret)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "not_configured", "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

func (h *HTTPServer) getSoul(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	scopeID := strings.TrimSpace(r.URL.Query().Get("scopeId"))
	if identity.ActorID != "" {
		scopeID = identity.ScopeID
	}
	if scopeID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "scopeId required"})
		return
	}
	orgScopeID := "org:" + h.config.QM.OrgID
	orgSoul, err := h.souls.Get(r.Context(), orgScopeID)
	if err != nil {
		h.fail(w, err)
		return
	}
	soul, err := h.souls.Get(r.Context(), scopeID)
	if err != nil {
		h.fail(w, err)
		return
	}
	var soulContent any
	if soul != nil {
		soulContent = soul.Content
	}
	var orgContent any
	if orgSoul != nil {
		orgContent = orgSoul.Content
	}
	soulVersion, orgVersion := 0, 0
	if soul != nil {
		soulVersion = soul.Version
	}
	if orgSoul != nil {
		orgVersion = orgSoul.Version
	}
	parts := []string{}
	if orgSoul != nil && orgSoul.Content != "" {
		parts = append(parts, orgSoul.Content)
	}
	includeScopeSoul := scopeID != orgScopeID && soul != nil && soul.Content != ""
	if includeScopeSoul {
		parts = append(parts, "--- Lower-scope instructions (may add to, but MUST NOT override, the organization policy above) ---\n"+soul.Content)
	}
	if orgSoul != nil && orgSoul.Content != "" && includeScopeSoul {
		parts = append(parts, "--- The organization policy above is authoritative and cannot be overridden by the lower-scope instructions. ---")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"scopeId":        scopeID,
		"soul":           soulContent,
		"soulVersion":    soulVersion,
		"orgScopeId":     orgScopeID,
		"orgSoul":        orgContent,
		"orgSoulVersion": orgVersion,
		"effectiveSoul":  strings.Join(parts, "\n\n"),
	})
}

func (h *HTTPServer) postSoul(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	var input struct {
		ScopeID *string `json:"scopeId"`
		Content *string `json:"content"`
		ActorID *string `json:"actorId"`
	}
	if !decodeJSON(w, raw, &input) {
		return
	}
	scopeID, content, actorID := "", "", ""
	if identity.ActorID != "" {
		if input.Content == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "content (string) required"})
			return
		}
		scopeID, content, actorID = identity.ScopeID, *input.Content, identity.ActorID
	} else {
		if input.ScopeID == nil || input.Content == nil || input.ActorID == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "scopeId, content, actorId required"})
			return
		}
		scopeID, content, actorID = *input.ScopeID, *input.Content, *input.ActorID
	}

	kind, ref := splitScopeID(scopeID)
	allowedPersonal := kind == "personal" && sameSoulPerson(ref, actorID)
	allowedShared := identity.ActorID != "" || h.sourceManagesSoulScope(r.Context(), kind, ref, actorID)
	if !allowedPersonal && !(allowedShared && (kind == "channel" || kind == "group")) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "soul_update_denied", "message": "not authorized to update SOUL for this scope"})
		return
	}
	version, err := h.souls.UpdateLatest(r.Context(), scopeID, content, actorID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "soul_update_failed", "message": err.Error()})
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actorID, Action: "soul_update", Resource: scopeID, ScopeLabel: scopeID, IdempotencyKey: requestID(r)}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "soul_update_failed", "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version})
}

// sourceManagesSoulScope is the Go equivalent of Node's canManageScope used
// by POST /v1/soul. Shared groups require current membership. Channels also
// require the channel to be private; public-channel participants cannot change
// governance configuration through the source-authenticated portal route.
func (h *HTTPServer) sourceManagesSoulScope(ctx context.Context, kind, ref, actorID string) bool {
	if kind != "channel" && kind != "group" || actorID == "" {
		return false
	}
	internal, err := h.directory.IsInternal(ctx, actorID)
	if err != nil || !internal {
		return false
	}
	if kind == "group" && strings.HasPrefix(ref, "web-project-") {
		ok, err := h.projectRepo.HasScopeMembership(ctx, "group:"+ref, actorID)
		return err == nil && ok
	}
	if kind == "channel" {
		channel, err := h.directory.Channel(ctx, ref)
		if err != nil || channel == nil || !channel.IsPrivate {
			return false
		}
	}
	ok, err := h.directory.IsScopeMember(ctx, kind, ref, actorID)
	return err == nil && ok
}

func splitScopeID(scopeID string) (string, string) {
	kind, ref, ok := strings.Cut(scopeID, ":")
	if !ok {
		return "", ""
	}
	switch kind {
	case "personal", "channel", "group", "team", "org":
		return kind, ref
	default:
		return "", ref
	}
}

func sameSoulPerson(left, right string) bool {
	left, right = strings.TrimSpace(left), strings.TrimSpace(right)
	if left == "" || right == "" {
		return false
	}
	if strings.Contains(left, "@") && strings.Contains(right, "@") {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func memoryHeadView(head data.MemoryHead) map[string]any {
	view := map[string]any{"content": head.Content, "revision": head.Revision}
	if head.UpdatedAt != nil {
		view["updatedAt"] = *head.UpdatedAt
	}
	return view
}

func (h *HTTPServer) getMemory(w http.ResponseWriter, r *http.Request) {
	principalID := strings.TrimSpace(r.URL.Query().Get("principalId"))
	if principalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "principalId required"})
		return
	}
	scopeID := "personal:" + principalID
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: principalID, Action: "memory.self.read", Resource: "memory", ScopeLabel: scopeID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	head, err := h.memory.Head(r.Context(), scopeID)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, memoryHeadView(head))
}

func (h *HTTPServer) putMemory(w http.ResponseWriter, r *http.Request, raw []byte) {
	var input struct {
		PrincipalID string          `json:"principalId"`
		Content     *string         `json:"content"`
		Revision    json.RawMessage `json:"revision"`
	}
	if !decodeJSON(w, raw, &input) {
		return
	}
	principalID := strings.TrimSpace(input.PrincipalID)
	if principalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "principalId required"})
		return
	}
	if input.Content == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "content required"})
		return
	}
	scopeID := "personal:" + principalID
	saved := true
	if revision, ok := rawJSONString(input.Revision); ok && revision != "" {
		var err error
		saved, err = h.memory.ReplaceIfRevision(r.Context(), scopeID, *input.Content, revision, principalID)
		if err != nil {
			h.fail(w, err)
			return
		}
	} else if err := h.memory.Replace(r.Context(), scopeID, *input.Content, principalID); err != nil {
		h.fail(w, err)
		return
	}
	if !saved {
		head, err := h.memory.Head(r.Context(), scopeID)
		if err != nil {
			h.fail(w, err)
			return
		}
		response := map[string]any{"error": "conflict", "message": "Memory changed while you were editing."}
		for key, value := range memoryHeadView(head) {
			response[key] = value
		}
		writeJSON(w, http.StatusConflict, response)
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: principalID, Action: "memory.self.update", Resource: "memory", ScopeLabel: scopeID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	head, err := h.memory.Head(r.Context(), scopeID)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.enqueueKnowledgeMemory(r.Context(), principalID, head)
	response := map[string]any{"ok": true}
	for key, value := range memoryHeadView(head) {
		response[key] = value
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *HTTPServer) enqueueKnowledgeMemory(ctx context.Context, principalID string, head data.MemoryHead) {
	if h.knowledgeAgent == nil || strings.TrimSpace(head.Content) == "" {
		return
	}
	_, err := h.knowledgeAgent.EnqueueMemory(knowlega.ScopeRef{
		OrgID:           h.config.QM.OrgID,
		ExternalScopeID: principalID,
		Kind:            "personal",
		Name:            "Personal Memory",
	}, head.Revision, []byte(head.Content))
	if err != nil {
		_ = h.audit.Record(ctx, data.AuditEvent{PrincipalID: principalID, Action: "knowledge.memory.enqueue_failed", Resource: "memory", ScopeLabel: "personal:" + principalID, Detail: err.Error()})
	}
}

func (h *HTTPServer) memoryPortalActor(w http.ResponseWriter, r *http.Request) (string, bool) {
	secret := h.config.Auth.PortalIdentitySecret
	if secret == "" {
		secret = h.config.Auth.SourceSigningSecret
	}
	portal, err := auth.VerifyPortalIdentity(r.Header.Get("x-portal-identity"), secret, time.Now())
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "capability_required"})
		return "", false
	}
	internal, err := h.directory.IsInternal(r.Context(), portal.PrincipalID)
	if err != nil {
		h.fail(w, err)
		return "", false
	}
	if !internal {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "capability_required"})
		return "", false
	}
	return portal.PrincipalID, true
}

func memoryScopeForCapability(identity auth.Identity, requestedScope string) string {
	if identity.Memory == nil {
		return ""
	}
	if requestedScope == "org" {
		return identity.Memory.OrgWrite
	}
	return identity.Memory.Write
}

func (h *HTTPServer) memoryHistory(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	if identity.ActorID != "" {
		principalID := strings.TrimSpace(r.URL.Query().Get("principalId"))
		if principalID != "" && principalID != identity.ActorID {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		requestedScope := r.URL.Query().Get("scope")
		if requestedScope != "" && requestedScope != "org" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "scope must be \"org\" when present"})
			return
		}
		scopeID := memoryScopeForCapability(identity, requestedScope)
		if scopeID == "" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		h.writeMemoryHistory(w, r, scopeID)
		return
	}
	viewer, ok := h.memoryPortalActor(w, r)
	if !ok {
		return
	}
	principalID := strings.TrimSpace(r.URL.Query().Get("principalId"))
	if principalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "principalId required"})
		return
	}
	if principalID != viewer {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	h.writeMemoryHistory(w, r, "personal:"+principalID)
}

func (h *HTTPServer) writeMemoryHistory(w http.ResponseWriter, r *http.Request, scopeID string) {
	revisions, err := h.memory.History(r.Context(), scopeID, 30)
	if err != nil {
		h.fail(w, err)
		return
	}
	views := make([]map[string]any, 0, len(revisions))
	for _, revision := range revisions {
		view := map[string]any{"revision": revision.Revision, "content": revision.Content, "operation": revision.Operation, "at": revision.At}
		if revision.Author != nil && *revision.Author != "" {
			view["author"] = *revision.Author
		}
		views = append(views, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"revisions": views})
}

func (h *HTTPServer) restoreMemory(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	var input struct {
		PrincipalID      string `json:"principalId"`
		Revision         string `json:"revision"`
		ExpectedRevision string `json:"expectedRevision"`
		Scope            string `json:"scope"`
	}
	if !decodeJSON(w, raw, &input) {
		return
	}
	if input.Revision == "" || input.ExpectedRevision == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "revision and expectedRevision required"})
		return
	}
	viewer, scopeID := "", ""
	if identity.ActorID != "" {
		if input.PrincipalID != "" && input.PrincipalID != identity.ActorID {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		if input.Scope != "" && input.Scope != "org" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "scope must be \"org\" when present"})
			return
		}
		viewer = identity.ActorID
		scopeID = memoryScopeForCapability(identity, input.Scope)
		if scopeID == "" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
	} else {
		var ok bool
		viewer, ok = h.memoryPortalActor(w, r)
		if !ok {
			return
		}
		if input.PrincipalID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "revision and expectedRevision required"})
			return
		}
		if input.PrincipalID != viewer {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		scopeID = "personal:" + viewer
	}
	restored, err := h.memory.Restore(r.Context(), scopeID, input.Revision, input.ExpectedRevision, viewer)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !restored {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "conflict", "message": "Memory changed, or that revision no longer exists."})
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: viewer, Action: "memory.self.restore", Resource: "memory:" + input.Revision, ScopeLabel: scopeID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	head, err := h.memory.Head(r.Context(), scopeID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if kind, external := splitScopeID(scopeID); kind != "" && external != "" && h.knowledgeAgent != nil {
		_, enqueueErr := h.knowledgeAgent.EnqueueMemory(knowlega.ScopeRef{OrgID: h.config.QM.OrgID, ExternalScopeID: external, Kind: kind, Name: "Restored Memory"}, head.Revision, []byte(head.Content))
		if enqueueErr != nil {
			_ = h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: viewer, Action: "knowledge.memory.enqueue_failed", Resource: "memory", ScopeLabel: scopeID, Detail: enqueueErr.Error()})
		}
	}
	response := map[string]any{"ok": true}
	for key, value := range memoryHeadView(head) {
		response[key] = value
	}
	writeJSON(w, http.StatusOK, response)
}

func rawJSONString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func deploymentView(item data.Deployment) map[string]any {
	versions := make([]map[string]any, 0, len(item.Versions))
	for _, version := range item.Versions {
		view := map[string]any{"version": version.Version, "createdAt": version.CreatedAt}
		if version.Commit != nil && *version.Commit != "" {
			view["commit"] = *version.Commit
		}
		if version.ParentCommit != nil && *version.ParentCommit != "" {
			view["parentCommit"] = *version.ParentCommit
		}
		versions = append(versions, view)
	}
	view := map[string]any{"id": item.ID, "ownerScopeId": item.OwnerScopeID, "createdBy": item.CreatedBy, "currentVersion": item.CurrentVersion, "status": item.Status, "versions": versions}
	if item.CreatedInScope != nil && *item.CreatedInScope != "" {
		view["createdInScope"] = *item.CreatedInScope
	}
	if item.Name != "" {
		view["name"] = item.Name
	}
	if item.DisplayName != "" {
		view["displayName"] = item.DisplayName
	}
	if item.AppliedVersion != nil {
		view["appliedVersion"] = *item.AppliedVersion
	}
	if item.LastAccessAt != nil {
		view["lastAccessAt"] = *item.LastAccessAt
	}
	if len(item.Versions) != 0 {
		view["createdAt"] = item.Versions[0].CreatedAt
		view["updatedAt"] = item.Versions[len(item.Versions)-1].CreatedAt
	}
	return view
}

func (h *HTTPServer) listDeployments(w http.ResponseWriter, r *http.Request) {
	deployments, err := h.deployments.List(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	views := make([]map[string]any, 0, len(deployments))
	for _, deployment := range deployments {
		views = append(views, deploymentView(deployment))
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployments": views})
}

func (h *HTTPServer) getDeployment(w http.ResponseWriter, r *http.Request) {
	deployment, err := h.deployments.Get(r.Context(), deploymentID(r.URL.Path))
	if err != nil {
		h.fail(w, err)
		return
	}
	if deployment == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployment": deploymentView(*deployment)})
}

func (h *HTTPServer) renameDeployment(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	h.updateDeploymentLabel(w, r, raw, identity, deploymentNameID(r.URL.Path), "name")
}

func (h *HTTPServer) setDeploymentDisplayName(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	h.updateDeploymentLabel(w, r, raw, identity, deploymentDisplayNameID(r.URL.Path), "displayName")
}

// shareDeployment is the durable ACL half of the deployment control plane.
// Unlike deploy, archive, restore, or fetch, Node's share operation never
// calls a deployment provider: it resolves a directory target, replaces that
// target's ACL grant, and appends an audit record. Keep the owner-only rule
// here rather than inferring broader management authority from a write grant.
func (h *HTTPServer) shareDeployment(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	if identity.ActorID == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "sharing requires an agent capability token"})
		return
	}
	var input struct {
		Scope     any `json:"scope"`
		Recipient any `json:"recipient"`
		Access    any `json:"access"`
	}
	if !decodeJSON(w, raw, &input) {
		return
	}
	access := "view"
	if value, ok := input.Access.(string); ok {
		access = strings.ToLower(value)
	}
	if access != "view" && access != "manage" && access != "none" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": `access must be "view", "manage", or "none"`})
		return
	}
	target, targetStatus, err := h.resolveDeploymentShareTarget(r.Context(), input.Scope, input.Recipient)
	if err != nil {
		h.fail(w, err)
		return
	}
	switch targetStatus {
	case "invalid":
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": target.Message})
		return
	case "none":
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "recipient_not_found", "message": target.Message})
		return
	case "ambiguous":
		writeJSON(w, http.StatusConflict, map[string]any{"error": "ambiguous_recipient", "message": "more than one teammate matches", "candidates": target.Candidates})
		return
	}

	deployment, err := h.deployments.Get(r.Context(), deploymentShareID(r.URL.Path))
	if err != nil {
		h.fail(w, err)
		return
	}
	if deployment == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found", "message": "no such app: " + deploymentShareID(r.URL.Path)})
		return
	}
	if deployment.OwnerScopeID != "personal:"+identity.ActorID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": `only the owner can change who can reach "` + firstNonEmpty(deployment.Name, deployment.ID) + `"`})
		return
	}
	ref := "deployment:" + deployment.ID
	if err := h.acl.Revoke(r.Context(), deployment.OwnerScopeID, ref, target.Scope); err != nil {
		h.fail(w, err)
		return
	}
	if access != "none" {
		permission := "read"
		if access == "manage" {
			permission = "write"
		}
		if err := h.acl.Put(r.Context(), data.Grant{OwnerScopeID: deployment.OwnerScopeID, Path: ref, GranteeScopeID: target.Scope, Permission: permission, GrantedBy: identity.ActorID}); err != nil {
			h.fail(w, err)
			return
		}
	}
	grants, err := h.acl.List(r.Context(), deployment.OwnerScopeID, ref)
	if err != nil {
		h.fail(w, err)
		return
	}
	grantees := make([]map[string]string, 0, len(grants))
	hasOrg := false
	for _, grant := range grants {
		grantees = append(grantees, map[string]string{"scope": grant.GranteeScopeID, "permission": grant.Permission})
		if kind, _ := splitScopeID(grant.GranteeScopeID); kind == "org" {
			hasOrg = true
		}
	}
	action := "deploy_share"
	if access == "none" {
		action = "deploy_unshare"
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: identity.ActorID, Action: action, Resource: ref, ScopeLabel: target.Scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	reach := "owner-only"
	if hasOrg {
		_, orgID := splitScopeID(target.Scope)
		// Node uses the org grant's ref, not necessarily the target scope.
		for _, grant := range grants {
			if kind, ref := splitScopeID(grant.GranteeScopeID); kind == "org" {
				orgID = ref
				break
			}
		}
		reach = "everyone in " + orgID
	} else if len(grantees) > 0 {
		reach = strconv.Itoa(len(grantees)) + " grantee"
		if len(grantees) != 1 {
			reach += "s"
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "target": map[string]string{"scope": target.Scope, "label": target.Label}, "access": access, "reach": reach, "grantees": grantees})
}

type deploymentShareTarget struct {
	Scope      string
	Label      string
	Message    string
	Candidates []map[string]string
}

func (h *HTTPServer) resolveDeploymentShareTarget(ctx context.Context, rawScope, rawRecipient any) (deploymentShareTarget, string, error) {
	scope, scopeOK := rawScope.(string)
	recipient, recipientOK := rawRecipient.(string)
	originalRecipient := recipient
	scope, recipient = strings.TrimSpace(scope), strings.TrimSpace(recipient)
	if scopeOK && recipientOK && scope != "" && recipient != "" {
		return deploymentShareTarget{Message: "pass either scope or recipient, not both"}, "invalid", nil
	}
	if scope != "" {
		if scope == "org" {
			return deploymentShareTarget{Scope: "org:" + h.config.QM.OrgID, Label: "everyone in the org"}, "ok", nil
		}
		if kind, _ := splitScopeID(scope); kind == "" {
			return deploymentShareTarget{Message: `invalid scope "` + scope + `" — use "org" or a scope id like personal:<id> or org:<id>`}, "invalid", nil
		}
		return deploymentShareTarget{Scope: scope, Label: scope}, "ok", nil
	}
	if recipient == "" {
		return deploymentShareTarget{Message: "a target is required: pass `scope` (\"org\" or a scope id) or `recipient` (a teammate's name)"}, "invalid", nil
	}
	members, err := h.directory.List(ctx)
	if err != nil {
		return deploymentShareTarget{}, "", err
	}
	match := resolveDeploymentRecipient(members, recipient)
	if len(match) == 0 {
		return deploymentShareTarget{Message: "no teammate matches \"" + originalRecipient + "\""}, "none", nil
	}
	if len(match) > 1 {
		candidates := make([]map[string]string, 0, len(match))
		for _, member := range match {
			candidates = append(candidates, map[string]string{"principalId": member.PrincipalID, "displayName": member.DisplayName})
		}
		return deploymentShareTarget{Candidates: candidates}, "ambiguous", nil
	}
	member := match[0]
	return deploymentShareTarget{Scope: "personal:" + member.PrincipalID, Label: member.DisplayName}, "ok", nil
}

func resolveDeploymentRecipient(members []data.DirectoryMember, query string) []data.DirectoryMember {
	raw := strings.TrimSpace(query)
	for _, member := range members {
		if member.Type != "internal" {
			continue
		}
		if member.SlackID != "" && member.SlackID == raw {
			return []data.DirectoryMember{member}
		}
	}
	normalized := strings.TrimPrefix(strings.TrimPrefix(deploymentRecipientKey(raw), "@"), "#")
	if normalized == "" {
		return nil
	}
	exactID, exactName := []data.DirectoryMember{}, []data.DirectoryMember{}
	for _, member := range members {
		if member.Type != "internal" {
			continue
		}
		if strings.ToLower(member.PrincipalID) == normalized {
			exactID = append(exactID, member)
		}
		if deploymentRecipientKey(member.DisplayName) == normalized {
			exactName = append(exactName, member)
		}
	}
	if len(exactID) > 0 {
		return exactID[:1]
	}
	if len(exactName) > 0 {
		return limitDeploymentRecipientCandidates(exactName)
	}
	prefix, contains := []data.DirectoryMember{}, []data.DirectoryMember{}
	for _, member := range members {
		if member.Type != "internal" {
			continue
		}
		name := deploymentRecipientKey(member.DisplayName)
		if strings.HasPrefix(name, normalized) {
			prefix = append(prefix, member)
		} else if strings.Contains(name, normalized) {
			contains = append(contains, member)
		}
	}
	if len(prefix) > 0 {
		return limitDeploymentRecipientCandidates(prefix)
	}
	return limitDeploymentRecipientCandidates(contains)
}

func deploymentRecipientKey(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func limitDeploymentRecipientCandidates(items []data.DirectoryMember) []data.DirectoryMember {
	if len(items) > 10 {
		return items[:10]
	}
	return items
}

func (h *HTTPServer) updateDeploymentLabel(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity, id, field string) {
	if id == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	deployment, err := h.deployments.Get(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if deployment == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	if identity.ActorID != "" {
		permission, err := h.deploymentGitPermission(r.Context(), *deployment, identity.ActorID)
		if err != nil {
			h.fail(w, err)
			return
		}
		if permission != "write" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "only someone who manages this app can rename it"})
			return
		}
	}
	var input map[string]json.RawMessage
	if !decodeJSON(w, raw, &input) {
		return
	}
	valueRaw, exists := input[field]
	var value string
	if !exists || json.Unmarshal(valueRaw, &value) != nil {
		message := field + " (string) required"
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": message})
		return
	}
	var updated json.RawMessage
	var next *data.Deployment
	if field == "name" {
		updated, next, err = h.deployments.UpdateName(r.Context(), deployment.ID, value)
	} else {
		updated, next, err = h.deployments.UpdateDisplayName(r.Context(), deployment.ID, value)
	}
	if err != nil {
		code := "display_name_failed"
		if field == "name" {
			code = "rename_failed"
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": code, "message": err.Error()})
		return
	}
	if next == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	action := "deploy_display_name"
	if field == "name" {
		action = "deploy_rename"
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: next.CreatedBy, Action: action, Resource: deployment.ID, ScopeLabel: next.OwnerScopeID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployment": jsonValue(updated)})
}

func (h *HTTPServer) listedViewerSessions(ctx context.Context, principalID string) ([]map[string]any, error) {
	sessions, err := h.sessions.ListViewerSessions(ctx, principalID)
	if err != nil {
		return nil, err
	}
	runtime, err := h.sessions.RuntimeState(ctx, time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	awaiting := make(map[string]bool)
	for _, session := range sessions {
		if !h.viewerCanAccessSession(ctx, &session, principalID) {
			continue
		}
		approvals, err := h.sessions.PendingApprovals(ctx, session.ID)
		if err != nil {
			return nil, err
		}
		for _, approval := range approvals {
			current, err := h.approvalCurrent(ctx, &session.SessionRecord, approval)
			if err != nil {
				return nil, err
			}
			if current && approval.BlocksInput {
				awaiting[session.ID] = true
				break
			}
		}
	}
	result := make([]map[string]any, 0, len(sessions))
	for _, session := range sessions {
		if !h.viewerCanAccessSession(ctx, &session, principalID) {
			continue
		}
		hasTitle := session.ParticipantTitle != nil && strings.TrimSpace(*session.ParticipantTitle) != "" || session.ParticipantTitle == nil && session.Title != nil && strings.TrimSpace(*session.Title) != ""
		if !session.HasEntries && !hasTitle && !runtime.WorkingThreadRefs[session.ThreadRef] && !awaiting[session.ID] && runtime.BackgroundJobs[session.ThreadRef] == 0 && runtime.Watches[session.ThreadRef] == 0 {
			continue
		}
		view := viewerSessionView(&session)
		if runtime.WorkingThreadRefs[session.ThreadRef] {
			view["working"] = true
		}
		if awaiting[session.ID] {
			view["awaitingInput"] = true
		}
		if count := runtime.BackgroundJobs[session.ThreadRef]; count > 0 {
			view["backgroundJobs"] = count
		}
		if count := runtime.Watches[session.ThreadRef]; count > 0 {
			view["watches"] = count
		}
		result = append(result, view)
	}
	return result, nil
}

func (h *HTTPServer) listSessions(w http.ResponseWriter, r *http.Request) {
	principalID := strings.TrimSpace(r.URL.Query().Get("principalId"))
	if principalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "principalId required"})
		return
	}
	sessions, err := h.listedViewerSessions(r.Context(), principalID)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (h *HTTPServer) listAgentConversations(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	if identity.ActorID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "capability_required", "message": "this endpoint is for the agent self-API"})
		return
	}
	sessions, err := h.listedViewerSessions(r.Context(), identity.ActorID)
	if err != nil {
		h.fail(w, err)
		return
	}
	conversations := make([]map[string]any, 0, len(sessions))
	for _, session := range sessions {
		title, _ := session["title"]
		conversations = append(conversations, map[string]any{"id": session["id"], "scopeId": session["scopeId"], "surface": firstNonEmpty(stringValue(session["surface"]), "unknown"), "title": title, "archived": session["archived"] == true, "pinned": session["pinned"] == true, "createdAt": session["createdAt"], "lastActivityAt": session["lastActivityAt"]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversations": conversations})
}

func (h *HTTPServer) sessionBackground(w http.ResponseWriter, r *http.Request) {
	viewer := strings.TrimSpace(r.URL.Query().Get("viewer"))
	if viewer == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "viewer required"})
		return
	}
	session, err := h.sessions.ViewerSession(r.Context(), sessionBackgroundID(r.URL.Path), viewer)
	if err != nil {
		h.fail(w, err)
		return
	}
	if session == nil || !h.viewerCanAccessSession(r.Context(), session, viewer) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	jobs, watches, err := h.sessions.Background(r.Context(), session.ThreadRef, time.Now().UnixMilli())
	if err != nil {
		h.fail(w, err)
		return
	}
	jobViews := make([]map[string]any, 0, len(jobs))
	for _, job := range jobs {
		jobViews = append(jobViews, map[string]any{"processId": job.ProcessID, "command": job.Command, "startedAt": job.StartedAt, "expiresAt": job.ExpiresAt})
	}
	watchViews := make([]map[string]any, 0, len(watches))
	for _, watch := range watches {
		view := map[string]any{"id": watch.ID, "processId": watch.ProcessID, "command": watch.Command, "createdAt": watch.CreatedAt, "expiresAt": watch.ExpiresAt}
		if watch.Pattern != nil {
			view["pattern"] = *watch.Pattern
		}
		if watch.Instructions != nil {
			view["instructions"] = *watch.Instructions
		}
		if watch.LastFiredAt != nil {
			view["lastFiredAt"] = *watch.LastFiredAt
		}
		watchViews = append(watchViews, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobViews, "watches": watchViews})
}

func (h *HTTPServer) approvalCurrent(ctx context.Context, session *data.SessionRecord, approval data.PendingApproval) (bool, error) {
	return h.projectRepo.AuthorizesScopeVersion(ctx, session.ScopeID, approval.ActorID, approval.ScopeVersion)
}

func (h *HTTPServer) approvalVisibleToViewer(ctx context.Context, session *data.ViewerSession, viewer string, approval data.PendingApproval) (bool, error) {
	if !samePrincipal(approval.ActorID, viewer) {
		return false, nil
	}
	current, err := h.approvalCurrent(ctx, &session.SessionRecord, approval)
	if err != nil || !current {
		return false, err
	}
	if !strings.HasPrefix(session.ScopeID, "group:web-project-") {
		return true, nil
	}
	if approval.CreatedAt == nil {
		return false, nil
	}
	window, err := h.sessions.ParticipantWindow(ctx, session.ID, viewer)
	if err != nil || window == nil {
		return false, err
	}
	return *approval.CreatedAt >= window.ValidFrom && (window.ValidTo == nil || *approval.CreatedAt < *window.ValidTo), nil
}

func approvalView(approval data.PendingApproval) map[string]any {
	hash := sha256.Sum256([]byte(approval.SessionID + "\x00" + approval.Command))
	requestID := hex.EncodeToString(hash[:])[:16]
	view := map[string]any{
		"requestId":   requestID,
		"command":     approval.Command,
		"reason":      firstNonEmpty(approval.Reason, "requires approval"),
		"blocksInput": approval.BlocksInput,
	}
	if approval.Matched != "" {
		view["matched"] = approval.Matched
	}
	if approval.Purpose != "" {
		view["purpose"] = approval.Purpose
	}
	if approval.Summary != "" {
		view["summary"] = approval.Summary
	}
	if len(approval.GrantModes) != 0 && string(approval.GrantModes) != "null" {
		view["grantModes"] = json.RawMessage(approval.GrantModes)
	}
	if approval.Kind == "approval" {
		view["kind"] = approval.Kind
	}
	return view
}

func (h *HTTPServer) getApproval(w http.ResponseWriter, r *http.Request) {
	approval, err := h.sessions.Approval(r.Context(), approvalID(r.URL.Path))
	if err != nil {
		h.fail(w, err)
		return
	}
	if approval == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	session, err := h.sessions.Get(r.Context(), approval.SessionID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if session == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	current, err := h.approvalCurrent(r.Context(), session, *approval)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !current {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	var view map[string]any
	if json.Unmarshal(approval.Raw, &view) != nil {
		h.fail(w, errors.New("invalid approval record"))
		return
	}
	view["requestId"] = approval.ID
	writeJSON(w, http.StatusOK, view)
}

func (h *HTTPServer) pendingApprovalForThread(w http.ResponseWriter, r *http.Request) {
	threadRef := strings.TrimSpace(r.URL.Query().Get("threadRef"))
	if threadRef == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "threadRef required"})
		return
	}
	session, err := h.sessions.GetByThread(r.Context(), threadRef)
	if err != nil {
		h.fail(w, err)
		return
	}
	if session == nil {
		writeJSON(w, http.StatusOK, map[string]any{"pending": nil})
		return
	}
	approvals, err := h.sessions.PendingApprovals(r.Context(), session.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	views := make([]map[string]any, 0, len(approvals))
	for _, approval := range approvals {
		current, err := h.approvalCurrent(r.Context(), session, approval)
		if err != nil {
			h.fail(w, err)
			return
		}
		if current && approval.BlocksInput {
			views = append(views, approvalView(approval))
		}
	}
	if len(views) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"pending": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pending": map[string]any{"status": "pending_approval", "sessionId": session.ID, "reason": "Approve or deny the pending command to continue.", "pendingApprovals": views}})
}

func (h *HTTPServer) sessionApprovals(w http.ResponseWriter, r *http.Request) {
	viewer := strings.TrimSpace(r.URL.Query().Get("viewer"))
	if viewer == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "viewer required"})
		return
	}
	sessionID := sessionApprovalsID(r.URL.Path)
	session, err := h.sessions.ViewerSession(r.Context(), sessionID, viewer)
	if err != nil {
		h.fail(w, err)
		return
	}
	if session == nil || !h.viewerCanAccessSession(r.Context(), session, viewer) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	approvals, err := h.sessions.PendingApprovals(r.Context(), sessionID)
	if err != nil {
		h.fail(w, err)
		return
	}
	views := make([]map[string]any, 0, len(approvals))
	for _, approval := range approvals {
		visible, err := h.approvalVisibleToViewer(r.Context(), session, viewer, approval)
		if err != nil {
			h.fail(w, err)
			return
		}
		if !visible {
			continue
		}
		views = append(views, approvalView(approval))
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": views})
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func (h *HTTPServer) activeRunForThread(w http.ResponseWriter, r *http.Request) {
	threadRef := strings.TrimSpace(r.URL.Query().Get("threadRef"))
	if threadRef == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "threadRef required"})
		return
	}
	session, err := h.sessions.GetByThread(r.Context(), threadRef)
	if err != nil {
		h.fail(w, err)
		return
	}
	lookup := threadRef
	if session != nil {
		lookup = session.ID
	}
	id, err := h.runs.ActiveForSession(r.Context(), lookup)
	if err == nil && id == "" && lookup != threadRef {
		id, err = h.runs.ActiveForSession(r.Context(), threadRef)
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	if id == "" {
		writeJSON(w, http.StatusOK, map[string]any{"runId": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"runId": id})
}

type transcriptWindow struct {
	tailTurns, sinceSeq, beforeSeq *int
}

func parseTranscriptWindow(query url.Values) (*transcriptWindow, bool) {
	parse := func(name string, minimum int) (*int, bool) {
		raw := query.Get(name)
		if raw == "" {
			return nil, true
		}
		value, err := strconv.Atoi(raw)
		if err != nil || value < minimum {
			return nil, false
		}
		return &value, true
	}
	tail, ok := parse("tailTurns", 1)
	if !ok {
		return nil, false
	}
	since, ok := parse("sinceSeq", 0)
	if !ok {
		return nil, false
	}
	before, ok := parse("beforeSeq", 1)
	if !ok {
		return nil, false
	}
	return &transcriptWindow{tailTurns: tail, sinceSeq: since, beforeSeq: before}, true
}

func viewerSessionView(session *data.ViewerSession) map[string]any {
	result := sessionRecordView(&session.SessionRecord)
	if session.ParticipantTitle != nil {
		result["title"] = *session.ParticipantTitle
	}
	if session.Archived {
		result["archived"] = true
	}
	if session.Pinned {
		result["pinned"] = true
	}
	if session.Color != nil {
		result["color"] = *session.Color
	}
	result["lastActivityAt"] = session.LastActivity
	result["hasEntries"] = session.HasEntries
	if session.ForkedFromSession != nil && session.ForkBoundarySeq != nil {
		forked := map[string]any{"sessionId": *session.ForkedFromSession}
		if session.ForkedFromTitle != nil {
			forked["title"] = *session.ForkedFromTitle
		}
		result["forkedFrom"] = forked
		result["forkBoundarySeq"] = *session.ForkBoundarySeq
	}
	return result
}

func transcriptEntryView(entry data.SessionEntry) map[string]any {
	return map[string]any{"sessionId": entry.SessionID, "seq": entry.Sequence, "parentSeq": entry.ParentSequence, "type": entry.Type, "payload": jsonValue(entry.Payload), "scopeLabel": entry.ScopeLabel, "createdAt": entry.CreatedAt}
}

func viewerTranscript(entries []data.SessionEntry, window *transcriptWindow) ([]map[string]any, int) {
	visible := make([]data.SessionEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Type != "soul" {
			visible = append(visible, entry)
		}
	}
	if window.beforeSeq != nil {
		at := len(visible)
		for i, entry := range visible {
			if entry.Sequence >= *window.beforeSeq {
				at = i
				break
			}
		}
		visible = visible[:at]
	}
	cut := 0
	if window.sinceSeq != nil {
		cut = len(visible)
		for i, entry := range visible {
			if entry.Sequence >= *window.sinceSeq {
				cut = i
				break
			}
		}
	} else if window.tailTurns != nil {
		turns := 0
		for i := len(visible) - 1; i >= 0; i-- {
			if visible[i].Type != "user" {
				continue
			}
			turns++
			if turns == *window.tailTurns {
				cut = i
				break
			}
		}
	}
	result := make([]map[string]any, 0, len(visible)-cut)
	for _, entry := range visible[cut:] {
		result = append(result, transcriptEntryView(entry))
	}
	return result, cut
}

func (h *HTTPServer) viewerSession(w http.ResponseWriter, r *http.Request) {
	viewer := r.URL.Query().Get("viewer")
	if viewer == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "viewer required"})
		return
	}
	window, ok := parseTranscriptWindow(r.URL.Query())
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "tailTurns and beforeSeq must be positive integers, sinceSeq a non-negative one"})
		return
	}
	session, err := h.sessions.ViewerSession(r.Context(), sessionID(r.URL.Path), viewer)
	if err != nil {
		h.fail(w, err)
		return
	}
	if session == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	entries, err := h.sessions.VisibleEntries(r.Context(), session.ID, viewer)
	if err != nil {
		h.fail(w, err)
		return
	}
	transcript, earlier := viewerTranscript(entries, window)
	result := map[string]any{"session": viewerSessionView(session), "entries": transcript}
	if earlier > 0 {
		result["earlierEntries"] = earlier
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *HTTPServer) agentConversation(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	if identity.ActorID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "capability_required", "message": "this endpoint is for the agent self-API"})
		return
	}
	if r.URL.Query().Has("sinceSeq") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "agent transcript paging supports tailTurns and beforeSeq"})
		return
	}
	window, ok := parseTranscriptWindow(r.URL.Query())
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "tailTurns and beforeSeq must be positive integers, sinceSeq a non-negative one"})
		return
	}
	if window.tailTurns == nil {
		defaultTail := 20
		window.tailTurns = &defaultTail
	}
	session, err := h.sessions.ViewerSession(r.Context(), conversationID(r.URL.Path), identity.ActorID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if session == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found", "message": "not a conversation you can see"})
		return
	}
	entries, err := h.sessions.VisibleEntries(r.Context(), session.ID, identity.ActorID)
	if err != nil {
		h.fail(w, err)
		return
	}
	transcript, earlier := viewerTranscript(entries, window)
	result := map[string]any{"session": viewerSessionView(session), "entries": transcript}
	if earlier > 0 {
		result["earlierEntries"] = earlier
	}
	writeJSON(w, http.StatusOK, result)
}

func parseSessionViewPatch(raw []byte, requiredMessage string) (data.SessionViewPatch, string) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return data.SessionViewPatch{}, "invalid JSON body"
	}
	patch := data.SessionViewPatch{}
	if value, exists := fields["title"]; exists {
		patch.TitleSet = true
		if string(value) != "null" {
			var title string
			if err := json.Unmarshal(value, &title); err != nil {
				return data.SessionViewPatch{}, "title must be a string or null"
			}
			title = strings.TrimSpace(title)
			if chars := []rune(title); len(chars) > 200 {
				title = string(chars[:200])
			}
			if title != "" {
				patch.Title = &title
			}
		}
	}
	if value, exists := fields["archived"]; exists {
		patch.ArchivedSet = true
		if err := json.Unmarshal(value, &patch.Archived); err != nil {
			return data.SessionViewPatch{}, "archived must be a boolean"
		}
	}
	if value, exists := fields["pinned"]; exists {
		patch.PinnedSet = true
		if err := json.Unmarshal(value, &patch.Pinned); err != nil {
			return data.SessionViewPatch{}, "pinned must be a boolean"
		}
	}
	if value, exists := fields["color"]; exists {
		patch.ColorSet = true
		if string(value) != "null" {
			var color string
			if err := json.Unmarshal(value, &color); err != nil || !conversationColorPattern.MatchString(color) {
				return data.SessionViewPatch{}, "color must be '#rrggbb' or null"
			}
			color = strings.ToLower(color)
			patch.Color = &color
		}
	}
	if !patch.TitleSet && !patch.ArchivedSet && !patch.PinnedSet && !patch.ColorSet {
		return data.SessionViewPatch{}, requiredMessage
	}
	return patch, ""
}

func (h *HTTPServer) viewerCanAccessSession(ctx context.Context, session *data.ViewerSession, principalID string) bool {
	if !strings.HasPrefix(session.ScopeID, "group:web-project-") {
		return true
	}
	ok, err := h.projectRepo.HasScopeMembership(ctx, session.ScopeID, principalID)
	return err == nil && ok
}

func (h *HTTPServer) updateViewerSession(ctx context.Context, sessionID, principalID string, patch data.SessionViewPatch) (*data.ViewerSession, error) {
	session, err := h.sessions.ViewerSession(ctx, sessionID, principalID)
	if err != nil || session == nil || !h.viewerCanAccessSession(ctx, session, principalID) {
		return nil, err
	}
	if err := h.sessions.UpdateViewerSession(ctx, sessionID, principalID, patch); err != nil {
		return nil, err
	}
	return h.sessions.ViewerSession(ctx, sessionID, principalID)
}

func (h *HTTPServer) patchViewerSession(w http.ResponseWriter, r *http.Request, raw []byte) {
	var request struct {
		PrincipalID string `json:"principalId"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "invalid JSON body"})
		return
	}
	if request.PrincipalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "principalId required"})
		return
	}
	patch, message := parseSessionViewPatch(raw, "title, archived, pinned, or color required")
	if message != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": message})
		return
	}
	session, err := h.updateViewerSession(r.Context(), sessionID(r.URL.Path), request.PrincipalID, patch)
	if err != nil {
		h.fail(w, err)
		return
	}
	if session == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": viewerSessionView(session)})
}

func (h *HTTPServer) patchAgentConversation(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	if identity.ActorID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "capability_required", "message": "this endpoint is for the agent self-API"})
		return
	}
	patch, message := parseSessionViewPatch(raw, "archived, pinned, title, or color required")
	if message != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": message})
		return
	}
	session, err := h.updateViewerSession(r.Context(), conversationID(r.URL.Path), identity.ActorID, patch)
	if err != nil {
		h.fail(w, err)
		return
	}
	if session == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found", "message": "not a conversation you can see"})
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: identity.ActorID, Action: "conversation.update", Resource: session.ID, ScopeLabel: session.ScopeID, Detail: string(raw), IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	if session.Archived {
		h.enqueueKnowledgeConversation(r.Context(), *session, identity.ActorID)
	}
	title := any(nil)
	if session.ParticipantTitle != nil {
		title = *session.ParticipantTitle
	} else if session.Title != nil {
		title = *session.Title
	}
	color := any(nil)
	if session.Color != nil {
		color = *session.Color
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversation": map[string]any{"id": session.ID, "title": title, "archived": session.Archived, "pinned": session.Pinned, "color": color}})
}

func (h *HTTPServer) enqueueKnowledgeConversation(ctx context.Context, session data.ViewerSession, actor string) {
	if h.knowledgeAgent == nil {
		return
	}
	entries, err := h.sessions.VisibleEntries(ctx, session.ID, actor)
	if err != nil {
		_ = h.audit.Record(ctx, data.AuditEvent{PrincipalID: actor, Action: "knowledge.conversation.enqueue_failed", Resource: session.ID, ScopeLabel: session.ScopeID, Detail: err.Error()})
		return
	}
	var transcript strings.Builder
	userTurns := 0
	for _, entry := range entries {
		if entry.Type == "soul" {
			continue
		}
		if entry.Type == "user" {
			userTurns++
		}
		fmt.Fprintf(&transcript, "[%s #%d]\n%s\n\n", entry.Type, entry.Sequence, strings.TrimSpace(string(entry.Payload)))
	}
	text := strings.TrimSpace(transcript.String())
	// Archive is the completion signal, but retain only conversations with
	// enough user turns and durable content to avoid filling the wiki with chat
	// noise. The LLM compiler can still decide which pages are worth keeping.
	valuable := userTurns >= 2 && len([]rune(text)) >= 240
	kind, external := splitScopeID(session.ScopeID)
	if kind == "" || external == "" {
		kind, external = "personal", actor
	}
	_, err = h.knowledgeAgent.EnqueueConversation(knowlega.ScopeRef{OrgID: h.config.QM.OrgID, ExternalScopeID: external, Kind: kind, Name: "Conversation " + session.ID}, session.ID, []byte(text), valuable)
	if err != nil {
		_ = h.audit.Record(ctx, data.AuditEvent{PrincipalID: actor, Action: "knowledge.conversation.enqueue_failed", Resource: session.ID, ScopeLabel: session.ScopeID, Detail: err.Error()})
	}
}

func (h *HTTPServer) viewerSessionEntry(w http.ResponseWriter, r *http.Request) {
	viewer := r.URL.Query().Get("viewer")
	if viewer == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "viewer required"})
		return
	}
	_, seq, ok := sessionEntryParts(r.URL.Path)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "seq must be a non-negative integer"})
		return
	}
	session, err := h.sessions.ViewerSession(r.Context(), sessionEntryID(r.URL.Path), viewer)
	if err != nil {
		h.fail(w, err)
		return
	}
	if session == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	entries, err := h.sessions.VisibleEntries(r.Context(), session.ID, viewer)
	if err != nil {
		h.fail(w, err)
		return
	}
	for _, entry := range entries {
		if entry.Sequence == seq && entry.Type != "soul" {
			writeJSON(w, http.StatusOK, map[string]any{"entry": transcriptEntryView(entry)})
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
}

func (h *HTTPServer) setRunDeliveryState(w http.ResponseWriter, r *http.Request, raw []byte) {
	var input struct {
		EditRef string `json:"editRef"`
	}
	if !decodeJSON(w, raw, &input) {
		return
	}
	if input.EditRef == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "editRef required"})
		return
	}
	found, err := h.runs.SetDeliveryState(r.Context(), runDeliveryStateID(r.URL.Path), input.EditRef)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *HTTPServer) signalRun(w http.ResponseWriter, r *http.Request, raw []byte) {
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "invalid JSON body"})
		return
	}
	kind, _ := input["kind"].(string)
	if kind != "abort" && kind != "steer" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "kind must be abort or steer"})
		return
	}
	text, _ := input["text"].(string)
	if kind == "steer" && strings.TrimSpace(text) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_request", "message": "text required", "accepted": false, "reason": "text_required"})
		return
	}
	payload := map[string]string{"kind": kind}
	if _, ok := input["text"].(string); ok {
		payload["text"] = text
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		h.fail(w, err)
		return
	}
	outcome, err := h.runs.SignalIfActive(r.Context(), runSignalID(r.URL.Path), encoded)
	if err != nil {
		h.fail(w, err)
		return
	}
	if outcome == "accepted" && kind == "abort" && h.runtimeTasks != nil {
		if _, err := h.runtimeTasks.Cancel(r.Context(), runSignalID(r.URL.Path)); err != nil {
			h.fail(w, err)
			return
		}
	}
	switch outcome {
	case "accepted":
		writeJSON(w, http.StatusOK, map[string]bool{"accepted": true})
	case "not_found":
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	default:
		writeJSON(w, http.StatusConflict, map[string]any{"accepted": false, "reason": "terminal"})
	}
}

func (h *HTTPServer) ackDeliveryByKey(w http.ResponseWriter, r *http.Request, raw []byte) {
	var input struct {
		IdempotencyKey string `json:"idempotencyKey"`
	}
	if !decodeJSON(w, raw, &input) {
		return
	}
	if input.IdempotencyKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "idempotencyKey required"})
		return
	}
	if err := h.deliveries.AckByKey(r.Context(), input.IdempotencyKey); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *HTTPServer) pendingDeliveries(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("type")
	claimMillis, _ := strconv.ParseFloat(r.URL.Query().Get("claimMs"), 64)
	if math.IsNaN(claimMillis) || math.IsInf(claimMillis, 0) || claimMillis <= 0 {
		claimMillis = 0
	}
	var (
		deliveries []data.PendingDelivery
		err        error
	)
	if claimMillis > 0 {
		deliveries, err = h.deliveries.ClaimPending(r.Context(), kind, claimMillis)
	} else {
		deliveries, err = h.deliveries.Pending(r.Context(), kind)
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": deliveries})
}

func (h *HTTPServer) ackDelivery(w http.ResponseWriter, r *http.Request, raw []byte) {
	var decoded any
	if !decodeJSON(w, raw, &decoded) {
		return
	}
	input, _ := decoded.(map[string]any)
	recipientThreadRef, _ := input["recipientThreadRef"].(string)
	var slackAPIMS *float64
	if value, ok := input["slackApiMs"].(float64); ok && !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 {
		slackAPIMS = &value
	}
	if err := h.deliveries.Ack(r.Context(), deliveryAckID(r.URL.Path), recipientThreadRef, slackAPIMS); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *HTTPServer) ingestEgressAudit(w http.ResponseWriter, r *http.Request, raw []byte) {
	var payload map[string]json.RawMessage
	if !decodeJSON(w, raw, &payload) {
		return
	}
	recordsRaw, ok := payload["records"]
	var records []json.RawMessage
	if !ok || json.Unmarshal(recordsRaw, &records) != nil || len(records) == 0 || len(records) > 500 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "records must be a non-empty array of at most 500"})
		return
	}
	accepted := make([]data.EgressAuditInput, 0, len(records))
	for _, rawRecord := range records {
		if record, ok := egressAuditRecord(rawRecord); ok {
			accepted = append(accepted, record)
		}
	}
	if err := h.egress.RecordAuditBatch(r.Context(), accepted); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"accepted": len(accepted), "rejected": len(records) - len(accepted)})
}

func (h *HTTPServer) claimBrokerNonce(w http.ResponseWriter, r *http.Request, raw []byte) {
	var payload map[string]json.RawMessage
	if !decodeJSON(w, raw, &payload) {
		return
	}
	var ids []string
	if rawIDs, ok := payload["ids"]; !ok || json.Unmarshal(rawIDs, &ids) != nil || len(ids) == 0 || len(ids) > 64 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "ids must hold 1 to 64 non-empty strings of at most 200 characters"})
		return
	}
	for _, id := range ids {
		if len([]rune(id)) == 0 || len([]rune(id)) > 200 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "ids must hold 1 to 64 non-empty strings of at most 200 characters"})
			return
		}
	}
	var expiresAtMillis float64
	if rawExpiry, ok := payload["expiresAtMs"]; !ok || json.Unmarshal(rawExpiry, &expiresAtMillis) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "expiresAtMs must be a future epoch-millisecond timestamp within 24 hours"})
		return
	}
	now := time.Now()
	if expiresAtMillis <= float64(now.UnixMilli()) || expiresAtMillis > float64(now.Add(24*time.Hour).UnixMilli()) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "expiresAtMs must be a future epoch-millisecond timestamp within 24 hours"})
		return
	}
	expiresAt := time.Unix(0, int64(expiresAtMillis*float64(time.Millisecond)))
	for _, id := range ids {
		claimed, err := h.replay.Claim(r.Context(), "authbroker:"+id, expiresAt)
		if err != nil {
			h.fail(w, err)
			return
		}
		if claimed {
			writeJSON(w, http.StatusOK, map[string]string{"claimed": id})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"claimed": nil})
}

func (h *HTTPServer) patchTurnMetrics(w http.ResponseWriter, r *http.Request, raw []byte) {
	var payload map[string]json.RawMessage
	if !decodeJSON(w, raw, &payload) {
		return
	}
	parseMetric := func(name string) *float64 {
		value, ok := payload[name]
		if !ok {
			return nil
		}
		var number float64
		if json.Unmarshal(value, &number) != nil || number < 0 {
			return nil
		}
		return &number
	}
	deliverMS, slackInflightMS := parseMetric("deliverMs"), parseMetric("slackInflightMs")
	if deliverMS == nil && slackInflightMS == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "deliverMs or slackInflightMs required"})
		return
	}
	if err := h.metrics.PatchByRunID(r.Context(), turnMetricsRunID(r.URL.Path), deliverMS, slackInflightMS); err != nil {
		h.logger.Error("patch turn metrics", "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func egressAuditRecord(raw json.RawMessage) (data.EgressAuditInput, bool) {
	var input map[string]any
	if json.Unmarshal(raw, &input) != nil {
		return data.EgressAuditInput{}, false
	}
	host, hostOK := egressAuditString(input["host"])
	verdict, verdictOK := egressAuditString(input["verdict"])
	if !hostOK || !verdictOK {
		return data.EgressAuditInput{}, false
	}
	scope, scopeOK := egressAuditString(input["scopeLabel"])
	if !scopeOK {
		scope = "unknown"
	}
	record := data.EgressAuditInput{Host: host, Allowed: verdict == "ok", Verdict: verdict, ScopeLabel: scope}
	if value, ok := egressAuditString(input["via"]); ok {
		record.Via = &value
	}
	if value, ok := egressAuditString(input["peerIp"]); ok {
		record.PeerIP = &value
	}
	if value, ok := egressAuditString(input["principalId"]); ok {
		record.PrincipalID = &value
	}
	if value, ok := input["port"].(float64); ok && value == float64(int(value)) && value > 0 && value <= 65535 {
		port := int(value)
		record.Port = &port
	}
	return record, true
}

func egressAuditString(value any) (string, bool) {
	text, ok := value.(string)
	if !ok || text == "" {
		return "", false
	}
	runes := []rune(text)
	if len(runes) > 512 {
		text = string(runes[:512])
	}
	return text, true
}

func (h *HTTPServer) listCrons(w http.ResponseWriter, r *http.Request) {
	records, err := h.crons.List(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	crons := make([]map[string]any, 0, len(records))
	for _, record := range records {
		cron, err := cronWithoutFireLog(record)
		if err != nil {
			h.fail(w, err)
			return
		}
		crons = append(crons, cron)
	}
	writeJSON(w, http.StatusOK, map[string]any{"crons": crons})
}

// decideTriggerConsent mirrors Node's recipient-only decision on a standing
// cron delivery. It changes only the cron's durable recipientConsent document;
// the scheduler observes that state before future delivery attempts.
func (h *HTTPServer) decideTriggerConsent(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	if identity.ActorID == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "consent requires an agent capability token"})
		return
	}
	var input struct {
		Decision string `json:"decision"`
	}
	if err := json.Unmarshal(raw, &input); err != nil || (input.Decision != "accept" && input.Decision != "decline") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": `decision must be "accept" or "decline"`})
		return
	}
	id := triggerConsentID(r.URL.Path)
	record, err := h.crons.Get(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if record == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(record.JSON, &document); err != nil || document == nil {
		h.fail(w, errors.New("cron JSON must be an object"))
		return
	}
	consentRaw, ok := document["recipientConsent"]
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "this trigger has no recipient consent to decide on"})
		return
	}
	var consent map[string]json.RawMessage
	if err := json.Unmarshal(consentRaw, &consent); err != nil || consent == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "this trigger has no recipient consent to decide on"})
		return
	}
	var recipientID string
	if err := json.Unmarshal(consent["recipientId"], &recipientID); err != nil || !sameSoulPerson(recipientID, identity.ActorID) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "only the delivery recipient can accept or decline this"})
		return
	}
	status := "accepted"
	if input.Decision == "decline" {
		status = "declined"
	}
	consent["status"], _ = json.Marshal(status)
	consent["decidedAt"], _ = json.Marshal(time.Now().UnixMilli())
	updatedConsent, err := json.Marshal(consent)
	if err != nil {
		h.fail(w, err)
		return
	}
	updated, err := h.crons.Merge(r.Context(), id, json.RawMessage(`{"recipientConsent":`+string(updatedConsent)+`}`), nil)
	if err != nil {
		h.fail(w, err)
		return
	}
	if updated == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(updated.JSON, &response); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "consent": jsonValue(response["recipientConsent"])})
}

// listCapabilityCrons is the durable half of Node's control.listCrons. A
// capability represents the current agent turn, so its actor and scope are
// enough to calculate the managed and read-only projections without asking the
// live Node runtime. Portal viewer queries continue to proxy because they do
// not carry that signed capability identity.
func (h *HTTPServer) listCapabilityCrons(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	records, err := h.crons.List(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	viewer, err := h.cronViewer(r.Context(), identity.ActorID)
	if err != nil {
		h.fail(w, err)
		return
	}
	managed, visible := make([]map[string]any, 0), make([]map[string]any, 0)
	for _, record := range records {
		cron, err := cronWithoutFireLog(record)
		if err != nil {
			h.fail(w, err)
			return
		}
		if !viewer.allows(cron) {
			continue
		}
		canManage, err := h.canAdministerCapabilityCron(r.Context(), cron, identity, viewer)
		if err != nil {
			h.fail(w, err)
			return
		}
		if canManage {
			managed = append(managed, viewer.withScopeName(cron))
			continue
		}
		if viewer.visible(cron) {
			visible = append(visible, viewer.withScopeName(cron))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"crons": managed, "visible": visible})
}

func (h *HTTPServer) getCapabilityCron(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/crons/")
	record, err := h.crons.Get(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if record == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	cron, err := cronWithoutFireLog(*record)
	if err != nil {
		h.fail(w, err)
		return
	}
	viewer, err := h.cronViewer(r.Context(), identity.ActorID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !viewer.allows(cron) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "not your cron"})
		return
	}
	canManage, err := h.canAdministerCapabilityCron(r.Context(), cron, identity, viewer)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !canManage && !viewer.visible(cron) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "not your cron"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cron": viewer.withScopeName(cron)})
}

func (h *HTTPServer) capabilityCronRuns(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/crons/"), "/runs")
	record, err := h.crons.Get(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if record == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	cron, err := cronWithoutFireLog(*record)
	if err != nil {
		h.fail(w, err)
		return
	}
	viewer, err := h.cronViewer(r.Context(), identity.ActorID)
	if err != nil {
		h.fail(w, err)
		return
	}
	canManage, err := h.canAdministerCapabilityCron(r.Context(), cron, identity, viewer)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !canManage {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "not your cron"})
		return
	}
	h.writeCronRuns(w, r, *record)
}

type cronCapabilityViewer struct {
	keys       map[string]bool
	scopeNames map[string]string
}

func (h *HTTPServer) cronViewer(ctx context.Context, principalID string) (cronCapabilityViewer, error) {
	keys := map[string]bool{cronPersonKey(principalID): true}
	member, err := h.directory.Get(ctx, principalID)
	if err != nil {
		return cronCapabilityViewer{}, err
	}
	internal := member != nil && member.Type == "internal"
	if member != nil {
		keys[cronPersonKey(member.PrincipalID)] = true
		if member.SlackID != "" {
			keys[cronPersonKey(member.SlackID)] = true
		}
	}
	scopeNames := map[string]string{"org:" + h.config.QM.OrgID: ""}
	if internal {
		channels, err := h.directory.ChannelsFor(ctx, principalID)
		if err != nil {
			return cronCapabilityViewer{}, err
		}
		for _, channel := range channels {
			scopeNames["channel:"+channel.ChannelID] = channel.Name
		}
	}
	projects, err := h.projects.List(ctx, principalID)
	if err != nil {
		return cronCapabilityViewer{}, err
	}
	for _, project := range projects {
		scopeNames[biz.ProjectScopeID(project.ID)] = project.Name
	}
	return cronCapabilityViewer{keys: keys, scopeNames: scopeNames}, nil
}

func (v cronCapabilityViewer) owns(principalID string) bool {
	return v.keys[cronPersonKey(principalID)]
}

func (v cronCapabilityViewer) allows(cron map[string]any) bool {
	scopeID, _ := cron["ownerScopeId"].(string)
	kind, ref := splitScopeID(scopeID)
	return kind != "group" || !strings.HasPrefix(ref, "web-project-") || v.scopeNames[scopeID] != ""
}

func (v cronCapabilityViewer) visible(cron map[string]any) bool {
	if v.owns(stringValue(cron["owner"])) {
		return false
	}
	scopeID, _ := cron["ownerScopeId"].(string)
	if _, ok := v.scopeNames[scopeID]; ok {
		return true
	}
	if members, ok := cron["members"].([]any); ok {
		for _, member := range members {
			if item, ok := member.(map[string]any); ok && v.owns(stringValue(item["id"])) {
				return true
			}
		}
	}
	if destination, ok := cron["destination"].(map[string]any); ok && destination["type"] == "principal" {
		return v.owns(stringValue(destination["target"]))
	}
	return false
}

func (v cronCapabilityViewer) withScopeName(cron map[string]any) map[string]any {
	name := v.scopeNames[stringValue(cron["ownerScopeId"])]
	if name == "" {
		return cron
	}
	copy := make(map[string]any, len(cron)+1)
	for key, value := range cron {
		copy[key] = value
	}
	copy["scopeName"] = name
	return copy
}

func (h *HTTPServer) canAdministerCapabilityCron(ctx context.Context, cron map[string]any, identity auth.Identity, viewer cronCapabilityViewer) (bool, error) {
	ownerScopeID := stringValue(cron["ownerScopeId"])
	kind, ref := splitScopeID(ownerScopeID)
	membershipControlled := kind == "group"
	if kind == "channel" {
		channel, err := h.directory.Channel(ctx, ref)
		if err != nil {
			return false, err
		}
		membershipControlled = channel != nil && channel.IsPrivate
	}
	if membershipControlled {
		return h.canManageResourceScope(ctx, identity.ActorID, ownerScopeID)
	}
	runAs := stringValue(cron["runAs"])
	team := runAs == "scopeFloor" || runAs == "scopeShared"
	if !team && viewer.owns(stringValue(cron["owner"])) {
		return true, nil
	}
	if team && ownerScopeID == identity.ScopeID {
		return true, nil
	}
	return h.canManageResourceScope(ctx, identity.ActorID, ownerScopeID)
}

func cronPersonKey(value string) string {
	value = strings.TrimSpace(value)
	if strings.Contains(value, "@") {
		return strings.ToLower(value)
	}
	return value
}

func (h *HTTPServer) getCron(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/crons/")
	record, err := h.crons.Get(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if record == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	cron, err := cronWithoutFireLog(*record)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cron": cron})
}

func (h *HTTPServer) disableCron(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/crons/"), "/disable")
	updated, err := h.crons.SetEnabled(r.Context(), id, false)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !updated {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// disableCapabilityCron ports the durable half of control.setCronEnabled.
// The notification is persisted to the shared delivery queue; Node remains the
// only process that performs the eventual provider delivery.
func (h *HTTPServer) disableCapabilityCron(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/crons/"), "/disable")
	record, err := h.crons.Get(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if record == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found", "message": "no cron " + id})
		return
	}
	cron, err := cronWithoutFireLog(*record)
	if err != nil {
		h.fail(w, err)
		return
	}
	viewer, err := h.cronViewer(r.Context(), identity.ActorID)
	if err != nil {
		h.fail(w, err)
		return
	}
	allowed, err := h.canAdministerCapabilityCron(r.Context(), cron, identity, viewer)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !allowed {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "not your cron"})
		return
	}
	updated, err := h.crons.SetEnabled(r.Context(), id, false)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !updated {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found", "message": "no cron " + id})
		return
	}
	if err := h.enqueueCapabilityCronEditNotice(r.Context(), *record, cron, identity.ActorID, "enabled=false"); err != nil {
		h.logger.Warn("cron edit notice enqueue failed", "cron_id", id, "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *HTTPServer) deleteCapabilityCron(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	id := cronIDForMutation(r.URL.Path)
	record, err := h.crons.Get(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if record == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found", "message": "no cron " + id})
		return
	}
	cron, err := cronWithoutFireLog(*record)
	if err != nil {
		h.fail(w, err)
		return
	}
	viewer, err := h.cronViewer(r.Context(), identity.ActorID)
	if err != nil {
		h.fail(w, err)
		return
	}
	allowed, err := h.canAdministerCapabilityCron(r.Context(), cron, identity, viewer)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !allowed {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "not your cron"})
		return
	}
	if err := h.crons.Delete(r.Context(), id); err != nil {
		h.fail(w, err)
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: stringValue(cron["owner"]), Action: "cron_delete", Resource: id, ScopeLabel: stringValue(cron["ownerScopeId"])}); err != nil {
		h.fail(w, err)
		return
	}
	if err := h.enqueueCapabilityCronEditNotice(r.Context(), *record, cron, identity.ActorID, "deleted"); err != nil {
		h.logger.Warn("cron edit notice enqueue failed", "cron_id", id, "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *HTTPServer) enqueueCapabilityCronEditNotice(ctx context.Context, record data.CronRecord, cron map[string]any, editorID, editFingerprint string) error {
	if stringValue(cron["runAs"]) != "scopeShared" {
		return nil
	}
	ownerID := stringValue(cron["owner"])
	if ownerID == "" {
		return nil
	}
	if h.sameCronPerson(ctx, ownerID, editorID) {
		return nil
	}
	editorName := editorID
	if editor, err := h.directory.Get(ctx, editorID); err != nil {
		return err
	} else if editor != nil && strings.TrimSpace(editor.DisplayName) != "" {
		editorName = editor.DisplayName
	}
	ref := "shared"
	if title := strings.TrimSpace(stringValue(cron["title"])); title != "" {
		ref = `"` + title + `"`
	}
	place := ""
	if kind, channelID := splitScopeID(stringValue(cron["ownerScopeId"])); kind == "channel" {
		if channel, err := h.directory.Channel(ctx, channelID); err != nil {
			return err
		} else if channel != nil && strings.TrimSpace(channel.Name) != "" {
			place = " in #" + strings.TrimPrefix(channel.Name, "#")
		}
	}
	verb := "edited"
	switch editFingerprint {
	case "enabled=false":
		verb = "paused"
	case "enabled=true":
		verb = "re-enabled"
	case "deleted":
		verb = "deleted"
	}
	digest := sha256.Sum256([]byte(record.ID + ":" + editorID + ":" + editFingerprint))
	destination, _ := json.Marshal(map[string]string{"type": "principal", "target": ownerID, "onBehalfOf": editorID})
	return h.deliveries.Enqueue(ctx, data.DeliveryEnqueueInput{
		Destination:    destination,
		Text:           "Heads up: " + editorName + " " + verb + " your " + ref + " cron" + place + ".",
		IdempotencyKey: "cron-edit-notice:" + record.ID + ":" + hex.EncodeToString(digest[:])[:16],
	})
}

func (h *HTTPServer) cronRuns(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/crons/"), "/runs")
	record, err := h.crons.Get(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if record == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	h.writeCronRuns(w, r, *record)
}

// writeCronRuns deliberately consumes the record already used for
// authorization. Capability callers must not authorize one cron revision and
// then make a second read which could observe a concurrent owner/visibility
// update before the fire log is returned.
func (h *HTTPServer) writeCronRuns(w http.ResponseWriter, r *http.Request, record data.CronRecord) {
	var document map[string]any
	if err := json.Unmarshal(record.JSON, &document); err != nil || document == nil {
		h.fail(w, errors.New("cron JSON must be an object"))
		return
	}
	runs := []any{}
	if entries, ok := document["fireLog"].([]any); ok {
		runs = entries
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "limit must be a positive integer"})
			return
		}
		if limit < len(runs) {
			runs = runs[len(runs)-limit:]
		}
	}
	cron, err := cronWithoutFireLog(record)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cron": cron, "runs": runs, "total": len(documentFireLog(document))})
}

func (h *HTTPServer) patchCron(w http.ResponseWriter, r *http.Request, raw []byte) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/crons/")
	current, err := h.crons.Get(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if current == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(raw, &input); err != nil || len(input) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": cronPatchMessage})
		return
	}
	if _, ok := input["runAs"]; ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "changing a cron's mode requires an agent capability token"})
		return
	}
	patch, err := sourceCronPatch(input, current.JSON, time.Now().UnixMilli())
	if err != nil {
		var updateErr *cronScheduleUpdateError
		if errors.As(err, &updateErr) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cron_update_failed", "message": updateErr.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": err.Error()})
		return
	}
	updated, err := h.crons.Merge(r.Context(), id, patch, nil)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cron_update_failed", "message": err.Error()})
		return
	}
	if updated == nil {
		writeJSON(w, http.StatusOK, map[string]any{"cron": nil})
		return
	}
	var before map[string]json.RawMessage
	if json.Unmarshal(current.JSON, &before) == nil {
		var owner, scope string
		_ = json.Unmarshal(before["owner"], &owner)
		_ = json.Unmarshal(before["ownerScopeId"], &scope)
		if owner != "" {
			if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: owner, Action: "cron_update", Resource: id, ScopeLabel: scope}); err != nil {
				h.fail(w, err)
				return
			}
		}
	}
	cron, err := cronWithoutFireLog(*updated)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cron": cron})
}

// patchCapabilityCron ports the owner-mode durable half of control.patchCron.
// Shared/team mode patches are deliberately proxied because Node must refresh
// the signed member snapshot and dispatch its richer edit notification.
func (h *HTTPServer) patchCapabilityCron(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	id := cronIDForMutation(r.URL.Path)
	record, err := h.crons.Get(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if record == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found", "message": "no cron " + id})
		return
	}
	cron, err := cronWithoutFireLog(*record)
	if err != nil {
		h.fail(w, err)
		return
	}
	if runAs := stringValue(cron["runAs"]); runAs == "scopeFloor" || runAs == "scopeShared" {
		h.proxy.ServeHTTP(w, r)
		return
	}
	viewer, err := h.cronViewer(r.Context(), identity.ActorID)
	if err != nil {
		h.fail(w, err)
		return
	}
	allowed, err := h.canAdministerCapabilityCron(r.Context(), cron, identity, viewer)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !allowed {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "not your cron"})
		return
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(raw, &input); err != nil || len(input) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": cronPatchMessage})
		return
	}
	patch, err := sourceCronPatch(input, record.JSON, time.Now().UnixMilli())
	if err != nil {
		var updateErr *cronScheduleUpdateError
		if errors.As(err, &updateErr) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cron_update_failed", "message": updateErr.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": err.Error()})
		return
	}
	updated, err := h.crons.Merge(r.Context(), id, patch, nil)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cron_update_failed", "message": err.Error()})
		return
	}
	if updated == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found", "message": "no cron " + id})
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: stringValue(cron["owner"]), Action: "cron_update", Resource: id, ScopeLabel: stringValue(cron["ownerScopeId"])}); err != nil {
		h.fail(w, err)
		return
	}
	view, err := cronWithoutFireLog(*updated)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cron": view})
}

// retargetCapabilityCron selects a destination only from the signed
// capability projection. This keeps the control plane from becoming an
// arbitrary Slack/provider-send surface while moving the durable cron update
// out of Node.
func (h *HTTPServer) retargetCapabilityCron(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	if identity.ActorID == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "retarget requires an agent capability token"})
		return
	}
	var input map[string]json.RawMessage
	if json.Unmarshal(raw, &input) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "destinationKey (string) required"})
		return
	}
	var destinationKey string
	if value, ok := input["destinationKey"]; !ok || json.Unmarshal(value, &destinationKey) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "destinationKey (string) required"})
		return
	}
	id := cronDestinationID(r.URL.Path)
	record, err := h.crons.Get(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if record == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found", "message": "no cron " + id})
		return
	}
	cron, err := cronWithoutFireLog(*record)
	if err != nil {
		h.fail(w, err)
		return
	}
	viewer, err := h.cronViewer(r.Context(), identity.ActorID)
	if err != nil {
		h.fail(w, err)
		return
	}
	allowed, err := h.canAdministerCapabilityCron(r.Context(), cron, identity, viewer)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !allowed {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "not your cron"})
		return
	}
	var chosen *auth.DestinationCandidate
	for i := range identity.Destinations {
		if identity.Destinations[i].Key == destinationKey {
			chosen = &identity.Destinations[i]
			break
		}
	}
	if chosen == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown_destination", "message": "destinationKey is not one of the destinations available for this conversation"})
		return
	}
	destination := map[string]string{"type": chosen.Type, "target": chosen.Target}
	if chosen.AudienceScopeID != "" {
		destination["audienceScopeId"] = chosen.AudienceScopeID
	}
	patch, _ := json.Marshal(map[string]any{"destination": destination})
	updated, err := h.crons.Merge(r.Context(), id, patch, nil)
	if err != nil {
		h.fail(w, err)
		return
	}
	if updated == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found", "message": "no cron " + id})
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: stringValue(cron["owner"]), Action: "cron_retarget", Resource: id, ScopeLabel: stringValue(cron["ownerScopeId"])}); err != nil {
		h.fail(w, err)
		return
	}
	if err := h.enqueueCapabilityCronDestinationNotice(r.Context(), *record, cron, identity.ActorID, chosen.Target, chosen.Label); err != nil {
		h.logger.Warn("cron destination edit notice enqueue failed", "cron_id", id, "error", err)
	}
	view, err := cronWithoutFireLog(*updated)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cron": view})
}

func (h *HTTPServer) enqueueCapabilityCronDestinationNotice(ctx context.Context, record data.CronRecord, cron map[string]any, editorID, target, destinationLabel string) error {
	if stringValue(cron["runAs"]) != "scopeShared" || h.deliveries == nil {
		return nil
	}
	ownerID := stringValue(cron["owner"])
	if ownerID == "" || h.sameCronPerson(ctx, ownerID, editorID) {
		return nil
	}
	editorName := editorID
	if editor, err := h.directory.Get(ctx, editorID); err != nil {
		return err
	} else if editor != nil && strings.TrimSpace(editor.DisplayName) != "" {
		editorName = editor.DisplayName
	}
	ref := "shared"
	if title := strings.TrimSpace(stringValue(cron["title"])); title != "" {
		ref = `"` + title + `"`
	}
	place := ""
	if kind, channelID := splitScopeID(stringValue(cron["ownerScopeId"])); kind == "channel" {
		if channel, err := h.directory.Channel(ctx, channelID); err != nil {
			return err
		} else if channel != nil && strings.TrimSpace(channel.Name) != "" {
			place = " in #" + strings.TrimPrefix(channel.Name, "#")
		}
	}
	label := "to post somewhere new"
	if strings.TrimSpace(destinationLabel) != "" {
		label = "to post to " + destinationLabel
	}
	digest := sha256.Sum256([]byte(record.ID + ":" + editorID + ":destination:" + target))
	destination, _ := json.Marshal(map[string]string{"type": "principal", "target": ownerID, "audienceScopeId": "personal:" + ownerID, "onBehalfOf": editorID})
	return h.deliveries.Enqueue(ctx, data.DeliveryEnqueueInput{
		Destination:    destination,
		Text:           "Heads up: " + editorName + " changed your " + ref + " cron" + place + " " + label + ".",
		IdempotencyKey: "cron-edit-notice:" + record.ID + ":" + hex.EncodeToString(digest[:])[:16],
	})
}

func (h *HTTPServer) deleteCron(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/crons/")
	current, err := h.crons.Get(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if current == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	if err := h.crons.Delete(r.Context(), id); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

const cronPatchMessage = "expected a cron patch: title (string), task (string), schedule, enabled (boolean), archived (boolean), unfurlLinks (boolean), and/or runAs (owner/scopeFloor/scopeShared)"

func sourceCronPatch(input map[string]json.RawMessage, current json.RawMessage, now int64) (json.RawMessage, error) {
	patch := map[string]json.RawMessage{}
	valid := false
	for _, field := range []string{"title", "action", "task", "enabled", "archived", "unfurlLinks", "schedule"} {
		if _, ok := input[field]; ok {
			valid = true
		}
	}
	if !valid {
		return nil, errors.New(cronPatchMessage)
	}
	if value, ok := input["title"]; ok && !jsonString(value) {
		return nil, errors.New(cronPatchMessage)
	} else if ok {
		var title string
		_ = json.Unmarshal(value, &title)
		patch["title"], _ = json.Marshal(normalizeCronTitle(title))
	}
	if action, ok := input["action"]; ok && !jsonString(action) {
		return nil, errors.New(cronPatchMessage)
	} else if ok {
		patch["action"] = action
	}
	if task, ok := input["task"]; ok && !jsonString(task) {
		return nil, errors.New(cronPatchMessage)
	} else if ok {
		patch["action"] = task
	}
	for _, field := range []string{"enabled", "archived"} {
		if value, ok := input[field]; ok {
			if !jsonBool(value) {
				return nil, errors.New(cronPatchMessage)
			}
			patch[field] = value
		}
	}
	if archived, ok := input["archived"]; ok && string(archived) == "true" {
		patch["enabled"] = json.RawMessage("false")
	}
	if value, ok := input["unfurlLinks"]; ok {
		if !jsonBool(value) {
			return nil, errors.New(cronPatchMessage)
		}
		var document map[string]json.RawMessage
		if err := json.Unmarshal(current, &document); err != nil {
			return nil, err
		}
		destination, ok := document["destination"]
		if !ok || string(destination) == "null" {
			return nil, errors.New("unfurlLinks can only be set on a cron with a delivery destination")
		}
		var destinationObject map[string]json.RawMessage
		if err := json.Unmarshal(destination, &destinationObject); err != nil || destinationObject == nil {
			return nil, errors.New("unfurlLinks can only be set on a cron with a delivery destination")
		}
		destinationObject["unfurlLinks"] = value
		patch["destination"], _ = json.Marshal(destinationObject)
	}
	if value, ok := input["schedule"]; ok {
		schedule, nextFireAt, err := normalizeSourceIntervalSchedule(value, now)
		if err != nil {
			return nil, err
		}
		patch["schedule"] = schedule
		patch["nextFireAt"], _ = json.Marshal(nextFireAt)
	}
	return json.Marshal(patch)
}

func jsonString(value json.RawMessage) bool {
	var out string
	return json.Unmarshal(value, &out) == nil
}
func jsonBool(value json.RawMessage) bool { var out bool; return json.Unmarshal(value, &out) == nil }

func normalizeCronTitle(value string) string {
	title := strings.Join(strings.Fields(value), " ")
	if title == "" {
		return ""
	}
	chars := []rune(title)
	if len(chars) <= 80 {
		return title
	}
	return string(chars[:79]) + "..."
}

func documentFireLog(document map[string]any) []any {
	entries, _ := document["fireLog"].([]any)
	if entries == nil {
		return []any{}
	}
	return entries
}

func cronWithoutFireLog(record data.CronRecord) (map[string]any, error) {
	var cron map[string]any
	if err := json.Unmarshal(record.JSON, &cron); err != nil || cron == nil {
		if err == nil {
			err = errors.New("cron JSON must be an object")
		}
		return nil, err
	}
	delete(cron, "fireLog")
	if _, ok := cron["id"]; !ok {
		cron["id"] = record.ID
	}
	return cron, nil
}

func (h *HTTPServer) listEnvironments(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	if identity.ActorID == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "environments require an agent capability token"})
		return
	}
	environments, err := h.environments.List(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	type environmentReply struct {
		ID             string   `json:"id"`
		Name           *string  `json:"name"`
		OwnerActorID   *string  `json:"ownerActorId"`
		AttachedScopes []string `json:"attachedScopes"`
	}
	result := make([]environmentReply, 0, len(environments))
	for _, environment := range environments {
		attachments, err := h.environments.AttachmentsFor(r.Context(), environment.ID)
		if err != nil {
			h.fail(w, err)
			return
		}
		scopes := make([]string, 0, len(attachments))
		for _, attachment := range attachments {
			scopes = append(scopes, attachment.ScopeID)
		}
		result = append(result, environmentReply{ID: environment.ID, Name: environment.Name, OwnerActorID: environment.OwnerActorID, AttachedScopes: scopes})
	}
	writeJSON(w, http.StatusOK, map[string]any{"environments": result})
}

func (h *HTTPServer) createEnvironment(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	if identity.ActorID == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "environments require an agent capability token"})
		return
	}
	var input struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, raw, &input) {
		return
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "name (string) required"})
		return
	}
	environment, err := h.environments.Create(r.Context(), identity.ScopeID, name, identity.ActorID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "environment_create_failed", "message": err.Error()})
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: identity.ActorID, Action: "environment_create", Resource: environment.ID, ScopeLabel: identity.ScopeID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"environment": map[string]any{"id": environment.ID, "name": environment.Name, "ownerActorId": environment.OwnerActorID}})
}

func (h *HTTPServer) attachEnvironment(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	if identity.ActorID == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "environments require an agent capability token"})
		return
	}
	var input struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, raw, &input) {
		return
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "name (string) required"})
		return
	}
	environment, err := h.environments.FindByName(r.Context(), name)
	if err != nil {
		h.fail(w, err)
		return
	}
	if environment == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "environment_not_found", "message": `no environment named "` + name + `"`})
		return
	}
	if environment.OwnerActorID != nil && !samePrincipal(*environment.OwnerActorID, identity.ActorID) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":        "owner_mediation_required",
			"message":      `environment "` + name + `" is owned by ` + *environment.OwnerActorID + `. Ask them to attach this conversation to it (the same way you'd ask an owner for a credential grant) — only its owner can attach others.`,
			"ownerActorId": *environment.OwnerActorID,
		})
		return
	}
	if err := h.environments.Attach(r.Context(), identity.ScopeID, environment.ID, identity.ActorID); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "environment_attach_failed", "message": err.Error()})
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: identity.ActorID, Action: "environment_attach", Resource: environment.ID, ScopeLabel: identity.ScopeID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "environment": map[string]any{"id": environment.ID, "name": environment.Name}})
}

func (h *HTTPServer) getSurfaceCachePolicy(w http.ResponseWriter, r *http.Request) {
	container := strings.TrimSpace(r.URL.Query().Get("container"))
	if container == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "container required"})
		return
	}
	policy, err := h.channelPolicy.Get(r.Context(), container)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policy": policy})
}

func (h *HTTPServer) setSurfaceCachePolicy(w http.ResponseWriter, r *http.Request, raw []byte) {
	var input struct {
		Container string  `json:"container"`
		Orders    *string `json:"orders"`
		SetBy     string  `json:"setBy"`
	}
	if !decodeJSON(w, raw, &input) {
		return
	}
	if strings.TrimSpace(input.Container) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "container required"})
		return
	}
	if input.Orders == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "orders (string) required"})
		return
	}
	policy, err := h.channelPolicy.Set(r.Context(), input.Container, *input.Orders, input.SetBy)
	if err != nil {
		h.fail(w, err)
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: firstNonEmpty(input.SetBy, "system"), Action: "surface.policy.set", Resource: policy.Container, ScopeLabel: "org:" + h.config.QM.OrgID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policy": policy})
}

func (h *HTTPServer) getContextPolicy(w http.ResponseWriter, r *http.Request) {
	principalID := strings.TrimSpace(r.URL.Query().Get("principalId"))
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	kind, container, ok := contextPolicyContainer(scope)
	if principalID == "" || scope == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "principalId and scope required"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "ambient policy applies to channel and group scopes only"})
		return
	}
	member, err := h.directory.IsScopeMember(r.Context(), kind, container, principalID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !member {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	policy, err := h.channelPolicy.Get(r.Context(), container)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policy": contextPolicyReply(policy)})
}

func (h *HTTPServer) setContextPolicy(w http.ResponseWriter, r *http.Request, raw []byte) {
	var input struct {
		PrincipalID    string          `json:"principalId"`
		Scope          string          `json:"scope"`
		Orders         *string         `json:"orders"`
		Bots           json.RawMessage `json:"bots"`
		AmbientEnabled json.RawMessage `json:"ambientEnabled"`
		BaseUpdatedAt  *int64          `json:"baseUpdatedAt"`
	}
	if !decodeJSON(w, raw, &input) {
		return
	}
	principalID, scope := strings.TrimSpace(input.PrincipalID), strings.TrimSpace(input.Scope)
	kind, container, ok := contextPolicyContainer(scope)
	if principalID == "" || scope == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "principalId and scope required"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "ambient policy applies to channel and group scopes only"})
		return
	}
	member, err := h.directory.IsScopeMember(r.Context(), kind, container, principalID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !member {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	if input.Orders == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "orders (string) required"})
		return
	}
	if len(*input.Orders) > 20_000 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "standing order is capped at 20000 characters — it is rendered into every ambient judgment"})
		return
	}
	bots, parseError := parsePolicyBots(input.Bots)
	if parseError != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": parseError})
		return
	}
	options := data.ChannelPolicySetOptions{Bots: &bots, ExpectedUpdatedAt: input.BaseUpdatedAt}
	if len(input.AmbientEnabled) > 0 {
		if string(input.AmbientEnabled) == "null" {
			options.SetAmbientEnabled = true
		} else {
			var enabled bool
			if err := json.Unmarshal(input.AmbientEnabled, &enabled); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "ambientEnabled must be a boolean or null (null = default rule)"})
				return
			}
			options.AmbientEnabled, options.SetAmbientEnabled = &enabled, true
		}
	}
	policy, conflict, err := h.channelPolicy.SetWithOptions(r.Context(), container, *input.Orders, principalID, options)
	if err != nil {
		h.fail(w, err)
		return
	}
	if conflict {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "conflict", "message": "this channel's policy changed since you loaded it — reload and re-apply your edit"})
		return
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: principalID, Action: "surface.policy.set", Resource: container, ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policy": contextPolicyReply(policy)})
}

// fulfillSurfaceContext is the source-authenticated callback from a surface
// worker. Node still creates and drains context requests; Go only performs the
// durable completion write so the waiting Node caller sees the same map row.
func (h *HTTPServer) fulfillSurfaceContext(w http.ResponseWriter, r *http.Request, raw []byte) {
	id := surfaceContextResultID(r.URL.Path)
	if id == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	var body map[string]json.RawMessage
	if !decodeJSON(w, raw, &body) {
		return
	}
	var failure *string
	if rawError, ok := body["error"]; ok {
		var value string
		if trimmed := bytes.TrimSpace(rawError); len(trimmed) > 0 && trimmed[0] == '"' && json.Unmarshal(trimmed, &value) == nil {
			failure = &value
		}
	}
	var result json.RawMessage
	if failure == nil {
		value := map[string]any{"messages": []any{}}
		if rawMessages, ok := body["messages"]; ok {
			var messages []any
			if trimmed := bytes.TrimSpace(rawMessages); len(trimmed) > 0 && trimmed[0] == '[' && json.Unmarshal(trimmed, &messages) == nil {
				value["messages"] = messages
			}
		}
		var hasMore bool
		if source, ok := body["hasMore"]; ok {
			trimmed := bytes.TrimSpace(source)
			if string(trimmed) == "true" || string(trimmed) == "false" {
				if json.Unmarshal(trimmed, &hasMore) == nil {
					value["hasMore"] = hasMore
				}
			}
		}
		for _, key := range []string{"nextBefore", "note"} {
			var text string
			if source, ok := body[key]; ok {
				trimmed := bytes.TrimSpace(source)
				if len(trimmed) > 0 && trimmed[0] == '"' && json.Unmarshal(trimmed, &text) == nil {
					value[key] = text
				}
			}
		}
		var file struct {
			BlobID, Name, Mimetype, Author string
			SizeBytes                      *float64
		}
		if source, ok := body["file"]; ok && json.Unmarshal(source, &file) == nil && file.BlobID != "" && file.Name != "" && file.SizeBytes != nil {
			entry := map[string]any{"blobId": file.BlobID, "name": file.Name, "sizeBytes": *file.SizeBytes}
			if file.Mimetype != "" {
				entry["mimetype"] = file.Mimetype
			}
			if file.Author != "" {
				entry["author"] = file.Author
			}
			value["file"] = entry
		}
		var group struct{ GroupID string }
		if source, ok := body["group"]; ok && json.Unmarshal(source, &group) == nil && group.GroupID != "" {
			value["group"] = map[string]any{"groupId": group.GroupID}
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			h.fail(w, err)
			return
		}
		result = encoded
	}
	ok, err := h.contextRequests.Fulfill(r.Context(), id, result, failure)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found", "message": "request expired or already answered"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func contextPolicyContainer(scope string) (string, string, bool) {
	parts := strings.SplitN(scope, ":", 2)
	if len(parts) != 2 || parts[1] == "" || (parts[0] != "channel" && parts[0] != "group") {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func contextPolicyReply(policy *data.ChannelPolicy) map[string]any {
	if policy == nil {
		return map[string]any{"orders": "", "bots": map[string]any{}, "ambientEnabled": nil, "updatedAt": 0}
	}
	return map[string]any{"orders": policy.Orders, "bots": policy.Bots, "ambientEnabled": policy.AmbientEnabled, "updatedAt": policy.UpdatedAt}
}

func parsePolicyBots(raw json.RawMessage) (map[string]any, string) {
	if len(raw) == 0 {
		return map[string]any{}, ""
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return nil, "bots must be an object keyed by bot author name"
	}
	if len(values) > 200 {
		return nil, "bot ledger is capped at 200 entries"
	}
	result := map[string]any{}
	seen := map[string]bool{}
	for name, rawPolicy := range values {
		key := strings.TrimSpace(name)
		if key == "" {
			return nil, "bot name must be non-empty"
		}
		lower := strings.ToLower(key)
		if seen[lower] {
			return nil, `duplicate bot "` + key + `" — names match case-insensitively`
		}
		seen[lower] = true
		var value struct {
			Mode        string   `json:"mode"`
			RollupHours *float64 `json:"rollupHours"`
		}
		if err := json.Unmarshal(rawPolicy, &value); err != nil || (value.Mode != "ignore" && value.Mode != "rollup" && value.Mode != "action" && value.Mode != "user") {
			return nil, `bot "` + key + `": mode must be one of ignore | rollup | action | user`
		}
		if value.RollupHours != nil && (*value.RollupHours <= 0 || *value.RollupHours != *value.RollupHours) {
			return nil, `bot "` + key + `": rollupHours must be a positive number`
		}
		out := map[string]any{"mode": value.Mode}
		if value.Mode == "rollup" && value.RollupHours != nil {
			out["rollupHours"] = *value.RollupHours
		}
		result[key] = out
	}
	return result, ""
}

func (h *HTTPServer) createProject(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	var input struct {
		PrincipalID string `json:"principalId"`
		Name        string `json:"name"`
	}
	if !decodeJSON(w, raw, &input) {
		return
	}
	principal, ok := capabilityPrincipal(w, identity, strings.TrimSpace(input.PrincipalID))
	if !ok {
		return
	}
	project, err := h.projects.Create(r.Context(), principal, input.Name)
	if err != nil {
		h.fail(w, err)
		return
	}
	if project == nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	h.ensureProjectKnowledge(r.Context(), *project, principal)
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: principal, Action: "project.create", Resource: project.ID, ScopeLabel: biz.ProjectScopeID(project.ID), IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	view, err := h.projectView(r.Context(), *project)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"project": view})
}

func (h *HTTPServer) renameProject(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	var input struct {
		PrincipalID string `json:"principalId"`
		Name        string `json:"name"`
	}
	if !decodeJSON(w, raw, &input) {
		return
	}
	principal, ok := capabilityPrincipal(w, identity, strings.TrimSpace(input.PrincipalID))
	if !ok {
		return
	}
	result, err := h.projects.Rename(r.Context(), projectRenameID(r.URL.Path), principal, input.Name)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.writeMutation(w, r, principal, identity, result, "project.rename", func(project biz.Project) {
		h.ensureProjectKnowledge(r.Context(), project, principal)
	})
}

func (h *HTTPServer) addProjectMember(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	var input struct {
		PrincipalID string `json:"principalId"`
		MemberID    string `json:"memberId"`
	}
	if !decodeJSON(w, raw, &input) {
		return
	}
	principal, ok := capabilityPrincipal(w, identity, strings.TrimSpace(input.PrincipalID))
	if !ok {
		return
	}
	result, err := h.projects.AddMember(r.Context(), projectMemberCollectionID(r.URL.Path), principal, strings.TrimSpace(input.MemberID))
	if err != nil {
		h.fail(w, err)
		return
	}
	h.writeMutation(w, r, principal, identity, result, "project.member.add", nil)
}

func (h *HTTPServer) removeProjectMember(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	var input struct {
		PrincipalID string `json:"principalId"`
	}
	// Capability callers intentionally send an empty DELETE body; source
	// callers may still supply principalId in the JSON object, as Node does.
	if len(bytes.TrimSpace(raw)) != 0 && !decodeJSON(w, raw, &input) {
		return
	}
	principal, ok := capabilityPrincipal(w, identity, strings.TrimSpace(input.PrincipalID))
	if !ok {
		return
	}
	projectID, memberID := projectMemberID(r.URL.Path)
	result, err := h.projects.RemoveMember(r.Context(), projectID, principal, memberID)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.writeMutation(w, r, principal, identity, result, "project.member.remove", nil)
}

func (h *HTTPServer) writeMutation(w http.ResponseWriter, r *http.Request, principal string, identity auth.Identity, result biz.ProjectMutation, action string, onChange func(biz.Project)) {
	switch result.Status {
	case "ok":
		if result.Changed {
			if onChange != nil {
				onChange(*result.Project)
			}
			if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: principal, Action: action, Resource: result.Project.ID, ScopeLabel: biz.ProjectScopeID(result.Project.ID), IdempotencyKey: requestID(r)}); err != nil {
				h.fail(w, err)
				return
			}
		}
		view, err := h.projectView(r.Context(), *result.Project)
		if err != nil {
			h.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"project": view})
	case "not_found":
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	case "forbidden":
		if identity.ActorID != "" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		} else {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		}
	case "invalid_name":
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_name", "message": "project name required"})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_member", "message": "member must be an internal directory member and cannot be the project owner"})
	}
}

// Node treats Knowledge Core as an optional index: a project remains usable
// when its workspace cannot be initialized, but operators receive a durable
// audit signal for retry and diagnosis.
func (h *HTTPServer) ensureProjectKnowledge(ctx context.Context, project biz.Project, actor string) {
	if h.knowledgeAgent == nil {
		return
	}
	ref := knowlega.ScopeRef{OrgID: project.OrgID, ExternalScopeID: biz.ProjectScopeID(project.ID), Kind: "project", Name: project.Name}
	status, err := h.knowledgeAgent.EnsureScope(ctx, ref)
	if err == nil {
		err = h.persistKnowledgeScope(ctx, ref, status)
	}
	if err != nil {
		_ = h.audit.Record(ctx, data.AuditEvent{PrincipalID: actor, Action: "project.knowledge.ensure_failed", Resource: project.ID, ScopeLabel: biz.ProjectScopeID(project.ID), Detail: err.Error()})
	}
}

func (h *HTTPServer) persistKnowledgeScope(ctx context.Context, ref knowlega.ScopeRef, status knowlega.ScopeStatus) error {
	if h.knowledgeScopes == nil {
		return nil
	}
	_, err := h.knowledgeScopes.Ensure(ctx, data.KnowledgeScope{
		OrgID:           ref.OrgID,
		ExternalScopeID: ref.ExternalScopeID,
		Kind:            ref.Kind,
		ProjectID:       status.ProjectID,
		ProjectName:     ref.Name,
		RootPath:        status.ProjectPath,
		Status:          status.State,
	})
	return err
}

func (h *HTTPServer) refreshKnowledgeScope(ctx context.Context, ref knowlega.ScopeRef) error {
	if h.knowledgeAgent == nil {
		return nil
	}
	status, err := h.knowledgeAgent.RefreshScope(ctx, ref)
	if err != nil {
		return err
	}
	return h.persistKnowledgeScope(ctx, ref, status)
}

func (h *HTTPServer) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	h.logger.Error("request failed", "error", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error", "message": "internal server error"})
}

func capabilityPrincipal(w http.ResponseWriter, identity auth.Identity, requested string) (string, bool) {
	if identity.ActorID == "" {
		return requested, true
	}
	if requested != "" && requested != identity.ActorID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return "", false
	}
	return identity.ActorID, true
}

func projectRenameID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 3 || parts[0] != "v1" || parts[1] != "projects" || parts[2] == "" {
		return ""
	}
	return parts[2]
}

func projectMemberCollectionID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "projects" || parts[2] == "" || parts[3] != "members" {
		return ""
	}
	return parts[2]
}

func projectMemberID(path string) (string, string) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 5 || parts[0] != "v1" || parts[1] != "projects" || parts[2] == "" || parts[3] != "members" || parts[4] == "" {
		return "", ""
	}
	return parts[2], parts[4]
}

func projectFileCollectionID(value string) string {
	parts := strings.Split(strings.Trim(value, "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "projects" || parts[2] == "" || parts[3] != "files" {
		return ""
	}
	return parts[2]
}

func projectKnowledgeDocumentID(value string) string {
	parts := strings.Split(strings.Trim(value, "/"), "/")
	if len(parts) != 5 || parts[0] != "v1" || parts[1] != "projects" || parts[2] == "" || parts[3] != "knowledge" || parts[4] != "documents" {
		return ""
	}
	return parts[2]
}

func projectFileID(value string) (string, string) {
	parts := strings.Split(strings.Trim(value, "/"), "/")
	if len(parts) != 5 || parts[0] != "v1" || parts[1] != "projects" || parts[2] == "" || parts[3] != "files" || parts[4] == "" {
		return "", ""
	}
	return parts[2], parts[4]
}

func projectFileRetryID(value string) (string, string) {
	parts := strings.Split(strings.Trim(value, "/"), "/")
	if len(parts) != 6 || parts[0] != "v1" || parts[1] != "projects" || parts[2] == "" || parts[3] != "files" || parts[4] == "" || parts[5] != "retry" {
		return "", ""
	}
	return parts[2], parts[4]
}
func approvalID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 3 || parts[0] != "v1" || parts[1] != "approvals" || parts[2] == "" || parts[2] == "pending" {
		return ""
	}
	return parts[2]
}
func deploymentID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 3 || parts[0] != "v1" || parts[1] != "deployments" || parts[2] == "" {
		return ""
	}
	return parts[2]
}
func deploymentNameID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "deployments" || parts[2] == "" || parts[3] != "name" {
		return ""
	}
	return parts[2]
}
func deploymentDisplayNameID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "deployments" || parts[2] == "" || parts[3] != "display-name" {
		return ""
	}
	return parts[2]
}

func deploymentShareID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "deployments" || parts[2] == "" || parts[3] != "share" {
		return ""
	}
	return parts[2]
}
func surfaceContextResultID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "surface-context" || parts[2] == "" || parts[3] != "result" {
		return ""
	}
	return parts[2]
}
func fileContentID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "files" || parts[2] == "" || parts[3] != "content" {
		return ""
	}
	return parts[2]
}
func blobTransferID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 3 || parts[0] != "v1" || parts[1] != "blobs" || !blobTransferIDPattern.MatchString(parts[2]) {
		return ""
	}
	return parts[2]
}
func triggerConsentID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "triggers" || parts[2] == "" || parts[3] != "consent" {
		return ""
	}
	return parts[2]
}
func runDeliveryStateID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "runs" || parts[2] == "" || parts[3] != "delivery-state" {
		return ""
	}
	return parts[2]
}
func runSignalID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "runs" || parts[2] == "" || parts[3] != "signal" {
		return ""
	}
	return parts[2]
}

func runRecordID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 3 || parts[0] != "v1" || parts[1] != "runs" || parts[2] == "" {
		return ""
	}
	return parts[2]
}
func turnMetricsRunID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "turns" || parts[2] == "" || parts[3] != "metrics" {
		return ""
	}
	return parts[2]
}
func deliveryAckID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "deliveries" || parts[2] == "" || parts[3] != "ack" {
		return ""
	}
	return parts[2]
}
func adminSessionLLMID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 5 || parts[0] != "v1" || parts[1] != "admin" || parts[2] != "sessions" || parts[3] == "" || parts[4] != "llm" {
		return ""
	}
	return parts[3]
}
func adminSessionDetailID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "admin" || parts[2] != "sessions" || parts[3] == "" {
		return ""
	}
	return parts[3]
}

func adminSkillID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "admin" || parts[2] != "skills" || parts[3] == "" {
		return ""
	}
	return parts[3]
}

func adminSkillPackID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "admin" || parts[2] != "skill-packs" || parts[3] == "" {
		return ""
	}
	return parts[3]
}

func adminCronDestinationID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 5 || parts[0] != "v1" || parts[1] != "admin" || parts[2] != "crons" || parts[3] == "" || parts[4] != "destination" {
		return ""
	}
	return parts[3]
}

func skillID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 3 || parts[0] != "v1" || parts[1] != "skills" || parts[2] == "" {
		return ""
	}
	return parts[2]
}

func adminUserOnboardingID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 5 || parts[0] != "v1" || parts[1] != "admin" || parts[2] != "users" || parts[3] == "" || parts[4] != "onboarding" {
		return ""
	}
	return parts[3]
}

func adminUserResetID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 5 || parts[0] != "v1" || parts[1] != "admin" || parts[2] != "users" || parts[3] == "" || parts[4] != "reset" {
		return ""
	}
	return parts[3]
}

func adminUserDetailID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "admin" || parts[2] != "users" || parts[3] == "" {
		return ""
	}
	return parts[3]
}

func sessionID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 3 || parts[0] != "v1" || parts[1] != "sessions" || parts[2] == "" {
		return ""
	}
	return parts[2]
}

func conversationID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 3 || parts[0] != "v1" || parts[1] != "conversations" || parts[2] == "" {
		return ""
	}
	return parts[2]
}

func sessionEntryParts(path string) (string, int, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 5 || parts[0] != "v1" || parts[1] != "sessions" || parts[2] == "" || parts[3] != "entries" {
		return "", 0, false
	}
	seq, err := strconv.Atoi(parts[4])
	if err != nil || seq < 0 {
		return "", 0, false
	}
	return parts[2], seq, true
}

func sessionEntryID(path string) string {
	id, _, ok := sessionEntryParts(path)
	if !ok {
		return ""
	}
	return id
}

func sessionBackgroundID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "sessions" || parts[2] == "" || parts[3] != "background" {
		return ""
	}
	return parts[2]
}

func sessionApprovalsID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "sessions" || parts[2] == "" || parts[3] != "approvals" {
		return ""
	}
	return parts[2]
}

func sessionEntryPath(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return len(parts) == 5 && parts[0] == "v1" && parts[1] == "sessions" && parts[2] != "" && parts[3] == "entries"
}

func samePrincipal(left, right string) bool {
	left, right = strings.TrimSpace(left), strings.TrimSpace(right)
	if left == "" || right == "" {
		return false
	}
	if strings.Contains(left, "@") || strings.Contains(right, "@") {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func principalKey(value string) string {
	value = strings.TrimSpace(value)
	if strings.Contains(value, "@") {
		return strings.ToLower(value)
	}
	return value
}

func anyString(value any) string {
	result, _ := value.(string)
	return result
}

func anyInt64(value any) int64 {
	switch result := value.(type) {
	case float64:
		return int64(result)
	case int64:
		return result
	case json.Number:
		parsed, _ := result.Int64()
		return parsed
	default:
		return 0
	}
}

func firstNonEmpty(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
func requestID(r *http.Request) string { return r.Header.Get("x-request-id") }
func decodeJSON(w http.ResponseWriter, raw []byte, target any) bool {
	if err := json.Unmarshal(raw, target); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "invalid JSON body"})
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

var thinkingLevels = []string{"auto", "low", "medium", "high", "xhigh", "max", "ultracode"}

func (h *HTTPServer) getRuntimeConfig(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	scopeID := r.URL.Query().Get("scopeId")
	if scopeID == "" && identity.ScopeID != "" {
		scopeID = identity.ScopeID
	}
	if scopeID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "scopeId required"})
		return
	}
	
	orgScopeID := "org:" + h.config.QM.OrgID
	
	// 构建 approvedHarnesses
	approvedHarnesses := []string{}
	for _, harness := range h.config.QM.Models.Harnesses {
		approvedHarnesses = append(approvedHarnesses, harness.ID)
	}
	
	// 构建 modelsByHarness
	modelsByHarness := map[string][]string{}
	for _, harness := range h.config.QM.Models.Harnesses {
		modelsByHarness[harness.ID] = harness.ModelIDs
	}
	
	// 构建 modelCatalog
	modelCatalog := []map[string]interface{}{}
	providerMap := map[string]config.ModelProviderConfig{}
	for _, provider := range h.config.QM.Models.Providers {
		providerMap[provider.ID] = provider
	}
	for _, harness := range h.config.QM.Models.Harnesses {
		provider, ok := providerMap[harness.Provider]
		if !ok {
			continue
		}
		for _, modelID := range harness.ModelIDs {
			var modelDef *config.ModelDefinition
			for i := range provider.Models {
				if provider.Models[i].ID == modelID {
					modelDef = &provider.Models[i]
					break
				}
			}
			if modelDef == nil {
				continue
			}
			modelCatalog = append(modelCatalog, map[string]interface{}{
				"harnessId":        harness.ID,
				"modelId":          modelID,
				"name":             modelDef.Name,
				"contextWindow":    modelDef.ContextWindow,
				"maxTokens":        modelDef.MaxTokens,
				"adaptiveThinking": modelDef.AdaptiveThinking,
			})
		}
	}
	
	// 读取 org default
	orgSelection, err := h.runtimeConfig.Selection(r.Context(), orgScopeID)
	if err != nil {
		h.logger.Error("get org runtime selection", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	
	var orgDefault map[string]interface{}
	if orgSelection != nil && h.isValidSelection(orgSelection, approvedHarnesses, modelsByHarness) {
		orgDefault = h.serializeSelection(orgSelection, false)
	} else {
		// fallback to config default
		defaultModel := h.config.QM.Models.DefaultModel()
		defaultHarness := h.config.QM.Models.DefaultHarness
		orgDefault = map[string]interface{}{
			"harnessId": defaultHarness,
			"modelId":   defaultModel,
			"revision":  int64(0),
		}
	}
	
	// 读取 scope override
	var scopeOverride map[string]interface{}
	if scopeID != orgScopeID {
		scopeSelection, err := h.runtimeConfig.Selection(r.Context(), scopeID)
		if err != nil {
			h.logger.Error("get scope runtime selection", "scope", scopeID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
			return
		}
		if scopeSelection != nil && h.isValidSelection(scopeSelection, approvedHarnesses, modelsByHarness) {
			scopeOverride = h.serializeSelection(scopeSelection, true)
		}
	}
	
	// 计算 effective 和 upgradeAvailable
	effective := orgDefault
	if scopeOverride != nil {
		effective = scopeOverride
	}
	
	upgradeAvailable := false
	if scopeOverride != nil {
		scopeOrgRev, _ := scopeOverride["orgRevision"].(int64)
		orgRev, _ := orgDefault["revision"].(int64)
		upgradeAvailable = scopeOrgRev != orgRev
	}
	
	// 构建 fastModeModelIds
	fastModeModelIds := []string{}
	for _, harness := range h.config.QM.Models.Harnesses {
		provider, ok := providerMap[harness.Provider]
		if !ok {
			continue
		}
		for _, modelID := range harness.ModelIDs {
			for _, modelDef := range provider.Models {
				if modelDef.ID == modelID && modelDef.FastMode {
					fastModeModelIds = append(fastModeModelIds, modelID)
					break
				}
			}
		}
	}

	response := map[string]interface{}{
		"scopeId":             scopeID,
		"approvedHarnesses":   approvedHarnesses,
		"modelsByHarness":     modelsByHarness,
		"modelCatalog":        modelCatalog,
		"orgDefault":          orgDefault,
		"effective":           effective,
		"upgradeAvailable":    upgradeAvailable,
		"fastModeModelIds":    fastModeModelIds,
		"interactiveFastMode": false,
	}
	if scopeOverride != nil {
		response["scopeOverride"] = scopeOverride
	}
	
	writeJSON(w, http.StatusOK, response)
}

func (h *HTTPServer) isValidSelection(sel *data.RuntimeSelection, approved []string, models map[string][]string) bool {
	harnessFound := false
	for _, h := range approved {
		if h == sel.HarnessID {
			harnessFound = true
			break
		}
	}
	if !harnessFound {
		return false
	}
	modelIDs, ok := models[sel.HarnessID]
	if !ok {
		return false
	}
	for _, m := range modelIDs {
		if m == sel.ModelID {
			return true
		}
	}
	return false
}

func (h *HTTPServer) serializeSelection(sel *data.RuntimeSelection, includeOrgRevision bool) map[string]interface{} {
	result := map[string]interface{}{
		"harnessId": sel.HarnessID,
		"modelId":   sel.ModelID,
		"revision":  sel.Revision,
	}
	if includeOrgRevision {
		result["orgRevision"] = sel.OrgRevision
	}
	if sel.EffortLevel != "" {
		result["effortLevel"] = sel.EffortLevel
	}
	if sel.FastMode != nil {
		result["fastMode"] = *sel.FastMode
	}
	return result
}

func (h *HTTPServer) putRuntimeConfig(w http.ResponseWriter, r *http.Request, body []byte, identity auth.Identity) {
	if len(body) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "request body required"})
		return
	}
	
	var req struct {
		ScopeID     string  `json:"scopeId"`
		HarnessID   string  `json:"harnessId"`
		ModelID     string  `json:"modelId"`
		EffortLevel string  `json:"effortLevel"`
		FastMode    *bool   `json:"fastMode"`
		Inherit     bool    `json:"inherit"`
		Keep        bool    `json:"keep"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "invalid JSON"})
		return
	}
	
	// capability check
	if identity.ActorID != "" && !identity.LiveActor {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "live_actor_required", "message": "runtime config changes require live actor capability"})
		return
	}
	
	scopeID := req.ScopeID
	if scopeID == "" && identity.ScopeID != "" {
		scopeID = identity.ScopeID
	}
	if scopeID == "" {
		scopeID = r.URL.Query().Get("scopeId")
	}
	
	// target check
	if scopeID == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "no target scope"})
		return
	}

	orgScopeID := "org:" + h.config.QM.OrgID
	if scopeID == orgScopeID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "org scope cannot be modified via this endpoint"})
		return
	}

	// belongsToScope check for non-capability requests
	if identity.ActorID == "" {
		belongs, err := h.belongsToScope(r.Context(), h.requestActor(r, identity), scopeID)
		if err != nil {
			h.logger.Error("belongsToScope check", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
			return
		}
		if !belongs {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "message": "scope access denied"})
			return
		}
	}
	
	// inherit → delete
	if req.Inherit {
		if err := h.runtimeConfig.DeleteSelection(r.Context(), scopeID); err != nil {
			h.logger.Error("delete runtime selection", "scope", scopeID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
			return
		}
		if err := h.audit.Record(r.Context(), data.AuditEvent{
			PrincipalID: h.requestActor(r, identity),
			Action:      "runtime-config.update",
			Resource:    "runtime-config",
			ScopeLabel:  scopeID,
			Status:      "success",
		}); err != nil {
			h.logger.Error("audit runtime-config inherit", "error", err)
		}
		// re-read and return
		h.getRuntimeConfig(w, r, identity)
		return
	}
	
	// keep → acknowledge
	if req.Keep {
		if err := h.runtimeConfig.AcknowledgeSelection(r.Context(), scopeID, orgScopeID); err != nil {
			h.logger.Error("acknowledge runtime selection", "scope", scopeID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
			return
		}
		if err := h.audit.Record(r.Context(), data.AuditEvent{
			PrincipalID: h.requestActor(r, identity),
			Action:      "runtime-config.update",
			Resource:    "runtime-config",
			ScopeLabel:  scopeID,
			Status:      "success",
		}); err != nil {
			h.logger.Error("audit runtime-config keep", "error", err)
		}
		h.getRuntimeConfig(w, r, identity)
		return
	}
	
	// explicit selection validation
	approvedHarnesses := []string{}
	modelsByHarness := map[string][]string{}
	for _, harness := range h.config.QM.Models.Harnesses {
		approvedHarnesses = append(approvedHarnesses, harness.ID)
		modelsByHarness[harness.ID] = harness.ModelIDs
	}
	
	harnessFound := false
	for _, h := range approvedHarnesses {
		if h == req.HarnessID {
			harnessFound = true
			break
		}
	}
	if !harnessFound {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "harness_not_approved", "message": "harness not approved"})
		return
	}
	
	modelIDs, ok := modelsByHarness[req.HarnessID]
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model_not_supported", "message": "model not supported by harness"})
		return
	}
	modelFound := false
	for _, m := range modelIDs {
		if m == req.ModelID {
			modelFound = true
			break
		}
	}
	if !modelFound {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model_not_supported", "message": "model not supported by harness"})
		return
	}
	
	// effort validation
	effortLevel := req.EffortLevel
	if effortLevel == "" {
		effortLevel = "auto"
	}
	effortValid := false
	for _, level := range thinkingLevels {
		if level == effortLevel {
			effortValid = true
			break
		}
	}
	if !effortValid {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "effort_not_supported", "message": "effort level not supported"})
		return
	}
	
	// fastMode validation
	fastMode := req.FastMode
	if fastMode == nil {
		f := false
		fastMode = &f
	}
	
	// fastMode silent downgrade if model doesn't support it
	if *fastMode {
		providerMap := map[string]config.ModelProviderConfig{}
		for _, provider := range h.config.QM.Models.Providers {
			providerMap[provider.ID] = provider
		}
		var harness *config.ModelHarnessConfig
		for i := range h.config.QM.Models.Harnesses {
			if h.config.QM.Models.Harnesses[i].ID == req.HarnessID {
				harness = &h.config.QM.Models.Harnesses[i]
				break
			}
		}
		if harness != nil {
			provider, ok := providerMap[harness.Provider]
			if ok {
				supported := false
				for _, modelDef := range provider.Models {
					if modelDef.ID == req.ModelID && modelDef.FastMode {
						supported = true
						break
					}
				}
				if !supported {
					f := false
					fastMode = &f
				}
			}
		}
	}
	
	selection := data.RuntimeSelection{
		HarnessID:   req.HarnessID,
		ModelID:     req.ModelID,
		EffortLevel: effortLevel,
		FastMode:    fastMode,
	}
	if effortLevel == "auto" {
		selection.EffortLevel = ""
	}
	
	if err := h.runtimeConfig.SetSelection(r.Context(), scopeID, orgScopeID, selection); err != nil {
		h.logger.Error("set runtime selection", "scope", scopeID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	
	if err := h.audit.Record(r.Context(), data.AuditEvent{
		PrincipalID: h.requestActor(r, identity),
		Action:      "runtime-config.update",
		Resource:    "runtime-config",
		ScopeLabel:  scopeID,
		Status:      "success",
	}); err != nil {
		h.logger.Error("audit runtime-config set", "error", err)
	}
	
	h.getRuntimeConfig(w, r, identity)
}

func (h *HTTPServer) belongsToScope(ctx context.Context, principalID, scopeID string) (bool, error) {
	kind, ref := splitScopeID(scopeID)
	switch kind {
	case "personal":
		return sameSoulPerson(ref, principalID), nil
	case "org":
		return true, nil
	case "group":
		if strings.HasPrefix(scopeID, "group:web-project-") {
			return h.projectRepo.HasScopeMembership(ctx, scopeID, principalID)
		}
		return h.directory.IsScopeMember(ctx, "group", ref, principalID)
	case "channel":
		channel, err := h.directory.Channel(ctx, ref)
		if err != nil {
			return false, err
		}
		if channel == nil {
			return false, nil
		}
		if !channel.IsPrivate {
			return true, nil
		}
		return h.directory.IsScopeMember(ctx, "channel", ref, principalID)
	default:
		return false, nil
	}
}

func (h *HTTPServer) getSurfaceConfig(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	scopeID := r.URL.Query().Get("scopeId")
	if scopeID == "" && identity.ScopeID != "" {
		scopeID = identity.ScopeID
	}
	if scopeID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "scopeId required"})
		return
	}
	
	webuiModels, err := h.runtimeConfig.WebuiModels(r.Context(), scopeID)
	if err != nil {
		h.logger.Error("read webui models", "scope", scopeID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	
	baseModel, err := h.runtimeConfig.LegacyModel(r.Context(), scopeID)
	if err != nil {
		h.logger.Error("read base model", "scope", scopeID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	if baseModel == "" && len(h.config.QM.Models.Harnesses) > 0 {
		baseModel = h.config.QM.Models.Harnesses[0].DefaultModel
	}
	
	externalSlack, err := h.runtimeConfig.ExternalSlackParticipants(r.Context(), scopeID)
	if err != nil {
		h.logger.Error("read external slack participants", "scope", scopeID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	
	brandingRaw, err := h.runtimeConfig.Branding(r.Context(), scopeID)
	if err != nil {
		h.logger.Error("read branding", "scope", scopeID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	
	harnessID := ""
	if len(h.config.QM.Models.Harnesses) > 0 {
		harnessID = h.config.QM.Models.Harnesses[0].ID
	}
	
	resp := map[string]interface{}{
		"webuiModels":               webuiModels,
		"baseModel":                 baseModel,
		"harnessId":                 harnessID,
		"externalSlackParticipants": externalSlack,
	}
	
	if brandingRaw != nil {
		branding := make(map[string]string)
		hasAny := false
		
		if brandingRaw.Accent != "" {
			matched := false
			for _, pattern := range []string{
				`^#[0-9a-fA-F]{3}$`, `^#[0-9a-fA-F]{4}$`,
				`^#[0-9a-fA-F]{6}$`, `^#[0-9a-fA-F]{8}$`,
			} {
				if matched, _ = regexp.MatchString(pattern, brandingRaw.Accent); matched {
					break
				}
			}
			if matched {
				branding["accent"] = brandingRaw.Accent
				hasAny = true
			}
		}
		
		if brandingRaw.Mark != "" {
			mark := regexp.MustCompile(`[\x00-\x1F\x7F-\x9F  "\\<>{}]`).ReplaceAllString(brandingRaw.Mark, "")
			if len(mark) > 2 {
				mark = mark[:2]
			}
			if mark != "" {
				branding["mark"] = mark
				hasAny = true
			}
		}
		
		if brandingRaw.SelfLabel != "" {
			label := regexp.MustCompile(`[\x00-\x1F\x7F-\x9F  ]`).ReplaceAllString(brandingRaw.SelfLabel, "")
			if len(label) > 40 {
				label = label[:40]
			}
			if label != "" {
				branding["selfLabel"] = label
				hasAny = true
			}
		}
		
		if hasAny {
			resp["branding"] = branding
		}
	}
	
	writeJSON(w, http.StatusOK, resp)
}

func (h *HTTPServer) sessionStateEvents(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	
	flusher, ok := w.(http.Flusher)
	if !ok {
		h.logger.Error("responsewriter does not support flushing")
		return
	}
	
	if _, err := w.Write([]byte(": open\n\n")); err != nil {
		return
	}
	flusher.Flush()
	
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

const SECRET_DROP_AUD = "secret-drop"

func (h *HTTPServer) mintSecretDrop(w http.ResponseWriter, r *http.Request, body []byte, identity auth.Identity) {
	if h.auth.CapabilitySecret == "" {
		http.Error(w, `{"code":"not_found","message":"Secret drops not configured"}`, http.StatusNotFound)
		return
	}

	if identity.ActorID == "" {
		http.Error(w, `{"code":"unauthorized","message":"secret-drop mint requires an agent capability token"}`, http.StatusUnauthorized)
		return
	}

	if identity.Triggered {
		http.Error(w, `{"code":"forbidden","message":"capability already triggered"}`, http.StatusForbidden)
		return
	}

	var req struct {
		Service   string                 `json:"service"`
		EnvKey    string                 `json:"envKey"`
		Host      string                 `json:"host"`
		Fields    []data.SecretDropField `json:"fields"`
		Purpose   string                 `json:"purpose"`
		GrantMode string                 `json:"grantMode"`
	}

	if err := json.Unmarshal(body, &req); err != nil || req.Service == "" || req.Purpose == "" {
		http.Error(w, `{"code":"bad_request","message":"service and purpose are required"}`, http.StatusBadRequest)
		return
	}

	if req.GrantMode != "once" && req.GrantMode != "standing" {
		http.Error(w, `{"code":"bad_request","message":"grantMode must be once or standing"}`, http.StatusBadRequest)
		return
	}

	if len(req.Fields) < 1 || len(req.Fields) > 8 {
		http.Error(w, `{"code":"bad_request","message":"fields must contain 1-8 items"}`, http.StatusBadRequest)
		return
	}

	dropID := uuid.New().String() + strings.ReplaceAll(uuid.New().String(), "-", "")

	drop := data.SecretDrop{
		ID:              dropID,
		OwnerID:         identity.ActorID,
		OrgID:           extractOrgID(identity.ScopeID),
		Service:         req.Service,
		EnvKey:          req.EnvKey,
		Host:            req.Host,
		Fields:          req.Fields,
		Purpose:         req.Purpose,
		RequestedBy:     identity.ActorID,
		AudienceScopeID: identity.ScopeID,
		ScopeVersion:    identity.ScopeVersion,
		GrantMode:       req.GrantMode,
		ThreadRef:       "",
		RequiresToken:   true,
		CreatedAt:       time.Now().Unix(),
	}

	if err := h.secretDrops.Mint(r.Context(), drop); err != nil {
		h.logger.Error("failed to mint secret drop", "error", err)
		http.Error(w, `{"code":"internal_error","message":"failed to create drop"}`, http.StatusInternalServerError)
		return
	}

	token, err := auth.MintCapability(auth.Claims{
		ActorID:   identity.ActorID,
		ScopeID:   identity.ScopeID,
		Audience:  SECRET_DROP_AUD,
		Drop:      dropID,
		ExpiresAt: time.Now().Add(data.SecretDropTTL).Unix(),
	}, h.auth.CapabilitySecret)
	if err != nil {
		h.logger.Error("failed to mint drop token", "error", err)
		http.Error(w, `{"code":"internal_error","message":"failed to create token"}`, http.StatusInternalServerError)
		return
	}

	h.audit.Record(r.Context(), data.AuditEvent{
		PrincipalID: identity.ActorID,
		Action:      "keychain.drop.mint",
		Resource:    req.Service + ":" + dropID,
		ScopeLabel:  identity.ScopeID,
	})

	formPath := "/drop/" + dropID + "/form?t=" + token
	publicURL := "http://localhost:18129"

	resp := map[string]string{
		"dropId":   dropID,
		"formPath": formPath,
		"url":      publicURL + formPath,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *HTTPServer) secretDropForm(w http.ResponseWriter, r *http.Request) {
	dropID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/keychain/drops/"), "/form")

	drop, err := h.secretDrops.Peek(r.Context(), dropID)
	if err != nil {
		h.logger.Error("failed to peek drop", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if drop == nil {
		http.Error(w, "Drop not found or expired", http.StatusNotFound)
		return
	}

	var fieldInputs string
	for _, field := range drop.Fields {
		inputType := "text"
		if field.IsSecret {
			inputType = "password"
		}
		fieldInputs += fmt.Sprintf(`
			<div style="margin-bottom: 1rem;">
				<label style="display: block; margin-bottom: 0.25rem; font-weight: 500;">%s</label>
				<input type="%s" name="%s" required style="width: 100%%; padding: 0.5rem; border: 1px solid #d1d5db; border-radius: 0.375rem;">
			</div>
		`, field.Label, inputType, field.Key)
	}

	html := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
	<meta charset="utf-8">
	<title>Secret Drop - %s</title>
	<meta name="viewport" content="width=device-width, initial-scale=1">
	<style>
		body { font-family: system-ui, -apple-system, sans-serif; max-width: 32rem; margin: 2rem auto; padding: 1rem; }
		h1 { font-size: 1.5rem; margin-bottom: 0.5rem; }
		.meta { color: #6b7280; margin-bottom: 2rem; font-size: 0.875rem; }
		button { background: #2563eb; color: white; padding: 0.5rem 1rem; border: none; border-radius: 0.375rem; cursor: pointer; }
		button:hover { background: #1d4ed8; }
	</style>
</head>
<body>
	<h1>%s</h1>
	<div class="meta">Purpose: %s<br>Requested by: %s</div>
	<form method="POST" action="/v1/keychain/drops/%s">
		%s
		<button type="submit">Submit</button>
	</form>
</body>
</html>`, drop.Service, drop.Service, drop.Purpose, drop.RequestedBy, dropID, fieldInputs)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}

func (h *HTTPServer) redeemSecretDrop(w http.ResponseWriter, r *http.Request, body []byte) {
	dropID := strings.TrimPrefix(r.URL.Path, "/v1/keychain/drops/")

	drop, err := h.secretDrops.Redeem(r.Context(), dropID)
	if err != nil {
		h.logger.Error("failed to redeem drop", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if drop == nil {
		http.Error(w, "Drop not found or expired", http.StatusNotFound)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	values := make(map[string]string)
	for _, field := range drop.Fields {
		val := r.FormValue(field.Key)
		if val == "" {
			http.Error(w, fmt.Sprintf("Missing required field: %s", field.Label), http.StatusBadRequest)
			return
		}
		values[field.Key] = val
	}

	var credentialKey strings.Builder
	if drop.EnvKey != "" {
		credentialKey.WriteString(drop.EnvKey)
	} else {
		credentialKey.WriteString(drop.Service)
		if drop.Host != "" {
			credentialKey.WriteString(":")
			credentialKey.WriteString(drop.Host)
		}
	}

	payload, _ := json.Marshal(values)

	credential := data.Credential{
		ID:          generateCredentialID(),
		OwnerID:     drop.OwnerID,
		OrgID:       drop.OrgID,
		Service:     drop.Service,
		Key:         credentialKey.String(),
		Values:      payload,
		GrantedBy:   drop.OwnerID,
		GrantedAt:   time.Now().Unix(),
		Destination: drop.Destination,
	}

	if err := h.keychain.CreateCredential(r.Context(), credential); err != nil {
		h.logger.Error("failed to store credential", "error", err)
		http.Error(w, "Failed to store credential", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(`<!DOCTYPE html>
<html>
<head>
	<meta charset="utf-8">
	<title>Success</title>
	<style>body { font-family: system-ui; max-width: 32rem; margin: 2rem auto; padding: 1rem; }</style>
</head>
<body>
	<h1>Credential stored</h1>
	<p>The credential has been securely stored and is now available to the agent.</p>
</body>
</html>`))
}

func extractOrgID(scopeID string) string {
	if strings.HasPrefix(scopeID, "org:") {
		return scopeID
	}
	if strings.HasPrefix(scopeID, "personal:") {
		return "org:default"
	}
	parts := strings.SplitN(scopeID, ":", 2)
	if len(parts) != 2 {
		return "org:default"
	}
	if parts[0] == "group" || parts[0] == "channel" {
		orgParts := strings.SplitN(parts[1], "_", 2)
		if len(orgParts) > 0 {
			return "org:" + orgParts[0]
		}
	}
	return "org:default"
}

func generateCredentialID() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
