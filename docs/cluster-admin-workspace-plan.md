# Plan: on-demand cluster-admin workspace with portal controls

**Status: implemented — first complete release (portal-controlled activation,
automatic expiry, revocation, status, and restricted workspace access). The
agent request extension in section 15 is deliberately deferred. Operations:
see [cluster-admin-runbook.md](cluster-admin-runbook.md).**

Build a second, isolated pi-pocket workspace, controlled through the portal,
with time-limited cluster-admin sessions. The normal workspace keeps its
current namespace-level permissions and never receives the elevated workspace's
credentials.

The first complete release includes activation, automatic expiry, revocation,
status, and restricted workspace access. Manual activation remains an emergency
fallback, not the primary workflow. The agent request extension follows only
after the portal-controlled lifecycle is verified.

## 1. Goals and boundaries

### Required outcomes

1. Normal pi-pocket remains namespace-admin, not cluster-admin.
2. An explicitly authorized human can activate a separate admin workspace
   through the portal.
3. Activation requires a justification and a bounded duration.
4. Cluster-admin access applies only to the admin workspace's service account.
5. Access expires automatically without relying on a browser remaining open.
6. The human can revoke access immediately.
7. The portal distinguishes a requested session, an active Kubernetes permission
   grant, a ready workspace, completed revocation, and an unknown or failed
   cleanup state.
8. Every activation starts with a fresh workspace home and application identity.
9. Existing installations behave exactly as before unless this feature is
   explicitly enabled.

### Not included initially

- Giving the normal agent an admin token.
- Temporarily elevating the normal service account.
- An arbitrary privileged command executor.
- Agent-driven activation without human approval.
- Concurrent admin sessions.
- Automatic session renewal.
- Sharing the normal workspace's home, SQLite database, browser profiles, or
  credentials.
- A claim that expiry reverses changes made with cluster-admin.

## 2. Architecture

```text
Normal workspace namespace
└── pi-pocket
    ├── Existing namespace-admin service account
    ├── Existing persistent workspace
    └── No admin credentials or lifecycle authority

Management namespace
├── Portal
│   ├── Existing configuration/workspace controls
│   ├── Human authentication and admin authorization
│   ├── Admin session UI and API
│   └── Durable session requests
│
└── Admin session controller
    ├── No public command API
    ├── Reconciles approved sessions
    ├── Manages one predefined cluster-admin binding
    ├── Starts/stops one predefined admin Deployment
    └── Revokes expired sessions independently of the portal

Admin workspace namespace
└── pi-pocket-admin
    ├── Separate service account
    ├── Fresh ephemeral workspace
    ├── Separate runtime Secret
    ├── Separate HTTPS agent/terminal hosts
    └── Cluster-admin only during an active session
```

Example namespace names:

- `pi-pocket`
- `pi-pocket-management`
- `pi-pocket-admin`

All three must be distinct when elevation is enabled.

### Why a separate controller?

The controller makes expiry independent of portal restarts, browser disconnects,
and HTTP request timeouts. It also keeps Kubernetes RBAC mutation out of
ordinary portal request handlers.

This does not make the portal unprivileged. If the controller trusts session
approvals written by the portal, compromising the portal could cause an
unauthorized grant. Both components belong to the elevated trust boundary.

## 3. Security contract

Encode these rules in tests and, where possible, configuration validation.

| Component | Allowed authority |
| --- | --- |
| Normal workspace | Existing namespace permissions only |
| Ordinary portal user | Existing portal functions; no admin workspace credentials |
| Authorized admin operator | Activate, inspect, open, and revoke admin sessions |
| Portal service account | Named session-state access and narrowly scoped admin access-data reads |
| Controller service account | One predefined grant and one predefined workspace lifecycle |
| Admin workspace service account | Cluster-admin during the approved session only |

### Critical restrictions

- The portal and controller cannot run in the normal workspace namespace.
- The admin workspace cannot run in either the normal or management namespace.
- The normal workspace gets no new RoleBinding into either protected namespace.
- Browser requests cannot choose arbitrary namespaces, Deployments, service
  accounts, ClusterRoles, binding names, or images.
- No Kubernetes tokens are returned by the portal's admin API.
- Admin owner-login links are never included in ordinary `/api/config` responses.
- The GitHub credential-broker endpoint cannot approve an admin session.
- Enabling an extension cannot enable cluster-admin.

### Honest limitations

An unrestricted cluster-admin agent can read cluster Secrets, create alternative
credentials, modify RBAC, and interfere with the controller or portal.

The system provides deliberate activation and bounded original access, not
containment of a malicious cluster administrator.

