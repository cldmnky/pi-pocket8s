package kube

import (
	"encoding/base64"
	"fmt"
)

// ObjectMeta is the subset of Kubernetes object metadata the portal uses.
type ObjectMeta struct {
	Name              string            `json:"name,omitempty"`
	Namespace         string            `json:"namespace,omitempty"`
	UID               string            `json:"uid,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	DeletionTimestamp *string           `json:"deletionTimestamp,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
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
	Selector *LabelSelector  `json:"selector,omitempty"`
	Template PodTemplateSpec `json:"template"`
}

// LabelSelector is the matchLabels subset of a label selector.
type LabelSelector struct {
	MatchLabels map[string]string `json:"matchLabels,omitempty"`
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

// RoleRef is the role a ClusterRoleBinding references.
type RoleRef struct {
	APIGroup string `json:"apiGroup"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
}

// Subject is one ClusterRoleBinding subject.
type Subject struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// ClusterRoleBinding is the rbac.authorization.k8s.io/v1 subset the portal and
// controller use for the one predefined cluster-admin binding.
type ClusterRoleBinding struct {
	APIVersion string     `json:"apiVersion,omitempty"`
	Kind       string     `json:"kind,omitempty"`
	Metadata   ObjectMeta `json:"metadata"`
	RoleRef    RoleRef    `json:"roleRef"`
	Subjects   []Subject  `json:"subjects"`
}

// Pod is the core/v1 subset needed to observe the admin workspace's active Pod.
type Pod struct {
	APIVersion string     `json:"apiVersion,omitempty"`
	Kind       string     `json:"kind,omitempty"`
	Metadata   ObjectMeta `json:"metadata"`
	Status     PodStatus  `json:"status"`
}

// PodStatus carries the fields the controller observes on a Pod.
type PodStatus struct {
	Phase             string            `json:"phase,omitempty"`
	PodIP             string            `json:"podIP,omitempty"`
	StartTime         string            `json:"startTime,omitempty"`
	Conditions        []PodCondition    `json:"conditions,omitempty"`
	ContainerStatuses []ContainerStatus `json:"containerStatuses,omitempty"`
}

// PodCondition is one Pod status condition.
type PodCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

// ContainerStatus is one container's status within a Pod.
type ContainerStatus struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
}

// PodList is a core/v1 PodList subset.
type PodList struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Items      []Pod  `json:"items"`
}
