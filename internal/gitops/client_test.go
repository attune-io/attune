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
