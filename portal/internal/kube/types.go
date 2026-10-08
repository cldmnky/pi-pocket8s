package kube

import (
	"encoding/base64"
	"fmt"
)

// ObjectMeta is the subset of Kubernetes object metadata the portal uses.
type ObjectMeta struct {
	Name            string            `json:"name,omitempty"`
	Namespace       string            `json:"namespace,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
}

// Secret is the core/v1 Secret subset used by the portal. Data values are
// base64 encoded exactly as they appear on the wire.
type Secret struct {
	APIVersion string            `json:"apiVersion,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	Metadata   ObjectMeta        `json:"metadata"`
	Type       string            `json:"type,omitempty"`
	Data       map[string]string `json:"data,omitempty"`
}

// Bytes returns the decoded value stored under key.
func (s *Secret) Bytes(key string) (value []byte, present bool, err error) {
	encoded, ok := s.Data[key]
	if !ok {
		return nil, false, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, true, fmt.Errorf("secret key %q is not valid base64", key)
	}
	return decoded, true, nil
}

// Deployment is the apps/v1 Deployment subset used by the portal.
type Deployment struct {
	APIVersion string           `json:"apiVersion,omitempty"`
	Kind       string           `json:"kind,omitempty"`
	Metadata   ObjectMeta       `json:"metadata"`
	Spec       DeploymentSpec   `json:"spec"`
	Status     DeploymentStatus `json:"status"`
}

// DeploymentSpec is the subset of a Deployment spec the portal reads.
type DeploymentSpec struct {
	Replicas *int32          `json:"replicas,omitempty"`
	Template PodTemplateSpec `json:"template"`
}

// PodTemplateSpec is the subset of a pod template the portal reads.
type PodTemplateSpec struct {
	Metadata ObjectMeta `json:"metadata"`
}

// DeploymentStatus is the subset of a Deployment status the portal reports.
type DeploymentStatus struct {
	Replicas          int32 `json:"replicas,omitempty"`
	ReadyReplicas     int32 `json:"readyReplicas,omitempty"`
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`
	UpdatedReplicas   int32 `json:"updatedReplicas,omitempty"`
}

// Scale is the autoscaling/v1 Scale representation of a Deployment.
type Scale struct {
	APIVersion string      `json:"apiVersion,omitempty"`
	Kind       string      `json:"kind,omitempty"`
	Metadata   ObjectMeta  `json:"metadata"`
	Spec       ScaleSpec   `json:"spec"`
	Status     ScaleStatus `json:"status"`
}

// ScaleSpec is the desired replica count of a Scale object.
type ScaleSpec struct {
	Replicas int32 `json:"replicas"`
}

// ScaleStatus is the observed replica count of a Scale object.
type ScaleStatus struct {
	Replicas int32 `json:"replicas,omitempty"`
}

// apiStatus is the metav1.Status returned with Kubernetes API errors.
type apiStatus struct {
	Message string `json:"message"`
	Reason  string `json:"reason"`
	Code    int    `json:"code"`
}
