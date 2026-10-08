# pi-pocket on OpenShift

A persistent [pi-pocket](https://github.com/TannerMidd/pi-pocket) coding workspace, a UBI RHEL 10 developer image, and a separate Go configuration portal. Both applications are exposed through Kubernetes **Ingress** resources; OpenShift converts them to HTTPS routes.

## Architecture

- `quay.io/cldmnky/pi-pocket`: upstream pi-pocket plus integrated developer tools. The base is `registry.access.redhat.com/ubi10/ubi:latest`, refreshed on image builds. Upstream source is pinned, not silently upgraded.
- `quay.io/cldmnky/pi-pocket-portal`: Go HTTP server with an embedded, dependency-free SPA, using a separate service account.
- `charts/pi-pocket`: one pi-pocket replica with `Recreate` updates, a retained PVC mounted at `/workspace`, namespace RBAC, user namespaces, services, ingresses and ingress NetworkPolicy.
- `/workspace/repos`: repositories. `/workspace/home`: persistent home, pi-pocket SQLite/settings, Pi provider logins, caches and SSH configuration.
- Portal configuration is stored in a named Kubernetes Secret and mounted into the pocket pod. Provider keys are loaded on startup; save configuration, then restart. No credentials belong in Git or Helm values.

The cluster profile in `deploy/openshift-values.yaml` targets OpenShift **4.22.8**, Kubernetes **1.35.6**, ingress domain `apps.voyager.blahonga.me`, and **LVMS** `lvms-vg1` storage. Use your own values for another cluster.

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

- Pi-pocket: **https://pocket.apps.voyager.blahonga.me**
- Portal: **https://pocket-portal.apps.voyager.blahonga.me**

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
  'import{readFileSync}from"node:fs";const c=JSON.parse(readFileSync(process.env.PI_POCKET_DIR+"/config.json","utf8"));console.log("https://pocket.apps.voyager.blahonga.me/login?token="+encodeURIComponent(c.ownerToken))'
```

Treat that output like a password. Sign in once, then invite trusted collaborators through upstream pi-pocket. Do not scrape logs into a public dashboard: upstream may log sensitive session information. Provider API keys can be configured in the portal; upstream Pi provider OAuth logins can also be configured interactively and persist in the home directory.

### SSH

Public keys from the portal are exposed as `~/.ssh/authorized_keys` for tools/configuration. **No SSH server is enabled**, and HTTP Ingress cannot provide SSH transport. For private Git repositories, public keys alone are not authentication: use agent credentials, a Git-provider token, or separately provision an `id_ed25519` private key in the runtime Secret. The portal intentionally refuses private keys. Set `known_hosts` using host keys whose fingerprints you verified independently, not blind trust-on-first-use. SSH strict host-key checking is enabled by the runtime image.

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
