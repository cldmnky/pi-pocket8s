package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/cldmnky/pi-pocket8s/portal/internal/kube"
)

// writeKubeError maps a Kubernetes client error to a sanitized HTTP response.
// Raw API server messages are logged, never returned to the browser.
func (s *Server) writeKubeError(w http.ResponseWriter, action string, err error) {
	switch {
	case kube.IsNotFound(err):
		s.log.Error("kubernetes object not found", "action", action, "error", err)
		writeError(w, http.StatusNotFound, action+": object not found in the cluster")
	case kube.IsConflict(err):
		s.log.Warn("kubernetes conflict", "action", action, "error", err)
		writeError(w, http.StatusConflict, action+": conflict, retry with fresh state")
	case errors.Is(err, context.DeadlineExceeded):
		s.log.Error("kubernetes request timed out", "action", action, "error", err)
		writeError(w, http.StatusGatewayTimeout, action+": timed out")
	case errors.Is(err, context.Canceled):
		s.log.Warn("kubernetes request canceled", "action", action, "error", err)
		writeError(w, http.StatusBadGateway, action+": request canceled")
	default:
		s.log.Error("kubernetes request failed", "action", action, "error", err)
		writeError(w, http.StatusBadGateway, action+": kubernetes API request failed")
	}
}
