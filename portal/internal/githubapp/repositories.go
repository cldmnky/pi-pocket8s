package githubapp

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// CheckOwner is a fresh org-level check, independent of an optional team gate.
// A team maintainer is NOT an organization owner.
func (c *Client) CheckOwner(ctx context.Context, login string) error {
	if !ownerPattern.MatchString(login) {
		return ErrDenied
	}
	token, err := c.installationToken(ctx, "membership")
	if err != nil {
		return err
	}
	var member struct {
		State string `json:"state"`
		Role  string `json:"role"`
	}
	if err := c.api(ctx, http.MethodGet, "/orgs/"+url.PathEscape(c.options.Organization)+"/memberships/"+url.PathEscape(login), token.Value, nil, &member); err != nil {
		return err
	}
	if member.State != "active" || member.Role != "admin" {
		return ErrDenied
	}
	return nil
}

// ValidateRepositories validates a portal policy, including installation owner
// routing. Empty explicitly denies all. It does not grant access by itself.
func (c *Client) ValidateRepositories(repos []string) ([]string, error) {
	if len(repos) > 500 {
		return nil, errors.New("select at most 500 repositories")
	}
	out := make([]string, 0, len(repos))
	seen := map[string]bool{}
	for _, repo := range repos {
		repo = strings.ToLower(strings.TrimSpace(repo))
		parts := strings.Split(repo, "/")
		if len(parts) != 2 || !ownerPattern.MatchString(parts[0]) || !namePattern.MatchString(parts[1]) || strings.HasSuffix(parts[1], ".git") {
			return nil, errors.New("repositories must be owner/name without .git")
		}
		if !strings.EqualFold(parts[0], c.options.Organization) && c.options.RepositoryInstallations[parts[0]] <= 0 {
			return nil, errors.New("repository owner has no configured App installation")
		}
		if seen[repo] {
			return nil, errors.New("duplicate repository")
		}
		seen[repo] = true
		out = append(out, repo)
	}
	sort.Strings(out)
	return out, nil
}

// MintRepositoryToken is called ONLY after the broker checks the live policy.
// Unlike the legacy static-policy method, no Helm repository list is consulted.
func (c *Client) MintRepositoryToken(ctx context.Context, repo string) (Token, error) {
	repos, err := c.ValidateRepositories([]string{repo})
	if err != nil {
		return Token{}, ErrDenied
	}
	return c.installationToken(ctx, repos[0])
}

// AvailableRepositories enumerates the App's access, not the human's user token.
// Pagination is bounded; fail rather than silently returning a truncated list.
func (c *Client) AvailableRepositories(ctx context.Context) ([]string, error) {
	owners := map[string]bool{strings.ToLower(c.options.Organization): true}
	for owner := range c.options.RepositoryInstallations {
		owners[owner] = true
	}
	out := []string{}
	seen := map[string]bool{}
	for owner := range owners {
		token, err := c.installationToken(ctx, "catalog:"+owner)
		if err != nil {
			return nil, err
		}
		for page := 1; ; page++ {
			if page > 100 {
				return nil, errors.New("repository catalog exceeds pagination limit")
			}
			var result struct {
				Repositories []struct {
					FullName string `json:"full_name"`
				} `json:"repositories"`
			}
			path := "/installation/repositories?per_page=100&page=" + strconv.Itoa(page)
			if err := c.api(ctx, http.MethodGet, path, token.Value, nil, &result); err != nil {
				return nil, err
			}
			for _, repo := range result.Repositories {
				normalized, err := c.ValidateRepositories([]string{repo.FullName})
				if err != nil || !strings.HasPrefix(normalized[0], owner+"/") {
					return nil, ErrDenied
				}
				if !seen[normalized[0]] {
					out = append(out, normalized[0])
					seen[normalized[0]] = true
				}
			}
			if len(result.Repositories) < 100 {
				break
			}
		}
	}
	sort.Strings(out)
	return out, nil
}
