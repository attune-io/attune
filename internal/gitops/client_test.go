/*
Copyright 2026.
*/
package gitops

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(code int, v interface{}) *http.Response {
	var body string
	switch t := v.(type) {
	case string:
		body = t
	default:
		b, _ := json.Marshal(v)
		body = string(b)
	}
	return &http.Response{
		StatusCode: code,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestGitHubClient_Create_HeadExists(t *testing.T) {
	t.Parallel()
	var sawAuth string
	var paths []string
	client := &GitHubClient{
		Token:      "secret-token-xyz",
		Repository: "org/repo",
		HTTP: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			sawAuth = r.Header.Get("Authorization")
			paths = append(paths, r.Method+" "+r.URL.Path)
			switch {
			case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pulls"):
				return jsonResp(200, "[]"), nil
			case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/git/ref/heads/"):
				// head already exists
				return jsonResp(200, map[string]interface{}{
					"object": map[string]string{"sha": "abc"},
				}), nil
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
				return jsonResp(201, map[string]interface{}{
					"number": 7, "html_url": "https://github.com/org/repo/pull/7",
				}), nil
			default:
				return jsonResp(500, `{"message":"unexpected `+r.Method+` `+r.URL.Path+`"}`), nil
			}
		}),
	}
	res, err := client.CreateOrUpdate(context.Background(), PRRequest{
		Title: "t", Body: "b", Head: "attune/x", Base: "main",
	})
	require.NoError(t, err)
	assert.Equal(t, 7, res.Number)
	assert.Contains(t, res.URL, "pull/7")
	assert.Equal(t, "Bearer secret-token-xyz", sawAuth)
	assert.Equal(t, "[redacted]", redactToken("secret-token-xyz", "secret-token-xyz"))
	// Must not have attempted bootstrap commit when head exists.
	for _, p := range paths {
		assert.NotContains(t, p, "/git/commits")
		assert.NotContains(t, p, "/git/refs")
	}
}

func TestGitHubClient_Create_BootstrapsMissingHead(t *testing.T) {
	t.Parallel()
	var methods []string
	client := &GitHubClient{
		Token:      "tok",
		Repository: "org/repo",
		HTTP: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			methods = append(methods, r.Method+" "+r.URL.Path)
			path := r.URL.Path
			switch {
			case r.Method == http.MethodGet && strings.Contains(path, "/pulls"):
				return jsonResp(200, "[]"), nil
			case r.Method == http.MethodGet && strings.Contains(path, "/git/ref/heads/attune/x"):
				return jsonResp(404, `{"message":"Not Found"}`), nil
			case r.Method == http.MethodGet && strings.Contains(path, "/git/ref/heads/main"):
				return jsonResp(200, map[string]interface{}{
					"object": map[string]string{"sha": "base-sha"},
				}), nil
			case r.Method == http.MethodGet && strings.Contains(path, "/git/commits/base-sha"):
				return jsonResp(200, map[string]interface{}{
					"tree": map[string]string{"sha": "tree-sha"},
				}), nil
			case r.Method == http.MethodPost && strings.HasSuffix(path, "/git/commits"):
				return jsonResp(201, map[string]string{"sha": "new-commit-sha"}), nil
			case r.Method == http.MethodPost && strings.HasSuffix(path, "/git/refs"):
				return jsonResp(201, map[string]interface{}{
					"ref": "refs/heads/attune/x",
				}), nil
			case r.Method == http.MethodPost && strings.HasSuffix(path, "/pulls"):
				return jsonResp(201, map[string]interface{}{
					"number": 42, "html_url": "https://github.com/org/repo/pull/42",
				}), nil
			default:
				return jsonResp(500, `{"message":"unexpected `+r.Method+` `+path+`"}`), nil
			}
		}),
	}
	res, err := client.CreateOrUpdate(context.Background(), PRRequest{
		Title: "t", Body: "drift table", Head: "attune/x", Base: "main",
	})
	require.NoError(t, err)
	assert.Equal(t, 42, res.Number)
	assert.False(t, res.Updated)
	// Bootstrap sequence must appear.
	joined := strings.Join(methods, "\n")
	assert.Contains(t, joined, "GET /repos/org/repo/git/ref/heads/attune/x")
	assert.Contains(t, joined, "GET /repos/org/repo/git/ref/heads/main")
	assert.Contains(t, joined, "POST /repos/org/repo/git/commits")
	assert.Contains(t, joined, "POST /repos/org/repo/git/refs")
	assert.Contains(t, joined, "POST /repos/org/repo/pulls")
}

