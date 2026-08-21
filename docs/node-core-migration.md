# Node Core Migration

`qm-backend` is deployed as the public `CORE_API_URL`. Its route mode controls
one domain at a time:

| Mode | Behavior |
| --- | --- |
| `proxy` | Forward every request to the existing Node core. |
| `shadow_read` | Compare capability-authenticated `GET /v1/projects` against Go, then return the Node response. |
| `go` | Serve projects, directory synchronization, principal activation, and admin grants from Go; proxy all remaining routes. |

Pass the QM YAML file to `qm-backend --config`. Database, organization,
signing, capability, connector-encryption, model-provider, Slack, OAuth, and
internal gRPC settings are read from that file; local files containing real
credentials must not be committed.
Keep `NODE_CORE_URL` private. Migrate with this order:

1. Run `qm-backend` with `QM_ROUTE_MODE=proxy` and apply its migrations.
2. Set `QM_ROUTE_MODE=shadow_read`; observe response mismatch logs for projects.
3. Set `QM_ROUTE_MODE=go`; keep Node core running for all routes not owned by Go.
4. Roll back by changing only `QM_ROUTE_MODE=proxy`; do not roll back database migrations.

## Service probes and Node gRPC cutovers

The Kratos process applies its PostgreSQL migrations before it starts either
listener. Use `GET /healthz` as a liveness probe: it only proves that the
process can serve HTTP. Use `GET /readyz` as the readiness probe: it performs
a bounded ping of the shared PostgreSQL pool and returns `503 {"ok":false}`
when that dependency is unavailable. These two endpoints are served directly
by Go in every route mode, so they do not depend on the Node proxy being
healthy.

The Go listener addresses and internal token are configured in the Kratos YAML:

```yaml
server:
  http_addr: ":18083"
  grpc_addr: ":9090" # bind privately in production
database:
  url: "postgres://…"
auth:
  grpc_internal_token: "the-same-long-random-value"
```

After the Go service is ready, enable Node-owned runtime adapters one at a
time. Each adapter is optional and falls back to the existing Node/PostgreSQL
implementation when its URL is absent:

| Node deployment variable | Kratos service | Moves to Go | Still runs in Node |
| --- | --- | --- | --- |
| `QM_BACKEND_RUNNER_GRPC_URL` | `qm.runner.v1.RunnerService` | durable run queue, leases, events, signals | admission, model execution, streams, delivery |
| `QM_BACKEND_CRON_GRPC_URL` | `qm.cron.v1.CronService` | durable cron records, claims, fire log | Croner evaluation, scheduling loop, delivery |
| `QM_BACKEND_DEPLOYMENT_LAYER_GRPC_URL` | `qm.deployment.v1.DeploymentLayerService` | singleton deployment-layer durable map and CAS | layer validation, skill materialization, runtime refresh/rollback |

Set `QM_BACKEND_GRPC_INTERNAL_TOKEN` in Node to the same value as
`auth.grpc_internal_token`. The current adapters use insecure gRPC transport,
so the gRPC listener must stay on a private network (or be protected by a
mutually authenticated tunnel/proxy); do not expose it to the public internet.
The Node and Kratos processes must use the same PostgreSQL database before
enabling any row. In particular, the deployment-layer cutover depends on the
`000023_deployment_layer.sql` migration and on shared `durable_map_versions`
for cache invalidation.

Recommended rollout for each row:

1. Deploy Kratos with the token configured; wait for `/readyz` to return 200.
2. Configure exactly one Node gRPC URL and restart/roll that Node workload.
3. Exercise its write path and verify the matching PostgreSQL records and
   Kratos logs; keep the other URLs unset.
4. Enable the next adapter only after the first is stable.

To roll back a single adapter, remove only its corresponding Node URL and
restart Node. It immediately returns to its existing PostgreSQL durable-map
path; retain the migrations and shared data. Route-mode rollback
(`QM_ROUTE_MODE=proxy`) is independent of these gRPC adapter toggles.

Source authentication follows the Node replay contract: signed `GET` requests
are repeatable polling reads, while non-GET requests consume a durable replay
key. A repeated signed mutation is therefore rejected without making normal
source-read polling fail.

## Go worker and Agent engine convergence

The Go process has one PostgreSQL `runtime_tasks` queue and multiple categorized
goroutine pools. Each task has an idempotency key, payload version, priority,
attempt limit, lease owner/token/expiry, heartbeat, cancellation state, event
sequence, result, and terminal error. Claims use `FOR UPDATE SKIP LOCKED`, so
several `qm-backend` replicas can drain the same queue without a separate Go
worker service. Pool concurrency, lease, heartbeat, polling, and retry backoff
come from `workers.pools` in the QM YAML.

The Go Agent engine preserves the Node Harness adapter contract instead of
normalizing all adapters to a generic model call. Its fixed profiles are:

| Harness | Control transport | Tool transport | Transcript | Capabilities |
| --- | --- | --- | --- | --- |
| `pi` | in-process | in-process | `pi` | abort, steer, images, thinking level, fast mode, provider sessions |
| `opencode` | HTTP | plugin | `opencode` | abort, steer, images, provider sessions |
| `codex` | JSON-RPC | dynamic | `responses-api` | abort, steer, images, provider sessions |
| `claude` | SDK | in-process MCP | `claude-agent-sdk` | abort, steer, images, thinking level, fast mode |
| `mock` | mock | mock | `qm` | deterministic test scaffold only |

Provider/model bindings and each adapter's runtime options are declared under
`qm.models.harnesses`. The durable resolver reads the existing
`approved_harness_configs` and `base_model_configs` maps, preserves org-to-scope
inheritance, rejects unapproved requested combinations, and falls back to YAML
defaults when stored settings are stale. `agent_harness_state` records the last
adapter used by each session, so an adapter switch triggers both reset hooks
even across replicas. `agent.turn` is the versioned runtime-task contract for
the turn pool and persists delta, text-block, and progress events. Runtime tasks
carry a durable `serial_key`; the partial unique lease index permits different
sessions to execute in parallel while preventing two replicas from running
turns for the same session at once.

