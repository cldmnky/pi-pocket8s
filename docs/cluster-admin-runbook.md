# Cluster-admin workspace: operations and recovery

This runbook covers the on-demand, portal-controlled cluster-admin workspace.
Read `docs/cluster-admin-workspace-plan.md` for the design and its security
contract first; this file is what an operator does.

## What the feature is

The normal pi-pocket workspace keeps its namespace-level permissions and never
receives the elevated workspace's credentials. A separate, stopped-by-default
admin workspace (a second Helm release of the same chart) is granted
`cluster-admin` for a bounded, justified session, and only while that session is
active. A reconciler in the management namespace applies and removes the grant
independently of the portal and of any browser.

Three namespaces are involved and all must differ:

| Namespace | Contents |
| --- | --- |
| workspace (e.g. `pi-pocket`) | the normal agent; namespace RBAC only |
| management (e.g. `pi-pocket-management`) | portal, controller, session state, GitHub App key |
| admin (e.g. `pi-pocket-admin`) | the elevated workspace; cluster-admin only during a session |

## Install

1. Create the namespaces and provision the admin workspace's own provider
   credentials. Do not copy the normal workspace's runtime Secret: the admin
   workspace must start with a fresh home and its own identity.
2. Install the admin workspace release, stopped, from `deploy/admin-workspace-values.yaml`:

   ```bash
   helm upgrade --install pi-pocket-admin charts/pi-pocket -n pi-pocket-admin \
     -f deploy/admin-workspace-values.yaml
   ```

   It renders `replicas: 0` with `emptyDir` storage: nothing persists, and no
   Pod exists while idle.
3. Install or upgrade the management release with elevation enabled. This is the
   explicit installer action that renders the privileged delegation:

   ```bash
   helm upgrade --install pi-pocket charts/pi-pocket -n pi-pocket \
     -f deploy/openshift-values.yaml \
     --set portal.authMode=github \
     --set adminElevation.enabled=true \
     --set adminElevation.bootstrap=true \
     --set adminElevation.namespace=pi-pocket-admin \
     --set adminElevation.deployment=pi-pocket-admin \
     --set adminElevation.serviceAccount=pi-pocket-admin \
     --set adminElevation.runtimeSecret=pi-pocket-admin-runtime \
     --set adminElevation.pocketURL=https://pocket-admin.apps.example.com \
     --set adminElevation.terminalURL=https://pocket-admin-terminal.apps.example.com \
     --set 'adminElevation.operators[0]=<numeric GitHub user id>'
   ```

   `adminElevation.bootstrap=true` renders the inert `ClusterRoleBinding`
   (`subjects: []`) plus the controller's named RBAC. Nothing is elevated by
   installing: an empty binding grants nothing.
4. Confirm idle state before handing the feature to anyone:

   ```bash
   oc get clusterrolebinding pi-pocket-admin-cluster-admin -o jsonpath='{.subjects}'; echo   # []
   oc get deploy pi-pocket-admin -n pi-pocket-admin -o jsonpath='{.spec.replicas}'; echo      # 0
   oc get pods -n pi-pocket-admin                                                            # none
   oc get pods -n pi-pocket-management -l app.kubernetes.io/component=admin-controller
   ```

## Activate

Sign in to the portal as an authorized operator (numeric GitHub user ID in
`adminElevation.operators`) with a **recent** sign-in, then open the
**Cluster administration** tab: enter a justification, choose a duration, type
`cluster-admin`, and activate.

The portal records the approval; the controller applies the grant, starts the
admin workspace, and marks the session active only after the Pod is ready and its
sign-in metadata matches the observed Pod UID. The session link is available only
to the operator who activated it, and only while the state is fresh.

## Revoke

Any authorized operator can revoke from the same panel. Revocation deliberately
needs no recent sign-in: an emergency shutdown must not depend on a new login
flow. The controller clears the binding subjects, scales the workspace to zero,
and clears the published sign-in link. The panel shows the session as ended only
once that cleanup is confirmed.

## Expiry

Sessions expire on their own. The controller re-reads the deadline every pass and
never extends it — including across restarts. `ADMIN_DEFAULT_DURATION_SECONDS`
(default 900) and `ADMIN_MAX_DURATION_SECONDS` (default 1800, hard maximum 3600)
bound a session; startup time counts toward it.

## Failure states

| Failure code | Meaning |
| --- | --- |
| `invalid_approval` | the stored approval was incomplete; the session was revoked |
| `target_unavailable` | the admin Deployment or the binding could not be read |
| `binding_missing` | the predefined binding is gone; re-bootstrap |
| `binding_not_owned` | the binding is not the managed inert one (extra subjects); nothing was mutated |
| `grant_failed` | the binding patch failed; the grant may still be present |
| `scale_failed` | the workspace could not be scaled |
| `workspace_timeout` | the workspace did not become ready in time; the session was revoked |
| `workspace_not_ready` | an active session whose workspace is not currently ready |
| `grant_drift` | the grant stopped matching the configured service account; the session ended |
| `pod_gone` | the active Pod disappeared; the session ended |
| `metadata_cleanup_failed` | the published sign-in link could not be cleared |
| `pod_shutdown_pending` | the Pod is still terminating; cleanup is not finished |
| `state_invalid` | the session record was malformed; cleanup was attempted |

`CleanupRequired` means cleanup is **not** confirmed. The panel says so instead
of showing a green "off" state, and new activations are refused until it clears.

## Emergency recovery (trusted local terminal)

Deleting the controller is **not** revocation: it can leave an active grant
behind. Do this instead, in order:

```bash
# 1. Stop reconciliation from acting on any approval by marking the session revoked.
oc patch secret pi-pocket-admin-session -n pi-pocket-management --type=merge \
  -p '{"data":{"session.json":null}}'

# 2. Remove the grant directly. This is the step that actually revokes access.
oc patch clusterrolebinding pi-pocket-admin-cluster-admin --type=merge \
  -p '{"subjects":[]}'

# 3. Stop the workspace.
oc scale deploy pi-pocket-admin -n pi-pocket-admin --replicas=0

# 4. Clear the published sign-in link so no stale link is usable.
oc patch secret pi-pocket-admin-runtime -n pi-pocket-admin --type=merge \
  -p '{"data":{"owner-login-url":null,"owner-login-pod-uid":null}}'

# 5. Verify: no subjects, no Pods, and the session Secret empty.
oc get clusterrolebinding pi-pocket-admin-cluster-admin -o jsonpath='{.subjects}'; echo
oc get pods -n pi-pocket-admin
```

Step 1 makes the record unusable for activation; the controller will treat the
empty record as idle and converge. If you must stop the controller as well,
scale it to zero **after** steps 2–4, and verify the grant is gone first.

## Rollback and uninstall

Only after confirmed cleanup. The session Secret and the workspace's runtime
Secret carry `helm.sh/resource-policy: keep`; uninstalling the management release
leaves the record and the Secret behind deliberately. Delete them by hand when
you really mean to forget the feature, and remember that
`helm template`/GitOps renders cannot see live session state: never apply an
offline render over a running elevation deployment.

## Boundaries worth repeating

- An unrestricted cluster-admin agent can read cluster Secrets, create
  alternative credentials, and modify RBAC. Expiry bounds the *original* grant;
  it does not reverse what was done with it.
- Anyone admitted to the elevated workspace shares its Kubernetes rights. Invite
  accordingly; the admin workspace starts with fresh owner credentials each
  session and shares nothing with the normal workspace.
- Controller or API outages delay revocation. The UI reports that uncertainty
  rather than claiming access has expired.
