# Pi Pocket image

`images/Containerfile` builds `quay.io/cldmnky/pi-pocket`: upstream
[pi-pocket](https://github.com/TannerMidd/pi-pocket) at pinned commit
`55849dd3769f1cc265404443193c1ab4385aeedb` (v0.11.0) on UBI 10, plus an integrated
developer toolchain. It runs as a fixed non-root user and keeps everything it writes on
the volumes mounted at `/workspace/home` and `/workspace/repos`.

The portal image lives in [`../portal/Containerfile`](../portal/Containerfile) and is
built from the `portal` directory (`quay.io/cldmnky/pi-pocket-portal`).

## Build

Build from the **repository root** (the pi-pocket Containerfile clones upstream itself,
it copies nothing from the context):

```sh
podman build --pull -f images/Containerfile -t quay.io/cldmnky/pi-pocket:dev .

# both architectures, same as CI
podman build --pull --platform linux/amd64,linux/arm64 \
    -f images/Containerfile -t quay.io/cldmnky/pi-pocket:dev .
```

The build needs network access to the Red Hat UBI repositories, GitHub, the npm
registry, `dl.k8s.io`, `get.helm.sh`, and `mirror.openshift.com`. Every downloaded tool is
verified against the vendor's published SHA-256; RPMS are GPG-verified by `dnf`.

## What is inside

| Component | Source |
| --- | --- |
| Base | `registry.access.redhat.com/ubi10/ubi:latest` (multi-arch, RHEL 10) |
| Pi Pocket | upstream commit `55849dd…`, cloned and `git rev-parse`-verified in the build, `npm ci` from the committed lockfile (with dev dependencies) into `/opt/pi-pocket` |
| Node | RHEL `nodejs24` (Node 24 + npm 11), exposed as `node`/`npm`/`npx` in `/usr/local/bin`; `node:sqlite` and `jiti` are runtime requirements |
| Go | RHEL `golang` |
| Python | RHEL `python3`, `python3-pip`, `python3-devel`, `venv` |
| C/C++ | `gcc`, `gcc-c++`, `make`, `cmake`, `pkgconf`, `patch` |
| SCM/SSH | `git`, `openssh`, `openssh-clients`, `known_hosts`/`authorized_keys` from the runtime secret |
| Containers | `podman`, `buildah`, `skopeo`, with `/etc/containers/storage.conf` defaulting to the `vfs` driver (no `/dev/fuse`, no host devices) |
| Kubernetes | pinned `kubectl` v1.35.6 (matches the 1.35 cluster; newer clients break skew policy), `oc` 4.22.17, `helm` v4.3.0, each checksum-verified |
| GitHub | `gh` CLI v2.102.0 (checksum-verified) at `/usr/local/libexec/gh`; `/usr/local/bin/gh` is a wrapper (`images/github-credentials.mjs`) that fetches a short-lived, repository-scoped GitHub App installation token from the credential broker per command, and doubles as a Git credential helper (`gh --git`) for `https://github.com` — tokens are never stored on disk or put in remote URLs. Active when `GITHUB_BROKER_URL` is set (the chart sets it in GitHub App mode) |
| Browser | Chrome-for-Testing `chrome-headless-shell` (pinned Stable, both arches) at `/usr/local/bin/chromium`; `PI_POCKET_BROWSER`/`PI_POCKET_BROWSER_ARGS=--no-sandbox` are preset, `--no-sandbox` is required inside user namespaces |
| Everyday tools | `ripgrep` (v15.2.0, checksum-verified), `jq`, `curl`, `tar`, `unzip`, `xz`, `rsync`, `procps-ng` (`ps`, `top`), `vim`, `less`, `file`, `diffutils` |
| Log safety | `images/log-filter.mjs` (`pi-pocket-log-filter`): runs the launcher, redacts the owner sign-in token and QR block from the container log, forwards signals, passes errors through |

## Runtime contract

The entrypoint (`images/entrypoint.sh`) prepares the workspace, loads configuration, and
then `exec`s the upstream launcher as PID 1:

```
node /opt/pi-pocket/bin/pi-pocket.js \
    --host 0.0.0.0 --port 8787 --cwd /workspace/repos --data /workspace/home/.pi-pocket
```

- **User**: fixed UID/GID `1000` (`pocket`), `HOME=/workspace/home`.
- **Port**: `8787` (`PI_POCKET_PORT` can override).
- **Working directory**: `/workspace/repos`; Pi Pocket uses it for new sessions.
- **Data**: `/workspace/home/.pi-pocket` (`pocket.sqlite`, `config.json`, uploads, push keys).
- **Signals**: the launcher is PID 1, so `SIGTERM` stops running work cleanly.
- **Noninteractive**: the container has no TTY, so `--host 0.0.0.0` selects the LAN
  (direct) access mode and no menu is shown. The launcher's sign-in link and QR code are
  redacted from the container log (see "Owner sign-in token").

### Volumes

| Path | Purpose | Notes |
| --- | --- | --- |
| `/workspace/home` | persistent home: `.pi`, `.pi-pocket` (data), `.ssh`, `.kube`, caches (`go`, `pip`, `npm`) | PVC; make it writable for UID 1000 |
| `/workspace/repos` | working trees for Pi sessions | PVC or `emptyDir` |
| `/tmp` | scratch space for builds and tools | writable `emptyDir` |
| `/run/pocket-config` | runtime secret (below) | read-only secret mount, **whole directory, never `subPath`** |

### Portal iframe embedding

The portal embeds the agent in an `iframe`. Two upstream defaults forbid that,
so the entrypoint applies `images/embed-patch.sh` when `POCKET_FRAME_ANCESTORS`
names the portal's https origin (the chart sets it from the portal ingress):

- `frame-ancestors 'self'` in `src/server/http/assets.ts` gains the portal origin;
- `SameSite=Lax` in `src/server/auth.ts` becomes `SameSite=None`, without which
  the session cookie is withheld inside the cross-site frame and logins never stick.

The patch is fail-closed: unless each pattern is found exactly as often as the
pinned upstream commit has it (once / twice), nothing is modified and boot
continues without embedding. `Secure` cookies still require HTTPS.

### Web terminal daemon

`/usr/local/bin/pi-terminal` (built from `terminal/` into the image) serves
the ghostty-web frontend and one-PTY-per-WebSocket shells on port 8081
(`TERMINAL_PORT`). The entrypoint starts it next to the launcher, supervises
both, and lets the launcher own the container exit code; a crashed daemon is
restarted after 5 s. Authentication re-reads the owner token from
`~/.pi-pocket/config.json` on every check, so rotation needs no restart.
`TERMINAL_FRAME_ANCESTORS` (chart: portal origin) controls which origin may
embed the page; empty denies all framing.

### Owner sign-in token

The launcher writes the owner's sign-in link to its output; that token is the owner's key,
so the entrypoint runs the launcher through `pi-pocket-log-filter`, which replaces the
token in `/login?token=…` with `[redacted]` and hides the QR block (which encodes the same
link). Errors and every other line pass through unchanged.

The token itself stays in the data directory, `~/.pi-pocket/config.json` (mode 0600).
Operators read it there, for example:

```sh
oc exec deploy/pi-pocket -- cat /workspace/home/.pi-pocket/config.json
```

Keep that output private: do not paste the token into tickets, logs, or CI output.

## Pod security profiles

The chart runs two profiles; the image supports both.

### `nested-container` (default in the chart: rootless Podman)

`hostUsers: false`, UID/GID 1000, capabilities `SETUID`/`SETGID`,
`allowPrivilegeEscalation: true`, `procMount: Unmasked`, SELinux label
`container_engine_t`, and the runtime-default seccomp profile. The image is prepared for
this profile:

- `/etc/containers/storage.conf` uses driver `vfs`, so nested containers need neither
  `/dev/fuse` nor host devices;
- `/etc/subuid` and `/etc/subgid` give `pocket` the range `10000:50000`, entirely inside
  the pod's outer 65536-UID mapping (`hostUsers: false`), which is what the helpers can
  map;
- `/usr/bin/newuidmap` and `/usr/bin/newgidmap` carry file capabilities
  (`cap_setuid=ep` / `cap_setgid=ep`); the build falls back to `chmod u+s` and fails if
  neither is present;
- no rootless network helper can create a tap inside the pod (`pasta` needs
  `/dev/net/tun`), so nested containers use the outer pod's network namespace:

  ```sh
  podman run --network=host --rm quay.io/example/image:tag <command>
  ```

  `vfs` is slower and copies more than overlay, but it is the only driver that needs
  neither `/dev/fuse` nor host mounts. Where fuse is available, override it with
  `STORAGE_DRIVER=overlay` (with `fuse-overlayfs` installed).

With those pieces in place `podman info`, `podman pull`, `podman build`, and
`podman run` work inside the pod to the extent the profile lets a nested user namespace
mount its filesystems; if a cluster additionally blocks userns mounts, that is a profile
setting (mounts/`procMount`/SELinux), not an image one.

### `restricted-v3`

Hardened defaults: no user namespaces, no privilege escalation, capabilities dropped.

```yaml
securityContext:
  runAsNonRoot: true
  runAsUser: 1000
  runAsGroup: 1000
  fsGroup: 1000            # so the PVC is writable by UID 1000
  allowPrivilegeEscalation: false
  capabilities:
    drop: ["ALL"]
  seccompProfile:
    type: RuntimeDefault
```

Rootless Podman/Buildah cannot run under `restricted-v3`; `skopeo` still inspects,
copies, and syncs images. `readOnlyRootFilesystem: true` works in either profile as long
as `/tmp`, `/workspace/home`, and `/workspace/repos` are writable. The image itself needs
no host mounts and no host devices for the app to run.

Never work around the profiles with `privileged: true`, extra host devices, or host mounts.

## Runtime secret: `/run/pocket-config`

Mount the secret (or projected secret) as a whole directory. `subPath` mounts do not
receive updates and are not supported.

The entrypoint also publishes the owner sign-in link (`${POCKET_PUBLIC_URL}/login?token=…`)
into the `owner-login-url` entry of the runtime secret (needs `RUNTIME_SECRET`,
`POCKET_PUBLIC_URL` and a mounted service account; skipped for local runs). The
portal shows it and its QR code to authenticated operators. Provider key values
are never written to the secret — only this one URL.

| File | Handling |
| --- | --- |
| `api-keys.json` | flat JSON object of provider keys; the entrypoint exports only the allowlisted names below and ignores everything else. Changes need a **pod restart** because the environment is read at start; the portal asks for that restart. |
| `authorized_keys` | copied to `~/.ssh/authorized_keys` (0600) as configuration for tooling |
| `known_hosts` | copied to `~/.ssh/known_hosts` (0600) |
| `id_ed25519` | optional outbound SSH key, copied to `~/.ssh/id_ed25519` (0600) |

When `GITHUB_BROKER_URL` is set, personal `GH_TOKEN`/`GITHUB_TOKEN` are dropped and Git operations at `https://github.com` resolve credentials through `/usr/local/bin/gh --git` (a `git credential` helper speaking the broker protocol). The wrapper refuses non-github.com hosts, absolute `gh api` URLs, and repositories outside the broker's allow-list; `useHttpPath` keeps per-repo scoping exact.

Allowlisted `api-keys.json` names: `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`,
`GEMINI_API_KEY`, `GOOGLE_API_KEY`, `GROQ_API_KEY`, `OPENROUTER_API_KEY`,
`MISTRAL_API_KEY`, `DEEPSEEK_API_KEY`, `XAI_API_KEY`, `AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `AWS_REGION`.

A missing or invalid `api-keys.json` is logged and ignored; the container still starts.
**No sshd runs in this image** — `authorized_keys` is exposed as a file only.

## Kubernetes access

When the service account is mounted, the entrypoint writes `~/.kube/config` and exports
`KUBECONFIG`. The config names the pod it belongs to, using the downward API
(`POD_NAMESPACE`, `POD_NAME`, `POD_UID`), and points at the mounted files instead of
copying them:

- `tokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token` — read per request,
  so rotated projected tokens are used without a restart (no token snapshot).
- `certificate-authority: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt`
- The API server URL comes from `KUBERNETES_SERVICE_HOST`/`_PORT`, falling back to
  `https://kubernetes.default.svc`.

Without a mounted service account the kubeconfig is skipped and the container starts
normally.

## CI and tags

`.github/workflows/images.yml` builds both images for `linux/amd64` and `linux/arm64`:

- **pull requests**: build only, no push; the amd64 pi-pocket image is loaded and smoke
  tested (server answers on `:8787`, workspace prepared, `node:sqlite`/`jiti` load,
  `rg`/`kubectl`/`helm`/`oc` run, the owner token never appears in the log, and `vfs`
  storage plus the nested-container helpers are in place).
- **main, tags, manual, scheduled**: each architecture is pushed to
  `quay.io/cldmnky/<image>` under `sha-<commit>-<arch>`, then merged into one
  manifest list tagged `sha-<commit>` (immutable); `latest` is added on `main`
  only. Scheduled runs pick up a newer UBI 10 base and RHEL packages.
- Registry credentials: repository secrets `QUAY_USERNAME` and `QUAY_TOKEN`.

## Local smoke test

```sh
podman build --pull -f images/Containerfile -t local/pi-pocket:test .
podman volume create smoke-home
podman volume create smoke-repos
podman run --rm -d --name smoke -p 8787:8787 \
    -v smoke-home:/workspace/home -v smoke-repos:/workspace/repos local/pi-pocket:test
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8787/
podman exec smoke ls -la /workspace/home/.pi /workspace/home/.pi-pocket
podman stop smoke && podman volume rm smoke-home smoke-repos
```

To exercise the secret and kubeconfig paths locally, mount a directory with
`api-keys.json`, `known_hosts`, `authorized_keys`, and `id_ed25519` at
`/run/pocket-config`, and a service account directory at
`/var/run/secrets/kubernetes.io/serviceaccount` (containing `token` and `ca.crt`).
