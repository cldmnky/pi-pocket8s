#!/usr/bin/env bash
#
# Pi Pocket's container entrypoint. It prepares the persistent workspace, loads the
# runtime configuration mounted at /run/pocket-config, writes a kubeconfig that reads
# the (rotating) service account token at request time, then execs the launcher through
# pi-pocket-log-filter as PID 1, so SIGTERM reaches it and the owner sign-in token never
# reaches the container log.
#
# Everything the container writes lives under /workspace/home (persistent volume) or
# /workspace/repos (working trees). The process runs as UID/GID 1000. See images/README.md.
set -euo pipefail

readonly HOME_DIR="${HOME:-/workspace/home}"
readonly REPOS_DIR="/workspace/repos"
readonly DATA_DIR="${PI_POCKET_DIR:-${HOME_DIR}/.pi-pocket}"
readonly CONFIG_DIR="/run/pocket-config"
readonly SA_DIR="/var/run/secrets/kubernetes.io/serviceaccount"
readonly API_KEYS_FILE="${CONFIG_DIR}/api-keys.json"
readonly WEB_SEARCH_FILE="${CONFIG_DIR}/web-search.json"
readonly OWNER_LOGIN_KEY="owner-login-url"

# The only secret keys that may become environment variables. Anything else in
# api-keys.json is ignored, so the runtime secret cannot inject arbitrary env.
readonly API_KEY_NAMES=(
    ANTHROPIC_API_KEY
    OPENAI_API_KEY
    GEMINI_API_KEY
    GOOGLE_API_KEY
    GROQ_API_KEY
    OPENROUTER_API_KEY
    MISTRAL_API_KEY
    DEEPSEEK_API_KEY
    XAI_API_KEY
    AWS_ACCESS_KEY_ID
    AWS_SECRET_ACCESS_KEY
    AWS_SESSION_TOKEN
    AWS_REGION
)

log() { printf '[pi-pocket-entrypoint] %s\n' "$*" >&2; }
warn() { printf '[pi-pocket-entrypoint] WARNING: %s\n' "$*" >&2; }

prepare_directories() {
    local dir

    for dir in \
        "${HOME_DIR}" \
        "${HOME_DIR}/.pi" \
        "${DATA_DIR}" \
        "${HOME_DIR}/.ssh" \
        "${HOME_DIR}/.kube" \
        "${HOME_DIR}/.config" \
        "${HOME_DIR}/.cache" \
        "${HOME_DIR}/.cache/pip" \
        "${HOME_DIR}/.cache/go-build" \
        "${HOME_DIR}/.local/bin" \
        "${HOME_DIR}/.local/share" \
        "${HOME_DIR}/.run" \
        "${HOME_DIR}/.npm" \
        "${HOME_DIR}/go" \
        "${REPOS_DIR}"; do
        if ! mkdir -p "${dir}" 2>/dev/null; then
            warn "cannot create ${dir}; check that the volume is writable by UID 1000 (fsGroup 1000)"
        fi
    done

    chmod 0700 "${HOME_DIR}/.ssh" "${HOME_DIR}/.run" "${HOME_DIR}/.pi" "${DATA_DIR}" 2>/dev/null || true
    # XDG_RUNTIME_DIR is the chart's emptyDir (/run/user/1000) in-cluster, or a
    # plain directory under /run for local runs.
    if mkdir -p "${XDG_RUNTIME_DIR:-/run/user/1000}" 2>/dev/null; then
        chmod 0700 "${XDG_RUNTIME_DIR:-/run/user/1000}" 2>/dev/null || true
    else
        warn "cannot create ${XDG_RUNTIME_DIR:-/run/user/1000}"
    fi
    chmod 0755 "${REPOS_DIR}" 2>/dev/null || true

    if [ ! -w "${HOME_DIR}" ]; then
        warn "${HOME_DIR} is not writable by UID $(id -u); Pi Pocket keeps its data there and will fail"
    fi
}