Each of the four production adapters is registered in `qm-backend` only when
YAML declares that Harness. All four are selected through the same `Engine`,
but retain their Node wire contracts instead of being flattened:

- Pi performs the in-process OpenAI-compatible/Anthropic multi-step tool loop.
- OpenCode launches its HTTP sidecar and a per-process HMAC loopback bridge.
  The bridge plugin is embedded in the Go binary and materialized into the
  isolated jail, so production startup does not read the Node source tree.
- Codex launches app-server JSON-RPC with an ephemeral read-only control jail
  and exposes QM tools as dynamic functions.
- Claude launches the CLI stream-JSON protocol with an ephemeral control jail
  and a token-protected loopback MCP server.

Pi uses the configured OpenAI-compatible or Anthropic endpoint, runs the
multi-step tool loop, consumes SSE deltas, applies cancellation and the turn
wall-clock cap, consumes durable `abort` and `steer` run signals, replays
persisted entries, emits Node-compatible user,
thinking, text, tool-call, tool-result, and assistant entries, and records
image-redacted request snapshots and usage in `session_llm_requests`. The turn
pool concurrency comes from `workers.pools.turn`; these are goroutines in the
one `qm-backend` process, not another service. Session writes use a PostgreSQL
advisory transaction lock, so sequence and parent links remain valid across
replicas. Pi also writes Node-compatible durable `session_tape` records. In
`serve` mode, cold starts fold legacy imports/patches, compaction events,
interrupt healing, and message rows, lint the result, reject tape written by a
different Harness, and only then use it instead of reconstructed entry history.

Browser `POST /v1/turns` now persists `runtime_owner=go`, enqueues the matching
`agent.turn` task, and lets only the Go turn pool claim it; the legacy Node
runner filters those rows out. The Go adapter emits the existing session-entry,
tape, model-call, request-capture, approval, progress, and run-completion
artifacts. Codex child threads and Claude Agent tasks also update the same
`tasks` and `task_events` tables used by Node, including terminal failure of
children left open when a parent turn exits. Inbound transfer attachments are
materialized inside the per-scope Go workspace, with safe paths and image-byte
redaction in durable observability records.

The `execute` primitive enqueues a separate `sandbox.exec` runtime task. A
categorized sandbox goroutine pool in the same `qm-backend` process claims it
and invokes the declared local Docker backend; the model command is passed to
the container shell and is never evaluated by the host shell. Local execution
therefore requires the configured sandbox image and a reachable Docker daemon.

This remains a staged control-plane cutover: Slack ingress/delivery and several
artifact/runtime mutations still proxy to Node. That compatibility boundary no
longer makes Node the browser turn executor, and it does not introduce a second
Go service. The remaining gaps are tracked by tool/route capability, not by
creating a fifth generic Harness.

In `route_mode: go`, project reads and all project mutations are Go-owned:
`GET`/`POST /v1/projects`, `PATCH /v1/projects/:id`,
`POST /v1/projects/:id/members`, and
`DELETE /v1/projects/:id/members/:memberId`. The Go path keeps Node's exact
capability-actor substitution and inaccessible-project 404 behavior, requires
the owner and newly added member to be active internal directory members, and
updates the shared project durable-map version. Responses include the same
`scopeId`, active member ids, and synchronized directory display names as
Node's `ProjectView`. Project creation and actual renames also initialize or
rename the corresponding Knowledge Core project scope; knowledge failures are
audit signals rather than a reason to reject an otherwise valid project
mutation. A name-only change intentionally does not alter `updatedAt`, so the
project roster `scopeVersion` captured by existing capabilities remains valid;
member additions and removals remain the version-changing operations.

The Go backend now owns compatible `runs`, `run_activity`, and `run_signals`
storage, including durable enqueue deduplication, per-session leasing,
heartbeats, idempotent event append/completion, and steering/abort signals.
`qm.runner.v1.RunnerService` exposes that contract over the internal gRPC
listener. When Node uses a PostgreSQL run store and
`QM_BACKEND_RUNNER_GRPC_URL` is set, its final `runs.enqueue` call is also
performed through the Go `EnqueueRun` RPC, then read back from the shared
database. Node deliberately retains turn admission, identity and roster checks,
model selection, wake routing, session-state publication, streaming, and
delivery. Set `QM_BACKEND_GRPC_INTERNAL_TOKEN` when configured on Go. Leave
the runner URL unset to keep the existing Node/PostgreSQL path. The Node agent
worker remains the execution adapter, but in Go-runner mode it already moves
claim, heartbeat, lease release, failure, completion, run-signal, and
run-activity calls to Go. Signal notification is poll-based during this
transition, so steering can take up to the configured Node signal polling
interval to reach an in-flight run. The Node reaper also delegates expired-run
requeue/parking to Go while retaining Node session-lease release and error-log
side effects. The two processes must point at the same `runs` database before
enabling this cutover. The synchronous turn path also claims its specific
queued run through `ClaimRunByID`; Node then executes and settles that lease so
its existing stream and session-state notifications remain intact.
Node's PostgreSQL run store also reads individual durable run snapshots through
`GetRun` in this mode. The RPC deliberately contains only persisted queue
fields; live partial text, reply state, tasks, and Slack delivery presentation
remain Node runtime concerns.
Its `activeForThread` lookup likewise calls `GetActiveRunForSession`, so
steering, ambient wake routing, and the session-state sweep agree with the Go
queue's pending/running view.
The sidebar's bulk work-state lookup uses `ListActiveSessionIDs` from that same
view, avoiding a second Node-owned interpretation of active queue rows.
In Go-runner mode the Node worker uses the primary run-store facade, which
delegates claim, heartbeat, lease release, completion, failure, and reaping to
the Runner RPC. After a terminal durable result is observed, that facade still
emits Node's local terminal listeners; session idle transitions and orphaned
signal replay therefore remain reliable during the split.