func TestGitLabClient_Create_BootstrapsMissingHead(t *testing.T) {
	t.Parallel()
	var methods []string
	client := &GitLabClient{
		Token:   "gl-token",
		Project: "g/p",
		HTTP: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			methods = append(methods, r.Method+" "+r.URL.Path)
			path := r.URL.Path
			switch {
			case r.Method == http.MethodGet && strings.Contains(path, "/merge_requests"):
				return jsonResp(200, "[]"), nil
			case r.Method == http.MethodGet && strings.Contains(path, "/repository/branches/"):
				return jsonResp(404, `{"message":"404 Branch Not Found"}`), nil
			case r.Method == http.MethodPost && strings.Contains(path, "/repository/commits"):
				return jsonResp(201, map[string]string{"id": "c1"}), nil
			case r.Method == http.MethodPost && strings.HasSuffix(path, "/merge_requests"):
				return jsonResp(201, map[string]interface{}{
					"iid": 11, "web_url": "https://gitlab.com/g/p/-/merge_requests/11",
				}), nil
			default:
				return jsonResp(500, `{"message":"unexpected `+r.Method+` `+path+`"}`), nil
			}
		}),
	}
	res, err := client.CreateOrUpdate(context.Background(), PRRequest{
		Title: "t", Body: "b", Head: "attune/x", Base: "main",
	})
	require.NoError(t, err)
	assert.Equal(t, 11, res.Number)
	assert.False(t, res.Updated)
	joined := strings.Join(methods, "\n")
	assert.Contains(t, joined, "repository/commits")
	assert.Contains(t, joined, "merge_requests")
}

func TestGitLabClient_Update(t *testing.T) {
	t.Parallel()
	client := &GitLabClient{
		Token:   "gl-token",
		Project: "g/p",
		HTTP: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodGet {
				body, _ := json.Marshal([]map[string]interface{}{
					{"iid": 3, "web_url": "https://gitlab.com/g/p/-/merge_requests/3"},
				})
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
		}),
	}
	res, err := client.CreateOrUpdate(context.Background(), PRRequest{
		Title: "t", Body: "b", Head: "attune/x", Base: "main",
	})
	require.NoError(t, err)
	assert.True(t, res.Updated)
	assert.Equal(t, 3, res.Number)
}

func TestGitHubClient_UpdateExisting(t *testing.T) {
	t.Parallel()
	var methods []string
	client := &GitHubClient{
		Token:      "tok",
		Repository: "org/repo",
		HTTP: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			methods = append(methods, r.Method)
			if r.Method == http.MethodGet {
				body, _ := json.Marshal([]map[string]interface{}{
					{
						"number": 9, "html_url": "https://github.com/org/repo/pull/9",
						"head": map[string]interface{}{"ref": "attune/x"},
					},
				})
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
		}),
	}
	res, err := client.CreateOrUpdate(context.Background(), PRRequest{
		Title: "t", Body: "b", Head: "attune/x", Base: "main",
	})
	require.NoError(t, err)
	assert.True(t, res.Updated)
	assert.Equal(t, 9, res.Number)
	assert.Contains(t, methods, http.MethodPatch)
}

func TestGitHubClient_MissingConfigAndListError(t *testing.T) {
	t.Parallel()
	_, err := (&GitHubClient{}).CreateOrUpdate(context.Background(), PRRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token and repository required")

	client := &GitHubClient{
		Token:      "super-secret-token",
		Repository: "org/repo",
		HTTP: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 401,
				Body:       io.NopCloser(strings.NewReader(`{"message":"Bad credentials super-secret-token"}`)),
				Header:     make(http.Header),
			}, nil
		}),
	}
	_, err = client.CreateOrUpdate(context.Background(), PRRequest{
		Title: "t", Body: "b", Head: "h", Base: "main",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 401")
	assert.NotContains(t, err.Error(), "super-secret-token")
}

func TestPathEscapeRef(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "heads/attune/x", pathEscapeRef("heads/attune/x"))
	assert.Equal(t, "heads/attune/recommendations-ns-pol", pathEscapeRef("heads/attune/recommendations-ns-pol"))
	// Spaces must be escaped; slashes preserved as separators.
	assert.Equal(t, "heads/foo%20bar", pathEscapeRef("heads/foo bar"))
}

