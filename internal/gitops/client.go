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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/attune-io/attune/internal/validation"
)

var errGitOpsRedirect = errors.New("redirects are not allowed")

// ErrSSRFBlocked is returned when the GitOps HTTP dial refuses the resolved address.
var ErrSSRFBlocked = errors.New("gitops SSRF blocked")

// PRRequest is the input for create-or-update PR.
type PRRequest struct {
	Title  string
	Body   string
	Head   string // branch name
	Base   string
	Labels []string
}

// PRResult is a successful PR reference.
type PRResult struct {
	URL     string
	Number  int
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
	BaseURL      string // default https://api.github.com
	Token        string
	Repository   string // owner/repo
	HTTP         HTTPDoer
	AllowPrivate bool
}

// GitLabClient implements PullRequestClient for GitLab REST API.
type GitLabClient struct {
	BaseURL      string // default https://gitlab.com/api/v4
	Token        string
	Project      string // URL-encoded path or numeric id path "group/project"
	HTTP         HTTPDoer
	AllowPrivate bool
}

const bootstrapCommitMessage = "chore(attune): bootstrap recommendation branch"

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
		httpClient = newGitOpsHTTPClient(c.AllowPrivate)
	}

	// Find existing open PR for this head branch. GitHub supports head=owner:ref
	// server-side filtering so we do not miss the PR when base has many open PRs
	// (default list page size is 30).
	owner := c.Repository
	if i := strings.IndexByte(c.Repository, '/'); i > 0 {
		owner = c.Repository[:i]
	}
	listURL := fmt.Sprintf("%s/repos/%s/pulls?state=open&base=%s&head=%s&per_page=100",
		base, c.Repository, url.QueryEscape(req.Base),
		url.QueryEscape(owner+":"+req.Head))
	body, code, err := c.doJSON(ctx, httpClient, http.MethodGet, listURL, nil)
	if err != nil {
		return PRResult{}, err
	}
	if code < 200 || code >= 300 {
		return PRResult{}, fmt.Errorf("github list PRs: status %d", code)
	}
	var existing []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if err := json.Unmarshal(body, &existing); err != nil {
		return PRResult{}, fmt.Errorf("github list PRs decode: %w", err)
	}
	if len(existing) > 0 {
		pr := existing[0]
		// Prefer an entry whose head ref matches when the response still includes
		// unrelated PRs (older GitHub or ignored head filter).
		for i := range existing {
			if existing[i].Head.Ref == req.Head {
				pr = existing[i]
				break
			}
		}
		patchURL := fmt.Sprintf("%s/repos/%s/pulls/%d", base, c.Repository, pr.Number)
		payload := map[string]string{"title": req.Title, "body": req.Body}
		_, code, err := c.doJSON(ctx, httpClient, http.MethodPatch, patchURL, payload)
		if err != nil {
			return PRResult{}, err
		}
		if code < 200 || code >= 300 {
			return PRResult{}, fmt.Errorf("github update PR: status %d", code)
		}
		if err := c.applyIssueLabels(ctx, httpClient, base, pr.Number, req.Labels); err != nil {
			return PRResult{}, err
		}
		return PRResult{URL: pr.HTMLURL, Number: pr.Number, Updated: true}, nil
	}

	// Ensure head branch exists (create from base with a bootstrap commit if needed).
	// GitHub rejects PRs when head and base point at the same commit, so we always
	// create an empty commit on a new branch rather than a pure ref to base.
	if err := c.ensureHeadBranch(ctx, httpClient, base, req.Head, req.Base); err != nil {
		return PRResult{}, err
	}

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
		return PRResult{}, fmt.Errorf("github create PR: status %d", code)
	}
	var created struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(respBody, &created); err != nil {
		return PRResult{}, fmt.Errorf("github create PR decode: %w", err)
	}
	if err := c.applyIssueLabels(ctx, httpClient, base, created.Number, req.Labels); err != nil {
		return PRResult{}, err
	}
	return PRResult{URL: created.HTMLURL, Number: created.Number, Updated: false}, nil
}