`qm/proto/qm/runner/v1/runner.proto` is the Node runtime copy of the canonical
`qm-backend/api/qm/runner/v1/runner.proto`; keep the two files identical when
the Runner contract changes.

Knowlega is now an internal Agent in `qm-backend`, not a public Knowledge gRPC
contract. QM writes files, memory revisions, and valuable completed
conversations first; the Agent then content-addresses them into raw sources,
queues ingestion, and compiles versioned Markdown pages. QM's outer Pi loop is
the sole user-facing reasoner and receives one deterministic `knowledge` tool:
search/discover/list navigate, read/follow_links/graph produce evidence,
submit validates the current-turn ledger, and writeback is explicit.

Cron state is owned by internal `qm.cron.v1.CronService` when Node is started
with `QM_BACKEND_CRON_GRPC_URL`. The service persists the complete cron JSON
payload alongside indexed scheduling fields and provides atomic CRUD, merge,
claim, rollback, fire-log, and mark-fired operations. Node still evaluates
schedules and executes deliveries, but its existing `CronStore` then uses the
Go service instead of the `crons` artifact map. Set
`QM_BACKEND_GRPC_INTERNAL_TOKEN` to the Go service's internal token as well.
On the first Go-backed access, if Go contains no cron records, Node imports its
existing artifact-map records with `PutIfAbsent`; after that, Go is the sole
cron state store. Without `QM_BACKEND_CRON_GRPC_URL`, Node keeps its existing
store, allowing a per-deployment gradual cutover.

The Go HTTP compatibility layer also owns `/v1/environments`,
`/v1/environments/attach` in `route_mode: go`. It uses the same PostgreSQL
`environments` and `environment_attachments` schema as Node, requires an agent
capability, preserves owner-only attachment mediation, and records the same
environment audit actions. In `proxy` mode these routes remain on Node.

`/v1/surface-cache/policy` is also Go-owned in `route_mode: go`. It requires
source authentication, stores the policy in Node-compatible `channel_policy`
and `channel_policy_history` tables, and preserves bot/ambient policy fields
that the Node surface runtime continues to consume. Surface event ingestion and
ambient execution remain on Node for now.

`/v1/contexts/policy` is Go-owned as well. Its source-auth handler verifies the
caller against the synchronized channel/group directory membership, applies the
same bot-ledger validation and ambient tri-state semantics, and enforces
`baseUpdatedAt` optimistic concurrency before writing the shared policy tables.

The Go compatibility layer now also serves the read-only admin endpoints
`/v1/admin/directory`, `/v1/admin/slack-mirror`, and
`/v1/admin/slack-mirror/messages`. They retain source/capability plus org-admin
authorization, read the shared directory and surface-cache tables, and enrich
message author names from the synchronized directory. Slack event ingestion,
ambient judgment, and delivery remain on Node.

`/v1/admin/audit` is Go-owned in `route_mode: go`; it retains org-admin
authorization, supports the existing scope filter, and reads the shared
`audit_log` tail with the same response shape.

`/v1/admin/ambient-judgments` and `/v1/admin/ack-emoji-picks` are Go-owned
read endpoints in `route_mode: go`. They keep source/capability plus org-admin
authorization, record the corresponding shared audit events, and query the
Node-compatible PostgreSQL observability tables. List responses retain cursor
pagination, filters, aggregate counts, and omit prompt/candidate details;
single-record reads return those details and the synchronized workspace URL.
Slack ambient evaluation and acknowledgement selection continue to write these
tables from Node during the gradual migration.

`/v1/admin/egress` is also Go-owned in `route_mode: go`. It retains the
required `scope` parameter and org-admin authorization, then combines the
shared broker `credential_usage` and firewall `egress_events` records with the
existing sort order, totals, denied count, host count, and source breakdown.
The brokers and egress enforcement processes continue to write those event
tables from Node.

`/v1/admin/errors` is Go-owned in `route_mode: go`. It preserves the required
`scope` parameter, optional session filter, `count` response mode, org-admin
authorization, and audit event while reading the Node-compatible
`error_events` table. Node workers remain responsible for recording errors.

`/v1/admin/whoami` is Go-owned in `route_mode: go`. It reports the current
capability or declared admin actor's grant-derived status and permissions,
returns a non-admin status when no actor is supplied, and preserves the
`admin.whoami` audit event for identified actors.

`/v1/admin/runs` is Go-owned in `route_mode: go`. It preserves the required
scope, org-admin authorization, active-run priority, and the Node response
shape by joining the shared runner `runs` records to the Node-maintained
`sessions` index. Node continues to create and maintain session records while
the Go runner and Node execution adapter share the run queue.

`/v1/admin/retention` is Go-owned in `route_mode: go`. It remains an org-wide,
org-admin read, computes the existing DAU/WAU/MAU, cohort, and per-user report
from the shared Node session, participant, and entry tables, and records the
same retention audit event. Node remains the session writer during this stage.

All Go-owned admin endpoints preserve the Node capability boundary: apart from
`/v1/admin/whoami`, capability calls require `liveActor: true`; grant and
impersonation changes remain portal-only; and migrated private-content reads
must originate from a personal capability scope. Source-authenticated portal
calls retain their existing org-admin checks.

