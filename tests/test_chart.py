#!/usr/bin/env python3
"""Render real Helm manifests and test critical security/lifecycle contracts."""
import json
import re
import subprocess
import unittest
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[1]


def render(*options):
    result = subprocess.run(
        ["helm", "template", "pi-pocket", str(ROOT / "charts/pi-pocket"),
         "-n", "test-pocket", "-f", str(ROOT / "deploy/openshift-values.yaml"), *options],
        capture_output=True, text=True, check=True,
    )
    return [item for item in yaml.safe_load_all(result.stdout) if item]


def find(objects, kind, name):
    return next(obj for obj in objects if obj["kind"] == kind and obj["metadata"]["name"] == name)


def render_profile(values_file, namespace, *options):
    result = subprocess.run(
        ["helm", "template", "pi-pocket", str(ROOT / "charts/pi-pocket"),
         "-n", namespace, "-f", str(ROOT / values_file), *options],
        capture_output=True, text=True, check=True,
    )
    return [item for item in yaml.safe_load_all(result.stdout) if item]


# GitHub-auth portal in a separate management namespace: the prerequisite for
# every adminElevation render.
GITHUB_MODE_OPTIONS = [
    "--set", "portal.namespace=management", "--set", "portal.authMode=github",
    "--set", "portal.github.clientID=client", "--set", "portal.github.appID=123",
    "--set", "portal.github.installationID=456", "--set", "portal.github.organization=example",
    "--set", "portal.github.existingSecret=github-app",
]

# A valid elevation target, minus the operator allowlist so a test can prove the
# empty-allowlist rule fires before operators are supplied.
ELEVATION_TARGET_OPTIONS = GITHUB_MODE_OPTIONS + [
    "--set", "adminElevation.enabled=true", "--set", "adminElevation.bootstrap=true",
    "--set", "adminElevation.namespace=pi-pocket-admin",
    "--set", "adminElevation.deployment=pi-pocket-admin",
    "--set", "adminElevation.serviceAccount=pi-pocket-admin",
    "--set", "adminElevation.runtimeSecret=pi-pocket-admin-runtime",
    "--set", "adminElevation.pocketURL=https://pocket-admin.apps.voyager.blahonga.me",
    "--set", "adminElevation.terminalURL=https://pocket-admin-terminal.apps.voyager.blahonga.me",
]

ELEVATION_OPTIONS = ELEVATION_TARGET_OPTIONS + [
    "--set", "adminElevation.operators[0]=12345", "--set", "adminElevation.operators[1]=67890",
]


def rbac_rules(docs):
    """Every Role/ClusterRole rule in a render, as a flat list."""
    return [rule for obj in docs if obj["kind"] in ("Role", "ClusterRole") for rule in obj["rules"]]