// applyIssueLabels POSTs configured labels onto the GitHub issue/PR.
// Failures are returned (not swallowed) so reconcile can retry.
func (c *GitHubClient) applyIssueLabels(ctx context.Context, httpClient HTTPDoer, apiBase string, number int, labels []string) error {
	if len(labels) == 0 || number <= 0 {
		return nil
	}
	labURL := fmt.Sprintf("%s/repos/%s/issues/%d/labels", apiBase, c.Repository, number)
	_, code, err := c.doJSON(ctx, httpClient, http.MethodPost, labURL, map[string]interface{}{"labels": labels})
	if err != nil {
		return fmt.Errorf("github apply labels: %w", err)
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("github apply labels: status %d", code)
	}
	return nil
}

// ensureHeadBranch creates req head from base with an empty bootstrap commit when
// the head ref is missing. If the head already exists, this is a no-op.
func (c *GitHubClient) ensureHeadBranch(ctx context.Context, httpClient HTTPDoer, apiBase, head, baseBranch string) error {
	headRefURL := fmt.Sprintf("%s/repos/%s/git/ref/%s", apiBase, c.Repository, pathEscapeRef("heads/"+head))
	_, code, err := c.doJSON(ctx, httpClient, http.MethodGet, headRefURL, nil)
	if err != nil {
		return fmt.Errorf("github check head branch: %w", err)
	}
	if code >= 200 && code < 300 {
		return nil
	}
	if code != http.StatusNotFound {
		return fmt.Errorf("github check head branch: status %d", code)
	}

	// Resolve base SHA.
	baseRefURL := fmt.Sprintf("%s/repos/%s/git/ref/%s", apiBase, c.Repository, pathEscapeRef("heads/"+baseBranch))
	baseBody, code, err := c.doJSON(ctx, httpClient, http.MethodGet, baseRefURL, nil)
	if err != nil {
		return fmt.Errorf("github get base branch: %w", err)
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("github get base branch %q: status %d", baseBranch, code)
	}
	var baseRef struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(baseBody, &baseRef); err != nil {
		return fmt.Errorf("github get base branch decode: %w", err)
	}
	if baseRef.Object.SHA == "" {
		return fmt.Errorf("github get base branch: empty sha")
	}

	// Load commit to get tree SHA for empty commit.
	commitURL := fmt.Sprintf("%s/repos/%s/git/commits/%s", apiBase, c.Repository, baseRef.Object.SHA)
	commitBody, code, err := c.doJSON(ctx, httpClient, http.MethodGet, commitURL, nil)
	if err != nil {
		return fmt.Errorf("github get base commit: %w", err)
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("github get base commit: status %d", code)
	}
	var baseCommit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := json.Unmarshal(commitBody, &baseCommit); err != nil {
		return fmt.Errorf("github get base commit decode: %w", err)
	}
	if baseCommit.Tree.SHA == "" {
		return fmt.Errorf("github get base commit: empty tree sha")
	}

	// Empty commit (same tree, parent = base) so head != base for PR creation.
	newCommitURL := fmt.Sprintf("%s/repos/%s/git/commits", apiBase, c.Repository)
	newCommitPayload := map[string]interface{}{
		"message": bootstrapCommitMessage,
		"tree":    baseCommit.Tree.SHA,
		"parents": []string{baseRef.Object.SHA},
	}
	newCommitBody, code, err := c.doJSON(ctx, httpClient, http.MethodPost, newCommitURL, newCommitPayload)
	if err != nil {
		return fmt.Errorf("github create bootstrap commit: %w", err)
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("github create bootstrap commit: status %d", code)
	}
	var newCommit struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(newCommitBody, &newCommit); err != nil {
		return fmt.Errorf("github create bootstrap commit decode: %w", err)
	}
	if newCommit.SHA == "" {
		return fmt.Errorf("github create bootstrap commit: empty sha")
	}

	// Create head ref.
	createRefURL := fmt.Sprintf("%s/repos/%s/git/refs", apiBase, c.Repository)
	createRefPayload := map[string]string{
		"ref": "refs/heads/" + head,
		"sha": newCommit.SHA,
	}
	_, code, err = c.doJSON(ctx, httpClient, http.MethodPost, createRefURL, createRefPayload)
	if err != nil {
		return fmt.Errorf("github create head branch: %w", err)
	}
	// 422 if another reconciler raced and created the ref; treat as success.
	if code == http.StatusUnprocessableEntity {
		_, checkCode, checkErr := c.doJSON(ctx, httpClient, http.MethodGet, headRefURL, nil)
		if checkErr == nil && checkCode >= 200 && checkCode < 300 {
			return nil
		}
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("github create head branch: status %d", code)
	}
	return nil
}

