package kube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

const GitHubBrokerAudience = "pi-pocket-github"

// CheckWorkspaceToken delegates verification to the Kubernetes API. The broker
// accepts only the configured workspace service account, and only tokens with
// the dedicated audience. Normal Kubernetes API tokens are not broker tokens.
func (c *Client) CheckWorkspaceToken(ctx context.Context, token, namespace, serviceAccount string) error {
	if token == "" || len(token) > 16384 {
		return errors.New("invalid workspace token")
	}
	body, _ := json.Marshal(map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "TokenReview", "spec": map[string]any{"token": token, "audiences": []string{GitHubBrokerAudience}}})
	var result struct {
		Status struct {
			Authenticated bool     `json:"authenticated"`
			Audiences     []string `json:"audiences"`
			User          struct {
				Username string `json:"username"`
			} `json:"user"`
		} `json:"status"`
	}
	if err := c.do(ctx, http.MethodPost, "/apis/authentication.k8s.io/v1/tokenreviews", "application/json", body, &result); err != nil {
		return errors.New("workspace token verification unavailable")
	}
	audience := false
	for _, a := range result.Status.Audiences {
		if a == GitHubBrokerAudience {
			audience = true
		}
	}
	if !result.Status.Authenticated || !audience || result.Status.User.Username != "system:serviceaccount:"+namespace+":"+serviceAccount || strings.ContainsAny(token, "\r\n") {
		return errors.New("workspace token denied")
	}
	return nil
}