For source-authenticated internal callers without `principalId`, the Go HTTP
layer now serves interval/one-shot `POST /v1/crons`, `GET /v1/crons`, `GET /v1/crons/:id`, and
`GET /v1/crons/:id/runs`, `PATCH /v1/crons/:id`, `DELETE /v1/crons/:id`, and
`POST /v1/crons/:id/disable` directly from the Go cron store. It keeps
`fireLog` out of normal cron responses as Node does. Source PATCH also owns
interval and one-shot schedule normalization: it applies the same one-minute
minimum and sub-day guardrail, materializes `firstFireAt`, and persists the
matching `nextFireAt` in both the cron document and indexed PostgreSQL column.
Source creation applies the same normalization, owner-consent guard, title
normalization, and Node-compatible 16-hex content key before `PutIfAbsent`, so
the same logical create from a rolling Node/Go deployment resolves to one
shared cron row. Calendar schedules containing `schedule.cron` remain
deliberately proxied to Node before persistence, where Croner is still the
compatibility authority for five-field syntax, IANA time zones, and DST. Portal
`viewer` and `principalId` cron requests still
proxy to Node while their portal identity rules are migrated. The
capability-only read projection is now Go-owned too: `GET /v1/crons`,
`GET /v1/crons/:id`, and `GET /v1/crons/:id/runs` resolve managed versus
visible crons from the signed actor/scope, synchronized directory aliases,
channel membership, project membership, member snapshots, and principal
destinations. Capability mutations that need live runtime behavior remain on
Node. The narrow
capability `POST /v1/crons/:id/disable` path is Go-owned: it applies the
existing capability-admin projection, pauses the shared cron row, and, when a
different person pauses a `scopeShared` cron, appends Node's idempotent edit
notice to the delivery queue. The same Go path owns capability
`DELETE /v1/crons/:id`, including Node-compatible `cron_delete` auditing and a
queued shared-edit notice. Node's worker still sends those notifications. Go
also owns capability `POST /v1/crons/:id/destination`: it selects exclusively
from the signed capability's destination candidates, records the same
`cron_retarget` audit event, and queues the shared-owner edit notice while
leaving provider delivery to Node. Run-now, mode changes, and general
capability PATCH remain on Node. Go additionally owns the owner-mode,
capability `PATCH /v1/crons/:id`
subtree for durable title/task, enabled/archive, link-preview, and
interval/one-shot schedule edits, including `cron_update` auditing. Team-mode
crons, `text` delivery changes, `runAs` member-snapshot changes, and calendar
cron expressions remain on Node so its signed member projection and Croner
timezone/DST semantics continue unchanged.

`POST /v1/triggers/:id/consent` is Go-owned for agent capabilities. It updates
only the shared cron record's `recipientConsent` field, preserves arbitrary
consent metadata, and requires the signed capability actor to be the designated
delivery recipient. Source callers are rejected, as in Node. The scheduler and
all trigger delivery execution remain Node-owned and observe the same durable
decision before attempting delivery.

`GET /v1/skills` is Go-owned for source-authenticated catalog reads. It reads
the shared Node-compatible `skills` durable map and preserves the visible-skill
resolution order: personal home first, then accessible channel/group homes in
durable skill-id order, then the organization home. Same-name rows are exposed
as Node's shadow chain when `includeShadowed=1`; archived rows remain visible
only to a manager of their home. `DELETE /v1/skills/:id` is Go-owned for the
narrow archive transition: it preserves Node's manager check, shared-scope
human-attendance check (`liveActor` or legacy `liveAuthor`), idempotent
`archived` DurableMap transition, and `skill_archive` audit event. Skill
create/edit/review/publish/restore, skill-pack synchronization, signature
validation, and runtime materialization remain Node-owned. This source route carries only `principalId`, so it does not
invent a team assertion; team-scoped capability workflows remain on Node.
`GET /v1/skills/:id` is Go-owned too: body-bearing detail requires either the
agent capability actor or a verified portal identity in addition to the source
signature, and returns 404 when the skill is neither visible nor manageable.
Capability scope revocation follows Node's project-group-only check; ordinary
channel/group capabilities are already issued after Node's live membership
decision and are not spuriously rejected by Go.

`GET /v1/runs?threadRef=…` is Go-owned in `route_mode: go`. It requires source
authentication and reads the current pending/running run directly from the
shared Go runner queue, returning the same nullable `runId` response shape.
`POST /v1/runs/:id/delivery-state` is also Go-owned with source authentication:
it atomically stores the run's `editRef` checkpoint and updates the pending
`run:<id>` delivery destination, preserving Node's Slack retry behavior. Signal
delivery and detailed run views remain on Node while terminal-race and streaming
semantics are migrated. The side-effect-free `POST /v1/deliveries/ack-by-key`
is Go-owned and preserves Node's delivery-or-tombstone idempotency behavior.

`POST /v1/egress-audit` is Go-owned with source authentication. It retains the
Node endpoint's 500-record batch limit, field validation/truncation, accepted
and rejected counts, and persists accepted proxy observations to `egress_events`.

`POST /v1/auth/broker/claim` is Go-owned with source authentication. It claims
the first available nonce in an ordered batch through the shared durable replay
table, with the same 64-ID, 200-character, and 24-hour expiry bounds as Node.
The replay schema now consistently uses PostgreSQL timestamps, so signed source
requests and broker claims interoperate between the Node and Go processes.

`POST /v1/turns/:runId/metrics` is Go-owned with source authentication. It
updates `deliverMs` and `slackInflightMs` in the shared `turn_metrics` row while
retaining Node's fire-and-forget success behavior when a metric row is absent.

`GET /v1/admin/deliveries/shadow` is Go-owned. It keeps Node's scoped-admin
check, audit entry, provenance scope filtering, and background/cron origin
shape. Cron metadata is returned only to the cron's scope or to org-wide reads.

`GET /v1/deliveries` is Go-owned with source authentication. It reads pending
deliveries or atomically claims them with the Node-compatible `claimMs` lease,
including the same destination, attachments, provenance, and delivery state
response shape.

`POST /v1/deliveries/:id/ack` is Go-owned with source authentication. It
preserves ordinary delivery latency/Slack timing updates and the principal
delivery path that records the recipient thread, creates or finds its DM
session, and adds the recipient participant before acknowledgment.

`GET /v1/admin/metrics` is Go-owned. It derives the Node-compatible latency,
queue, throughput, phase-distribution, cache, and trace-anatomy report from
the shared PostgreSQL metric and runner tables, while retaining scoped-admin
authorization and audit records.