func TestGitHubClient_EnsureHead_RaceRefExists(t *testing.T) {
	t.Parallel()
	var headGets int
	client := &GitHubClient{
		Token:      "tok",
		Repository: "org/repo",
		HTTP: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			path := r.URL.Path
			switch {
			case r.Method == http.MethodGet && strings.Contains(path, "/pulls"):
				return jsonResp(200, "[]"), nil
			case r.Method == http.MethodGet && strings.Contains(path, "/git/ref/heads/attune/x"):
				headGets++
				if headGets == 1 {
					return jsonResp(404, `{"message":"Not Found"}`), nil
				}
				// After concurrent create race: ref exists.
				return jsonResp(200, map[string]interface{}{
					"object": map[string]string{"sha": "other"},
				}), nil
			case r.Method == http.MethodGet && strings.Contains(path, "/git/ref/heads/main"):
				return jsonResp(200, map[string]interface{}{
					"object": map[string]string{"sha": "base-sha"},
				}), nil
			case r.Method == http.MethodGet && strings.Contains(path, "/git/commits/base-sha"):
				return jsonResp(200, map[string]interface{}{
					"tree": map[string]string{"sha": "tree-sha"},
				}), nil
			case r.Method == http.MethodPost && strings.HasSuffix(path, "/git/commits"):
				return jsonResp(201, map[string]string{"sha": "new-commit-sha"}), nil
			case r.Method == http.MethodPost && strings.HasSuffix(path, "/git/refs"):
				return jsonResp(422, `{"message":"Reference already exists"}`), nil
			case r.Method == http.MethodPost && strings.HasSuffix(path, "/pulls"):
				return jsonResp(201, map[string]interface{}{
					"number": 5, "html_url": "https://github.com/org/repo/pull/5",
				}), nil
			default:
				return jsonResp(500, `{"message":"unexpected `+r.Method+` `+path+`"}`), nil
			}
		}),
	}
	res, err := client.CreateOrUpdate(context.Background(), PRRequest{
		Title: "t", Body: "b", Head: "attune/x", Base: "main",
	})
	require.NoError(t, err)
	assert.Equal(t, 5, res.Number)
	assert.GreaterOrEqual(t, headGets, 2)
}

func TestGitLabClient_EnsureHead_FileExistsOnBase_UsesUpdate(t *testing.T) {
	t.Parallel()
	var commitBodies []string
	client := &GitLabClient{
		Token:   "gl-token",
		Project: "g/p",
		HTTP: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			path := r.URL.Path
			switch {
			case r.Method == http.MethodGet && strings.Contains(path, "/merge_requests"):
				return jsonResp(200, "[]"), nil
			case r.Method == http.MethodGet && strings.Contains(path, "/repository/branches/"):
				return jsonResp(404, `{"message":"404 Branch Not Found"}`), nil
			case r.Method == http.MethodPost && strings.Contains(path, "/repository/commits"):
				b, _ := io.ReadAll(r.Body)
				commitBodies = append(commitBodies, string(b))
				if strings.Contains(string(b), `"action":"create"`) {
					return jsonResp(400, `{"message":"A file with this name already exists"}`), nil
				}
				return jsonResp(201, map[string]string{"id": "c2"}), nil
			case r.Method == http.MethodPost && strings.HasSuffix(path, "/merge_requests"):
				return jsonResp(201, map[string]interface{}{
					"iid": 12, "web_url": "https://gitlab.com/g/p/-/merge_requests/12",
				}), nil
			default:
				return jsonResp(500, `{"message":"unexpected `+r.Method+` `+path+`"}`), nil
			}
		}),
	}
	res, err := client.CreateOrUpdate(context.Background(), PRRequest{
		Title: "t", Body: "b", Head: "attune/x", Base: "main",
	})
	require.NoError(t, err)
	assert.Equal(t, 12, res.Number)
	require.Len(t, commitBodies, 2)
	assert.Contains(t, commitBodies[0], `"create"`)
	assert.Contains(t, commitBodies[1], `"update"`)
}

func TestRedactToken_Encodings(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "[redacted]", redactToken("secret-token-xyz", "secret-token-xyz"))
	assert.Equal(t, "plain", redactToken("plain", ""))
	tok := "ghp_a+b/c=d"
	assert.Equal(t, "err [redacted] end", redactToken("err "+tok+" end", tok))
	assert.Equal(t, "q=[redacted]", redactToken("q="+url.QueryEscape(tok), tok))
	assert.Equal(t, "p=[redacted]", redactToken("p="+url.PathEscape(tok), tok))
}
