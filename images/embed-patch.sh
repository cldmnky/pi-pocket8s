#!/usr/bin/env bash
#
# Optional pi-pocket embedding support for the portal iframe.
#
# When POCKET_FRAME_ANCESTORS is set to the portal's https origin, this script
# patches the pinned upstream sources (plain text in /opt/pi-pocket) so the app
# can run embedded cross-origin:
#
#   src/server/http/assets.ts  frame-ancestors 'self'  ->  + portal origin
#   src/server/auth.ts         SameSite=Lax            ->  SameSite=None (x2)
#
# Without SameSite=None the session cookie is withheld inside a cross-site
# iframe and the embedded login never sticks (upstream default is Lax).
#
# Fail-closed: if the expected patterns are not found exactly (e.g. an upstream
# upgrade changed them), nothing is modified and the caller logs a warning. The
# patterns are tied to the pinned upstream commit in the Containerfile.
set -u

readonly ORIGIN="${POCKET_FRAME_ANCESTORS:-}"
readonly APP_DIR="/opt/pi-pocket"

log() { printf '[pi-pocket-embed] %s\n' "$*" >&2; }

[ -n "${ORIGIN}" ] || exit 0

if [[ ! "${ORIGIN}" =~ ^https://[A-Za-z0-9.-]+(:[0-9]+)?$ ]]; then
    log "WARNING: refusing invalid origin ${ORIGIN}; embedding stays disabled"
    exit 1
fi

readonly ASSETS="${APP_DIR}/src/server/http/assets.ts"
readonly AUTH="${APP_DIR}/src/server/auth.ts"
readonly MARKER="${APP_DIR}/.embed-patched"

if [ -f "${MARKER}" ] && [ "$(cat "${MARKER}" 2>/dev/null)" = "${ORIGIN}" ]; then
    log "already patched for ${ORIGIN}"
    exit 0
fi

for file in "${ASSETS}" "${AUTH}"; do
    if [ ! -w "${file}" ]; then
        log "WARNING: ${file} is not writable; embedding stays disabled"
        exit 1
    fi
done

if [ "$(grep -c -F "frame-ancestors 'self'" "${ASSETS}")" != "1" ]; then
    log "WARNING: CSP pattern not found exactly once in assets.ts; upstream may have changed, embedding stays disabled"
    exit 1
fi
if [ "$(grep -c -F 'SameSite=Lax' "${AUTH}")" != "2" ]; then
    log "WARNING: cookie pattern not found exactly twice in auth.ts; upstream may have changed, embedding stays disabled"
    exit 1
fi

sed -i "s|frame-ancestors 'self'|frame-ancestors 'self' ${ORIGIN}|" "${ASSETS}"
sed -i 's/SameSite=Lax/SameSite=None/g' "${AUTH}"

if [ "$(grep -c -F "frame-ancestors 'self' ${ORIGIN}" "${ASSETS}")" != "1" ] \
    || [ "$(grep -c -F 'SameSite=None' "${AUTH}")" != "2" ]; then
    log "WARNING: patch verification failed; embedding stays disabled"
    exit 1
fi

printf '%s' "${ORIGIN}" > "${MARKER}"
log "embedded mode enabled for ${ORIGIN} (CSP frame-ancestors, SameSite=None cookies)"
