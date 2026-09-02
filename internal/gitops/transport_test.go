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
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
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
