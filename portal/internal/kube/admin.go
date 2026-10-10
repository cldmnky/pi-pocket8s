// Cluster-scoped and admin-workspace operations for the cluster-admin
// elevation lifecycle. Every path is fixed by configuration; there is no
// generic resource access here either.
package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// GetClusterRoleBinding reads the one predefined cluster-admin binding.
func (c *Client) GetClusterRoleBinding(ctx context.Context, name string) (*ClusterRoleBinding, error) {
	var binding ClusterRoleBinding
	if err := c.do(ctx, http.MethodGet, clusterRoleBindingPath(name), "", nil, &binding); err != nil {
		return nil, err
	}
	return &binding, nil
}

// PatchClusterRoleBinding applies a JSON merge patch to the named binding.
func (c *Client) PatchClusterRoleBinding(ctx context.Context, name string, patch []byte) (*ClusterRoleBinding, error) {
	var binding ClusterRoleBinding
	if err := c.do(ctx, http.MethodPatch, clusterRoleBindingPath(name), mergePatchContentType, patch, &binding); err != nil {
		return nil, err
	}
	return &binding, nil
}

// ListPods lists Pods in one namespace, optionally filtered by label selector.
// Only metadata and status are decoded; nothing else is read.
func (c *Client) ListPods(ctx context.Context, namespace, labelSelector string) (*PodList, error) {
	path := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods"
	if labelSelector != "" {
		path += "?labelSelector=" + url.QueryEscape(labelSelector)
	}
	var pods PodList
	if err := c.do(ctx, http.MethodGet, path, "", nil, &pods); err != nil {
		return nil, err
	}
	return &pods, nil
}

func clusterRoleBindingPath(name string) string {
	return "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/" + url.PathEscape(name)
}

// BindingSubjectsPatch renders the merge patch that sets the binding's subjects
// to exactly the given service account (or to none, when namespace is empty).
func BindingSubjectsPatch(namespace, serviceAccount string) ([]byte, error) {
	subjects := []Subject{}
	if namespace != "" || serviceAccount != "" {
		if namespace == "" || serviceAccount == "" {
			return nil, fmt.Errorf("binding subject needs both namespace and service account")
		}
		subjects = append(subjects, Subject{Kind: "ServiceAccount", Name: serviceAccount, Namespace: namespace})
	}
	patch, err := json.Marshal(map[string]any{"subjects": subjects})
	if err != nil {
		return nil, fmt.Errorf("encode binding patch: %w", err)
	}
	return patch, nil
}

// BindingOwnedBy reports whether a binding is the inert predefined one the
// controller manages: roleRef cluster-admin with either no subjects or exactly
// the configured admin service account. Any other subject means the object is
// not ours to mutate.
func BindingOwnedBy(binding *ClusterRoleBinding, clusterRole, namespace, serviceAccount string) bool {
	if binding == nil {
		return false
	}
	if binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != clusterRole {
		return false
	}
	if len(binding.Subjects) == 0 {
		return true
	}
	if len(binding.Subjects) != 1 {
		return false
	}
	subject := binding.Subjects[0]
	return subject.Kind == "ServiceAccount" && subject.Name == serviceAccount && subject.Namespace == namespace
}

// BindingHasSubject reports whether the binding currently grants the configured
// admin service account.
func BindingHasSubject(binding *ClusterRoleBinding, namespace, serviceAccount string) bool {
	if binding == nil || len(binding.Subjects) != 1 {
		return false
	}
	subject := binding.Subjects[0]
	return subject.Kind == "ServiceAccount" && subject.Name == serviceAccount && subject.Namespace == namespace
}

// ServiceAccountUsername renders the Kubernetes username of a service account,
// the form a TokenReview returns.
func ServiceAccountUsername(namespace, name string) string {
	return "system:serviceaccount:" + namespace + ":" + name
}

// OwnerLoginURLKey and OwnerLoginPodUIDKey are the admin runtime Secret keys the
// entrypoint publishes and the controller clears.
const (
	OwnerLoginURLKey    = "owner-login-url"
	OwnerLoginPodUIDKey = "owner-login-pod-uid"
)

// ClearOwnerLoginPatch renders a merge patch that removes the published owner
// sign-in metadata. Both keys are removed in one patch so a stale link cannot
// outlive its Pod.
func ClearOwnerLoginPatch() ([]byte, error) {
	patch, err := json.Marshal(map[string]any{"data": map[string]any{OwnerLoginURLKey: nil, OwnerLoginPodUIDKey: nil}})
	if err != nil {
		return nil, fmt.Errorf("encode owner-login cleanup patch: %w", err)
	}
	return patch, nil
}

// SecretString reads a Secret key as a trimmed string.
func SecretString(secret *Secret, key string) (string, bool, error) {
	raw, present, err := secret.Bytes(key)
	if err != nil || !present {
		return "", present, err
	}
	return strings.TrimSpace(string(raw)), true, nil
}
