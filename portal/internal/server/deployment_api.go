package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/cldmnky/pi-pocket8s/portal/internal/kube"
)

// restartAnnotation is the standard kubectl rollout-restart annotation.
const restartAnnotation = "kubectl.kubernetes.io/restartedAt"

const (
	replicasRunning int32 = 1
	replicasStopped int32 = 0
)

// statusView is the deployment status shown to the browser.
type statusView struct {
	Namespace         string `json:"namespace"`
	Deployment        string `json:"deployment"`
	DesiredReplicas   int32  `json:"desiredReplicas"`
	ReadyReplicas     int32  `json:"readyReplicas"`
	AvailableReplicas int32  `json:"availableReplicas"`
	UpdatedReplicas   int32  `json:"updatedReplicas"`
	Running           bool   `json:"running"`
	RestartedAt       string `json:"restartedAt,omitempty"`
	PocketURL         string `json:"pocketUrl"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	deployment, err := s.kube.GetDeployment(r.Context(), s.cfg.Namespace, s.cfg.Deployment)
	if err != nil {
		s.writeKubeError(w, "read deployment", err)
		return
	}
	writeJSON(w, http.StatusOK, s.statusView(deployment))
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	s.scaleDeployment(w, r, replicasRunning, "start deployment")
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	s.scaleDeployment(w, r, replicasStopped, "stop deployment")
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]any{
						restartAnnotation: time.Now().UTC().Format(time.RFC3339),
					},
				},
			},
		},
	})
	if err != nil {
		s.log.Error("encode restart patch", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	deployment, err := s.kube.PatchDeployment(r.Context(), s.cfg.Namespace, s.cfg.Deployment, patch)
	if err != nil {
		s.writeKubeError(w, "restart deployment", err)
		return
	}
	writeJSON(w, http.StatusOK, s.statusView(deployment))
}

// scaleDeployment sets the named deployment's replica count, retrying once on
// a write conflict, then reports the resulting status.
func (s *Server) scaleDeployment(w http.ResponseWriter, r *http.Request, desired int32, action string) {
	scale, err := s.kube.GetDeploymentScale(r.Context(), s.cfg.Namespace, s.cfg.Deployment)
	if err != nil {
		s.writeKubeError(w, "read deployment scale", err)
		return
	}
	if scale.Spec.Replicas != desired {
		scale.Spec.Replicas = desired
		if _, err := s.kube.UpdateDeploymentScale(r.Context(), s.cfg.Namespace, s.cfg.Deployment, scale); err != nil {
			if !kube.IsConflict(err) {
				s.writeKubeError(w, action, err)
				return
			}
			fresh, getErr := s.kube.GetDeploymentScale(r.Context(), s.cfg.Namespace, s.cfg.Deployment)
			if getErr != nil {
				s.writeKubeError(w, "read deployment scale", getErr)
				return
			}
			fresh.Spec.Replicas = desired
			if _, err := s.kube.UpdateDeploymentScale(r.Context(), s.cfg.Namespace, s.cfg.Deployment, fresh); err != nil {
				s.writeKubeError(w, action, err)
				return
			}
		}
	}
	deployment, err := s.kube.GetDeployment(r.Context(), s.cfg.Namespace, s.cfg.Deployment)
	if err != nil {
		s.writeKubeError(w, "read deployment", err)
		return
	}
	writeJSON(w, http.StatusOK, s.statusView(deployment))
}

func (s *Server) statusView(deployment *kube.Deployment) statusView {
	desired := replicasRunning
	if deployment.Spec.Replicas != nil {
		desired = *deployment.Spec.Replicas
	}
	return statusView{
		Namespace:         s.cfg.Namespace,
		Deployment:        s.cfg.Deployment,
		DesiredReplicas:   desired,
		ReadyReplicas:     deployment.Status.ReadyReplicas,
		AvailableReplicas: deployment.Status.AvailableReplicas,
		UpdatedReplicas:   deployment.Status.UpdatedReplicas,
		Running:           desired > 0,
		RestartedAt:       deployment.Spec.Template.Metadata.Annotations[restartAnnotation],
		PocketURL:         s.cfg.PocketURL,
	}
}
