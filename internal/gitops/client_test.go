/*
Copyright 2026.
*/
package gitops

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestGitHubClient_Create(t *testing.T) {
	t.Parallel()
	var sawAuth string
	client := &GitHubClient{
		Token:      "secret-token-xyz",
		Repository: "org/repo",
		HTTP: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			sawAuth = r.Header.Get("Authorization")
			if r.Method == http.MethodGet {
				return &http.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader("[]")),
					Header:     make(http.Header),
				}, nil
			}
			body, _ := json.Marshal(map[string]interface{}{
				"number": 7, "html_url": "https://github.com/org/repo/pull/7",
			})
			return &http.Response{
				StatusCode: 201,
				Body:       io.NopCloser(strings.NewReader(string(body))),
				Header:     make(http.Header),
			}, nil
		}),
	}
	res, err := client.CreateOrUpdate(context.Background(), PRRequest{
		Title: "t", Body: "b", Head: "attune/x", Base: "main",
	})
	require.NoError(t, err)
	assert.Equal(t, 7, res.Number)
	assert.Contains(t, res.URL, "pull/7")
	assert.Equal(t, "Bearer secret-token-xyz", sawAuth)
	// Error paths must not echo token in message when redacted helper used
	assert.Equal(t, "[redacted]", redactToken("secret-token-xyz", "secret-token-xyz"))
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
	// API error path must not surface the raw token from the response body.
	assert.NotContains(t, err.Error(), "super-secret-token")
}
