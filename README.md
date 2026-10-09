# pi-pocket on OpenShift

A persistent [pi-pocket](https://github.com/TannerMidd/pi-pocket) coding workspace, a UBI RHEL 10 developer image, and a separate Go configuration portal. Both applications are exposed through Kubernetes **Ingress** resources; OpenShift converts them to HTTPS routes.

## Architecture

- `quay.io/cldmnky/pi-pocket`: upstream pi-pocket plus integrated developer tools. The base is `registry.access.redhat.com/ubi10/ubi:latest`, refreshed on image builds. Upstream source is pinned, not silently upgraded.
- `quay.io/cldmnky/pi-pocket-portal`: Go HTTP server with an embedded, dependency-free SPA, using a separate service account.
- `charts/pi-pocket`: one pi-pocket replica with `Recreate` updates, a retained PVC mounted at `/workspace`, namespace RBAC, user namespaces, services, ingresses and ingress NetworkPolicy.
- `/workspace/repos`: repositories. `/workspace/home`: persistent home, pi-pocket SQLite/settings, Pi provider logins, caches and SSH configuration.
- Portal configuration is stored in a named Kubernetes Secret and mounted into the pocket pod. Provider keys are loaded on startup; save configuration, then restart. No credentials belong in Git or Helm values.

The cluster profile in `deploy/openshift-values.yaml` is an example: set your own OpenShift version-appropriate values, ingress domain and hosts, and an idmap-capable storage class. Never use NFS-backed storage for these user-namespace pods.

## Security: read before granting access

**Anyone who can steer pi-pocket can execute code in this workspace and exercise its namespace-level `admin` service-account rights.** This includes reading namespace Secrets, including portal credentials, and modifying other workloads. This is a trusted-user workstation, not a hostile multi-tenant sandbox. Install into a dedicated namespace, never a shared application namespace. View-only upstream access is not equivalent to steering access.

Every application pod sets `hostUsers: false`: UID 1000 inside the pod is mapped to an unprivileged host UID. This reduces host exposure; it does **not** limit Kubernetes RBAC, provider-key access, or arbitrary code execution within the namespace.

The default pocket profile uses OpenShift's built-in `nested-container` SCC to support rootless Podman: limited SETUID/SETGID capabilities, privilege escalation for UID-map helpers, and unmasked `/proc`, **within the pod user namespace**. No privileged containers, host paths, host PID/network namespaces, container-runtime sockets, or host device mounts are used. The SCC supplies SELinux `container_engine_t` and the namespace MCS label.

The pocket pod defaults to an `Unconfined` seccomp profile under `nested-container`: the default RuntimeDefault filter blocks the nested user namespaces rootless Podman needs (verified live). Override with `podSeccomp` if your policy demands it (nested Podman then stops working). Ingress TLS uses the router default certificate unless `ingress.pocketTLSSecret`/`portalTLSSecret` name explicit Secrets.

For stricter operation without nested container execution:

```bash
helm upgrade pi-pocket charts/pi-pocket -n pi-pocket \
  -f deploy/openshift-values.yaml --set openshift.pocketSCC=restricted-v3
```

The portal always uses `restricted-v3`, a read-only root filesystem, no extra capabilities, and a Role restricted to the pocket Deployment and runtime Secret. Its token is a separate administrative credential, not a pi-pocket invite. The SPA keeps the entered token in memory only; keys are never returned by the API. Use HTTPS, a trusted device, and an appropriately secured cluster/etcd.

## Optional: GitHub App authentication for the portal

By default the portal asks for a shared portal token. When `portal.github.clientID` is set (or `portal.authMode=github`), the portal switches to **GitHub App sign-in** instead, and token login is disabled entirely: the SPA replaces the token form with "Sign in with GitHub", and API bearer tokens are rejected.

What you get:

- **Who signs in**: only active members of a GitHub organization, or of one team (`portal.github.team`). Membership is verified server-side against `GET /orgs/{org}/teams/{team}/memberships/{user}` at login and re-checked at least every 5 minutes; removed members lose access without waiting for session expiry. Login uses the GitHub App web flow with `state` plus PKCE (S256). GitHub user tokens are used only to identify the user and are then discarded; sessions are opaque random cookies (`__Host-`, Secure, HttpOnly), kept in portal memory only (the portal runs with `Recreate`, so upgrades sign everyone out).
- **What the agent gets**: a separate **GitHub App bot identity**. The portal mints one-hour **installation tokens** scoped to one explicitly allow-listed repository per request, with `contents: write`, `pull_requests: write`, `actions: write` — enough for clone/push, PRs, and triggering/inspecting workflow runs. `workflows` and `administration` permissions are deliberately never granted, so workflow files under `.github/workflows` cannot be edited through the API (note: the agent could still edit build scripts that workflows execute; this is not a CI sandbox).
- **How the agent authenticates**: the workspace pod never sees the App private key. It projects its own service-account token with the dedicated `pi-pocket-github` audience and calls the portal's broker endpoint, which verifies it with a `TokenReview` (accepting only the configured workspace namespace/service account) and returns a bot token. In the image, `gh` is a wrapper that fetches a fresh token per command, and a Git credential helper does the same for HTTPS clones/pushes — tokens are never stored on disk or embedded in remote URLs, and they survive neither pod restarts nor lingering in configuration. The App private key and client secret stay in the portal's own secret.
- **Namespace boundary (required)**: GitHub mode requires `portal.namespace` — a **separate, pre-created management namespace**. The workspace namespace's `admin` service account could read any Secret in its own namespace, so the App key must not live there. The chart creates the portal's ServiceAccount in the management namespace, binds its existing namespace-scoped Role in the workspace namespace to that identity, and grants only `tokenreviews create` (cluster-scoped, reveals nothing by itself). The chart will refuse to render GitHub mode with `portal.namespace` equal to the release namespace.

Setup:

1. Register a **GitHub App** (organization-owned). Homepage `https://<portalHost>`, callback `https://<portalHost>/auth/github/callback` (exact match), uncheck webhook (none are used), enable "Request user authorization (OAuth) during installation" if you want per-user authorization prompts.
2. Repository permissions: Contents **Read and write**, Pull requests **Read and write**, Actions **Read and write**. Organization permission: Members **Read-only** (for team checks). Nothing else.
3. Install the App on the login organization. Note its **installation ID** (`GET /orgs/{org}/installation` with a JWT, or from the App settings page URL). This installation verifies membership and serves allow-listed repositories owned by that organization. Additional repository owners can use separate installations of the same App (see below).
4. Create the App credentials Secret in the management namespace: `client-secret` (the App's client secret) and `private-key.pem` (a generated App private key, RS256).
5. Create the management namespace, then install/upgrade with:

```bash
helm upgrade --install pi-pocket charts/pi-pocket -n workspace -f deploy/openshift-values.yaml \
  --set portal.namespace=management \
  --set portal.github.clientID=Iv1.xxxxxxxx \
  --set portal.github.appID=123456 \
  --set portal.github.installationID=789012 \
  --set portal.github.organization=your-org \
  --set portal.github.team=optional-team \
  --set 'portal.github.repositories[0]=your-org/one-repo' \
  --set portal.github.existingSecret=pi-pocket-github-app
```

The first sign-in must be performed by an organization owner (to approve the App authorization if prompted). Every subsequent sign-in requires active team/organization membership. Because anyone admitted can still steer the shared agent and its namespace-admin rights, only invite teams you would trust at the workspace keyboard; this does not isolate users from each other. Non-GitHub prerequisites (provider keys, SSH keys, Quay) are unchanged.

### Personal repositories with organization login

The portal login gate and bot repository ownership can be different. For example,
keep `portal.github.organization=blahonga` and its `installationID` for membership
checks, while allowing selected `cldmnky` repositories:

1. Install the **same GitHub App** on the `cldmnky` personal account, granting
   access only to the intended repositories. An organization-owned private App
   can only be installed on its owning organization; make it public before
   installing it on another account. Public visibility does not automatically
   grant access to repositories or bypass the portal's membership checks.
2. Find the personal account's installation ID (`GET /users/cldmnky/installation`
   with an App JWT, or the installation settings URL).
3. Add this to your existing deployment values, substituting the actual numeric
   personal installation ID. Leave the login organization's `installationID`,
   `organization`, and optional `team` unchanged:

   ```yaml
   portal:
     github:
       repositoryInstallations:
         cldmnky: 12345678 # example only: installation on the personal account
       repositories:
         - cldmnky/pi-pocket8s
         # Include any existing organization repositories you still need.
   ```

`repositoryInstallations` maps account owners to installation IDs; it does not
grant all repositories belonging to those owners. Every repository still needs
an explicit `owner/name` entry in `repositories` and access in the App's
installation settings. Before first use, the broker validates each installation's
account and rejects suspended or mismatched installations. With an
empty mapping, existing organization-only configurations behave as before.

**All trusted users who can steer the shared workspace can exercise these
personal-repository bot permissions**, not just the personal account owner.
Login does not delegate a user's token or restrict repository access per user.
In the workspace, use `gh repo clone cldmnky/pi-pocket8s` or
`gh pr list -R cldmnky/pi-pocket8s`; each command obtains a one-repository App bot token.

## Build and publish locally

A Quay account able to create repositories in `cldmnky` is required for the first push. Afterward grant the CI robot write access to **both** repositories.

```bash
podman login quay.io
# Build both architectures; the host must support cross-architecture execution.
podman build --pull=always --platform linux/amd64,linux/arm64 \
  --manifest quay.io/cldmnky/pi-pocket:latest -f images/Containerfile .
podman manifest push --all quay.io/cldmnky/pi-pocket:latest \
  docker://quay.io/cldmnky/pi-pocket:latest
podman build --pull=always --platform linux/amd64,linux/arm64 \
  --manifest quay.io/cldmnky/pi-pocket-portal:latest -f portal/Containerfile portal
podman manifest push --all quay.io/cldmnky/pi-pocket-portal:latest \
  docker://quay.io/cldmnky/pi-pocket-portal:latest
```

Prefer immutable release/build tags for deployed workloads; `latest` is the initial bootstrap default. Ensure Quay repositories are publicly readable, or create a separate pull-robot `kubernetes.io/dockerconfigjson` Secret and set `imagePullSecrets: [{name: quay-pull}]`. Do not give the workload the Actions push credential.

## GitHub Actions

`.github/workflows/images.yml` builds `linux/amd64` and `linux/arm64`, pushing both image repositories to Quay. Set these **repository secrets**:

- `QUAY_USERNAME`: robot name, e.g. `cldmnky+pi_pocket_builder`.
- `QUAY_TOKEN`: robot token (not a password), with write permission on both repositories.

Pull requests build but never authenticate/push. Main/release/manual/maintenance builds refresh the UBI base. The separate validation workflow runs Go tests/vet, image entrypoint tests and Helm rendering assertions. Workflow files must be committed/pushed to GitHub before Actions can run; creating secrets alone does not publish local code.

## Install

Verify the active context before changing the cluster:

```bash
oc whoami
kubectl config current-context
oc get scc nested-container restricted-v3
oc get storageclass ingressclass
helm upgrade --install pi-pocket charts/pi-pocket \
  --namespace pi-pocket --create-namespace \
  -f deploy/openshift-values.yaml --wait --timeout 10m
```

The installer needs permission to bind the requested namespace role and built-in SCC roles. The chart grants **no cluster-admin role** and does not modify default SCCs or cluster feature gates. An existing cluster must support user namespaces, idmapped mounts, and the chosen storage driver/filesystem. Do not select NFS for this chart. Set `persistence.existingClaim` to reuse a compatible PVC.

URLs for the included cluster profile:

- Pi-pocket: **https://`ingress.pocketHost`** (e.g. `https://pocket.apps.example.com`)
- Portal: **https://`ingress.portalHost`** (e.g. `https://pocket-portal.apps.example.com`)

The router's default certificate is used when no ingress TLS Secret is specified. Other ingress controllers need an appropriate class, certificate Secrets and ingress namespace selector in the NetworkPolicy. Do not use `curl -k` or bypass certificate validation in normal operation.

### Open the portal

Retrieve the token locally (do not paste it into issue reports, chat, URLs, or shell history):

```bash
oc get secret pi-pocket-portal-token -n pi-pocket \
  -o jsonpath='{.data.token}' | base64 --decode; echo
```

Enter it into the portal password field. The Workspace tab embeds the agent itself (with full-screen mode) in pi-pocket's own dark styling, and shows the owner sign-in link with a QR code once the agent pod has synced it. Configure provider keys, SSH public keys and verified SSH `known_hosts`, then save and restart the workspace. The portal can start/stop/restart the one workspace; it cannot change generic Kubernetes resources or grant user roles.

**Access rights:** use pi-pocket's owner invitations, steer/view roles and session scopes in its People menu. Cluster rights are selected separately with `serviceAccount.namespaceRole` (`admin` by default, `edit`, `view`, or empty). The portal does not create another, incompatible upstream identity system.

### First pi-pocket owner sign-in

The owner key is persisted in pi-pocket's private config, independently of the portal token. Obtain the owner sign-in link from a trusted local terminal after the pod is Ready:

```bash
oc exec -n pi-pocket deploy/pi-pocket -- node --input-type=module -e \
  'import{readFileSync}from"node:fs";const c=JSON.parse(readFileSync(process.env.PI_POCKET_DIR+"/config.json","utf8"));console.log(process.env.POCKET_PUBLIC_URL+"/login?token="+encodeURIComponent(c.ownerToken))'
```

Treat that output like a password. Sign in once, then invite trusted collaborators through upstream pi-pocket. Do not scrape logs into a public dashboard: upstream may log sensitive session information. Provider API keys can be configured in the portal; upstream Pi provider OAuth logins can also be configured interactively and persist in the home directory.

### Web terminal

The workspace tab's **Open terminal in new tab** button opens a real shell in
the agent pod, rendered with [ghostty-web](https://github.com/coder/ghostty-web)
(`ghostty-web@0.4.0`, MIT, vendored under `terminal/web/static`). It is served
by a small Go daemon (`terminal/`, built into the image as
`/usr/local/bin/pi-terminal`) that bridges one WebSocket connection to one PTY
shell running as the pocket user in `/workspace/repos`, with the same
environment the agent itself gets (provider keys, `gh`, `kubectl`, namespace
kubeconfig).

This is deliberately not a pi-pocket extension: extensions can add tools,
prompt sections, hooks, and tasks, but offer no web-UI or HTTP-route surface,
and a terminal needs both plus a PTY backend. See `terminal/README.md` for the
protocol, authentication (`?token=` owner check against `config.json`, then an
in-memory `__Host-` session cookie), and environment contract.

The terminal has its own `Ingress` (`ingress.terminalHost`, edge TLS like the
rest) and `Service` (`pi-pocket-terminal:8081`); the pod's NetworkPolicy
already allows the router to reach it. A human typing in the terminal bypasses
Lancet Guard exactly like the owner's own `!`-commands — only the owner token
opens it, and keystrokes are never logged.

### Provider OAuth (browser) logins

Interactive provider logins (e.g. OpenAI with a browser) start a `localhost`
callback listener **inside the pod**, so the provider's redirect to
`http://localhost:<port>/...` cannot reach your machine directly. Two ways
through, both one-time (logins persist in the workspace home):

1. **Port-forward the callback port** (smoothest). The agent prints the login
   URL including its port — OpenAI Codex always uses `1455`. Forward it, then
   complete the login in your local browser; the redirect lands in the pod:

   ```bash
   oc port-forward -n pi-pocket deploy/pi-pocket 1455:1455
   ```

   For providers with ephemeral ports, read the port from the printed URL and
   forward that instead.

2. **Paste the redirect URL.** The login flow also accepts the authorization
   code / redirect URL pasted back into the agent chat. Complete the login in
   your browser, and when it lands on an unreachable `localhost` address,
   copy the full address-bar URL and paste it to the agent.

No tunnel is needed for API-key providers: put the keys in the portal and
restart instead.

### SSH

Public keys from the portal are exposed as `~/.ssh/authorized_keys` for tools/configuration. **No SSH server is enabled**, and HTTP Ingress cannot provide SSH transport. For private Git repositories, public keys alone are not authentication: use agent credentials, a Git-provider token, or separately provision an `id_ed25519` private key in the runtime Secret. The portal intentionally refuses private keys. Set `known_hosts` using host keys whose fingerprints you verified independently, not blind trust-on-first-use. SSH strict host-key checking is enabled by the runtime image.

## Web search

The image ships an agent **web search** tool: `web_search`, a Pi Pocket built-in extension
(`extensions/` in this repository, installed under `/opt/pi-pocket/src/server/extensions/`).
It is on by default and can be turned off — or back on — in Menu → Extensions.

It calls the **search model's own provider API** rather than scraping: Google Gemini grounding,
OpenAI/Codex Responses, xAI Grok, Anthropic, DeepSeek, Ollama Cloud and OpenCode Zen/Go. Which
model does the searching is the install's choice, taken from `~/.pi/agent/web-search.json` in the
workspace home:

```json
{ "provider": "opencode-go", "model": "muse-spark-1.3-contributor" }
```

With no such file the tool picks the highest-ranked available model from an allow-list of
providers it has been verified against. A gateway (OpenRouter, for one) is never picked on its own:
it serves many providers behind one endpoint, and whether it implements a given provider's search
tool is per-gateway. Naming one explicitly works — OpenRouter's Anthropic-shaped models were
verified to run the search tool — it is just not a safe default.

There is no separate engine switch: **the provider of the chosen model runs the search**, in its own
way (Google Search grounding for Gemini, the Responses `web_search` tool for OpenAI/Codex, xAI's
search for Grok, Anthropic's tool, Ollama Cloud's search API). Picking the provider picks the
backend. Every result also reports which model answered and whether search results actually came
back, because a search-capable model may decide *not* to search — and that should not look like a
success.

Costs and access: a search is a **billable model call on the install's own provider credentials**
(the same `auth.json` the agent uses), and the result text is then sent to the model the
conversation uses. Nothing is fetched from a search provider that the install has no credentials
for, and no search runs unless the agent calls the tool. Details, verification and the vendored
upstream source: [`extensions/web-search/`](extensions/web-search/README.md).

## Development tools and cluster access

See `images/README.md` for exact bundled versions, package sources and nested Podman limitations. The image integrates tools rather than mounting another image: an image-volume containing `/usr/bin` cannot supply its dependencies, interpreter paths and dynamic libraries safely by simply extending `PATH`.

The Downward API supplies pod name, UID and namespace. The runtime writes a namespace-scoped kubeconfig using the in-cluster API CA and **tokenFile** referencing the projected service-account token, so `oc`/`kubectl` can follow token rotation. It never copies your host kubeconfig or cluster-admin identity.

Nested Podman uses a VFS storage driver to avoid FUSE device access. Buildah uses chroot isolation. Do not assume all Docker/Podman networking or kernel features work inside a pod; validate required builds in the deployed environment. Nested containers share workspace trust and are not a separate tenant boundary.

## Persistence, upgrades and backup

One server process owns SQLite. Do not scale above one; the chart rejects it. `Recreate` and a 60-second termination grace period avoid rolling-update database overlap. A portal Stop leaves the PVC and credentials intact. Helm upgrades can reset replicas to the configured value; set `replicas=0` explicitly to keep it stopped during an upgrade.

The runtime Secret and portal token use Helm `lookup` to preserve portal edits and token values across connected upgrades. An **offline `helm template`/GitOps render cannot discover existing credentials** and emits fresh defaults: use `runtimeSecret.existingSecret` and `portal.tokenSecret` with externally managed Secrets for GitOps. Do not apply offline renders over a live installation's generated Secrets.

PVC and generated credential Secrets have `helm.sh/resource-policy: keep`. Uninstall retains them intentionally. Reinstallation can adopt retained Helm-owned resources for the same release; alternatively set `persistence.existingClaim`, `runtimeSecret.existingSecret` and `portal.tokenSecret`. Deleting the namespace still deletes its resources; the LVMS StorageClass has a Delete reclaim policy. Back up the workspace outside the namespace. Stop the application before filesystem-level copies, or use a SQLite-consistent backup/snapshot procedure.

To rotate portal access, replace the token Secret value with a cryptographically strong random token; reload the portal after the projected Secret update. Rotate pi-pocket's owner token using the upstream launcher `--rotate-token` operation, with the original database offline and the same persistent data directory. Rotating an owner token does not remove all existing invited users; remove users in the People menu as needed.

## Validate

```bash
python3 -m venv .venv
.venv/bin/pip install -r requirements-dev.txt
make test PYTHON=.venv/bin/python
make lint
make dry-run                 # server-side schema validation; namespace must exist
oc get pods,pvc,ingress,route -n pi-pocket
oc exec -n pi-pocket deploy/pi-pocket -- cat /proc/self/uid_map
oc exec -n pi-pocket deploy/pi-pocket -- oc auth can-i get secrets
```

Server-side dry-run of Deployments checks API schemas, not actual pod scheduling/mount support. See [research and validation notes](docs/research.md) for the live user-namespace/PVC admission probe and source references.
