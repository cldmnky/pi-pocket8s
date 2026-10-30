# Pi Pocket image

`images/Containerfile` builds `quay.io/cldmnky/pi-pocket`: upstream
[pi-pocket](https://github.com/TannerMidd/pi-pocket) at pinned commit
`5e13a8b161226cbfef3e696c1bd31b8da110e381` (v0.26.0) on UBI 10, plus an integrated
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
verified against a SHA-256 — the vendor's published one where the release publishes one,
otherwise a per-asset digest pinned in `images/Containerfile` — and RPMs are GPG-verified
by `dnf`.

## What is inside

| Component | Source |
| --- | --- |
| Base | `registry.access.redhat.com/ubi10/ubi:latest` (multi-arch, RHEL 10) |
| Pi Pocket | upstream commit `5e13a8b…`, cloned and `git rev-parse`-verified in the build, its `package.json` version checked against `PI_POCKET_VERSION`, `npm ci` from the committed lockfile (with dev dependencies) into `/opt/pi-pocket`. Its six direct `@earendil-works/*` packages are 1.4.0, and the extension API it loads is 1 to 2 |
| Node | RHEL `nodejs24` (Node 24 + npm 11), exposed as `node`/`npm`/`npx` in `/usr/local/bin`; `node:sqlite` and `jiti` are runtime requirements |
| Go | RHEL `golang` |
| Python | RHEL `python3`, `python3-pip`, `python3-devel`, `venv` |
| C/C++ | `gcc`, `gcc-c++`, `make`, `cmake`, `pkgconf`, `patch` |
| Audio | `libsndfile` (WAV, FLAC, Ogg/Vorbis, Opus, AIFF and the rest, plus the C API) — what Python's `soundfile`, `librosa`, `sox` front ends, and most audio tooling link against. Installed because a workspace that processes audio files should not have to build it first |
| SCM/SSH | `git`, `openssh`, `openssh-clients`, `known_hosts`/`authorized_keys` from the runtime secret |
| Containers | `podman`, `buildah`, `skopeo`, with `/etc/containers/storage.conf` defaulting to the `vfs` driver (no `/dev/fuse`, no host devices) |
| Kubernetes | pinned `kubectl` v1.35.6 (matches the 1.35 cluster; newer clients break skew policy), `oc` 4.22.17, `helm` v4.3.0, each checksum-verified |
| Kubernetes MCP server | `kubernetes-mcp-server` v0.1.1 ([upstream](https://github.com/containers/kubernetes-mcp-server), Apache-2.0) at `/usr/local/bin/kubernetes-mcp-server`, ~87 MB on amd64 and ~82 MB on arm64, statically linked. The release publishes no `checksums.txt`, so the pinned sha256 is the per-asset `digest` from the release API, captured at pin time; the build verifies the download with `sha256sum` and makes no API call. Started only by Pi's MCP client — see "MCP: read-only Kubernetes tools" in the root README |
| GitHub | `gh` CLI v2.102.0 (checksum-verified) at `/usr/local/libexec/gh`; `/usr/local/bin/gh` is a wrapper (`images/github-credentials.mjs`) that fetches a short-lived, repository-scoped GitHub App installation token from the credential broker per command, and doubles as a Git credential helper (`gh --git`) for `https://github.com` — tokens are never stored on disk or put in remote URLs. The token is scoped to one repository, so run `gh` inside a clone whose `origin` is on github.com, or name the repository (`-R owner/name`, `GH_REPO=owner/name`); without one in scope there is nothing to ask the broker for, and it says so. Active when `GITHUB_BROKER_URL` is set (the chart sets it in GitHub App mode) |
| Browser | Chrome-for-Testing `chrome-headless-shell` (pinned Stable, both arches) at `/usr/local/bin/chromium`; `PI_POCKET_BROWSER`/`PI_POCKET_BROWSER_ARGS=--no-sandbox` are preset, `--no-sandbox` is required inside user namespaces |
| Browser automation | `agent-browser` CLI (pinned, pruned to the build arch) driving that Chromium over CDP; `AGENT_BROWSER_EXECUTABLE_PATH`/`AGENT_BROWSER_ARGS=--no-sandbox` are preset (see "Browser automation for agents") |
| Everyday tools | `ripgrep` (v15.2.0, checksum-verified), `jq`, `curl`, `tar`, `unzip`, `xz`, `rsync`, `procps-ng` (`ps`, `top`), `vim`, `less`, `file`, `diffutils` |
| Shell and workflow lint | `shellcheck` 0.11.0 (GPL-3.0-or-later, which the shipped binary is under; no vendor checksum, so the per-arch digest is pinned) and `actionlint` 1.7.12 (MIT, vendor checksum) — the checks this repository runs on `images/*.sh` and `.github/workflows/**`, in the image so an agent finds them before it pushes rather than in CI after |
| YAML/JSON | `yq` v4.54.1 (MIT, vendor checksum) — read and edit a manifest or a config file from the shell without writing a script for it |
| Search | `fd` 10.5.0 (MIT or Apache-2.0, pinned digest) — `find` with the defaults you meant, for locating a file in a large working tree |
| Highlighting | `bat` 0.26.1 (MIT or Apache-2.0, pinned digest) — syntax-highlighted `cat`, which people also alias or set as `PAGER` |
| Diff viewer | `delta` 0.20.1 (MIT, pinned digest; the release tag has no `v` and publishes no aarch64 musl asset, so both arches take the gnu build) — readable diffs in a terminal. **Opt-in per user**: the image sets no `core.pager`, so a workspace turns it on itself with `git config --global core.pager delta` |
| Python packaging | `uv` 0.13.6 and `uvx` (MIT or Apache-2.0, vendor checksum per asset) — a fast virtualenv and tool runner beside the RHEL `python3`/`pip`, so a project that pins `uv` can use it |
| Databases | `sqlite3` 3.46.1 (UBI `sqlite`, public domain) — inspect a workspace database from the shell. The live `/workspace/home/.pi-pocket/pocket.sqlite` is **read-only** from a shell: the app is its single writer (`replicas: 1`, `Recreate`), so open it as `sqlite3 -readonly` and change data through the app |
| Networking | `ncat`/`nc` (`nmap-ncat` 7.92, LicenseRef-Nmap, the licence applying to the shipped binary) and `socat` 1.7.4.4 (GPL-2.0) — connect to a port, or find out which port a service really listens on, without a language runtime |
| Subagent types | `extensions/agent-types.ts`, `extensions/_agent-types-policy.ts` plus `extensions/agent-types/`, installed as a Pi Pocket built-in extension in `/opt/pi-pocket/src/server/extensions/` (on by default, toggled in Menu → Extensions). Wraps the built-in `subagent` tool so a spawn can name a `type:` — `architect`, `coder`, `reviewer` — and get that type's model, thinking level, tool list, and brief, and lists the types in the agent's prompt. The type files themselves are data: copied to `/usr/share/pi-agent/agents/` and seeded into `~/.pi/agent/agents/` at start, copy-if-missing, so the workspace's copies stay editable. Project types follow Pi's project trust (`.pi/agents` unless distrusted, `.agents/agents` only when trusted), and a type's model falls back to the session's model when it cannot think at the session's level. The build fails if the module stops loading or a shipped type loses its frontmatter |
| Web search | `extensions/web-search.ts` plus `extensions/web-search/`, installed as a Pi Pocket built-in extension in `/opt/pi-pocket/src/server/extensions/` (on by default, toggled in Menu → Extensions). Adds the agent's `web_search` tool, backed by the search model's own provider API (Gemini grounding, OpenAI/Codex Responses, xAI, Anthropic, DeepSeek, Ollama Cloud, OpenCode). Provider code vendored unmodified from `pi-web-search` 1.7.0 (MIT, `extensions/web-search/NOTICE.md`); searches are billable model calls on the install's own credentials. The build fails if the module stops loading or stops installing its tool |
| Log safety | `images/log-filter.mjs` (`pi-pocket-log-filter`): runs the launcher, redacts the owner sign-in token and QR block from the container log, forwards signals, passes errors through |

The versions above are the ones shipping today; the three RPM rows name what UBI 10 appstream
has now (`sqlite` 3.46.1, `nmap-ncat` 7.92, `socat` 1.7.4.4), and a scheduled image rebuild
follows RHEL's updates for them. Everything downloaded is pinned in `images/Containerfile`
and does not move until that pin is bumped.

Size: the downloaded toolchain adds about 35 MiB of downloads and 88 MiB of installed
binaries on amd64, and 116 MiB on arm64 — most of that difference is its statically linked
`shellcheck` at 52.5 MiB against 15.5 MiB on x86_64. If the image has to slim down, `delta`,
`bat` and `fd` are the conveniences to drop first; the linters, `yq` and `uv` are not.

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
- **Data**: `/workspace/home/.pi-pocket` (`pocket.sqlite`, `config.json`, uploads, push keys,
  `background/` logs).
- **Configuration mount**: `/run/pocket-config` (the runtime Secret). `api-keys.json` is exported as the allow-listed provider environment variables; `web-search.json`, when present, is exported as `PI_WEB_SEARCH_CONFIG`, which points the agent's web-search configuration at the portal's choice.
- **MCP**: `~/.pi/agent/mcp.json` is seeded from `/usr/share/pi-agent/mcp.json` (copy-if-missing), and the read-only server it names is configured by the root-owned `/usr/share/pi-agent/mcp/kubernetes-read-only.toml`. Neither starts anything on its own: the entry waits for the owner to install the gallery **mcp** extension and switch the `kubernetes` server on.
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

### Built-in extensions

The two modules this repository ships (`extensions/web-search.ts` and
`extensions/agent-types.ts`, plus `extensions/_agent-types-policy.ts` as a
helper the loader skips) are copied into `/opt/pi-pocket/src/server/extensions/`,
where Pi Pocket loads its own built-ins from. They are written for **extension
API 2** and declare `pocketApi = 2`, which is also why every tool result carries
its parts in `output`: Pi Durable 1.2 renamed it from `content`, and Pi Pocket's
shim for the old name applies to drop-ins only. The build calls the installed
tools and fails if one answers with nothing usable.

The `_agent-types-policy.ts` helper keeps its leading `_` so the loader's
discovery (`.ts` files, not `*.test.ts`, not `_*`) does not treat it as a module.

### Background tasks and the data volume

Since 0.24 a `background` task runs its command in `bash`, keeps its output in
segments cut with `split` under `<data>/background/<conversation>/`, and watches
its processes through `/proc`; the image has bash, coreutils and the pod's
`/proc`. The tasks live in the data folder, so they survive a server restart but
not pod replacement (the chart's `Recreate` strategy replaces the pod on every
upgrade), and a noisy long-lived task spends PVC space until it or its log is
stopped.

### Upgrades and backups

Upgrade by rebuilding and redeploying the image. **Menu → Update Pi Pocket**
(0.23) updates a git checkout and re-runs `npm ci`, and this image deletes
`.git` after verifying the commit, so the updater has nothing to update; its
release check is on by default and only the owner can turn it off (Menu →
Extensions → Pi Pocket updates).

0.21's change of SQLite schema (Pi Durable 1.4.0 opens version 2 and migrates) is
one-way: before the first start on 0.25, stop the app and copy the whole data
folder, and treat that copy as the rollback. An older image cannot open the
migrated database.

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

### Browser automation for agents

The image ships the [`agent-browser`](https://github.com/vercel-labs/agent-browser)
CLI so agents can drive real web pages with the bundled Chromium: navigate,
snapshot accessibility trees (`snapshot -i` with compact `@eN` refs), click,
fill forms, extract text, screenshot, keep login state in profiles, and store
credentials in its auth vault (the LLM never sees passwords). Usage
instructions always match the installed version and ship with the CLI itself:

```sh
agent-browser skills get core        # start here: workflow, patterns, troubleshooting
agent-browser skills get core --full # plus the full command reference
```

Conventions for agents in this pod:

- Use a **named session** per task so parallel agents don't share one browser:
  `export AGENT_BROWSER_SESSION="$(agent-browser session id --scope worktree --prefix task)"`.
- The browser executable and `--no-sandbox` are preset via
  `AGENT_BROWSER_EXECUTABLE_PATH`/`AGENT_BROWSER_ARGS`; no `agent-browser install`
  is needed (it would download a second browser).
- Headless is the default; there is no display, so never `--headed`.
- Daemon state, profiles, and vault live under the workspace home and persist
  on the PVC; `agent-browser close` (or `--all`) when done.
- Provider OAuth logins done through the browser persist like manual logins —
  see the root README's provider-login section.

### Seeded Pi skill and prompt template

`/usr/share/pi-agent/` holds the Pi configuration shipped in the image
(`pi-agent/` in this repo): the `agent-browser` skill, the `/agent-browser`
prompt template, and the MCP server list. The entrypoint copies each into
`~/.pi/agent/` on boot when it is not already there, so every session gets
the `/agent-browser …` slash command with no further setup. In-pod edits
survive restarts; deleting a file re-seeds the shipped copy on the next pod
start, which is also how updates reach an existing volume.

`mcp.json` names the read-only Kubernetes MCP server the image bundles. It is
inert on purpose: MCP support arrives with the gallery's **mcp** extension, which
this image does not ship, so the file starts nothing until the owner installs it
and switches the `kubernetes` server on. The server's own configuration,
`/usr/share/pi-agent/mcp/kubernetes-read-only.toml`, stays root-owned and is
read from the image rather than the workspace, so it is not editable from a
session — a session that wants different rules has to name a different file in
its own `mcp.json`. The root README's "MCP: read-only Kubernetes tools" section
says what it can and cannot prevent.

At boot the entrypoint also copies the shipped subagent types (`~/.pi/agent/agents/`,
copy-if-missing), and merges the `models.json` with the `kimi-k3` strict-mode override
before the app starts. Pi Pocket 0.26.0's **Refresh models** re-reads that same file, so
the override survives a refresh. The merge leaves other keys alone but forces that one
flag to `false`, even in a file that says `true`: a workspace with strict mode on again
would send exactly the requests the gateway rejects.

What a subagent may run on is the owner's allow-list in **Menu → Providers**, and
0.26.0's rule is strict in both directions: an empty list means the session's own model
only, a nonempty one means only the models it lists — the session's own included, so a
spawn that names no model is refused while that model is off the list. The entrypoint
therefore allows the shipped types' models once per model through the app's API.
`~/.pi-pocket/.subagent-models-seeded` records what was seeded, so a model the owner
stops allowing stays stopped across restarts; delete that file (and restart) to seed the
shipped models again.

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

When `GITHUB_BROKER_URL` is set, personal `GH_TOKEN`/`GITHUB_TOKEN` are dropped and Git operations at `https://github.com` resolve credentials through `/usr/local/bin/gh --git` (a `git credential` helper speaking the broker protocol). The wrapper refuses non-github.com hosts and absolute `gh api` URLs; the broker rejects repositories outside the portal-managed allowlist; `useHttpPath` keeps per-repo scoping exact.

The broker may authorize personal-account repositories through a separate
installation of the same GitHub App (`portal.github.repositoryInstallations`),
independent of the organization used for portal login. No wrapper configuration
change is needed: pass the full `owner/name` with `gh -R`, `gh repo clone`, or use
the repository's GitHub remote. Select repositories in portal Configuration →
GitHub App bot; the Helm repository list no longer grants access. The broker
reads the management-namespace policy before every credential response, so
selection changes need no container restart. These remain shared bot credentials,
not the signed-in user's personal token.

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

- **pull requests**: build only, no push; the amd64 pi-pocket image is smoke
  tested (server answers on `:8787`, workspace prepared, `node:sqlite`/`jiti` load,
  `rg`/`kubectl`/`helm`/`oc` run, the owner token never appears in the log, `vfs`
  storage plus the nested-container helpers are in place, and the bundled read-only
  Kubernetes MCP server, its TOML, and the seeded `mcp.json` are all present). The same
  job asserts the added toolchain really runs: `yq` evaluates an expression
  (`-e '.a == 1'`), `shellcheck` is clean on both `/usr/local/bin/pi-pocket-*`
  scripts, `actionlint` is clean on the repository's own workflows copied into the
  container, and `sqlite3`, `ncat`/`nc`, `socat`, `fd`, `bat`, `delta`, `uv` and
  `uvx` each answer a version query.
- **main, tags, manual, scheduled**: each architecture is pushed to
  `quay.io/cldmnky/<image>` under `sha-<commit>-<arch>`, then merged into one
  manifest list tagged `sha-<commit>` (immutable); `latest` is added on `main`
  only. Scheduled runs pick up a newer UBI 10 base and RHEL packages.
- Registry credentials: repository secrets `QUAY_USERNAME` and `QUAY_TOKEN`.

The build runs on **podman**, the tool the Makefile and README use, and pulls only
from `registry.access.redhat.com` and `quay.io`. Docker Hub is deliberately absent:
its anonymous pull limits returned `429`/`504` for `docker.io/library/golang` and
failed whole runs, and every image here has a Red Hat or quay source. The Go build
stages therefore come from `registry.access.redhat.com/ubi10/go-toolset:<stream>`
(the RHEL 10.2 stream today, the same Go the runtime image installs with
`dnf install golang`), not from Docker Hub's `golang`.

Three consequences worth knowing:

- The per-architecture jobs push `sha-<commit>-<arch>` tags; the merge job builds the
  index with `podman manifest create` and pushes it with `--all=false`, so the index
  is written without re-uploading the layers it points at (the default, `--all`, copies
  every blob again). It then verifies with `skopeo` that the published index really
  references both built images.
- Each architecture builds on its **own native runner** — `ubuntu-24.04` for amd64 and
  GitHub's `ubuntu-24.04-arm` for arm64 (free for public repositories). No QEMU, no
  `binfmt_misc` registration, and no emulated `RUN` steps; the arm64 build is as fast
  as the amd64 one.
- There is no shared build cache: buildx's `type=gha` cache has no podman
  equivalent, and a registry cache would mean pushing cache images from pull
  requests. Each build starts from the base image.

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
