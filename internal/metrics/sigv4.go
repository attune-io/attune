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

package metrics

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/go-logr/logr"
)

// sigV4ServiceName is the AWS service name Amazon Managed Prometheus expects.
const sigV4ServiceName = "aps"

// emptyPayloadHash is the hex SHA-256 of an empty body.
const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// SigV4HTTPSigner signs one HTTP request with AWS SigV4.
// SignHTTP edits the request in place and does not read the body.
type SigV4HTTPSigner interface {
	SignHTTP(ctx context.Context, credentials aws.Credentials, r *http.Request, payloadHash string, service string, region string, signingTime time.Time, optFns ...func(*v4.SignerOptions)) error
}

// SigV4Options configures SigV4 signing for Amazon Managed Prometheus.
// The service name is aps. A nil pointer on CollectorOptions means do not sign.
type SigV4Options struct {
	// Region is the AWS region of the AMP workspace. Required when signing.
	Region string
	// RoleARN is an optional IAM role to assume. Empty uses the default chain.
	RoleARN string
	// Credentials, when set with an empty RoleARN, replaces the default chain.
	Credentials aws.CredentialsProvider
	// STSClient, when set with RoleARN, assumes that role without loading AWS config.
	STSClient stscreds.AssumeRoleAPIClient
	// Signer signs same-origin requests. Nil uses the AWS SigV4 signer.
	Signer SigV4HTTPSigner
}

// sigv4Transport signs same-origin Prometheus requests and forwards every
// other host unsigned, with Authorization and X-Amz-* removed.
type sigv4Transport struct {
	base         http.RoundTripper
	origin       *url.URL
	opts         *SigV4Options
	signer       SigV4HTTPSigner
	checkBlocked bool
	logger       logr.Logger

	mu     sync.Mutex
	cached aws.CredentialsProvider
}

func newSigV4Transport(base http.RoundTripper, origin *url.URL, opts *SigV4Options, checkBlocked bool, logger logr.Logger) *sigv4Transport {
	signer := optsSigner(opts)
	return &sigv4Transport{
		base:         base,
		origin:       origin,
		opts:         opts,
		signer:       signer,
		checkBlocked: checkBlocked,
		logger:       logger,
	}
}

func optsSigner(opts *SigV4Options) SigV4HTTPSigner {
	if opts != nil && opts.Signer != nil {
		return opts.Signer
	}
	return v4.NewSigner()
}

func (t *sigv4Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("nil request")
	}
	if t.checkBlocked {
		host := ""
		if req.URL != nil {
			host = req.URL.Hostname()
		}
		if err := rejectBlockedHost(req.Context(), host); err != nil {
			return nil, err
		}
	}
	if req.URL == nil || !sameOrigin(t.origin, req.URL) {
		clone := req.Clone(req.Context())
		stripSignedHeaders(clone.Header)
		return t.base.RoundTrip(clone)
	}
	if t.opts == nil || strings.TrimSpace(t.opts.Region) == "" {
		return nil, fmt.Errorf("AWS region is required")
	}
	signed, err := cloneForSign(req)
	if err != nil {
		return nil, err
	}
	payloadHash, err := hashRequestBody(signed)
	if err != nil {
		return nil, err
	}
	provider, err := t.provider(req.Context())
	if err != nil {
		return nil, err
	}
	creds, err := provider.Retrieve(req.Context())
	if err != nil {
		return nil, fmt.Errorf("prometheus sigv4 credentials: %w", err)
	}
	stripSignedHeaders(signed.Header)
	unsent := dropUnsentHeaders(signed.Header)
	if err := t.signer.SignHTTP(req.Context(), creds, signed, payloadHash, sigV4ServiceName, t.opts.Region, time.Now()); err != nil {
		restoreHeaderKeys(signed.Header, unsent)
		return nil, fmt.Errorf("prometheus sigv4 sign: %w", err)
	}
	restoreHeaderKeys(signed.Header, unsent)
	resp, err := t.base.RoundTrip(signed)
	if err != nil {
		return nil, err
	}
	if resp != nil && resp.StatusCode >= 400 {
		t.logger.Info("prometheus sigv4 request failed", "host", req.URL.Host, "status", resp.StatusCode)
	}
	return resp, nil
}

