/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing limitations under
the License.
*/

package gitops

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitopsDialBlocked(t *testing.T) {
	t.Parallel()
	private := net.ParseIP("10.1.2.3")
	loopback := net.ParseIP("127.0.0.1")
	imds := net.ParseIP("169.254.169.254")
	public := net.ParseIP("1.1.1.1")
	require.NotNil(t, private)
	require.NotNil(t, loopback)
	require.NotNil(t, imds)
	require.NotNil(t, public)

	assert.True(t, gitopsDialBlocked(private, false))
	assert.False(t, gitopsDialBlocked(private, true))
	assert.True(t, gitopsDialBlocked(loopback, true), "loopback stays blocked")
	assert.True(t, gitopsDialBlocked(imds, true), "IMDS stays blocked")
	assert.False(t, gitopsDialBlocked(public, false))
	assert.False(t, gitopsDialBlocked(public, true))
}

func TestGitopsDialBlocked_AWSIMDSv6(t *testing.T) {
	t.Parallel()
	imds := net.ParseIP("fd00:ec2::254")
	require.NotNil(t, imds)
	assert.True(t, gitopsDialBlocked(imds, false))
	assert.True(t, gitopsDialBlocked(imds, true), "IPv6 IMDS stays blocked when allowPrivate is set")
}

func TestGitopsSafeTransport_DNSRebindBlocked(t *testing.T) {
	public := net.ParseIP("1.1.1.1")
	imds := net.ParseIP("169.254.169.254")
	require.NotNil(t, public)
	require.NotNil(t, imds)

	var lookups atomic.Int32
	orig := lookupIPAddr
	t.Cleanup(func() { lookupIPAddr = orig })
	lookupIPAddr = func(_ context.Context, _ string) ([]net.IPAddr, error) {
		if lookups.Add(1) == 1 {
			return []net.IPAddr{{IP: public}}, nil
		}
		return []net.IPAddr{{IP: imds}}, nil
	}

	var wroteMu sync.Mutex
	var wroteHeaders bool
	var wroteRequest bool
	client := newGitOpsHTTPClient(false)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://forge.example/api", strings.NewReader(`{"token":"must-not-leak"}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer must-not-leak")
	req.Header.Set("PRIVATE-TOKEN", "must-not-leak")
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteHeaders: func() {
			wroteMu.Lock()
			wroteHeaders = true
			wroteMu.Unlock()
		},
		WroteRequest: func(httptrace.WroteRequestInfo) {
			wroteMu.Lock()
			wroteRequest = true
			wroteMu.Unlock()
		},
	}))

	resp, err := client.Do(req)
	if resp != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSSRFBlocked)
	assert.GreaterOrEqual(t, lookups.Load(), int32(2), "dial must re-resolve so a rebind is visible")
	wroteMu.Lock()
	defer wroteMu.Unlock()
	assert.False(t, wroteHeaders, "must not write Authorization/PRIVATE-TOKEN to the rebound address")
	assert.False(t, wroteRequest, "must not send the request body to the rebound address")
}

func TestGitopsSafeTransport_DNSRebindBlockedIMDSv6AllowPrivate(t *testing.T) {
	private := net.ParseIP("10.1.2.3")
	imds := net.ParseIP("fd00:ec2::254")
	require.NotNil(t, private)
	require.NotNil(t, imds)

	var lookups atomic.Int32
	orig := lookupIPAddr
	t.Cleanup(func() { lookupIPAddr = orig })
	lookupIPAddr = func(_ context.Context, _ string) ([]net.IPAddr, error) {
		if lookups.Add(1) == 1 {
			return []net.IPAddr{{IP: private}}, nil
		}
		return []net.IPAddr{{IP: imds}}, nil
	}

	client := newGitOpsHTTPClient(true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://forge.example/api", strings.NewReader(`{"token":"must-not-leak"}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer must-not-leak")
	req.Header.Set("PRIVATE-TOKEN", "must-not-leak")

	resp, err := client.Do(req)
	if resp != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSSRFBlocked)
	assert.GreaterOrEqual(t, lookups.Load(), int32(2), "dial must re-resolve so a rebind is visible")
}

func TestGitopsSafeTransport_HTTPSProxyPrivateNotSSRF(t *testing.T) {
	// Corporate HTTPS_PROXY is often RFC1918. SSRF is the request host
	// (the forge), not the proxy hop DialContext sees.
	orig := lookupIPAddr
	t.Cleanup(func() { lookupIPAddr = orig })
	lookupIPAddr = func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host == "forge.example" {
			return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
		}
		if ip := net.ParseIP(host); ip != nil {
			return []net.IPAddr{{IP: ip}}, nil
		}
		return nil, errors.New("unexpected lookup")
	}

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "[]")
	}))
	t.Cleanup(proxy.Close)

	dialer := &net.Dialer{Timeout: 2 * time.Second}
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &gitopsSSRFTransport{
			allowPrivate: false,
			base: &http.Transport{
				Proxy: func(*http.Request) (*url.URL, error) {
					return url.Parse(proxy.URL)
				},
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					return gitopsDialContext(ctx, network, addr, false, dialer)
				},
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://forge.example/api", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	if resp != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	require.NoError(t, err, "HTTPS_PROXY to a private hop must not return ErrSSRFBlocked")
}

func TestGitopsSafeTransport_HTTPSProxyPrivateIPNotSSRF(t *testing.T) {
	orig := lookupIPAddr
	t.Cleanup(func() { lookupIPAddr = orig })
	lookupIPAddr = func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host == "forge.example" {
			return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
		}
		if ip := net.ParseIP(host); ip != nil {
			return []net.IPAddr{{IP: ip}}, nil
		}
		return nil, errors.New("unexpected lookup")
	}

	dialer := &net.Dialer{Timeout: 200 * time.Millisecond}
	client := &http.Client{
		Timeout: time.Second,
		Transport: &gitopsSSRFTransport{
			allowPrivate: false,
			base: &http.Transport{
				Proxy: func(*http.Request) (*url.URL, error) {
					return url.Parse("http://10.1.2.3:9")
				},
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					return gitopsDialContext(ctx, network, addr, false, dialer)
				},
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://forge.example/api", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	if resp != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrSSRFBlocked, "RFC1918 HTTPS_PROXY hop is not the SSRF target")
}

func TestGitopsSafeTransport_AllowPrivateBlocksIMDSv6RequestHost(t *testing.T) {
	orig := lookupIPAddr
	t.Cleanup(func() { lookupIPAddr = orig })
	lookupIPAddr = func(_ context.Context, _ string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("fd00:ec2::254")}}, nil
	}

	client := newGitOpsHTTPClient(true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://forge.example/latest/meta-data", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	if resp != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSSRFBlocked, "allowPrivate still blocks fd00:ec2::254 as request host")
}
