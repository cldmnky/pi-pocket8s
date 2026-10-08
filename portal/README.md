# pi-pocket portal

Small Go web server that manages the pi-pocket agent runtime inside its
namespace:

- runtime API keys stored in the Helm-created runtime Secret
- SSH `authorized_keys` and `known_hosts`
- start, stop, and restart of the pi-pocket Deployment

The server embeds a dependency-free vanilla SPA (no CDN, no build step) and
listens on port 8080. The SPA is styled after pi-pocket's own dark theme and
embeds the agent itself in an iframe (with full-screen mode) on its Workspace
tab. The owner sign-in link — synced into the runtime secret by the agent
entrypoint as `owner-login-url` — is shown there with a QR code rendered
locally by the vendored Nayuki generator (`web/qrcodegen.js`, MIT).
Embedding requires the agent image's runtime patch (`images/embed-patch.sh`)
for upstream's `frame-ancestors` CSP and `SameSite` session cookie; the
portal's own CSP permits `frame-src` for the configured agent origin only.

## Security model

- **Bearer token only.** Every `/api` request is checked in constant time
  against `PORTAL_TOKEN_FILE` (default `/run/portal/token`). The file is
  re-read on every request so projected-Secret rotation needs no restart.
  A missing, empty, or weak token fails closed: startup aborts and requests
  return `503`.
- **TLS-verified Kubernetes API.** The server talks to the in-cluster API
  server using `KUBERNETES_SERVICE_HOST`/`KUBERNETES_SERVICE_PORT`, verifies
  the server certificate against the projected CA bundle, and re-reads the
  service account token for every request.
- **Fixed operations only.** The client exposes one named Secret, one named
  Deployment, and its scale subresource. There are no generic resource paths
  and no exec or RBAC management.
- **Redacted reads.** `GET /api/config` returns API key names and set/unset
  status, never values. SSH public keys and `known_hosts` are returned because
  they are not secrets.
- **Optimistic concurrency.** Config writes are JSON merge patches carrying the
  `resourceVersion` the browser loaded. A concurrent write produces `409` and
  the response is only a success after the API server stored the patch.
- **Mutation hardening.** Portal responses set CSP (`default-src 'none'`),
  `Cache-Control: no-store`, `nosniff`, and frame denial. Mutation requests
  with an `Origin` header must match `PORTAL_ORIGIN`; without an Origin header
  (curl, tests) the bearer token alone authorizes. Cookies are never used.
  Request bodies are limited to 1 MiB, writes are validated and bounded, and
  upstream Kubernetes errors are logged, never echoed to the browser.
- **No private keys.** The API rejects SSH private key material and only
  accepts public keys parsed with `golang.org/x/crypto/ssh`. `known_hosts` is
  stored as text and is never executed or interpolated into a shell.

## Configuration

| Environment variable | Required | Description |
| --- | --- | --- |
| `POD_NAMESPACE` | yes | Namespace of the pi-pocket agent (downward API). |
| `POCKET_DEPLOYMENT` | yes | Name of the pi-pocket Deployment to start/stop/restart. |
| `CONFIG_SECRET` | yes | Name of the existing runtime Secret the chart created. |
| `POCKET_URL` | yes | Public `https` URL of the agent, shown as a link in the SPA. |
| `PORTAL_ORIGIN` | yes | `https` origin of the portal; mutations must match it. |
| `PORTAL_TOKEN_FILE` | no | Token file path, default `/run/portal/token`. |

`PORTAL_ORIGIN` is canonicalized (lower-case host, default `:443` removed) so
browser `Origin` headers match exactly.

## Runtime Secret contract

Only these keys are ever read or patched; any other keys in the Secret are
left untouched:

| Key | Format |
| --- | --- |
| `api-keys.json` | JSON object mapping allowed environment names to values. |
| `authorized_keys` | OpenSSH public keys, one per line. Comments and blank lines allowed. |
| `known_hosts` | Free-form `known_hosts` text. |

