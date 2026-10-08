// Package kube is a small Kubernetes REST client for the portal's fixed set
// of operations: one named Secret and one named Deployment in one namespace.
// It intentionally exposes no generic resource paths.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ServiceAccountDir is where Kubernetes projects the pod's service account.
const ServiceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// maxResponseBytes bounds responses read from the API server.
const maxResponseBytes = 4 << 20

// mergePatchContentType is the content type used for all portal writes.
const mergePatchContentType = "application/merge-patch+json"

// APIError is a non-2xx response from the Kubernetes API server.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("kubernetes API returned status %d", e.StatusCode)
	}
	return fmt.Sprintf("kubernetes API returned status %d: %s", e.StatusCode, e.Message)
}

// IsConflict reports whether err is a 409 Conflict from the API server.
func IsConflict(err error) bool {
	return hasStatus(err, http.StatusConflict)
}

// IsNotFound reports whether err is a 404 from the API server.
func IsNotFound(err error) bool {
	return hasStatus(err, http.StatusNotFound)
}

func hasStatus(err error, code int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == code
}

// Client performs the portal's Kubernetes operations over a TLS-verified
// connection, reading the service account token for every request so that
// token rotation needs no restart.
type Client struct {
	baseURL   string
	tokenFile string
	http      *http.Client
}

// NewClient builds a client for the API server at baseURL. caFile must hold
// the PEM CA bundle used to verify the API server certificate.
func NewClient(baseURL, caFile, tokenFile string, timeout time.Duration) (*Client, error) {
	if tokenFile == "" {
		return nil, errors.New("service account token file is required")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse kubernetes API URL: %w", err)
	}
	if u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("kubernetes API URL %q must be an absolute https URL", baseURL)
	}
	pemData, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read kubernetes CA bundle: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pemData) {
		return nil, fmt.Errorf("kubernetes CA bundle %s contains no certificates", caFile)
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	transport := &http.Transport{
		TLSClientConfig:       &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	return &Client{
		baseURL:   strings.TrimSuffix(u.String(), "/"),
		tokenFile: tokenFile,
		http:      &http.Client{Transport: transport, Timeout: timeout},
	}, nil
}

// InClusterClient builds a client from the standard in-cluster environment.
func InClusterClient(timeout time.Duration) (*Client, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" {
		return nil, errors.New("KUBERNETES_SERVICE_HOST is not set; the portal must run in a cluster")
	}
	if port == "" {
		port = "443"
	}
	return NewClient(
		"https://"+net.JoinHostPort(host, port),
		filepath.Join(ServiceAccountDir, "ca.crt"),
		filepath.Join(ServiceAccountDir, "token"),
		timeout,
	)
}

// GetSecret reads the named Secret.
func (c *Client) GetSecret(ctx context.Context, namespace, name string) (*Secret, error) {
	var secret Secret
	if err := c.do(ctx, http.MethodGet, secretPath(namespace, name), "", nil, &secret); err != nil {
		return nil, err
	}
	return &secret, nil
}

// PatchSecret applies a JSON merge patch to the named Secret. The patch is
// built by the caller and touches only the keys the portal manages.
func (c *Client) PatchSecret(ctx context.Context, namespace, name string, patch []byte) (*Secret, error) {
	var secret Secret
	if err := c.do(ctx, http.MethodPatch, secretPath(namespace, name), mergePatchContentType, patch, &secret); err != nil {
		return nil, err
	}
	return &secret, nil
}

// GetDeployment reads the named Deployment.
func (c *Client) GetDeployment(ctx context.Context, namespace, name string) (*Deployment, error) {
	var deployment Deployment
	if err := c.do(ctx, http.MethodGet, deploymentPath(namespace, name), "", nil, &deployment); err != nil {
		return nil, err
	}
	return &deployment, nil
}

// PatchDeployment applies a JSON merge patch to the named Deployment.
func (c *Client) PatchDeployment(ctx context.Context, namespace, name string, patch []byte) (*Deployment, error) {
	var deployment Deployment
	if err := c.do(ctx, http.MethodPatch, deploymentPath(namespace, name), mergePatchContentType, patch, &deployment); err != nil {
		return nil, err
	}
	return &deployment, nil
}

// GetDeploymentScale reads the scale subresource of the named Deployment.
func (c *Client) GetDeploymentScale(ctx context.Context, namespace, name string) (*Scale, error) {
	var scale Scale
	if err := c.do(ctx, http.MethodGet, scalePath(namespace, name), "", nil, &scale); err != nil {
		return nil, err
	}
	return &scale, nil
}

// UpdateDeploymentScale writes the scale subresource of the named Deployment.
// The supplied scale must carry the resourceVersion read by the caller so the
// API server can reject conflicting writes.
func (c *Client) UpdateDeploymentScale(ctx context.Context, namespace, name string, scale *Scale) (*Scale, error) {
	payload, err := json.Marshal(scale)
	if err != nil {
		return nil, fmt.Errorf("encode scale: %w", err)
	}
	var updated Scale
	if err := c.do(ctx, http.MethodPut, scalePath(namespace, name), "application/json", payload, &updated); err != nil {
		return nil, err
	}
	return &updated, nil
}

func secretPath(namespace, name string) string {
	return "/api/v1/namespaces/" + url.PathEscape(namespace) + "/secrets/" + url.PathEscape(name)
}

func deploymentPath(namespace, name string) string {
	return "/apis/apps/v1/namespaces/" + url.PathEscape(namespace) + "/deployments/" + url.PathEscape(name)
}

func scalePath(namespace, name string) string {
	return deploymentPath(namespace, name) + "/scale"
}

// do performs one API request, reading the service account token file first.
func (c *Client) do(ctx context.Context, method, path, contentType string, payload []byte, out any) error {
	token, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return fmt.Errorf("read service account token: %w", err)
	}
	bearer := strings.TrimSpace(string(token))
	if bearer == "" {
		return errors.New("service account token is empty")
	}

	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("build kubernetes request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "pi-pocket8s-portal")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("%s %s: read response: %w", method, path, err)
	}
	if len(raw) > maxResponseBytes {
		return fmt.Errorf("%s %s: response exceeds %d bytes", method, path, maxResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		message := strings.TrimSpace(string(raw))
		var status apiStatus
		if json.Unmarshal(raw, &status) == nil && status.Message != "" {
			message = status.Message
		}
		if len(message) > 1024 {
			message = message[:1024]
		}
		return &APIError{StatusCode: resp.StatusCode, Message: message}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}