Controller or API outages can delay revocation. The UI must report that
uncertainty rather than claiming access has definitely expired.

## 4. Reuse the existing workspace chart

Use two Helm releases of `charts/pi-pocket`, rather than duplicating the
workspace templates:

- Normal release: existing persistent workspace.
- Admin release: isolated, stopped-by-default workspace.

This preserves the existing image, entrypoint, single-writer rules, services,
and ingress implementation.

### Proposed chart additions

Names below are illustrative:

```yaml
workspace:
  mode: standard # standard or admin

persistence:
  mode: pvc # pvc or ephemeral
```

Normal defaults remain unchanged. The admin profile would resemble:

```yaml
fullnameOverride: pi-pocket-admin

workspace:
  mode: admin

replicas: 0

serviceAccount:
  namespaceRole: ""

persistence:
  mode: ephemeral

portal:
  enabled: false

openshift:
  pocketSCC: restricted-v3

runtimeSecret:
  existingSecret: pi-pocket-admin-runtime

ingress:
  pocketHost: pocket-admin.apps.example.com
  terminalHost: pocket-admin-terminal.apps.example.com
```

### Admin profile safeguards

- Render with `replicas: 0`.
- Require ephemeral workspace storage initially.
- Reject a supplied existing PVC.
- Do not grant namespace `admin` by default.
- Disable its own configuration portal; use the management portal.
- Keep `Recreate` and the existing maximum of one replica.
- Keep `hostUsers: false`.
- Prefer `restricted-v3`: cluster maintenance generally does not require nested
  Podman.

The controller temporarily scales the Deployment to one. Helm values remain
zero.

Do not upgrade either admin control-plane configuration or the admin workspace
during an active session. The deployment runbook must require confirmed
revocation first.

### Storage consequences

Using `emptyDir` for `/workspace` means:

- Owner credentials and invitations are fresh each session.
- No persistent agent configuration is carried forward.
- Provider browser logins do not persist.
- Work must be explicitly exported before shutdown.
- Replacement Pods also start with fresh application state.

Provider API keys can still come from a separate admin runtime Secret. They
must not be copied automatically from the normal workspace.

## 5. Portal authentication and authorization

### Recommended initial policy

Require:

1. GitHub authentication.
2. Active membership under the existing portal login policy.
3. An explicit operator allowlist based on numeric GitHub user IDs.
4. A recent portal authentication for activation and access-link retrieval.
5. A fresh membership check before activation.

The existing `githubapp.User` contains both numeric ID and login, so the identity
representation can be reused.

### Why not the existing shared portal token?

In a same-namespace installation, the normal namespace-admin workspace can
read the portal token Secret. That credential must not become an elevation
credential.

For the first release:

- Reject elevation configuration in token-auth mode.
- Require the management namespace.
- Do not treat organization ownership alone as authorization to administer the
  cluster.
- Keep the operator allowlist in deployment configuration controlled by the
  installer, not editable through normal portal configuration.

### Session ownership

An activation belongs to the approving operator:

- Only that operator receives the admin workspace login link.
- Other authorized operators may revoke it.
- No concurrent sessions.
- No existing normal-workspace invitations are migrated.
- Any collaborator admitted to the elevated workspace shares its Kubernetes
  rights; make that explicit.

For a personal installation, self-activation with explicit confirmation is
sufficient. Two-person approval can be added later if desired.

## 6. Portal API

Add fixed admin endpoints, separate from the current normal-workspace endpoints.

| Method | Endpoint | Purpose |
| --- | --- | --- |
| `GET` | `/api/admin/status` | Feature availability, caller capabilities, session and observed lifecycle status |
| `POST` | `/api/admin/activate` | Create a bounded, explicitly approved session |
| `POST` | `/api/admin/revoke` | Request revocation of the current session |
| `POST` | `/api/admin/access` | Retrieve an owner-login link for the active session's operator |

### Activation request

```json
{
  "resourceVersion": "12345",
  "reason": "Investigate a failed operator upgrade",
  "durationSeconds": 900,
  "confirmation": "cluster-admin"
}
```

The server generates the session ID, approver identity, approval time, and
expiry time. The client cannot supply those authoritative values.

### API behavior

- Use existing strict Origin checks for cookie mutations.
- Reject unknown fields and malformed input.
- Bound reason length and request size.
- Reject zero, negative, or excessive durations.
- Reject activation while another session is active or cleanup is incomplete.
- Use `resourceVersion` for optimistic concurrency.
- Return `202 Accepted` when lifecycle work has been queued, not completed.
- Return `409` for conflicting or stale session operations.
- Return sanitized errors without credentials or raw Kubernetes responses.
- Keep ordinary deployment start/stop routes pinned to the normal workspace.