# Seed the shipped Pi skill, prompt template, and subagent types into the workspace home.
# Copy-if-missing: in-pod edits survive restarts, and deleting a file re-seeds
# the shipped copy on the next start (the update path for existing volumes).
seed_pi_agent_files() {
    local src="${PI_AGENT_SOURCE:-/usr/share/pi-agent}"
    local dest="${HOME_DIR}/.pi/agent"
    local pair
    for pair in \
        "skills/agent-browser/SKILL.md:skills/agent-browser/SKILL.md" \
        "prompts/agent-browser.md:prompts/agent-browser.md"; do
        local from="${src}/${pair%%:*}" to="${dest}/${pair##*:}"
        if [ -e "${to}" ]; then
            continue
        fi
        if [ -r "${from}" ]; then
            mkdir -p "$(dirname "${to}")" 2>/dev/null || true
            if cp "${from}" "${to}" 2>/dev/null; then
                log "seeded ${to#$HOME_DIR/} from the image"
            else
                warn "cannot seed ${to}"
            fi
        else
            warn "shipped Pi agent file missing: ${from}"
        fi
    done

    # The subagent types the image ships (extensions/agent-types/), every *.md including the
    # _readme.md that documents the format. A file the workspace already has is left alone: that
    # copy is the user's, and an image update must not undo an edit — or resurrect a type that
    # was deliberately deleted. Delete the file to get the shipped copy back.
    local file to
    for file in "${src}/agents"/*.md; do
        if [ ! -r "${file}" ]; then
            continue
        fi
        to="${dest}/agents/$(basename "${file}")"
        if [ -e "${to}" ]; then
            continue
        fi
        mkdir -p "$(dirname "${to}")" 2>/dev/null || true
        if cp "${file}" "${to}" 2>/dev/null; then
            log "seeded ${to#$HOME_DIR/} from the image"
        else
            warn "cannot seed ${to}"
        fi
    done
}

# api-keys.json is a flat JSON object of allowlisted provider/env names. Values are
# exported into the launcher's environment; a changed secret therefore needs a restart
# (the portal asks for one) because the variables are read once, at process start.
load_api_keys() {
    if [ ! -f "${API_KEYS_FILE}" ]; then
        return 0
    fi

    if ! jq -e 'type == "object"' "${API_KEYS_FILE}" >/dev/null 2>&1; then
        warn "${API_KEYS_FILE} is not a JSON object; no provider keys were exported"
        return 0
    fi

    local name value loaded=0

    for name in "${API_KEY_NAMES[@]}"; do
        value="$(jq -r --arg key "${name}" \
            'if (.[$key] | type) == "string" then .[$key] else empty end' \
            "${API_KEYS_FILE}" 2>/dev/null)" || value=""
        if [ -z "${value}" ]; then
            continue
        fi
        export "${name}=${value}"
        loaded=$((loaded + 1))
    done

    log "exported ${loaded} of ${#API_KEY_NAMES[@]} allowlisted provider key(s) from ${API_KEYS_FILE}"
}

# web-search.json (optional) is the portal's choice of the model that searches the web. Pi's own
# configuration file for the search extension is pointed at the mounted copy, so the extension reads
# the portal's setting; because the mount is read-only, an in-session change cannot take it over
# (the extension says so, and where to change it). Absent or unusable: nothing is exported, and the
# workspace's own ~/.pi/agent/web-search.json applies.
load_web_search() {
    if [ ! -f "${WEB_SEARCH_FILE}" ]; then
        return 0
    fi

    if ! jq -e 'type == "object" and (.provider | type) == "string" and (.model | type) == "string"
        and (.provider | length) > 0 and (.model | length) > 0' "${WEB_SEARCH_FILE}" >/dev/null 2>&1; then
        warn "${WEB_SEARCH_FILE} is not {\"provider\":…,\"model\":…}; the workspace's own web-search.json applies instead"
        return 0
    fi

    export PI_WEB_SEARCH_CONFIG="${WEB_SEARCH_FILE}"
    log "web search model set by the portal: $(jq -r '"\(.provider)/\(.model)"' "${WEB_SEARCH_FILE}")"
}

# authorized_keys and known_hosts are exposed as files for tooling (git, ssh clients);
# no sshd runs in this image. The optional id_ed25519 is the agent's outbound SSH key.
install_ssh_file() {
    local name="$1"
    local mode="$2"
    local source="${CONFIG_DIR}/${name}"
    local target="${HOME_DIR}/.ssh/${name}"

    if [ ! -f "${source}" ]; then
        return 0
    fi

    if cp "${source}" "${target}" 2>/dev/null && chmod "${mode}" "${target}" 2>/dev/null; then
        log "installed ~/.ssh/${name}"
    else
        warn "could not install ~/.ssh/${name}"
    fi
}

# The kubeconfig points at the mounted token file and CA bundle instead of a snapshot:
# kubectl and oc read the token on every request, so projected service account tokens
# rotate without a restart. POD_NAMESPACE/POD_NAME/POD_UID name the pod it belongs to.
write_kubeconfig() {
    if [ ! -r "${SA_DIR}/token" ] || [ ! -r "${SA_DIR}/ca.crt" ]; then
        log "no service account at ${SA_DIR}; skipping kubeconfig"
        return 0
    fi

    local namespace="${POD_NAMESPACE:-default}"
    local pod="${POD_NAME:-pi-pocket}"
    local uid="${POD_UID:-unknown}"
    local server="https://kubernetes.default.svc"

    if [ -n "${KUBERNETES_SERVICE_HOST:-}" ]; then
        server="https://${KUBERNETES_SERVICE_HOST}:${KUBERNETES_SERVICE_PORT_HTTPS:-${KUBERNETES_SERVICE_PORT:-443}}"
    fi

    cat > "${HOME_DIR}/.kube/config" <<EOF
# Written by pi-pocket-entrypoint for pod ${pod} (${uid}) in namespace ${namespace}.
# The bearer token is read from ${SA_DIR}/token on every request, so rotated
# service account tokens are used without restarting the container.
apiVersion: v1
kind: Config
clusters:
    - name: in-cluster
      cluster:
          server: ${server}
          certificate-authority: ${SA_DIR}/ca.crt
users:
    - name: pi-pocket
      user:
          tokenFile: ${SA_DIR}/token
contexts:
    - name: ${namespace}/${pod}
      context:
          cluster: in-cluster
          user: pi-pocket
          namespace: ${namespace}
current-context: ${namespace}/${pod}
preferences: {}
EOF
    chmod 0600 "${HOME_DIR}/.kube/config"
    export KUBECONFIG="${HOME_DIR}/.kube/config"
    log "wrote kubeconfig for namespace ${namespace} (service account token is live, not a snapshot)"
}

# Publish the owner sign-in link to the runtime secret so the portal can show
# it (and its QR code) to authenticated operators. Provider key *values* are
# never written here: only this one URL, which the portal already has the
# rights to read. Runs at container start; a rotation re-syncs on restart.
sync_owner_url() {
    local secret="${RUNTIME_SECRET:-}" public="${POCKET_PUBLIC_URL:-}"
    local config="${DATA_DIR}/config.json"

    if [ -z "${secret}" ] || [ -z "${public}" ]; then
        return 0
    fi
    if [ ! -r "${SA_DIR}/token" ] || [ ! -r "${SA_DIR}/ca.crt" ] || [ -z "${KUBERNETES_SERVICE_HOST:-}" ]; then
        log "no service account API; skipping owner link sync"
        return 0
    fi
    if [ ! -r "${config}" ]; then
        log "no ${config} yet; skipping owner link sync"
        return 0
    fi

    local token url encoded body code
    token="$(jq -r '.ownerToken // empty' "${config}" 2>/dev/null)" || token=""
    if [ -z "${token}" ]; then
        warn "could not read ownerToken from ${config}"
        return 0
    fi
    url="${public%/}/login?token=${token}"
    encoded="$(printf '%s' "${url}" | base64 -w0)"
    body="$(jq -n --arg v "${encoded}" --arg k "${OWNER_LOGIN_KEY}" '{data: {($k): $v}}' 2>/dev/null)" || body=""
    if [ -z "${body}" ]; then
        warn "could not encode owner link"
        return 0
    fi
    code="$(curl -s -o /dev/null -w '%{http_code}' --cacert "${SA_DIR}/ca.crt" \
        -H "Authorization: Bearer $(cat "${SA_DIR}/token")" \
        -H 'Content-Type: application/merge-patch+json' -X PATCH \
        --data "${body}" --max-time 10 \
        "https://${KUBERNETES_SERVICE_HOST}:${KUBERNETES_SERVICE_PORT_HTTPS:-${KUBERNETES_SERVICE_PORT:-443}}/api/v1/namespaces/${POD_NAMESPACE:-default}/secrets/${secret}" 2>/dev/null)" || code=""
    if [ "${code}" = "200" ]; then
        log "published owner sign-in link to secret ${secret}"
    else
        warn "owner link sync failed (status ${code:-none})"
    fi
}

# Remove a stale single-writer lock. Upstream refuses to start when
# <data>/harness.lock exists, and the lock survives pod restarts on the
# persistent volume. A lock whose pid is dead (always the case in a fresh
# container after a clean shutdown was interrupted) is safe to drop; a live
# pid means a real second writer, so the lock is kept and boot fails loudly.
clear_stale_lock() {
    local lock="${DATA_DIR}/harness.lock"
    local pid

    if [ ! -e "${lock}" ]; then
        return 0
    fi
    pid="$(tr -cd '0-9' < "${lock}" 2>/dev/null)" || pid=""
    if [ -n "${pid}" ] && kill -0 "${pid}" 2>/dev/null; then
        warn "${lock} is held by live pid ${pid}; refusing to start a second writer"
        return 0
    fi
    log "removing stale lock ${lock} (pid ${pid:-unknown} is not running)"
    rm -f "${lock}" || warn "could not remove ${lock}"
}

launch() {
    local port="${PI_POCKET_PORT:-8787}"

    # Upstream launches $PI_POCKET_BROWSER when its browser feature needs one;
    # --no-sandbox is required inside user namespaces. Operators can override.
    export PI_POCKET_BROWSER="${PI_POCKET_BROWSER:-/usr/local/bin/chromium}"
    export PI_POCKET_BROWSER_ARGS="${PI_POCKET_BROWSER_ARGS:---no-sandbox}"

    if ! cd "${REPOS_DIR}" 2>/dev/null; then
        warn "cannot enter ${REPOS_DIR}; starting from ${HOME_DIR}"
        cd "${HOME_DIR}"
    fi

    log "pod=${POD_NAME:-?} uid=${POD_UID:-?} namespace=${POD_NAMESPACE:-?}"
    log "home=${HOME_DIR} repos=${REPOS_DIR} data=${DATA_DIR}"
    log "starting Pi Pocket on 0.0.0.0:${port} (noninteractive)"

    # pi-pocket-log-filter redacts the owner sign-in token and QR code from the launcher
    # log (errors pass through) and forwards signals to the launcher it runs.
    # This shell stays PID 1: it publishes the owner link once the server has
    # written config.json, then waits on the filter so signals and exit codes
    # propagate and no unreaped children are left behind.
    clear_stale_lock
    # Web terminal daemon (pi-terminal): same container, same user and
    # workspace as pi-pocket. It restarts on its own if it crashes; the
    # launcher below owns the container's exit code.
    TERMINAL_PORT="${TERMINAL_PORT:-8081}"
    export TERMINAL_PORT
    /usr/local/bin/pi-terminal &
    local terminal=$!
    log "terminal daemon started (pid ${terminal}, port ${TERMINAL_PORT})"
    /usr/local/bin/pi-pocket-log-filter \
        --host 0.0.0.0 \
        --port "${port}" \
        --cwd "${REPOS_DIR}" \
        --data "${DATA_DIR}" &
    local child=$!
    trap 'kill -TERM "${child}" "${terminal}" 2>/dev/null' TERM INT HUP USR2
    local waited=0
    while [ "${waited}" -lt 90 ] && [ ! -r "${DATA_DIR}/config.json" ]; do
        kill -0 "${child}" 2>/dev/null || break
        sleep 1
        waited=$((waited + 1))
    done
    sync_owner_url
    # Supervise both processes: the launcher owns the exit code, while a
    # crashed terminal daemon is restarted (it holds no state worth keeping).
    while true; do
        wait -n "${child}" "${terminal}"
        if ! kill -0 "${child}" 2>/dev/null; then
            wait "${child}"
            local code=$?
            kill -TERM "${terminal}" 2>/dev/null
            wait 2>/dev/null
            trap - TERM INT HUP USR2
            return "${code}"
        fi
        log "terminal daemon exited; restarting in 5s"
        sleep 5
        /usr/local/bin/pi-terminal &
        terminal=$!
    done
}

prepare_directories
seed_pi_agent_files
if [ -n "${POCKET_FRAME_ANCESTORS:-}" ] && [ -x /usr/local/bin/pi-pocket-embed-patch ]; then
    /usr/local/bin/pi-pocket-embed-patch || warn "continuing without embedded portal support"
fi
load_api_keys
load_web_search
if [ -n "${GITHUB_BROKER_URL:-}" ]; then
    # No tokens in URLs/config files; Git asks the helper for current credentials
    # for each repository (including clones), and gh uses the broker per command.
    unset GH_TOKEN GITHUB_TOKEN
    git config --global credential.https://github.com.useHttpPath true
    git config --global --replace-all credential.https://github.com.helper ''
    git config --global --add credential.https://github.com.helper '/usr/local/bin/gh --git'
fi
install_ssh_file authorized_keys 0600
install_ssh_file known_hosts 0600
install_ssh_file id_ed25519 0600
write_kubeconfig

# `docker run <image> bash` (or a Kubernetes command override) still gets a prepared
# workspace; with no arguments the image starts Pi Pocket itself.
if [ "$#" -gt 0 ]; then
    log "running command: $*"
    exec "$@"
fi

launch