func (c *GitHubClient) doJSON(ctx context.Context, httpClient HTTPDoer, method, rawURL string, payload interface{}) ([]byte, int, error) {
	if err := validation.GitOpsAPIURLAllowingPrivate(rawURL, c.AllowPrivate); err != nil {
		return nil, 0, fmt.Errorf("gitops api url rejected")
	}
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
		httpClient = newGitOpsHTTPClient(c.AllowPrivate)
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
		payload := map[string]interface{}{
			"title":       req.Title,
			"description": req.Body,
			"labels":      strings.Join(req.Labels, ","),
		}
		_, code, err := c.doJSON(ctx, httpClient, http.MethodPut, patchURL, payload)
		if err != nil {
			return PRResult{}, err
		}
		if code < 200 || code >= 300 {
			return PRResult{}, fmt.Errorf("gitlab update MR: status %d", code)
		}
		return PRResult{URL: existing[0].WebURL, Number: existing[0].IID, Updated: true}, nil
	}

	if err := c.ensureHeadBranch(ctx, httpClient, base, project, req.Head, req.Base); err != nil {
		return PRResult{}, err
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
		return PRResult{}, fmt.Errorf("gitlab create MR: status %d", code)
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

// ensureHeadBranch creates the source branch from base with a bootstrap commit
// when missing. GitLab rejects MRs with no commit delta, so we add a small
// marker file under .attune/ rather than an empty commit.
func (c *GitLabClient) ensureHeadBranch(ctx context.Context, httpClient HTTPDoer, apiBase, projectEscaped, head, baseBranch string) error {
	branchURL := fmt.Sprintf("%s/projects/%s/repository/branches/%s", apiBase, projectEscaped, url.PathEscape(head))
	_, code, err := c.doJSON(ctx, httpClient, http.MethodGet, branchURL, nil)
	if err != nil {
		return fmt.Errorf("gitlab check head branch: %w", err)
	}
	if code >= 200 && code < 300 {
		return nil
	}
	if code != http.StatusNotFound {
		return fmt.Errorf("gitlab check head branch: status %d", code)
	}

	// Create branch + bootstrap file commit in one request (start_branch).
	// Prefer "create"; if the marker already exists on base (prior merge),
	// retry with "update" so re-bootstrap still produces a non-empty delta.
	commitURL := fmt.Sprintf("%s/projects/%s/repository/commits", apiBase, projectEscaped)
	markerContent := "Attune recommendation drift branch.\n\nSee the merge request description for the drift table. Apply template patches via `kubectl attune diff` or your GitOps pipeline.\n"
	for _, action := range []string{"create", "update"} {
		payload := map[string]interface{}{
			"branch":         head,
			"start_branch":   baseBranch,
			"commit_message": bootstrapCommitMessage,
			"actions": []map[string]string{
				{
					"action":    action,
					"file_path": ".attune/RECOMMENDATION_DRIFT.md",
					"content":   markerContent,
				},
			},
		}
		respBody, code, err := c.doJSON(ctx, httpClient, http.MethodPost, commitURL, payload)
		if err != nil {
			return fmt.Errorf("gitlab create head branch: %w", err)
		}
		if code >= 200 && code < 300 {
			return nil
		}
		// Race: branch appeared; treat as success if GET now succeeds.
		if code == http.StatusBadRequest || code == http.StatusConflict {
			_, checkCode, checkErr := c.doJSON(ctx, httpClient, http.MethodGet, branchURL, nil)
			if checkErr == nil && checkCode >= 200 && checkCode < 300 {
				return nil
			}
		}
		// File already on base: try update action once.
		if action == "create" && (code == http.StatusBadRequest || code == http.StatusUnprocessableEntity) {
			lower := strings.ToLower(string(respBody))
			if strings.Contains(lower, "already exists") || strings.Contains(lower, "a file with this name already exists") {
				continue
			}
		}
		return fmt.Errorf("gitlab create head branch: status %d", code)
	}
	return fmt.Errorf("gitlab create head branch: exhausted create/update attempts")
}

func (c *GitLabClient) doJSON(ctx context.Context, httpClient HTTPDoer, method, rawURL string, payload interface{}) ([]byte, int, error) {
	if err := validation.GitOpsAPIURLAllowingPrivate(rawURL, c.AllowPrivate); err != nil {
		return nil, 0, fmt.Errorf("gitops api url rejected")
	}
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

func newGitOpsHTTPClient(allowPrivate bool) *http.Client {
	return &http.Client{
		Timeout:       30 * time.Second,
		Transport:     gitopsSafeTransport(allowPrivate),
		CheckRedirect: gitopsCheckRedirect,
	}
}

func gitopsCheckRedirect(*http.Request, []*http.Request) error {
	return errGitOpsRedirect
}

// lookupIPAddr is the DNS hook used by GitOps SSRF checks. Tests replace it
// to simulate rebinding (allowlisted IP, then IMDS).
var lookupIPAddr = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// gitopsSafeTransport checks the request URL host (the SSRF target) before
// the request is sent. The inner transport may use HTTPS_PROXY; the proxy
// hop itself is not the SSRF target. allowPrivate permits RFC1918/ULA
// forges. Loopback, link-local, unspecified, and AWS IPv6 IMDS stay blocked.
func gitopsSafeTransport(allowPrivate bool) http.RoundTripper {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &gitopsSSRFTransport{
		allowPrivate: allowPrivate,
		base: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return gitopsDialContext(ctx, network, addr, allowPrivate, dialer)
			},
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   10,
		},
	}
}