Revocation must remain available to an authenticated authorized operator even
if activation's recent-login requirement has expired. Emergency shutdown should
not require first completing a new login flow.

## 7. Durable session state

Store session state in one named management-namespace Secret, using the
repository's existing named-resource and optimistic-concurrency patterns.

Do not store the admin Kubernetes token or owner token in this record.

Suggested fields:

```text
sessionID
requestedState
approvedByUserID
approvedByLogin
reason
approvedAt
expiresAt
observedPhase
adminDeploymentUID
activePodUID
grantObserved
lastReconciledAt
failureCode
resourceVersion
```

Separate desired state from observed state. A valid approval record is not
proof that the workspace is running or that permissions were granted.

### Proposed defaults

- Default duration: 15 minutes.
- Maximum duration: 30 minutes.
- No renewal endpoint in the first version.
- Startup timeout: configurable and bounded.
- Controller reconciliation: approximately every 5 seconds.
- Startup time counts toward the session duration.

These are operational defaults, not security guarantees about exact revocation
timing.

### State preservation

- Controller restarts must recover from durable state.
- Connected Helm upgrades must preserve state.
- Offline renders must not overwrite live state.
- Missing or malformed state must never cause activation.
- Invalid state should trigger attempted cleanup of the explicitly managed
  grant and workspace.
- Cleanup failures remain visible until resolved.

## 8. Lifecycle controller

Implement the lifecycle as a small Go reconciler, independent of HTTP handlers.

Suggested phases:

```text
Idle
  → ActivationRequested
  → Activating
  → Active
  → RevocationRequested / Expired
  → Revoking
  → Idle
```

Failures enter an explicit error or cleanup-required state.

### Activation sequence

1. Read and validate the durable approval.
2. Confirm it is unexpired and within configured limits.
3. Verify the target Deployment, service account, binding, and namespaces match
   fixed configuration.
4. Confirm no previous admin Pod remains.
5. Clear stale owner-login metadata from the admin runtime Secret.
6. Persist the activating state.
7. Add the configured admin service account to the predefined cluster-admin
   binding.
8. Scale the predefined Deployment to one.
9. Observe the new Pod and its UID.
10. Wait for readiness and matching owner-login metadata.
11. Mark active only after the required observations succeed.

If any step fails after granting access, attempt revocation immediately.

### Revocation sequence

1. Persist the revoked/expired intent so reconciliation cannot reactivate it.
2. Clear the binding's subjects.
3. Independently attempt to scale the Deployment to zero.
4. Observe Pod termination.
5. Clear owner-login metadata.
6. Mark idle only after cleanup is confirmed.

Failure to remove the grant must not prevent an attempt to stop the Pod, or
vice versa.

### Restart and failure behavior

- Re-read expiry after every restart.
- Never reset or extend a deadline during recovery.
- Reconcile partially completed activation.
- Revoke expired sessions before considering new activation.
- Reject new sessions while cleanup remains incomplete.
- Do not report success based only on desired replica count.
- Treat stale controller observations as unknown state.

Deploy one active controller initially, with non-overlapping updates. Do not
introduce multiple reconcilers without a concurrency design.

## 9. Kubernetes RBAC and binding ownership

### Use a predefined, inert binding

Provision one dedicated ClusterRoleBinding:

```text
roleRef: cluster-admin
subjects: []
```

While idle, it has no subjects and grants no workspace access. During a session,
the controller sets exactly one subject: the configured admin service account.

An inert binding exists while stopped, but no active cluster-admin grant exists.

### Why this structure?

Kubernetes cannot restrict top-level resource creation by `resourceNames`. A
controller that creates arbitrary ClusterRoleBindings would need broader
permissions. A predefined binding allows named `get`/`patch` access instead.

The controller also needs permission to `bind` the referenced `cluster-admin`
ClusterRole unless it already possesses that authority.

That delegation is highly privileged. Restricting the binding's name does not,
by itself, restrict which subjects a compromised controller could insert.
Treat this controller as cluster-admin-equivalent authority.