func (t *sigv4Transport) provider(ctx context.Context) (aws.CredentialsProvider, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cached != nil {
		return t.cached, nil
	}
	provider, err := resolveSigV4Provider(ctx, t.opts)
	if err != nil {
		return nil, err
	}
	t.cached = provider
	return provider, nil
}

func resolveSigV4Provider(ctx context.Context, opts *SigV4Options) (aws.CredentialsProvider, error) {
	if opts == nil {
		return nil, fmt.Errorf("prometheus sigv4 credentials: missing sigv4 options")
	}
	if opts.Credentials != nil && opts.RoleARN == "" {
		return opts.Credentials, nil
	}
	if opts.STSClient != nil && opts.RoleARN != "" {
		return assumeRoleProvider(opts.STSClient, opts.RoleARN), nil
	}
	cfg, err := loadAWSConfig(ctx, opts.Region, opts.RoleARN)
	if err != nil {
		return nil, fmt.Errorf("prometheus sigv4 credentials: %w", err)
	}
	if cfg.Credentials == nil {
		return nil, fmt.Errorf("prometheus sigv4 credentials: AWS config has no credentials")
	}
	return cfg.Credentials, nil
}

// cloneForSign copies the request so SignHTTP cannot edit the caller.
// The caller body is rewound to the same bytes when it had to be read.
func cloneForSign(req *http.Request) (*http.Request, error) {
	clone := req.Clone(req.Context())
	if req.Body == nil || req.Body == http.NoBody {
		return clone, nil
	}
	payload, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, fmt.Errorf("reading prometheus request body: %w", err)
	}
	_ = req.Body.Close()
	rewind := func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(payload)), nil
	}
	body, err := rewind()
	if err != nil {
		return nil, err
	}
	req.Body = body
	req.GetBody = rewind
	req.ContentLength = int64(len(payload))
	cloneBody, err := rewind()
	if err != nil {
		return nil, err
	}
	clone.Body = cloneBody
	clone.GetBody = rewind
	clone.ContentLength = req.ContentLength
	return clone, nil
}

func hashRequestBody(req *http.Request) (string, error) {
	if req.Body == nil || req.Body == http.NoBody {
		req.Body = http.NoBody
		req.ContentLength = 0
		req.GetBody = func() (io.ReadCloser, error) { return http.NoBody, nil }
		return emptyPayloadHash, nil
	}
	payload, err := io.ReadAll(req.Body)
	if err != nil {
		return "", fmt.Errorf("reading prometheus request body: %w", err)
	}
	_ = req.Body.Close()
	sum := sha256.Sum256(payload)
	req.ContentLength = int64(len(payload))
	req.Body = io.NopCloser(bytes.NewReader(payload))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(payload)), nil
	}
	return hex.EncodeToString(sum[:]), nil
}

func stripSignedHeaders(h http.Header) {
	if h == nil {
		return
	}
	for name := range h {
		lower := strings.ToLower(name)
		if lower == "authorization" || strings.HasPrefix(lower, "x-amz-") {
			h.Del(name)
		}
	}
}

// dropUnsentHeaders removes keys with no values and returns them.
// net/http keeps a nil Idempotency-Key as a retry marker and does not
// write that key. Signing the name would not match the request on the wire.
func dropUnsentHeaders(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	var saved http.Header
	for k, v := range h {
		if len(v) != 0 {
			continue
		}
		if saved == nil {
			saved = make(http.Header)
		}
		saved[k] = v
		delete(h, k)
	}
	return saved
}

func restoreHeaderKeys(h, saved http.Header) {
	if h == nil || saved == nil {
		return
	}
	for k, v := range saved {
		h[k] = v
	}
}
