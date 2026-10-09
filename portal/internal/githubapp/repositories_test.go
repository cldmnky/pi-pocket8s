package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPortalRepositoryCatalogOwnerAndValidation(t *testing.T) {
	options := testOptions(t)
	options.Repositories = nil
	options.RepositoryInstallations = map[string]int64{"personal": 789}
	c, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/456":
			fmt.Fprint(w, `{"account":{"login":"example"}}`)
		case "/app/installations/789":
			fmt.Fprint(w, `{"account":{"login":"personal"}}`)
		case "/app/installations/456/access_tokens", "/app/installations/789/access_tokens":
			var payload struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			json.NewDecoder(r.Body).Decode(&payload)
			if len(payload.Repositories) != 0 {
				t.Error("catalog or membership was repository scoped")
			}
			if len(payload.Permissions) != 1 || (payload.Permissions["metadata"] != "read" && payload.Permissions["members"] != "read") {
				t.Errorf("unexpected privilege %v", payload.Permissions)
			}
			token := "catalog-org"
			if strings.Contains(r.URL.Path, "789") {
				token = "catalog-personal"
			}
			if payload.Permissions["members"] == "read" {
				token = "membership"
			}
			json.NewEncoder(w).Encode(map[string]any{"token": token, "expires_at": time.Now().Add(time.Hour)})
		case "/orgs/example/memberships/alice":
			fmt.Fprint(w, `{"state":"active","role":"admin"}`)
		case "/orgs/example/memberships/bob":
			fmt.Fprint(w, `{"state":"active","role":"member"}`)
		case "/installation/repositories":
			repos := []map[string]string{}
			if r.Header.Get("Authorization") == "Bearer catalog-personal" {
				repos = append(repos, map[string]string{"full_name": "personal/private"})
			} else if r.URL.Query().Get("page") == "1" {
				for i := 0; i < 100; i++ {
					repos = append(repos, map[string]string{"full_name": fmt.Sprintf("example/repo%d", i)})
				}
			} else {
				repos = append(repos, map[string]string{"full_name": "example/last"})
			}
			json.NewEncoder(w).Encode(map[string]any{"repositories": repos})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer api.Close()
	c.apiURL = api.URL
	ctx := context.Background()
	if err := c.CheckOwner(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckOwner(ctx, "bob"); err == nil {
		t.Fatal("member promoted to org owner")
	}
	repos, err := c.AvailableRepositories(ctx)
	if err != nil || len(repos) != 102 {
		t.Fatalf("catalog count %d err %v", len(repos), err)
	}
	for _, invalid := range [][]string{{"other/repo"}, {"personal/*"}, {"personal/repo.git"}, {"personal/a", "PERSONAL/A"}, {"personal/../bad"}} {
		if _, err := c.ValidateRepositories(invalid); err == nil {
			t.Fatalf("invalid policy accepted %v", invalid)
		}
	}
	if repos, err := c.ValidateRepositories([]string{}); err != nil || repos == nil {
		t.Fatal("empty deny-all invalid")
	}
	normalized, err := c.ValidateRepositories([]string{"Personal/Private", "example/repo"})
	if err != nil || normalized[1] != "personal/private" {
		t.Fatalf("normalization %v %v", normalized, err)
	}
}