`GET /v1/admin/sessions` is Go-owned. It provides scoped, cursor-paginated
conversation and background session summaries, category/origin/cron filters,
cron grouping, delivery counts, and the matching aggregate counters directly
from the shared session and delivery tables.

`GET /v1/admin/sessions/:id/llm` is also Go-owned. It provisions the shared
LLM request audit table, enforces the session's requested admin scope, and
returns metadata-only request records by default; explicit individual
turn/orphan filters return the matching stored request as Node does.

`GET /v1/admin/sessions/:id` is Go-owned. It returns the scoped transcript
with Node-compatible limits and participant labels, plus recipient and outbound
delivery timelines, background/cron origins, and same-scope provenance sidecars.

`GET /v1/admin/users` is Go-owned. It derives participant-window attribution,
turn/session counts, last-seen times, admin status, and grants from the shared
session and admin-grant tables. `GET /v1/admin/users/:principalId` is also
Go-owned: it composes that person's participant-window activity, personal
files, crons, deployments, memory onboarding status, SOUL revision, and safe
read-only durable-map configuration. Connector secrets remain excluded; only
Node's public connector metadata is returned.
`PUT /v1/admin/users/:principalId/onboarding` is Go-owned too: it preserves the
Node v2 onboarding marker format in that user's append-only personal memory and
records the same admin audit event. `POST /v1/admin/users/:principalId/reset`
also now performs Node-compatible personal-memory reset and transactional
cleanup of that user's personal sessions, entries, LLM records, leases, and
tape.

The viewer-scoped transcript reads `GET /v1/sessions/:id` and
`GET /v1/sessions/:id/entries/:seq` are Go-owned with source authentication.
They retain Node's required `viewer` parameter, participant sequence/time
visibility window, soul-entry exclusion, transcript paging validation, and
viewer-specific title/archive/pin/color state.

`GET /v1/conversations/:id` is also Go-owned for an agent capability. It uses
the capability actor as the viewer, keeps the default 20-turn window, and
rejects `sinceSeq` just as Node's agent self-API does. Conversation listing and
spawning/forking mutations still proxy to Node because they involve runtime
state or execution orchestration.

The participant-local view mutations `POST /v1/sessions/:id` and
`POST /v1/conversations/:id` are Go-owned as well. They validate and persist
only title, archive, pin, and color changes in the shared `participants` row,
enforce project membership when applicable, and retain the agent-side audit
event. Title regeneration and all forking paths remain on Node because they
invoke orchestration and session-copy behavior.

`GET /v1/contexts` is Go-owned with source authentication. It derives the
personal, channel, group, and project context list from the synchronized
directory, project membership, and participant-scoped session index, including
the existing session-count and last-activity rules. Resource listings for a
context remain on Node because files, skills, and deployments are not yet
migrated as Go-owned durable stores.

`GET /v1/sessions` and `GET /v1/conversations` are Go-owned. They retain the
sidebar visibility rule (entries/title or live work), project-membership
filtering, and viewer-local presentation fields. Go reads live runs and
background processes from their shared PostgreSQL tables, and pending approvals
and monitors from Node's shared durable JSONB maps, so `working`,
`awaitingInput`, `backgroundJobs`, and `watches` remain accurate during the
transition.

`GET /v1/sessions/:id/background` is Go-owned with source authentication and
participant visibility. It reads live background process records and enabled
monitor definitions from those same shared PostgreSQL stores; process-output
streaming remains proxied to Node because it requires the active sandbox.

`GET /v1/sessions/:id/approvals` is Go-owned for personal and ordinary shared
sessions. It retains Node's source authentication, required viewer, participant
visibility, actor filtering, stable command-derived request ID, optional grant
metadata, and default blocking behavior while reading the shared `approvals`
durable JSONB map. Project-session approvals are also Go-owned: Go checks the
requester's current project membership and roster version plus the viewer's
participant window before exposing the record. The same current-record rule is
used when deriving the sidebar's `awaitingInput` state.

The source-worker approval reads `GET /v1/approvals/:id` and
`GET /v1/approvals/pending` are Go-owned too. They expose the original durable
approval record or the Node-compatible pending-turn result only when the
session and project roster snapshot remain current. This lets Node workers and
Go control-plane endpoints share the same approval state without a second
approval store.

The admin impersonation handoff endpoints `POST /v1/admin/impersonate` and
`POST /v1/admin/impersonate/stop` are Go-owned. They retain org-admin checks,
canonical directory display names, request validation, and audit records; the
browser remains responsible for keeping the short-lived impersonation UI state.

`GET /v1/admin/files` is Go-owned for artifact metadata. It uses the shared
`file_artifacts` table, including scope or org-wide authorization, enabled
filtering, stable ordering, blob-openability metadata, and audit events.
When `qm.file_store.mode: local` shares Node's `DATA_DIR/docstore`, Go also
owns `GET /v1/admin/files/read` and `/download`: it retains Node's org-admin
check, 256 KiB UTF-8 preview bound, safe disposition headers, inline media
policy, and audit events. With the same local transfer directory configured,
`POST /v1/admin/files/upload` consumes and deletes a staged raw Blob, writes
the immutable docstore object, and persists the artifact metadata. S3 or an
undeclared byte-store topology continues to proxy all three byte routes to
Node.