Allowed API key names: `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`,
`GEMINI_API_KEY`, `GOOGLE_API_KEY`, `GROQ_API_KEY`, `OPENROUTER_API_KEY`,
`MISTRAL_API_KEY`, `DEEPSEEK_API_KEY`, `XAI_API_KEY`, `AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `AWS_REGION`.

Names outside this list are rejected so the portal cannot be used as a generic
environment injector. Unknown names already present in `api-keys.json` are
preserved on write.

## HTTP API

| Method | Path | Auth | Purpose |
| --- | --- | --- | --- |
| `GET` | `/healthz` | none | Liveness probe. |
| `GET` | `/` | none | Embedded SPA. |
| `GET` | `/api/config` | bearer | Redacted config: `resourceVersion`, `allowedApiKeys`, `apiKeys` (name → set), `authorizedKeys`, `knownHosts`, `pocketUrl`, `ownerLoginUrl` (empty until the agent syncs it). |
| `POST` | `/api/config` | bearer | Partial update, see below. |
| `GET` | `/api/status` | bearer | Deployment replica status and last restart time. |
| `POST` | `/api/deployment/start` | bearer | Scale the Deployment to 1 replica. |
| `POST` | `/api/deployment/stop` | bearer | Scale the Deployment to 0 replicas. |
| `POST` | `/api/deployment/restart` | bearer | Patch the pod template `kubectl.kubernetes.io/restartedAt` annotation. |

`POST /api/config` body (all fields optional except `resourceVersion`, at
least one change required):

```json
{
  "resourceVersion": "12345",
  "apiKeys": {
    "set": { "GROQ_API_KEY": "gsk-..." },
    "remove": ["OPENAI_API_KEY"]
  },
  "authorizedKeys": "ssh-ed25519 AAAAC3... user@example.com\n",
  "knownHosts": "pocket.example.com ssh-ed25519 AAAAC3...\n"
}
```

Example:

```sh
curl -sf -H "Authorization: Bearer $PORTAL_TOKEN" \
  https://portal.example.com/api/config
```

If `api-keys.json` in the Secret is not valid JSON, the portal refuses reads
and writes with `500` instead of guessing, so existing keys are never dropped.

## Kubernetes RBAC

The Helm chart owns the manifests; the portal expects a namespaced Role with
exactly these rules (and no cluster-scoped permissions):

```yaml
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    resourceNames: ["<CONFIG_SECRET>"]
    verbs: ["get", "patch"]
  - apiGroups: ["apps"]
    resources: ["deployments"]
    resourceNames: ["<POCKET_DEPLOYMENT>"]
    verbs: ["get", "patch"]
  - apiGroups: ["apps"]
    resources: ["deployments/scale"]
    resourceNames: ["<POCKET_DEPLOYMENT>"]
    verbs: ["get", "update"]
```

The chart also mounts the portal token Secret at `/run/portal/token` and sets
the environment variables above, including `POD_NAMESPACE` via the downward
API.

## Access model

Access to the pi-pocket agent itself is governed upstream by pi-pocket
invitations and roles; the portal does not manage users, roles, or invitations.
The agent's service account has namespace admin permissions in this namespace
(requested configuration), so anything running in the agent can read and
modify resources here, including the runtime Secret.

## Build, test, and run

```sh
cd portal
go test -race ./...
go vet ./...
podman build --platform linux/amd64,linux/arm64 -t quay.io/<org>/pi-pocket8s-portal:latest .
```

The Containerfile builds the Go binary for `TARGETOS`/`TARGETARCH` on the
native build platform and runs it from the latest UBI 10 minimal image as
non-root UID 1000. It needs no credentials, package installs, or writable
filesystem.

The server keeps no state on disk and starts with `readOnlyRootFilesystem:
true` (mount an `emptyDir` at `/tmp` if the platform expects one). It is
compatible with `restricted-v3` SCCs, `hostUsers: false`, and the chart's env
contract: `POD_NAMESPACE`, `POCKET_DEPLOYMENT`, `CONFIG_SECRET`,
`PORTAL_TOKEN_FILE=/run/portal/token`, `PORTAL_ORIGIN=https://<host>`,
`POCKET_URL=https://<host>`. Kubernetes probes should use `GET /healthz` on
port 8080 (unauthenticated).

Generate a portal token with `openssl rand -base64 32` (the server requires at
least 32 printable characters).

After saving runtime configuration, press **Restart** to apply it immediately;
projected Secret updates can take up to a minute to appear in the running pod.
