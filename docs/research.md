# Research and deployment decisions

## Primary sources

- [Upstream pi-pocket README](https://github.com/TannerMidd/pi-pocket/blob/main/README.md), [package.json](https://github.com/TannerMidd/pi-pocket/blob/main/package.json), [launcher options](https://github.com/TannerMidd/pi-pocket/blob/main/src/launcher/options.ts), [server authentication](https://github.com/TannerMidd/pi-pocket/blob/main/src/server/auth.ts), [persistent config](https://github.com/TannerMidd/pi-pocket/blob/main/src/server/config.ts).
- [OpenShift 4.22 SCC documentation](https://docs.redhat.com/en/documentation/openshift_container_platform/4.22/html/authentication_and_authorization/managing-pod-security-policies).
- [Red Hat: nested containers with user namespaces](https://developers.redhat.com/articles/2024/12/02/enable-nested-containers-openshift-dev-spaces-user-namespaces).
- [Kubernetes user namespaces](https://kubernetes.io/docs/concepts/workloads/pods/user-namespaces/): `hostUsers: false`, idmapped volume support, limits and isolation boundaries.
- [RHEL 10 container documentation](https://docs.redhat.com/en/documentation/red_hat_enterprise_linux/10/html/building_running_and_managing_containers/), [UBI image catalog](https://catalog.redhat.com/software/containers/ubi10/ubi/66f2b5e7ecb3d1d2f7ee5667).

Upstream was inspected directly at commit `55849dd3769f1cc265404443193c1ab4385aeedb` (`0.11.0`). It contains a lockfile and launches through `bin/pi-pocket.js`; the server is not a static frontend. It persists SQLite and settings, has an owner sign-in key, and already implements scoped invites and owner/steer/view access. Its CLI supports explicit host, port, working and data directories. That avoids an interactive launcher in Kubernetes. Forwarded HTTPS headers are used for external URLs and secure cookies.

## Why a separate portal

Upstream covers application invitations/roles but not provisioning namespace Secrets or controlling a Deployment. The small portal owns only a named configuration Secret and named Deployment. It avoids holding provider keys in its own PVC, never returns stored provider key values, uses optimistic concurrency, and uses a separate bearer credential. The portal is not a substitute for pi-pocket's identity system and cannot provide namespace isolation from the admin-enabled coding agent.

SSH public keys are configuration, not Git credentials. No SSH daemon is introduced merely to satisfy a public-key input. Private keys, if needed, must be provisioned separately and treated as secrets. The requested namespace admin authority is explicit and configurable, not a cluster-admin grant.

## Why integrated devtools

An OCI image volume is read-only content, not an automatically activated second root filesystem. Mounting an arbitrary UBI tool image into a Node image does not resolve ELF interpreters, dynamic libraries or language-runtime lookup paths. A single integrated UBI10 image is simpler and verifiable on both supported architectures. Toolchain caches and user-installed tools can persist under the workspace home.

## Why nested-container and VFS

The inspected `restricted-v3` SCC requires pod user namespaces, non-root IDs 1000–65534, RuntimeDefault seccomp, dropped capabilities and no privilege escalation. It is the portal profile and an available stricter pocket profile.

The inspected built-in `nested-container` SCC also **requires** pod user namespaces, but permits SETUID/SETGID and privilege escalation for subordinate-ID helpers and specifies SELinux `container_engine_t`. Its permissions do not apply to the host user namespace. The chart deliberately does not request a privileged container or mount host paths/sockets/devices. Podman's VFS driver avoids requesting `/dev/fuse`; this costs space and performance. Rootless subordinate IDs must fit inside the outer 65536-ID pod mapping. Podman functionality still requires runtime testing; simply installing a binary does not prove nested builds work.

Do not modify either built-in SCC. Their use is granted by namespace RoleBindings referencing the built-in SCC ClusterRoles. The namespace-level admin RoleBinding is separate. Do not replace `hostUsers: false` to make storage work: select storage that supports idmapped mounts.

## Live cluster evidence (2026-10-08)

Read-only inspection found:

- Active context: `voyage-test/api-voyager-blahonga-me:6443/kube:admin`.
- OpenShift **4.22.8**, Kubernetes **v1.35.6**, CRI-O **1.35.5**.
- Single amd64 RHCOS worker/control-plane node with Red Hat's 5.14-based kernel. Upstream kernel-version guidance alone is insufficient: Red Hat backports must be tested.
- `UserNamespacesSupport` and `UserNamespacesPodSecurityStandards` enabled. No feature gate was changed.
- Existing `restricted-v3` and `nested-container` SCCs both require pod-level user namespaces.
- Default `lvms-vg1` CSI storage, RWO and WaitForFirstConsumer; a separate NFS class exists but was not selected.
- `openshift-default` ingress class and `apps.voyager.blahonga.me` ingress domain.

A temporary probe in the dedicated `pi-pocket` namespace used the **latest UBI10** image, UID/GID1000, the nested-container SCC, `hostUsers: false`, projected service-account credentials and a 1Gi LVMS PVC. It reached Ready, wrote/read the PVC, could read the token file, and reported:

```text
uid=1000 gid=1000
/proc/self/uid_map:
         0 2609053696      65536
SCC: nested-container
hostUsers: false
SELinux: {level: s0:c32,c24, type: container_engine_t}
```

The image reported **RHEL 10.2 (Coughlan)**, digest `sha256:27e14f4987d7abe56664d7e1b1dddcd0226d4ca593da5726f0f65697a17d0fee`. This is observed evidence, not a permanent base pin: future `latest` builds will refresh it.

The first admission attempt explicitly set only SELinux type and failed because its level was empty. Removing explicit SELinux options allowed the SCC to supply both its required type and the namespace MCS label. The chart incorporates this fix. No namespace UID/MCS annotations were changed. All probe-only resources were deleted after the check; the dedicated installation namespace remains.

Five automated Helm rendering test cases pass, covering user namespaces, security profiles, single-writer updates, namespace and named-resource RBAC, Downward API fields, whole-directory Secret mounts, retained storage, TLS ingresses, disabled portal, reused resources and rejected unsafe inputs. API server dry-run of the rendered resources passed. The nested-container Deployment produced an expected restricted Pod Security warning for its explicit UID-map-helper/proc permissions; actual probe admission and execution succeeded under the required SCC.

## Integration findings from the live install (2026-10-08)

Three issues were found during rollout and fixed in the chart, all verified:

1. **Service-link env collision.** A Service named `pi-pocket` makes Kubernetes inject `PI_POCKET_PORT=tcp://<cluster-ip>:8787`, overriding the image's numeric `PI_POCKET_PORT=8787` and crashing the launcher (`--port must be a port number`). Both pod specs set `enableServiceLinks: false`. The entrypoint's in-cluster API discovery uses `KUBERNETES_SERVICE_HOST/PORT`, which are always injected and unaffected.

2. **Empty `tls:` stanza blocks Route creation.** An Ingress `tls:` block with hosts but no `secretName` is silently ignored by the ingress operator: no Route is created and no event is emitted (verified with a minimal probe Ingress, which produced a Route immediately). The chart renders `tls:` only when an explicit TLS Secret is configured; otherwise `route.openshift.io/termination: edge` serves the router's default certificate. Both routes now show `edge/Redirect`.

3. **RuntimeDefault seccomp blocks nested user namespaces.** `unshare -U` inside the pod failed with `ENOSYS`, and `podman info` failed with `cannot clone: Operation not permitted`. A probe pod with `seccompProfile: Unconfined` under the same `nested-container` SCC created user namespaces successfully. The chart therefore defaults the pocket pod to `Unconfined` under `nested-container` (that SCC permits any profile) and keeps `RuntimeDefault` under `restricted-v3` (the only profile that SCC allows) via the `pocket.seccomp` helper and `podSeccomp` override. Verified after the fix: `podman info` reports `vfs rootless=true`, and `podman run --network=host --rm <image> echo nested-run-ok` succeeds. (The stock `quay.io/libpod/banner` demo fails only because its nginx cannot bind privileged port 80 as a rootless user — expected.)

End-to-end verification after the fixes: both pods Running, both HTTPS routes return 200, portal `/api/status` reports the deployment running, `/api/config` returns the redacted view, podman pull/run works nested, `oc auth can-i get secrets` answers `yes`, and the owner token never appears in container logs.

## Separate GitHub login and repository installations (2026-10-09)

The original integration deliberately discarded the GitHub user token after
identity lookup and used one organization installation for both membership
checks and repository bot tokens. That coupled the login gate to repository
ownership and prevented personal-account repositories even when their owner
could sign in as an organization member.

`portal.github.repositoryInstallations` now explicitly maps additional account
owners to installations of the same App. The original `installationID` still
serves organization/team membership checks; the repository allow-list remains
mandatory, and each broker response still has only one repository with
`contents`, `pull_requests`, and `actions` write permissions. Installation
validation is tracked per installation; token caches remain keyed by full
`owner/name`, not just repository name. This is not per-user delegation:
everyone trusted to steer the shared workspace can use all configured bot
permissions. App keys remain outside the namespace-admin workspace, and no
Kubernetes permissions or network policies change.

GitHub documents that [installation tokens access resources belonging to the
installed account](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation),
cannot exceed the installation's repository access or permissions, and expire
after one hour. An organization-owned App must be
[public to install on another account](https://docs.github.com/en/apps/creating-github-apps/setting-up-a-github-app/making-a-github-app-public-or-private);
App visibility is not a repository access grant.

Offline tests cover separate organization login and personal repository minting,
same-named repositories across owners, per-repository cache renewal, owner
mismatches, suspended installations, malformed mappings, and Helm propagation
without changing the login gate. Personal-account installation and live-cluster
rollout have not been verified in this change.

## Portal post-login rendering regression (2026-10-09)

After deploying the merged web-search UI, live logs showed repeated successful
OAuth callbacks (`303`), session checks (`200`), and authenticated configuration
reads (`200`), with no subsequent status request. The web-search input IDs were
missing from the SPA element cache, so rendering configuration threw before
loading deployment status. A broad initialization catch mislabeled this as
"Authentication unavailable". Registering both inputs fixes the rendering error;
a DOM/cache contract test guards against recurrence. Workspace-load errors now
have a separate message from authentication-bootstrap failures.

OAuth diagnostics log fixed stage/outcome/reason fields, not raw provider
errors, authorization codes, state/verifiers, cookies, or callback query strings.
Identity exchange and membership validation share a provider operation and are
reported as one `identity_and_membership` stage. Tests assert success/failure
events and absence of transaction/session secrets and provider error details.
