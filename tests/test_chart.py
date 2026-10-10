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

    def test_reject_unsafe_options(self):
        for options in (("replicas=2",), ("openshift.pocketSCC=privileged",), ("serviceAccount.namespaceRole=cluster-admin",), ("ingress.pocketHost=",), ("ingress.portalHost=",), ("ingress.enabled=false",)):
            with self.subTest(options=options), self.assertRaises(subprocess.CalledProcessError):
                render("--set", options[0])


if __name__ == "__main__":
    unittest.main()
