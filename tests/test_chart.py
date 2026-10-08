#!/usr/bin/env python3
"""Render real Helm manifests and test critical security/lifecycle contracts."""
import json
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
        self.assertEqual(len(ingresses), 2)
        for ingress in ingresses:
            self.assertEqual(ingress["spec"]["ingressClassName"], "openshift-default")
            # No tls stanza without an explicit Secret: an empty one blocks
            # Route creation; edge termination comes from the annotation.
            self.assertNotIn("tls", ingress["spec"])
            self.assertEqual(ingress["metadata"]["annotations"]["route.openshift.io/termination"], "edge")
            self.assertEqual(ingress["metadata"]["annotations"]["route.openshift.io/insecureEdgeTerminationPolicy"], "Redirect")
        docs = render("--set", "ingress.pocketTLSSecret=pocket-tls", "--set", "ingress.portalTLSSecret=portal-tls")
        ingresses = [item for item in docs if item["kind"] == "Ingress"]
        self.assertEqual(ingresses[0]["spec"]["tls"][0]["secretName"], "pocket-tls")
        self.assertEqual(ingresses[1]["spec"]["tls"][0]["secretName"], "portal-tls")

    def test_reject_unsafe_options(self):
        for options in (("replicas=2",), ("openshift.pocketSCC=privileged",), ("serviceAccount.namespaceRole=cluster-admin",), ("ingress.pocketHost=",), ("ingress.portalHost=",), ("ingress.enabled=false",)):
            with self.subTest(options=options), self.assertRaises(subprocess.CalledProcessError):
                render("--set", options[0])


if __name__ == "__main__":
    unittest.main()