class ChartTests(unittest.TestCase):
    def test_default_usernamespaces_and_sqlite_single_writer(self):
        docs = render()
        for name in ("pi-pocket", "pi-pocket-portal"):
            obj = find(docs, "Deployment", name)
            pod = obj["spec"]["template"]["spec"]
            self.assertIs(pod["hostUsers"], False)
            self.assertEqual(pod["securityContext"]["runAsUser"], 1000)
            self.assertEqual(pod["securityContext"]["fsGroup"], 1000)
            self.assertTrue(pod["securityContext"]["runAsNonRoot"])
            self.assertEqual(pod["containers"][0]["securityContext"]["capabilities"]["drop"], ["ALL"])
        pocket = find(docs, "Deployment", "pi-pocket")
        pod = pocket["spec"]["template"]["spec"]
        # Nested user namespaces need syscalls RuntimeDefault filters out.
        self.assertEqual(pod["securityContext"]["seccompProfile"]["type"], "Unconfined")
        self.assertEqual(pocket["spec"]["strategy"]["type"], "Recreate")
        self.assertEqual(pocket["spec"]["replicas"], 1)
        security = pocket["spec"]["template"]["spec"]["containers"][0]["securityContext"]
        self.assertEqual(security["procMount"], "Unmasked")
        self.assertEqual(security["capabilities"]["add"], ["SETUID", "SETGID"])
        pvc = find(docs, "PersistentVolumeClaim", "pi-pocket-workspace")
        self.assertEqual(pvc["spec"]["storageClassName"], "lvms-vg1")
        self.assertEqual(pvc["metadata"]["annotations"]["helm.sh/resource-policy"], "keep")

    def test_rbac_and_secret_mounts(self):
        docs = render()
        self.assertEqual(find(docs, "RoleBinding", "pi-pocket-namespace")["roleRef"]["name"], "admin")
        portal_role = find(docs, "Role", "pi-pocket-portal")
        self.assertEqual(len(portal_role["rules"]), 2)
        for rule in portal_role["rules"]:
            self.assertIn("resourceNames", rule)
            self.assertNotIn("*", json.dumps(rule))
            self.assertNotIn("create", rule["verbs"])
        runtime = find(docs, "Secret", "pi-pocket-runtime")
        self.assertEqual(runtime["data"]["api-keys.json"], "e30=")
        pocket = find(docs, "Deployment", "pi-pocket")["spec"]["template"]["spec"]
        mounts = pocket["containers"][0]["volumeMounts"]
        self.assertTrue(all("subPath" not in item for item in mounts))
        env = {item["name"]: item for item in pocket["containers"][0]["env"]}
        for name, field in (("POD_NAMESPACE", "metadata.namespace"), ("POD_NAME", "metadata.name"), ("POD_UID", "metadata.uid")):
            self.assertEqual(env[name]["valueFrom"]["fieldRef"]["fieldPath"], field)
        self.assertEqual(env["RUNTIME_SECRET"]["value"], "pi-pocket-runtime")
        self.assertEqual(env["POCKET_PUBLIC_URL"]["value"], "https://pocket.apps.voyager.blahonga.me")
        self.assertEqual(env["POCKET_FRAME_ANCESTORS"]["value"], "https://pocket-portal.apps.voyager.blahonga.me")
        portal = find(docs, "Deployment", "pi-pocket-portal")["spec"]["template"]["spec"]
        self.assertFalse(portal["containers"][0]["securityContext"]["allowPrivilegeEscalation"])
        self.assertTrue(portal["containers"][0]["securityContext"]["readOnlyRootFilesystem"])

    def test_restricted_profile_and_disabled_portal(self):
        docs = render("--set", "openshift.pocketSCC=restricted-v3", "--set", "portal.enabled=false")
        pod = find(docs, "Deployment", "pi-pocket")["spec"]["template"]["spec"]
        self.assertEqual(pod["securityContext"]["seccompProfile"]["type"], "RuntimeDefault")
        self.assertFalse(any(item["metadata"]["name"].endswith("portal") for item in docs))
        container = find(docs, "Deployment", "pi-pocket")["spec"]["template"]["spec"]["containers"][0]
        self.assertFalse(container["securityContext"]["allowPrivilegeEscalation"])
        self.assertNotIn("procMount", container["securityContext"])
        self.assertNotIn("add", container["securityContext"]["capabilities"])

    def test_ingress_https_and_reusable_resources(self):
        docs = render("--set", "persistence.existingClaim=repos", "--set", "runtimeSecret.existingSecret=config", "--set", "portal.tokenSecret=access")
        self.assertFalse(any(item["kind"] in ("Secret", "PersistentVolumeClaim") for item in docs))
        ingresses = [item for item in docs if item["kind"] == "Ingress"]
        self.assertEqual(len(ingresses), 3)
        for ingress in ingresses:
            self.assertEqual(ingress["spec"]["ingressClassName"], "openshift-default")
            # No tls stanza without an explicit Secret: an empty one blocks
            # Route creation; edge termination comes from the annotation.
            self.assertNotIn("tls", ingress["spec"])
            self.assertEqual(ingress["metadata"]["annotations"]["route.openshift.io/termination"], "edge")
            self.assertEqual(ingress["metadata"]["annotations"]["route.openshift.io/insecureEdgeTerminationPolicy"], "Redirect")
        docs = render("--set", "ingress.pocketTLSSecret=pocket-tls", "--set", "ingress.portalTLSSecret=portal-tls", "--set", "ingress.terminalTLSSecret=terminal-tls")
        ingresses = [item for item in docs if item["kind"] == "Ingress"]
        self.assertEqual(ingresses[0]["spec"]["tls"][0]["secretName"], "pocket-tls")
        self.assertEqual(ingresses[1]["spec"]["tls"][0]["secretName"], "portal-tls")
        self.assertEqual(ingresses[2]["spec"]["tls"][0]["secretName"], "terminal-tls")

    def test_web_terminal_routing_and_env(self):
        docs = render()
        terminal_svc = find(docs, "Service", "pi-pocket-terminal")
        self.assertEqual(terminal_svc["spec"]["ports"], [{"name": "terminal", "port": 8081, "targetPort": "terminal"}])
        terminal_ingress = find(docs, "Ingress", "pi-pocket-terminal")
        self.assertEqual(terminal_ingress["spec"]["rules"][0]["host"], "pocket-terminal.apps.voyager.blahonga.me")
        self.assertEqual(terminal_ingress["spec"]["rules"][0]["http"]["paths"][0]["backend"]["service"],
                         {"name": "pi-pocket-terminal", "port": {"name": "terminal"}})
        policy = find(docs, "NetworkPolicy", "pi-pocket-ingress")
        self.assertIn({"protocol": "TCP", "port": 8081}, policy["spec"]["ingress"][0]["ports"])
        pocket = find(docs, "Deployment", "pi-pocket")["spec"]["template"]["spec"]["containers"][0]
        self.assertIn({"name": "terminal", "containerPort": 8081}, pocket["ports"])
        env = {item["name"]: item.get("value") for item in pocket["env"]}
        self.assertEqual(env["TERMINAL_FRAME_ANCESTORS"], "https://pocket-portal.apps.voyager.blahonga.me")
        portal = find(docs, "Deployment", "pi-pocket-portal")["spec"]["template"]["spec"]["containers"][0]
        portal_env = {item["name"]: item.get("value") for item in portal["env"]}
        self.assertEqual(portal_env["TERMINAL_URL"], "https://pocket-terminal.apps.voyager.blahonga.me")
        with self.assertRaises(subprocess.CalledProcessError):
            render("--set", "ingress.terminalHost=")

    def test_github_auto_mode_and_namespace_boundary(self):
        options = ["--set", "portal.namespace=management", "--set", "portal.github.clientID=client",
                   "--set", "portal.github.appID=123", "--set", "portal.github.installationID=456",
                   "--set", "portal.github.organization=example", "--set", "portal.github.repositories[0]=example/repo",
                   "--set", "portal.github.existingSecret=github-app"]
        docs = render(*options)
        portal = find(docs, "Deployment", "pi-pocket-portal")
        self.assertEqual(portal["metadata"]["namespace"], "management")
        self.assertEqual(portal["spec"]["strategy"]["type"], "Recreate")
        pod = portal["spec"]["template"]["spec"]
        env = {item["name"]: item.get("value") for item in pod["containers"][0]["env"]}
        self.assertEqual(env["PORTAL_AUTH_MODE"], "auto")
        self.assertEqual(env["GITHUB_CLIENT_ID"], "client")
        self.assertEqual(json.loads(env["GITHUB_REPOSITORY_INSTALLATIONS"]), {})
        self.assertEqual(env["POCKET_NAMESPACE"], "test-pocket")
        self.assertEqual(env["POCKET_SERVICE_ACCOUNT"], "pi-pocket")
        self.assertEqual(pod["volumes"][0]["secret"]["secretName"], "github-app")
        self.assertFalse(any(item["kind"] == "Secret" and "portal-token" in item["metadata"]["name"] for item in docs))
        for kind in ("ServiceAccount", "Service", "Ingress"):
            self.assertEqual(find(docs, kind, "pi-pocket-portal")["metadata"]["namespace"], "management")
        role = find(docs, "RoleBinding", "pi-pocket-portal")
        self.assertEqual(role["subjects"][0]["namespace"], "management")
        self.assertEqual(find(docs, "ClusterRole", "test-pocket-pi-pocket-github-tokenreview")["rules"],
                         [{"apiGroups": ["authentication.k8s.io"], "resources": ["tokenreviews"], "verbs": ["create"]}])
        agent = find(docs, "Deployment", "pi-pocket")["spec"]["template"]["spec"]
        self.assertTrue(all(v.get("secret", {}).get("secretName") != "github-app" for v in agent["volumes"]))
        audience = next(v for v in agent["volumes"] if v["name"] == "github-broker")["projected"]["sources"][0]["serviceAccountToken"]["audience"]
        self.assertEqual(audience, "pi-pocket-github")
        with self.assertRaises(subprocess.CalledProcessError):
            render(*options, "--set", "portal.namespace=test-pocket")
        with self.assertRaises(subprocess.CalledProcessError):
            render(*options, "--set", "portal.github.existingSecret=")
        policy = find(docs, "Secret", "pi-pocket-github-repositories")
        self.assertEqual(policy["metadata"]["namespace"], "management")
        self.assertEqual(policy["metadata"]["annotations"]["helm.sh/resource-policy"], "keep")
        self.assertEqual(policy["data"]["repositories.json"], "W10=")  # [] denies all
        self.assertNotIn("GITHUB_REPOSITORIES", env)
        self.assertEqual(env["GITHUB_REPOSITORY_POLICY_SECRET"], "pi-pocket-github-repositories")
        role = find(docs, "Role", "pi-pocket-github-repositories")
        self.assertEqual(role["metadata"]["namespace"], "management")
        self.assertEqual(role["rules"], [{"apiGroups": [""], "resources": ["secrets"],
                         "resourceNames": ["pi-pocket-github-repositories"], "verbs": ["get", "patch"]}])
        self.assertEqual(find(docs, "RoleBinding", "pi-pocket-github-repositories")["subjects"][0]["name"], "pi-pocket-portal")
        # Legacy Helm selections cannot grant workspace access or seed policy.
        legacy = render(*options, "--set", "portal.github.repositories[0]=other/repo")
        self.assertEqual(find(legacy, "Secret", "pi-pocket-github-repositories")["data"], policy["data"])

    def test_github_personal_repository_installation(self):
        options = ["--set", "portal.namespace=management", "--set", "portal.github.clientID=client",
                   "--set", "portal.github.appID=123", "--set", "portal.github.installationID=456",
                   "--set", "portal.github.organization=blahonga", "--set", "portal.github.team=builders",
                   "--set", "portal.github.repositories[0]=cldmnky/repo",
                   "--set", "portal.github.repositories[1]=blahonga/repo",
                   "--set", "portal.github.existingSecret=github-app"]
        for setting in ("--set", "--set-string"):
            docs = render(*options, setting, "portal.github.repositoryInstallations.CLDMNKY=789")
            portal = find(docs, "Deployment", "pi-pocket-portal")["spec"]["template"]["spec"]
            env = {item["name"]: item.get("value") for item in portal["containers"][0]["env"]}
            self.assertEqual(env["GITHUB_ORGANIZATION"], "blahonga")
            self.assertEqual(env["GITHUB_TEAM"], "builders")
            self.assertEqual(env["GITHUB_INSTALLATION_ID"], "456")
            self.assertNotIn("GITHUB_REPOSITORIES", env)
            self.assertEqual(env["GITHUB_REPOSITORY_POLICY_SECRET"], "pi-pocket-github-repositories")
            self.assertEqual(json.loads(env["GITHUB_REPOSITORY_INSTALLATIONS"]), {"cldmnky": 789})
        for extra in (
            ["--set", "portal.github.repositoryInstallations.cldmnky=0"],
            ["--set", "portal.github.repositoryInstallations.cldmnky=-1"],
            ["--set-string", "portal.github.repositoryInstallations.cldmnky=1.5"],
            ["--set-string", "portal.github.repositoryInstallations.cldmnky=not-an-id"],
            ["--set-string", "portal.github.repositoryInstallations.cldmnky=9223372036854775808"],
            ["--set", "portal.github.repositoryInstallations.cldmnky=456"],
            ["--set", "portal.github.repositoryInstallations.cldmnky=789", "--set", "portal.github.repositoryInstallations.other=789"],
            ["--set", "portal.github.repositoryInstallations.cldmnky=789", "--set", "portal.github.repositoryInstallations.CLDMNKY=789"],
            ["--set", "portal.github.repositoryInstallations.blahonga=789"],
            ["--set", "portal.github.repositoryInstallations.bad/owner=789"],
            ["--set", "portal.github.repositoryInstallations=789"],
        ):
            with self.subTest(extra=extra), self.assertRaises(subprocess.CalledProcessError):
                render(*options, *extra)

    def test_admin_elevation_disabled_adds_no_privilege(self):
        """Elevation off (the default) renders no admin object and no new grant."""
        docs = render()
        self.assertFalse(any("admin" in obj["metadata"]["name"] for obj in docs))
        self.assertEqual([obj["kind"] for obj in docs if obj["kind"] == "ClusterRoleBinding"], [])
        for rule in rbac_rules(docs):
            self.assertNotIn("*", rule["resources"])
            self.assertNotIn("*", rule["verbs"])
            self.assertNotIn("*", rule["apiGroups"])
        portal = find(docs, "Deployment", "pi-pocket-portal")["spec"]["template"]["spec"]
        env = {item["name"] for item in portal["containers"][0]["env"]}
        self.assertFalse(any(name.startswith("ADMIN_") for name in env))

    def test_admin_workspace_profile_is_stopped_and_ephemeral(self):
        """deploy/admin-workspace-values.yaml: replicas 0, emptyDir only, no admin grant."""
        docs = render_profile("deploy/admin-workspace-values.yaml", "pi-pocket-admin")
        deployment = find(docs, "Deployment", "pi-pocket-admin")
        self.assertEqual(deployment["spec"]["replicas"], 0)
        self.assertEqual(deployment["spec"]["strategy"]["type"], "Recreate")
        pod = deployment["spec"]["template"]["spec"]
        self.assertIs(pod["hostUsers"], False)
        mounts = {item["name"]: item["mountPath"] for item in pod["containers"][0]["volumeMounts"]}
        self.assertEqual(mounts["workspace-home"], "/workspace/home")
        self.assertEqual(mounts["workspace-repos"], "/workspace/repos")
        self.assertNotIn("workspace", mounts)
        volumes = {item["name"]: item for item in pod["volumes"]}
        self.assertIn("emptyDir", volumes["workspace-home"])
        self.assertIn("emptyDir", volumes["workspace-repos"])
        self.assertFalse(any("persistentVolumeClaim" in item for item in pod["volumes"]))
        self.assertFalse(any(obj["kind"] == "PersistentVolumeClaim" for obj in docs))
        self.assertFalse(any(obj["kind"] == "RoleBinding" and obj["metadata"]["name"] == "pi-pocket-admin-namespace"
                             for obj in docs))
        self.assertFalse(any(obj["kind"] in ("Deployment", "ServiceAccount", "Service", "Secret")
                             and "portal" in obj["metadata"]["name"] for obj in docs))
        self.assertFalse(any(obj["kind"] in ("RoleBinding", "ClusterRoleBinding")
                             and obj["roleRef"]["name"] == "cluster-admin" for obj in docs))

    def test_admin_elevation_renders_controller_and_split_rbac(self):
        """Elevation on: controller, scope-split RBAC, retained session Secret, inert binding."""
        docs = render(*ELEVATION_OPTIONS)
        controller = find(docs, "Deployment", "pi-pocket-admin-controller")
        self.assertEqual(controller["metadata"]["namespace"], "management")
        self.assertEqual(controller["spec"]["replicas"], 1)
        self.assertEqual(controller["spec"]["strategy"]["type"], "Recreate")
        pod = controller["spec"]["template"]["spec"]
        self.assertIs(pod["hostUsers"], False)
        self.assertFalse(pod["containers"][0]["securityContext"]["allowPrivilegeEscalation"])
        self.assertTrue(pod["containers"][0]["securityContext"]["readOnlyRootFilesystem"])
        self.assertEqual(pod["containers"][0]["securityContext"]["capabilities"]["drop"], ["ALL"])
        self.assertEqual(find(docs, "ServiceAccount", "pi-pocket-admin-controller")["metadata"]["namespace"], "management")
        container = controller["spec"]["template"]["spec"]["containers"][0]
        self.assertEqual(container["command"], ["/usr/local/bin/admin-controller"])
        self.assertEqual(container["ports"], [{"name": "health", "containerPort": 8090}])
        self.assertEqual(container["readinessProbe"], {"httpGet": {"path": "/healthz", "port": "health"}})
        self.assertEqual(container["livenessProbe"], {"httpGet": {"path": "/healthz", "port": "health"}})
        self.assertIn({"name": "tmp", "mountPath": "/tmp"}, container["volumeMounts"])
        env = {item["name"]: item.get("value") for item in container["env"]}
        self.assertEqual(env["ADMIN_OPERATORS"], "12345,67890")
        self.assertEqual(env["ADMIN_HEALTH_ADDR"], ":8090")
        self.assertEqual(env["ADMIN_BOOTSTRAP"], "true")
        self.assertEqual(env["ADMIN_RECONCILE_SECONDS"], "5")
        self.assertEqual(container["env"][0]["valueFrom"]["fieldRef"]["fieldPath"], "metadata.namespace")

        # The controller ClusterRole is cluster-scoped only: no Pod, Deployment or
        # Secret rule, so it never gets cluster-wide read of those.
        cluster_role = find(docs, "ClusterRole", "pi-pocket-admin-controller")
        resources = {res for rule in cluster_role["rules"] for res in rule["resources"]}
        self.assertEqual(resources, {"clusterrolebindings", "clusterroles"})
        self.assertEqual(cluster_role["rules"][0]["resourceNames"], ["pi-pocket-admin-cluster-admin"])
        self.assertEqual(cluster_role["rules"][1]["resourceNames"], ["cluster-admin"])
        self.assertEqual(cluster_role["rules"][1]["verbs"], ["bind"])
        find(docs, "ClusterRoleBinding", "pi-pocket-admin-controller")

        # Namespaced authority is scoped by Role + RoleBinding in each namespace.
        role = find(docs, "Role", "pi-pocket-admin-controller")
        self.assertEqual(role["metadata"]["namespace"], "pi-pocket-admin")
        by_resource = {rule["resources"][0]: rule for rule in role["rules"]}
        self.assertEqual(by_resource["deployments"]["resourceNames"], ["pi-pocket-admin"])
        self.assertEqual(by_resource["deployments"]["verbs"], ["get", "patch"])
        self.assertEqual(by_resource["deployments/scale"]["resourceNames"], ["pi-pocket-admin"])
        self.assertEqual(by_resource["deployments/scale"]["verbs"], ["get", "update"])
        self.assertEqual(by_resource["pods"]["verbs"], ["get", "list"])
        self.assertNotIn("resourceNames", by_resource["pods"])
        self.assertEqual(by_resource["secrets"]["resourceNames"], ["pi-pocket-admin-runtime"])
        session_role = find(docs, "Role", "pi-pocket-admin-controller-session")
        self.assertEqual(session_role["metadata"]["namespace"], "management")
        self.assertEqual(session_role["rules"], [{"apiGroups": [""], "resources": ["secrets"],
                                                  "resourceNames": ["pi-pocket-admin-session"], "verbs": ["get", "patch"]}])

        # Portal gets read-only access-data reads, never lifecycle authority.
        portal_role = find(docs, "Role", "pi-pocket-portal-admin")
        self.assertEqual(portal_role["metadata"]["namespace"], "pi-pocket-admin")
        self.assertEqual(portal_role["rules"], [
            {"apiGroups": ["apps"], "resources": ["deployments"],
             "resourceNames": ["pi-pocket-admin"], "verbs": ["get"]},
            {"apiGroups": [""], "resources": ["secrets"],
             "resourceNames": ["pi-pocket-admin-runtime"], "verbs": ["get"]},
        ])
        portal_session = find(docs, "Role", "pi-pocket-portal-admin-session")
        self.assertEqual(portal_session["metadata"]["namespace"], "management")
        self.assertEqual(find(docs, "RoleBinding", "pi-pocket-portal-admin")["subjects"][0]["name"], "pi-pocket-portal")

        # No rule anywhere may fall back to a wildcard resource or verb.
        for rule in rbac_rules(docs):
            self.assertNotIn("*", rule["resources"])
            self.assertNotIn("*", rule["verbs"])
            self.assertNotIn("*", rule["apiGroups"])

        session = find(docs, "Secret", "pi-pocket-admin-session")
        self.assertEqual(session["metadata"]["namespace"], "management")
        self.assertEqual(session["metadata"]["annotations"]["helm.sh/resource-policy"], "keep")
        self.assertEqual(session["data"]["session.json"], "e30=")  # {}
        inert = find(docs, "ClusterRoleBinding", "pi-pocket-admin-cluster-admin")
        self.assertEqual(inert["roleRef"], {"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "cluster-admin"})
        self.assertEqual(inert["subjects"], [])

        # The portal gets the admin configuration but never bootstrap/reconcile/health.
        portal_container = find(docs, "Deployment", "pi-pocket-portal")["spec"]["template"]["spec"]["containers"][0]
        portal_env = {item["name"]: item.get("value") for item in portal_container["env"]}
        for name in ("ADMIN_NAMESPACE", "ADMIN_DEPLOYMENT", "ADMIN_SERVICE_ACCOUNT", "ADMIN_RUNTIME_SECRET",
                     "ADMIN_POCKET_URL", "ADMIN_TERMINAL_URL", "ADMIN_SESSION_SECRET", "ADMIN_CLUSTER_ROLE_BINDING",
                     "ADMIN_CLUSTER_ROLE", "ADMIN_OPERATORS", "ADMIN_DEFAULT_DURATION_SECONDS",
                     "ADMIN_MAX_DURATION_SECONDS", "ADMIN_RECENT_LOGIN_SECONDS", "ADMIN_STARTUP_TIMEOUT_SECONDS"):
            self.assertIn(name, portal_env)
        for name in ("ADMIN_BOOTSTRAP", "ADMIN_RECONCILE_SECONDS", "ADMIN_HEALTH_ADDR"):
            self.assertNotIn(name, portal_env)

    def test_admin_elevation_validation_rejects_unsafe_combinations(self):
        for options in (
            ("--set", "adminElevation.bootstrap=true"),  # rule 5: bootstrap needs enabled
        ):
            with self.subTest(options=options), self.assertRaises(subprocess.CalledProcessError):
                render(*options)
        with self.subTest("empty operators"), self.assertRaises(subprocess.CalledProcessError):
            render(*ELEVATION_TARGET_OPTIONS)
        for options in (
            ("--set", "workspace.mode=admin"),  # rule 7
            ("--set", "portal.authMode=token"),  # rule 4: github only
            ("--set", "adminElevation.bootstrap=false"),  # rule 4: bootstrap required
            ("--set", "adminElevation.namespace="),  # rule 4: target required
            ("--set", "adminElevation.deployment="),
            ("--set", "adminElevation.serviceAccount="),
            ("--set", "adminElevation.runtimeSecret="),
            ("--set", "adminElevation.pocketURL="),
            ("--set", "adminElevation.namespace=test-pocket"),  # rule 4: distinct namespaces
            ("--set", "adminElevation.namespace=management"),
            ("--set", "portal.namespace=test-pocket"),
            ("--set", "adminElevation.defaultDurationSeconds=0"),  # rule 4: durations
            ("--set", "adminElevation.defaultDurationSeconds=3600"),
            ("--set", "adminElevation.maxDurationSeconds=3601"),
            ("--set", "adminElevation.recentLoginSeconds=0"),
            ("--set", "adminElevation.startupTimeoutSeconds=0"),
            ("--set", "adminElevation.reconcileSeconds=0"),
            ("--set", "adminElevation.operators[0]=0"),  # rule 4: numeric GitHub ids
            ("--set", "adminElevation.operators[0]=-1"),
            ("--set-string", "adminElevation.operators[0]=not-an-id"),
            ("--set", "adminElevation.operators[0]=1.5"),
        ):
            with self.subTest(options=options), self.assertRaises(subprocess.CalledProcessError):
                render(*ELEVATION_OPTIONS, *options)

    def test_workspace_mode_validation_rejects_unsafe_combinations(self):
        for options in (
            ("--set", "workspace.mode=weird"),  # rule 1
            ("--set", "persistence.mode=weird"),
            ("--set", "persistence.mode=ephemeral", "--set", "persistence.existingClaim=reuse"),  # rule 3
        ):
            with self.subTest(options=options), self.assertRaises(subprocess.CalledProcessError):
                render(*options)
        for options in (
            ("--set", "replicas=1"),  # rule 2
            ("--set", "persistence.mode=pvc"),
            ("--set", "serviceAccount.namespaceRole=admin"),
            ("--set", "portal.enabled=true", "--set", "ingress.portalHost=portal.example.com"),
            ("--set", "runtimeSecret.existingSecret=shared"),
            ("--set", "ingress.pocketHost="),
            ("--set", "adminElevation.enabled=true"),
        ):
            with self.subTest(options=options), self.assertRaises(subprocess.CalledProcessError):
                render_profile("deploy/admin-workspace-values.yaml", "pi-pocket-admin", *options)

    def test_normal_workspace_never_cluster_admin(self):
        """No render gives the normal or admin workspace a populated cluster-admin grant."""
        renders = [
            render(),
            render_profile("deploy/admin-workspace-values.yaml", "pi-pocket-admin"),
            render(*ELEVATION_OPTIONS),
        ]
        for docs in renders:
            for obj in docs:
                if obj["kind"] not in ("RoleBinding", "ClusterRoleBinding"):
                    continue
                if obj["roleRef"]["name"] != "cluster-admin":
                    continue
                # Any cluster-admin binding must stay inert while idle: it never
                # carries a subject, and never a workspace service account.
                subjects = obj.get("subjects") or []
                self.assertEqual(subjects, [])
                names = [subject.get("name") for subject in subjects]
                self.assertNotIn("pi-pocket", names)
                self.assertNotIn("pi-pocket-admin", names)

    def test_hostusers_and_single_writer_contracts_survive(self):
        for docs, name, replicas in (
            (render(), "pi-pocket", 1),
            (render_profile("deploy/admin-workspace-values.yaml", "pi-pocket-admin"), "pi-pocket-admin", 0),
            (render(*ELEVATION_OPTIONS), "pi-pocket", 1),
        ):
            deployment = find(docs, "Deployment", name)
            pod = deployment["spec"]["template"]["spec"]
            self.assertIs(pod["hostUsers"], False)
            self.assertEqual(deployment["spec"]["strategy"]["type"], "Recreate")
            self.assertEqual(deployment["spec"]["replicas"], replicas)
            self.assertTrue(pod["securityContext"]["runAsNonRoot"])
            self.assertEqual(pod["securityContext"]["runAsUser"], 1000)
        controller = find(render(*ELEVATION_OPTIONS), "Deployment", "pi-pocket-admin-controller")
        self.assertIs(controller["spec"]["template"]["spec"]["hostUsers"], False)
        self.assertEqual(controller["spec"]["strategy"]["type"], "Recreate")
        self.assertEqual(controller["spec"]["replicas"], 1)

    def test_chart_app_version_matches_the_pinned_upstream(self):
        """The chart's appVersion names the pi-pocket the image ships, so a bump cannot update one and miss the other."""
        chart = yaml.safe_load((ROOT / "charts/pi-pocket/Chart.yaml").read_text())
        containerfile = (ROOT / "images/Containerfile").read_text()
        versions = set(re.findall(r"^ARG PI_POCKET_VERSION=(.+)$", containerfile, re.M))
        commits = set(re.findall(r"^ARG PI_POCKET_COMMIT=(.+)$", containerfile, re.M))
        self.assertEqual(len(versions), 1, "PI_POCKET_VERSION is set once, to one value")
        self.assertEqual(len(commits), 1, "PI_POCKET_COMMIT is set once, to one value")
        self.assertEqual(chart["appVersion"], versions.pop(), "appVersion and PI_POCKET_VERSION disagree")
        self.assertRegex(
            commits.pop(),
            r"^[0-9a-f]{40}$",
            "pin a full commit sha: the build checks git rev-parse HEAD against it",
        )

    def test_token_auth_elevation_requires_explicit_opt_in(self):
        options = ["--set", "portal.namespace=management", "--set", "portal.github.clientID=client",
                   "--set", "portal.github.appID=123", "--set", "portal.github.installationID=456",
                   "--set", "portal.github.organization=blahonga", "--set", "portal.github.existingSecret=github-app",
                   "--set", "adminElevation.enabled=true", "--set", "adminElevation.bootstrap=true",
                   "--set", "adminElevation.namespace=admin-ns", "--set", "adminElevation.deployment=pi-pocket-admin",
                   "--set", "adminElevation.serviceAccount=pi-pocket-admin",
                   "--set", "adminElevation.runtimeSecret=pi-pocket-admin-runtime",
                   "--set", "adminElevation.pocketURL=https://pocket-admin.example.com",
                   "--set", "adminElevation.operators[0]=42"]
        # Token authentication is refused unless the opt-in is explicit.
        with self.assertRaises(subprocess.CalledProcessError):
            render(*options, "--set", "portal.authMode=token")
        docs = render(*options, "--set", "portal.authMode=token",
                      "--set", "adminElevation.allowTokenAuth=true")
        portal = find(docs, "Deployment", "pi-pocket-portal")["spec"]["template"]["spec"]
        env = {item["name"]: item.get("value") for item in portal["containers"][0]["env"]}
        self.assertEqual(env["PORTAL_AUTH_MODE"], "token")
        self.assertEqual(env["ADMIN_ALLOW_TOKEN_AUTH"], "true")
        # The controller never receives the portal's auth decision.
        controller = find(docs, "Deployment", "pi-pocket-admin-controller")["spec"]["template"]["spec"]
        controller_env = {item["name"]: item.get("value") for item in controller["containers"][0]["env"]}
        self.assertNotIn("ADMIN_ALLOW_TOKEN_AUTH", controller_env)
        # And the default stays fail-closed.
        docs = render(*options, "--set", "portal.authMode=github")
        portal = find(docs, "Deployment", "pi-pocket-portal")["spec"]["template"]["spec"]
        env = {item["name"]: item.get("value") for item in portal["containers"][0]["env"]}
        self.assertEqual(env["ADMIN_ALLOW_TOKEN_AUTH"], "false")

    def test_reject_unsafe_options(self):
        for options in (("replicas=2",), ("openshift.pocketSCC=privileged",), ("serviceAccount.namespaceRole=cluster-admin",), ("ingress.pocketHost=",), ("ingress.portalHost=",), ("ingress.enabled=false",)):
            with self.subTest(options=options), self.assertRaises(subprocess.CalledProcessError):
                render("--set", options[0])


if __name__ == "__main__":
    unittest.main()
