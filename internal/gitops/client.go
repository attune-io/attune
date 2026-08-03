/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gitops

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// PRRequest is the input for create-or-update PR.
type PRRequest struct {
	Title string
	Body  string
	Head  string // branch name
	Base  string
	Labels []string
}

// PRResult is a successful PR reference.
type PRResult struct {
	URL    string
	Number int
	Updated bool
}

// PullRequestClient opens or updates a pull/merge request.
// Implementations must never log the token.
type PullRequestClient interface {
	CreateOrUpdate(ctx context.Context, req PRRequest) (PRResult, error)
}

// HTTPDoer is the subset of http.Client used for tests.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// GitHubClient implements PullRequestClient for GitHub REST API.
type GitHubClient struct {
	BaseURL    string // default https://api.github.com
	Token      string
	Repository string // owner/repo
	HTTP       HTTPDoer
}

// GitLabClient implements PullRequestClient for GitLab REST API.
type GitLabClient struct {
	BaseURL    string // default https://gitlab.com/api/v4
	Token      string
	Project    string // URL-encoded path or numeric id path "group/project"
	HTTP       HTTPDoer
}

func (c *GitHubClient) CreateOrUpdate(ctx context.Context, req PRRequest) (PRResult, error) {
	if c.Token == "" || c.Repository == "" {
		return PRResult{}, fmt.Errorf("github: token and repository required")
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	// Find existing open PR with same head branch (filter client-side).
	listURL := fmt.Sprintf("%s/repos/%s/pulls?state=open&base=%s", base, c.Repository, url.QueryEscape(req.Base))
	body, code, err := c.doJSON(ctx, httpClient, http.MethodGet, listURL, nil)
	if err != nil {
		return PRResult{}, err
	}
	if code < 200 || code >= 300 {
		return PRResult{}, fmt.Errorf("github list PRs: status %d", code)
	}
	var existing []struct {
		Number int    `json:"number"`
		HTMLURL string `json:"html_url"`
		Head   struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if err := json.Unmarshal(body, &existing); err != nil {
		return PRResult{}, fmt.Errorf("github list PRs decode: %w", err)
	}
	for _, pr := range existing {
		if pr.Head.Ref == req.Head {
			// Update body/title
			patchURL := fmt.Sprintf("%s/repos/%s/pulls/%d", base, c.Repository, pr.Number)
			payload := map[string]string{"title": req.Title, "body": req.Body}
			_, code, err := c.doJSON(ctx, httpClient, http.MethodPatch, patchURL, payload)
			if err != nil {
				return PRResult{}, err
			}
			if code < 200 || code >= 300 {
				return PRResult{}, fmt.Errorf("github update PR: status %d", code)
			}
			return PRResult{URL: pr.HTMLURL, Number: pr.Number, Updated: true}, nil
		}
	}

	// Create: GitHub requires the head branch to exist. We create a PR that
	// documents drift only; teams apply patches in CI. If the branch is
	// missing, return a clear error so status shows Failed with message.
	createURL := fmt.Sprintf("%s/repos/%s/pulls", base, c.Repository)
	payload := map[string]interface{}{
		"title": req.Title,
		"body":  req.Body,
		"head":  req.Head,
		"base":  req.Base,
	}
	respBody, code, err := c.doJSON(ctx, httpClient, http.MethodPost, createURL, payload)
	if err != nil {
		return PRResult{}, err
	}
	if code < 200 || code >= 300 {
		return PRResult{}, fmt.Errorf("github create PR: status %d: %s", code, redactToken(string(respBody), c.Token))
	}
	var created struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(respBody, &created); err != nil {
		return PRResult{}, fmt.Errorf("github create PR decode: %w", err)
	}
	// Labels (best-effort)
	if len(req.Labels) > 0 && created.Number > 0 {
		labURL := fmt.Sprintf("%s/repos/%s/issues/%d/labels", base, c.Repository, created.Number)
		_, _, _ = c.doJSON(ctx, httpClient, http.MethodPost, labURL, map[string]interface{}{"labels": req.Labels})
	}
	return PRResult{URL: created.HTMLURL, Number: created.Number, Updated: false}, nil
}

func (c *GitHubClient) doJSON(ctx context.Context, httpClient HTTPDoer, method, rawURL string, payload interface{}) ([]byte, int, error) {
	var rdr io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func (c *GitLabClient) CreateOrUpdate(ctx context.Context, req PRRequest) (PRResult, error) {
	if c.Token == "" || c.Project == "" {
		return PRResult{}, fmt.Errorf("gitlab: token and project required")
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://gitlab.com/api/v4"
	}
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	project := url.PathEscape(c.Project)

	listURL := fmt.Sprintf("%s/projects/%s/merge_requests?state=opened&source_branch=%s",
		base, project, url.QueryEscape(req.Head))
	body, code, err := c.doJSON(ctx, httpClient, http.MethodGet, listURL, nil)
	if err != nil {
		return PRResult{}, err
	}
	if code < 200 || code >= 300 {
		return PRResult{}, fmt.Errorf("gitlab list MRs: status %d", code)
	}
	var existing []struct {
		IID    int    `json:"iid"`
		WebURL string `json:"web_url"`
	}
	if err := json.Unmarshal(body, &existing); err != nil {
		return PRResult{}, fmt.Errorf("gitlab list MRs decode: %w", err)
	}
	if len(existing) > 0 {
		patchURL := fmt.Sprintf("%s/projects/%s/merge_requests/%d", base, project, existing[0].IID)
		payload := map[string]string{"title": req.Title, "description": req.Body}
		_, code, err := c.doJSON(ctx, httpClient, http.MethodPut, patchURL, payload)
		if err != nil {
			return PRResult{}, err
		}
		if code < 200 || code >= 300 {
			return PRResult{}, fmt.Errorf("gitlab update MR: status %d", code)
		}
		return PRResult{URL: existing[0].WebURL, Number: existing[0].IID, Updated: true}, nil
	}

	createURL := fmt.Sprintf("%s/projects/%s/merge_requests", base, project)
	payload := map[string]interface{}{
		"title":         req.Title,
		"description":   req.Body,
		"source_branch": req.Head,
		"target_branch": req.Base,
		"labels":        strings.Join(req.Labels, ","),
	}
	respBody, code, err := c.doJSON(ctx, httpClient, http.MethodPost, createURL, payload)
	if err != nil {
		return PRResult{}, err
	}
	if code < 200 || code >= 300 {
		return PRResult{}, fmt.Errorf("gitlab create MR: status %d: %s", code, redactToken(string(respBody), c.Token))
	}
	var created struct {
		IID    int    `json:"iid"`
		WebURL string `json:"web_url"`
	}
	if err := json.Unmarshal(respBody, &created); err != nil {
		return PRResult{}, fmt.Errorf("gitlab create MR decode: %w", err)
	}
	return PRResult{URL: created.WebURL, Number: created.IID, Updated: false}, nil
}

func (c *GitLabClient) doJSON(ctx context.Context, httpClient HTTPDoer, method, rawURL string, payload interface{}) ([]byte, int, error) {
	var rdr io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("PRIVATE-TOKEN", c.Token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func redactToken(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "[redacted]")
}