`GET /v1/files` is Go-owned for agent capabilities scoped to personal,
organization, or synchronized web-project contexts. It uses the Node-compatible
`file_artifacts` cursor (`createdAt|id` base64url), returns the same `owned` and
ACL-derived `shared` groups, and keeps blob-openability metadata. Portal calls,
and channel, non-project group, or team capability scopes, still proxy to Node
because their live identity semantics are not fully represented in Go.
When Node uses its local durable byte store, `GET /v1/files/:id/content` can
also be Go-owned for the same capability scopes. Set
`qm.file_store.mode: local` and `qm.file_store.local_dir` to Node's
`DATA_DIR/docstore`; Go validates the `files/<sha256>` key, applies the same
owner/ACL check, then streams the shared file. Leave this setting unset for S3,
portal, or unprojected shared/team traffic, which continues to proxy to Node.
`POST /v1/files/upload` is Go-owned under the same local mode when
`qm.file_store.transfer_local_dir` points to Node's `DATA_DIR/transfer`.
Go then owns the local streaming `POST /v1/blobs` and `GET /v1/blobs/:id`
boundary as well: source signatures use the Node-compatible canonical
`method + path + declared SHA-256` form without replay deduplication, and
`blob-transfer` capabilities retain their read/write and exact-id constraints.
Staged blobs stay opaque until upload. Go consumes and deletes a staged file
when creating the durable artifact, writes the content-addressed docstore file,
persists metadata, and adds the shared-scope read grant. Keep this setting
unset for S3 staging and channel, non-project group, or team capability scopes,
which continue to proxy to Node.

Anonymous/source reads `GET /v1/deployments` and `GET /v1/deployments/:id`
are Go-owned. They read the shared durable `deployments` map and return the
Node-compatible metadata view, including version history and timestamps.
Identified deployment reads still proxy to Node because their result includes
per-viewer ACL permissions and minted Git access URLs; runtime deployment,
Git, and publish mutations remain Node-owned.
Source-authenticated `POST /v1/deployments/:id/name` and
`POST /v1/deployments/:id/display-name` are Go-owned durable-map edits. They
preserve Node validation, per-deployment advisory locking, duplicate-name
checks, full-document retention, version invalidation, and audit records.
Capability-authenticated edits still proxy to Node until the complete team-scope
management projection is represented in Go.
`POST /v1/deployments/:id/share` is also Go-owned for the narrow, durable ACL
operation: only the personal owner capability may resolve an internal directory
recipient or a scope, replace that target's `deployment:<id>` read/write grant,
and append the matching share/unshare audit event. Deployment provider calls,
Git access, and every lifecycle mutation remain Node-owned.
`GET /v1/admin/deployments` is Go-owned as the scoped-admin summary projection
of that same durable map. It preserves owner-scope filtering, audit behavior,
version counts, and removes transient `access` query grants from public URLs.
`GET /v1/admin/crons` is likewise Go-owned as the scoped-admin summary of the
shared cron store. It preserves owner-scope filtering and summary fields while
excluding the operational `fireLog`; scheduling, trigger evaluation, and cron
delivery execution remain Node-owned.
`PUT /v1/admin/crons/:id/destination` is Go-owned for an administrator's
durable destination edit. It applies Node's strict principal/slack destination
shape, preserves the complete cron document, records both retarget and admin
audit events, and, for `scopeShared` crons, appends the same idempotent owner
notice to the shared delivery queue. The Node delivery worker still claims and
sends that notice, so this migration does not move Slack/provider side effects
into Go.

### Deployment-layer boundary

`/v1/deployment-layer` deliberately remains Node-owned for now, even though
its current revision is stored in PostgreSQL's `deployment_layer` durable map.
It is not a plain configuration row: a successful write validates every tool
descriptor and skill manifest, checks collisions against published skills and
skill packs, materializes the accepted skills under a fleet advisory lock, and
restores the previous projection if materialization fails. Node also refreshes
the loaded tool layer used by active agent processes. Moving only the JSON
write to Go would acknowledge a revision that has not been atomically applied
to the runtime.

The Kratos `qm.deployment.v1.DeploymentLayerService` now owns the underlying
one-row durable map with `Get`, `PutIfAbsent`, and JSONB-equality
`CompareAndSet` operations. These calls are internal-token protected and bump
the same `durable_map_versions` key as Node's PostgreSQL map, so a future Node
adapter can stop directly querying this table without blind overwrite races.
That adapter is now available in Node and is opt-in through
`QM_BACKEND_DEPLOYMENT_LAYER_GRPC_URL`; it reuses
`QM_BACKEND_GRPC_INTERNAL_TOKEN`, performs bounded retries on CAS conflicts,
and falls back to the existing PostgreSQL DurableMap when the endpoint is not
configured.
The HTTP route is still Node-owned: it may become Go-owned only after that
adapter reports the supplied content hash as applied, while keeping
validation/materialization as one acknowledged workflow and preserving the
existing `applied`/`degraded` response state.

### Service probes

`GET /healthz` is Go-owned liveness and always returns `{ "ok": true }` once
the Kratos HTTP process is accepting requests. `GET /readyz` is Go-owned
readiness: it pings the shared PostgreSQL pool with a bounded timeout and
returns `503 { "ok": false }` until the durable control-plane state is
reachable. Deployments should use `/readyz` for traffic admission and keep
`/healthz` for process-restart liveness.

`POST /v1/surface-context` and `POST /v1/surface-file` are Go-owned in
`route_mode: go`. Go applies the same channel resolution, public/private
visibility, request limits, polling timeouts, result merge, and short-lived
Blob download capability contract while the existing Node Slack worker drains
the shared request rows. The source callback
`POST /v1/surface-context/:id/result` is Go-owned as well.

The Go pending implementation and PostgreSQL `context_request_tokens` sidecar
are complete, including Node-compatible AES-GCM key derivation and fallback-key
decryption from `auth.connector_secret_key`. The public
`GET /v1/surface-context/pending` routing boundary deliberately still proxies
to Node: Node-internal live-search callers currently keep their viewer token in
that process's memory. Switch pending to Go only when both the live-search
request producer and the Slack context worker move together; otherwise a
partial cutover would silently downgrade authenticated Slack search.

`GET /v1/admin/skills` and `GET /v1/admin/skills/:id` are Go-owned read
projections of the shared `skills` and `skill_packs` durable maps. They retain
scoped-admin authorization, private-content capability boundaries, audit
records, pack provenance, and file metadata without exposing skill file
contents. `DELETE /v1/admin/skills/:id` is Go-owned as well: it preserves the
scoped-admin check and archive audit event, performs Node's idempotent archive
transition, and bumps `durable_map_versions` so Node's durable-map cache sees
it. Skill creation, review/publish transitions, capability validation,
materialization, and pack synchronization remain Node-owned.

