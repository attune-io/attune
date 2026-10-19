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

package weeklyreport

import (
	"bytes"
	"context"
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

const (
	httpTimeout = 30 * time.Second
	drainLimit  = 4 << 10
)

// Poster sends one JSON body. Status is the HTTP status code.
type Poster interface {
	Post(ctx context.Context, rawURL string, body []byte) (int, error)
}

// ErrRejected means the URL or a resolved address failed the allowlist.
// The error text does not include the URL or the address.
var ErrRejected = errors.New("chat webhook rejected")

// ErrRedirect means the server asked for a redirect. Redirects are not followed.
var ErrRedirect = errors.New("chat webhook redirect")

// ErrPostFailed is a transport failure whose text does not include the URL.
var ErrPostFailed = errors.New("chat webhook post failed")

type requestHostKey struct{}

type allowedIPsKey struct{}

// Client is the production poster. It checks the URL, then pin-dials the
// request host addresses it already allowed. An HTTPS_PROXY hop is dialed
// as-is because that hop is not the webhook target.
type Client struct {
	http *http.Client
}

// NewClient builds the production HTTP client.
func NewClient() *Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, ErrPostFailed
			}
			reqHost, _ := ctx.Value(requestHostKey{}).(string)
			if reqHost != "" && !sameHost(host, reqHost) {
				return dialer.DialContext(ctx, network, addr)
			}
			ips, _ := ctx.Value(allowedIPsKey{}).([]net.IP)
			if len(ips) == 0 {
				var allowErr error
				ips, allowErr = allowedIPs(ctx, host)
				if allowErr != nil {
					return nil, ErrRejected
				}
			}
			return dialAllowed(ctx, dialer.DialContext, network, port, ips)
		},
	}
	return &Client{http: &http.Client{
		Timeout:       httpTimeout,
		Transport:     transport,
		CheckRedirect: refuseRedirect,
	}}
}

func refuseRedirect(*http.Request, []*http.Request) error {
	return ErrRedirect
}

// HTTPClient exposes the production client for tests of timeout, proxy, and dial.
func (c *Client) HTTPClient() *http.Client { return c.http }

// Post checks the URL and the request host, then sends the body.
func (c *Client) Post(ctx context.Context, rawURL string, body []byte) (int, error) {
	if err := validation.ChatWebhookURL(rawURL); err != nil {
		return 0, ErrRejected
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, ErrRejected
	}
	ips, err := allowedIPs(ctx, u.Hostname())
	if err != nil {
		return 0, ErrRejected
	}
	ctx = context.WithValue(ctx, requestHostKey{}, u.Hostname())
	ctx = context.WithValue(ctx, allowedIPsKey{}, ips)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return 0, ErrRejected
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, ErrRedirect) {
			return 0, ErrRedirect
		}
		return 0, errPostForLog(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
	return resp.StatusCode, nil
}

func errPostForLog(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Err != nil {
			return fmt.Errorf("%w: %w", ErrPostFailed, ue.Err)
		}
		return ErrPostFailed
	}
	return err
}

func dialAllowed(ctx context.Context, dial func(context.Context, string, string) (net.Conn, error), network, port string, ips []net.IP) (net.Conn, error) {
	var last error
	for _, ip := range ips {
		conn, err := dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	if last == nil {
		return nil, ErrPostFailed
	}
	return nil, last
}

func sameHost(a, b string) bool {
	return strings.EqualFold(strings.TrimSuffix(a, "."), strings.TrimSuffix(b, "."))
}

func allowedIPs(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if validation.GitOpsBlockedIP(ip) {
			return nil, ErrRejected
		}
		return []net.IP{ip}, nil
	}
	found, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(found) == 0 {
		return nil, ErrRejected
	}
	ips := make([]net.IP, 0, len(found))
	for _, ip := range found {
		if validation.GitOpsBlockedIP(ip.IP) {
			return nil, ErrRejected
		}
		ips = append(ips, ip.IP)
	}
	return ips, nil
}