func gitopsDialContext(ctx context.Context, network, addr string, allowPrivate bool, dialer *net.Dialer) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("gitops dial: invalid address %q: %w", addr, err)
	}
	if validation.GitOpsBlockedHost(host) {
		return nil, fmt.Errorf("%w: host %q is a disallowed address", ErrSSRFBlocked, host)
	}
	ips, err := lookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("gitops dial: DNS resolution failed")
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("gitops dial: DNS resolution failed")
	}
	for _, ip := range ips {
		if gitopsDialBlocked(ip.IP, allowPrivate) {
			return nil, fmt.Errorf("%w: host %q resolved to a disallowed address", ErrSSRFBlocked, host)
		}
	}
	return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
}

type gitopsSSRFTransport struct {
	allowPrivate bool
	base         http.RoundTripper
}

func (t *gitopsSSRFTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Hostname()
	if host == "" {
		return nil, fmt.Errorf("gitops dial: invalid address")
	}
	if validation.GitOpsBlockedHost(host) {
		return nil, fmt.Errorf("%w: host %q is a disallowed address", ErrSSRFBlocked, host)
	}
	ips, err := lookupIPAddr(req.Context(), host)
	if err != nil {
		return nil, fmt.Errorf("gitops dial: DNS resolution failed")
	}
	for _, ip := range ips {
		if gitopsDialBlocked(ip.IP, t.allowPrivate) {
			return nil, fmt.Errorf("%w: host %q resolved to a disallowed address", ErrSSRFBlocked, host)
		}
	}
	return t.base.RoundTrip(req)
}

func gitopsDialBlocked(ip net.IP, allowPrivate bool) bool {
	if allowPrivate {
		return validation.GitOpsAlwaysBlockedIP(ip)
	}
	return validation.GitOpsBlockedIP(ip)
}

func redactToken(s, token string) string {
	if token == "" {
		return s
	}
	// Exact match first, then common encodings that APIs may echo in error bodies.
	s = strings.ReplaceAll(s, token, "[redacted]")
	if enc := url.QueryEscape(token); enc != token {
		s = strings.ReplaceAll(s, enc, "[redacted]")
	}
	if enc := url.PathEscape(token); enc != token {
		s = strings.ReplaceAll(s, enc, "[redacted]")
	}
	return s
}

// pathEscapeRef escapes each path segment of a git ref (e.g. heads/attune/foo)
// so slashes remain path separators for the GitHub git/ref API.
func pathEscapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