`GET /v1/admin/skill-packs` is Go-owned as an org-admin durable-map read. It
retains the complete pack document and calculates the Node-compatible distinct
published upstream-skill import count. `DELETE /v1/admin/skill-packs/:id` is
also Go-owned: under Node's shared skill-materialization advisory lock it
removes only skills created by that pack, its cached bundle, and the pack row,
with the corresponding per-map durable-version invalidations and audit event.
`PATCH /v1/admin/skill-packs/:id` is Go-owned too: it applies the Node patch
grammar for ref/url, trust tier, sync mode, subset, and normalized config,
then invalidates the shared pack map and audits the resulting target scope.
Registration, fetch/catalog, import, and sync remain in Node because they
execute Git and skill-review workflows.

`GET /v1/admin/resources` is Go-owned as the static administrator navigation
contract. It preserves the Node resource ids, kinds, scope targets, clearable
and secret flags, descriptions, supported harnesses, and model choices without
loading a secret or runtime adapter. `PUT /v1/admin/scopes/:scope/:resource`
remains Node-owned until each configuration resource's validation, persistence,
and runtime refresh behavior is represented by Go.

`GET /v1/admin/keychain` is Go-owned as an org-admin metadata projection of
the shared `keychain_credentials`, `keychain_grants`, and `keychain_asks`
durable maps. It never decrypts a credential or returns `secretEnc`, excludes
managed connector and broker entries as Node does, includes the same person
summary, and atomically marks expired asks while invalidating Node's map cache.
Credential creation, encryption, OAuth exchange/refresh, grants, approval,
revocation, and sandbox materialization remain Node-owned.

Model providers, the base model, Slack installation, and OAuth clients are now
YAML-managed by Go. `GET /v1/admin/onboarding`, the legacy model/custom-provider
reads, and `GET /v1/admin/slack-installation` return safe status projections
without secrets. Their legacy HTTP mutations return `409 yaml_managed` and
point operators to `qm.models`, `qm.slack`, or `qm.oauth.clients`. Node no
longer registers those administration routes; it receives the secret-bearing
runtime projection only through the internal-token-protected Control gRPC
service and remains the execution adapter for model, Slack, and OAuth traffic.

`GET /v1/admin/sandbox-routes` can be Go-owned after declaring both
`qm.sandbox_default_backend` and the ordered `qm.sandbox_backends` list in the
Go YAML configuration. With no declared topology it proxies to Node, avoiding a
guess about which sandbox adapters are constructed. After cutover Go reads the
shared `sandbox_routing` durable map and returns route metadata only. The
workspace copy, provisioning, re-sync, capability-loss decision, teardown, and
route mutation operations remain Node-owned.

`GET /v1/admin/scopes` is Go-owned as the organization-admin scope overview.
It rebuilds the same labels and per-scope conversation/background activity,
last-message, cron, deployment, and skill counts from shared PostgreSQL state;
resource configuration detail and mutations remain Node-owned.

`GET` and `POST /v1/soul` are Go-owned. They read and update the shared
`soul_configs` durable-map projection, construct the same
organization-authoritative lower-scope effective view, append Node-compatible
embedded history revisions, take the shared governance advisory lock, and bump
`durable_map_versions` so Node cache refreshes see Go writes. Source callers
retain personal/self and private-channel/current-group management checks;
capability callers retain their capability-scoped shared-scope path. The older
`soul_history` map is read only to preserve legacy revisions while existing
Node administration-resource editing and conditional `expectedVersion` writes
remain on Node.

`GET /v1/scope-resources` is Go-owned for source-authenticated surface calls.
It reconstructs Node context visibility from synchronized directory and project
membership, then returns scoped file metadata (including file ACL handles),
crons, deployments with Git read/write permission derivation, skills, and the
management flag. File bytes, deployment runtime, and mutation routes remain on
Node. Team-scope requests remain proxied to Node until Go owns the identity
team-membership projection.

Source-authenticated `POST /v1/grants` and `/v1/grants/revoke` are Go-owned.
They use the shared ACL table, preserve idempotent grant insertion and
all-permission revoke behavior, resolve the Node artifact author for shared
scope manager checks, and record the corresponding audit entries. Capability
`POST /v1/share` now also has a Go-owned capability branch for its ordinary
share operation: it resolves the Node-compatible `toScope` form (an explicit
personal/org/channel/group scope or an internal-directory teammate), verifies
the artifact home manager and target-context membership, then appends the same
idempotent `file`, `skill`, `cron`, or `deployment` ACL grant and `grant` audit
record. `move:true`, skill-to-org promotion, and `team:*` targets continue to
proxy to Node: those paths require its skill review/promotion or project-team
membership/runtime semantics. Skill publish and every other workflow that
materializes or executes artifacts remain Node-owned.

`POST /v1/runs/:id/signal` is Go-owned with source authentication. Go validates
abort/steer input, preserves the steer-text validation and `not_found`/terminal
responses, then locks the shared run row before appending to `run_signals`.
The worker and live turn stream remain in Node; both runtimes therefore consume
the same durable signal queue during the transition.

`POST /v1/session-cap` is Go-owned with source authentication plus a valid
`x-portal-identity` token. Configure `auth.portal_identity_secret` (or the
existing `PORTAL_IDENTITY_SECRET` deployment secret); in development it falls
back to the source-signing secret, matching Node. Go validates that the portal
principal is an active internal directory member, then mints the compatible
one-hour personal capability token using `auth.capability_secret`.
Both portal and capability verification accept the legacy two-part HMAC format
as well as current compact-JWS tokens, so a rollout does not invalidate
already-issued Node tokens.