Reference: [Kubernetes RBAC authorization](https://kubernetes.io/docs/reference/access-authn-authz/rbac/),
including resource-name restrictions and restrictions on role binding creation
or update.

### Controller permissions

Grant only the operations actually required:

- Named binding read/update.
- `bind` on the named `cluster-admin` ClusterRole.
- Named session-state read/update.
- Named admin Deployment read/scale.
- Required Pod observation within the dedicated admin namespace.
- Named admin runtime Secret cleanup.

Do not grant generic Kubernetes resource mutation, arbitrary role creation,
`pods/exec`, token issuance, or arbitrary Secret access across namespaces.

Verify binding ownership before mutation. A name collision with an unrelated
object must fail, not be silently adopted.

Bootstrap of the delegation must be an explicit action by a suitably privileged
installer. It must not appear in default installations.

## 10. Owner-login and terminal access

The current entrypoint publishes `owner-login-url` into the runtime Secret.
Extend that synchronization to include the originating Pod UID:

```text
owner-login-url
owner-login-pod-uid
```

The portal should return a link only when:

- The caller owns the active session.
- The session has not expired.
- The controller observation is fresh.
- The workspace is ready.
- The stored Pod UID matches the observed active Pod.
- The URL matches the configured admin HTTPS origin.

This prevents an old login link from being presented after a new Pod has
replaced the previous workspace.

### Credential handling

- No admin link in normal configuration responses.
- No admin link in audit logs or session-state records.
- No browser localStorage/sessionStorage persistence.
- Clear links when the portal locks, the session ends, or status becomes
  uncertain.
- Use the existing log-filter behavior for owner-token redaction.
- Clear published login metadata during cleanup.

The admin terminal shares the elevated pod identity. Opening it is also
cluster-admin access, not a lower-privilege alternative.

Use a new browser tab initially instead of expanding iframe embedding and CSP
rules.

## 11. Portal UI

Add a separate **Cluster administration** panel.

### Idle view

Show:

- "Normal workspace remains namespace-admin."
- Target cluster/workspace identity.
- Required reason.
- Duration selector.
- Explicit cluster-admin warning.
- Confirmation control.
- Activate button for authorized operators only.

### Active view

Show:

- Session owner.
- Justification.
- Approval and expiry times.
- Countdown.
- Grant status.
- Workspace readiness.
- Controller observation freshness.
- Open admin workspace.
- Open admin terminal.
- Revoke access.

### Failure view

Distinguish activation failed and cleanup completed, permission removal failed,
Pod shutdown failed, controller unavailable, and state unknown.

Do not show a green "off" state merely because the countdown reached zero.

Follow the current SPA patterns:

- Plain JavaScript and CSS.
- Render user-controlled strings with `textContent`.
- No new frontend dependency.
- DOM/cache contract tests.
- Admin-panel errors must not break the normal workspace panel.

## 12. Proposed repository changes

| Path | Planned changes |
| --- | --- |
| `charts/pi-pocket/values.yaml` | Workspace mode, ephemeral storage, disabled-by-default portal admin configuration |
| `charts/pi-pocket/templates/validate.yaml` | Fail-fast checks for unsafe elevation combinations |
| `charts/pi-pocket/templates/deployment.yaml` | Ephemeral workspace support; retain existing normal defaults |
| `charts/pi-pocket/templates/pvc.yaml` | Render PVC only for persistent mode |
| `charts/pi-pocket/templates/portal.yaml` | Admin configuration environment variables |
| New chart templates | Controller Deployment, named RBAC, session-state resource and explicit bootstrap support |
| `deploy/admin-workspace-values.yaml` | Separate stopped admin release profile |
| `portal/internal/config/` | Admin configuration and validation |
| `portal/internal/server/admin_api.go` | Status, activation, revocation, and access endpoints |
| `portal/internal/server/server.go` | Route registration and authorization integration |
| `portal/internal/githubauth/` | Authentication timestamp/recent-login support |
| `portal/internal/admin/` | Session types, validation, storage and reconciliation |
| `portal/internal/kube/` | Fixed binding, Pod observation and lifecycle operations |
| `portal/cmd/admin-controller/main.go` | Independent controller process |
| `portal/Containerfile` | Build controller binary into the existing portal image |
| `portal/web/index.html`, `app.js`, `app.css` | Admin panel and lifecycle rendering |
| `images/entrypoint.sh` | Pod-bound owner-login metadata |
| Tests | Chart, API, authorization, reconciler, credential safety and UI contracts |
| Documentation | Architecture, security model, install, revocation and recovery runbooks |

Keep the Go dependency set unchanged unless a specific need emerges and is
approved. The existing REST client and standard library should be sufficient.

## 13. Verification plan

### Chart tests

Verify:

- Elevation disabled produces unchanged normal behavior.
- Normal service account never gains cluster-admin.
- Admin release renders stopped.
- Ephemeral mode does not render or reuse a PVC.
- No shared workspace storage.
- All protected namespaces differ.
- Token-auth elevation is rejected.
- Empty operator allowlist is rejected.
- Unsafe durations and incomplete target configuration are rejected.
- Idle binding has no subjects.
- Controller permissions are explicit and named where possible.
- `hostUsers: false`, single-writer and SCC contracts remain intact.

Run rendering tests under both Helm 3.19 and Helm 4.

### Portal authorization tests

Cover:

- Anonymous denial.
- Ordinary-member denial.
- Organization-owner denial unless explicitly allowlisted.
- Numeric identity matching.
- Expired/recent-login requirements.
- Cross-origin and missing-Origin mutation denial.
- Workspace-token denial.
- Conflicting activation.
- Session ownership for access links.
- Other operators' ability to revoke.
- Credentials absent from normal responses and logs.

### Reconciler tests

Use a fake clock and fake Kubernetes API. Cover failure after every lifecycle
step, including:

- Grant succeeds but scaling fails.
- Pod never becomes ready.
- Expiry during startup.
- Controller restart during activation.
- Restart after deadline.
- Simultaneous activation/revocation.
- Malformed state.
- Binding drift or ownership mismatch.
- Grant removal succeeds but shutdown fails.
- Shutdown succeeds but grant removal fails.
- Stale observations.
- Retries without deadline extension.
- Old Pod/login metadata.
- No new activation before cleanup.

### UI tests

Cover panel rendering, capability-based controls, stale-state warnings,
countdown behavior, credential clearing, and normal-panel isolation.

### Local completion checks

Run the repository's required lint, test, and formatting checks. Server-side
dry-run and live probes require separate approval.

No implementation is considered finished merely because the UI works.

## 14. Live rollout and recovery

Perform live validation only after explicit permission.

### Rollout order

1. Create the dedicated namespaces.
2. Provision separate admin provider credentials.
3. Install the admin workspace release, stopped.
4. Install the management portal configuration and controller.
5. Explicitly bootstrap the privileged delegation.
6. Confirm idle state: no active grant and no admin Pod.
7. Activate a short session through the portal.
8. Confirm the admin workspace can perform a harmless cluster-scoped read.
9. Confirm the normal workspace still cannot.
10. Revoke manually and verify cleanup.
11. Repeat with automatic expiry.
12. Test portal restart during an active session.
13. Confirm fresh owner credentials on a subsequent session.

### Emergency recovery

Document a trusted-local-terminal procedure that:

1. Marks the session revoked or disables further reconciliation.
2. Clears the managed binding's subjects.
3. Scales the admin Deployment to zero.
4. Verifies cleanup.
5. Disables further activations.

Deleting only the controller is not revocation: it could leave an existing grant
active.

Rollback or uninstall must happen only after confirmed cleanup. Retained
session records and Secrets must be handled deliberately.

## 15. Later: request extension

After the portal lifecycle is verified, add an optional extension with tools
such as:

```text
cluster_admin_request
cluster_admin_request_status
```

The request contains a task description and justification, not executable
authority.

A human reviews the request in the portal and activates the separate workspace.
The extension receives only request status.

Important properties:

- No approval credential in the normal pod.
- No admin token returned.
- No remote execution channel into the admin workspace.
- No automatic transfer of executable files or agent configuration.
- Extension toggling affects request availability only.
- Normal workspace identity does not identify the individual human steering it.

If a dedicated-audience projected token is used to authenticate requests, it
still belongs to the normal service account and grants no additional Kubernetes
permissions.

## 16. Delivery sequence and acceptance criteria

Implement in reviewable slices:

1. **Isolation and chart support:** second release, ephemeral workspace,
   validation and tests.
2. **Session model and reconciler:** durable state, bounded grants, recovery and
   failure tests.
3. **Portal authorization and API:** operator policy, activation, revocation,
   restricted access.
4. **Portal UI and credential lifecycle:** admin panel, Pod-bound login metadata,
   UI tests.
5. **Integration and runbooks:** complete offline verification, approved live
   probes, emergency recovery.
6. **Optional request extension:** only after the core security contract is
   demonstrated.

### Release acceptance

The feature is ready when:

- Default installations are unchanged.
- The normal workspace has no elevated credentials or control.
- Only explicitly authorized humans can activate sessions.
- Idle state has no admin Pod or active workspace grant.
- Activation produces a fresh isolated workspace.
- Expiry survives portal restarts.
- Manual revocation is available and observable.
- Partial failures never masquerade as successful cleanup.
- Stale login links are not exposed.
- Audit events contain identities and lifecycle outcomes, not credentials.
- Limitations of unrestricted cluster-admin are clearly documented.

The recommended first complete release is portal-controlled activation and
revocation, a separate expiry controller, and a fresh isolated admin workspace.
Defer the agent request extension until those foundations are verified.