Source-authenticated personal-memory endpoints `GET`/`PUT /v1/memory`,
`GET /v1/memory/history`, and `POST /v1/memory/restore` are Go-owned. They use
the shared append-only `memory_revisions` table, retain the Node normalization,
optimistic revision token, advisory-lock, no-op update, history, and restore
semantics, and record the same memory audit events. History and restore still
require a valid internal `x-portal-identity` matching `principalId` for source
callers. Capability-authenticated `GET /v1/memory/history` and
`POST /v1/memory/restore` are Go-owned too: Go projects the token's narrow
`memory.write` / `memory.orgWrite` grants, so the caller can inspect or restore
only the explicitly granted personal or organization notebook; missing grants
remain a 404 as in Node. Agent fact capture, recall/search, and memory
self-curation remain Node runtime responsibilities for now.

`GET /v1/apis` is Go-owned for control-plane capabilities. It renders the
stable agent discovery categories in Node order: standard self-service routes,
optional memory routes, and the live-admin plane. Admin visibility is derived
only from shared `admin_grants`; autonomous admin tokens retain only
`/v1/admin/whoami`. Every discovery request records the existing `apis.list`
audit event. The catalog may advertise routes that still proxy to Node during
the staged cutover, but it does not invoke their runtime adapters.

The side-effect-free capability metadata reads `GET /v1/keychain/credentials`,
`GET /v1/keychain/grants`, and `GET /v1/keychain/asks` are Go-owned. They read
the shared durable maps, expose only credentials owned by the token actor,
combine grants owned by that actor with grants usable in the token scope, and
show only relevant asks. The Go projection never decrypts or returns
`secretEnc`; credential saving, grant/ask approval, ask delivery,
OAuth consent/exchange/status handling, and secret materialization remain
Node-owned.
`GET /v1/keychain/overview` is Go-owned as the corresponding complete owner
view: it adds safe managed-connector status metadata, the owner's grants and
pending asks, recent credential-usage records, and resolved scope names. It
uses no credential plaintext or refresh operation; connector token refresh and
all credential side effects other than the explicit persisted revoke route
remain Node-owned.
`POST /v1/keychain/asks/:id/decline` is Go-owned for the credential owner on a
person-sent capability turn. It atomically changes only the shared ask record,
trims its optional note, records the Node-compatible audit event, and bumps
`keychain_asks` map freshness. It intentionally leaves `notifiedAt` absent, so
Node's existing ask sweep remains the sole runtime authority to resume the
waiting turn or send its fallback delivery.
`DELETE /v1/keychain/credentials/:id` is Go-owned for a capability actor's
ordinary credential. Go atomically revokes that credential's active grants,
removes the credential, and increments the shared Node durable-map versions;
managed connector and broker credentials remain non-deletable through this
path. Credential creation, grant/ask mutation, and materialization stay in
Node.
`POST /v1/keychain/grants/:id/revoke` is Go-owned for the grant owner. It
preserves Node's successful no-op for an already non-active grant and increments
the grants durable-map version only when an active grant is transitioned to
revoked. The direct `POST /v1/keychain/grants` branch is Go-owned too: a live
capability owner may grant their own non-broker credential to its current scope;
Go preserves Node's purpose, expiration, credential-expiry, stable ID,
durable-version, ask-adoption, and audit behavior without reading secret
ciphertext. `ask`-based grant approval remains Node-owned because it resumes a
waiting agent turn; ask delivery and all secret materialization also remain in
Node.
`POST /v1/connectors/oauth/revoke` is Go-owned for the persisted OAuth
disconnect operation. It preserves Node's provider host catalog and the three
connector account slots (default, personal, company), removes each deterministic
managed credential id, revokes its active grants, and preserves DurableMap
version increments even for an already absent slot. Capability callers may
revoke themselves or an id in the token's narrow `keychainMembers` conversation
claim; signed source callers retain their existing explicit `principalId`
behavior. The credentials/grants DurableMap tables are created lazily on the
first Go revoke too, so rollout does not depend on a Node worker having touched
keychain state first. Go also reads each keychain DurableMap independently, so
a never-created asks map cannot hide already-created credentials or grants.
Capability validation distinguishes an omitted `memory` or `keychainMembers`
claim from an explicit `null`, matching Node's object/array claim validation.
OAuth consent, callback/exchange, refresh, token writes, and all other external
OAuth interactions remain Node execution work. The read-only
`GET /v1/connectors/catalog` and `GET /v1/connectors/oauth/status` endpoints are
Go-owned, with availability derived from YAML OAuth clients:

```yaml
qm:
  oauth:
    clients:
      - provider: google
        client_id: example.apps.googleusercontent.com
        client_secret: replace-me
```

Go returns the public provider catalog and aggregates the three shared keychain
connector slots (default, personal, company) without exposing a client secret
or invoking a refresh. Node receives the selected YAML client through protected
internal gRPC when it performs the OAuth flow.

The generic administrative scope configuration routes
`GET /v1/admin/scopes/:scope` and
`PUT /v1/admin/scopes/:scope/:resource` remain Node-owned except for YAML-managed
`base-model` and `connectors` writes, which Go rejects with HTTP 409. Their payloads
combine persisted settings with live model/provider availability, harness
approval, connector configuration, and in-process config-cache flushes. Go
continues to own the individual persisted admin endpoints already listed here,
but must not claim the generic route until it has an equivalent runtime
configuration projection. The same boundary applies to secret drops,
deployment archive/restore/redeploy, and all other operations that invoke an
external provider, runtime worker, or secret materialization.

The scoped org-admin content endpoints `GET`/`PUT /v1/admin/memory` are also
Go-owned, with the same personal-scope safety restriction for agent calls,
scope authorization, audit events, and revision-table writes. Admin scope
discovery at `GET /v1/admin/memory/scopes` is Go-owned too: it reconstructs
the Node scope set from shared sessions, participants, grants, directory labels,
and revision metadata without reading private memory bodies.

The remaining migration is the rest of the Node control-plane surface. Legacy
Knowledge Core CLI, MCP, and standalone service entrypoints are intentionally
not part of this backend.
