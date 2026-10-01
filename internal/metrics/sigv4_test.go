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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type countingProvider struct {
	n     int
	creds aws.Credentials
	err   error
}

func (p *countingProvider) Retrieve(context.Context) (aws.Credentials, error) {
	p.n++
	if p.err != nil {
		return aws.Credentials{}, p.err
	}
	return p.creds, nil
}

type recordingSigner struct {
	calls   int
	hosts   []string
	queries []string
	headers []http.Header
	hashes  []string
}

func (s *recordingSigner) SignHTTP(_ context.Context, _ aws.Credentials, r *http.Request, payloadHash, service, region string, _ time.Time, _ ...func(*v4.SignerOptions)) error {
	s.calls++
	if r != nil && r.URL != nil {
		s.hosts = append(s.hosts, r.URL.Host)
		s.queries = append(s.queries, r.URL.RawQuery)
	}
	if r != nil {
		s.headers = append(s.headers, r.Header.Clone())
	}
	s.hashes = append(s.hashes, payloadHash)
	if service != sigV4ServiceName {
		return errSigV4Service(service)
	}
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/"+region+"/"+service+"/aws4_request")
	r.Header.Set("X-Amz-Security-Token", "session-token")
	r.Header.Set("X-Amz-Content-Sha256", payloadHash)
	return nil
}

type sigV4ServiceError string

func (e sigV4ServiceError) Error() string { return "unexpected sigv4 service " + string(e) }

func errSigV4Service(service string) error { return sigV4ServiceError(service) }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type errSigner struct {
	headers []http.Header
}

func (s *errSigner) SignHTTP(_ context.Context, _ aws.Credentials, r *http.Request, _, _, _ string, _ time.Time, _ ...func(*v4.SignerOptions)) error {
	if r != nil {
		s.headers = append(s.headers, r.Header.Clone())
	}
	return errors.New("sign failed")
}

func signedHeaderNames(auth string) []string {
	const marker = "SignedHeaders="
	i := strings.Index(auth, marker)
	if i < 0 {
		return nil
	}
	rest := auth[i+len(marker):]
	if j := strings.Index(rest, ","); j >= 0 {
		rest = rest[:j]
	}
	if rest == "" {
		return nil
	}
	return strings.Split(rest, ";")
}

// delegatingSigner records the payload hash, then calls the real signer.
type delegatingSigner struct {
	inner  SigV4HTTPSigner
	hashes []string
}

func (s *delegatingSigner) SignHTTP(ctx context.Context, creds aws.Credentials, r *http.Request, payloadHash, service, region string, signingTime time.Time, optFns ...func(*v4.SignerOptions)) error {
	s.hashes = append(s.hashes, payloadHash)
	return s.inner.SignHTTP(ctx, creds, r, payloadHash, service, region, signingTime, optFns...)
}

type stubAssumeRole struct {
	calls int
	arn   string
}

func (s *stubAssumeRole) AssumeRole(_ context.Context, params *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	s.calls++
	if params != nil {
		s.arn = aws.ToString(params.RoleArn)
	}
	exp := time.Now().Add(time.Hour)
	return &sts.AssumeRoleOutput{
		Credentials: &ststypes.Credentials{
			AccessKeyId:     aws.String("AKIDEXAMPLE"),
			SecretAccessKey: aws.String("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"),
			SessionToken:    aws.String("session-token"),
			Expiration:      &exp,
		},
	}, nil
}

func TestLoadAWSConfig_EmptyRegionIgnoresEnv(t *testing.T) {
	t.Setenv("AWS_REGION", "us-west-2")
	t.Setenv("AWS_DEFAULT_REGION", "eu-west-1")
	for _, region := range []string{"", " ", "\t"} {
		_, err := loadAWSConfig(context.Background(), region, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "AWS region is required")
		assert.NotContains(t, err.Error(), "loading AWS config")
	}
}

func TestNewCloudWatchCollector_EmptyRegion(t *testing.T) {
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_DEFAULT_REGION", "us-east-1")
	_, err := NewCloudWatchCollector(context.Background(), " ", "prod", "", logr.Discard())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AWS region is required")
	assert.NotContains(t, err.Error(), "loading AWS config")
}

func TestPrometheusCollector_SigV4UnsetDoesNotSign(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedInstantResponse()))
	}))
	defer server.Close()

	collector, err := NewPrometheusCollectorWithOptions(server.URL, logr.Discard(), nil, server.Client().Transport)
	require.NoError(t, err)
	_, err = collector.Query(context.Background(), "up", time.Now())
	require.NoError(t, err)
	assert.Empty(t, got.Get("Authorization"))
	assert.Empty(t, got.Get("X-Amz-Security-Token"))
}

func TestPrometheusCollector_SigV4SignsPostBody(t *testing.T) {
	const secret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	var got http.Header
	var gotHost string
	var body []byte
	var rawQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		gotHost = r.Host
		rawQuery = r.URL.RawQuery
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedInstantResponse()))
	}))
	defer server.Close()

	signer := &delegatingSigner{inner: v4.NewSigner()}
	opts := &CollectorOptions{
		Headers:         map[string]string{"X-Scope-OrgID": "tenant-a"},
		QueryParameters: map[string]string{"dedup": "true"},
		BearerToken:     "super-secret-bearer",
		SigV4: &SigV4Options{
			Region: "us-east-1",
			Credentials: credentials.NewStaticCredentialsProvider(
				"AKIDEXAMPLE", secret, "session-token",
			),
			Signer: signer,
		},
	}
	collector, err := NewPrometheusCollectorWithOptions(server.URL, logr.Discard(), opts, server.Client().Transport)
	require.NoError(t, err)
	_, err = collector.Query(context.Background(), "up", time.Now())
	require.NoError(t, err)

	auth := got.Get("Authorization")
	assert.True(t, strings.HasPrefix(auth, "AWS4-HMAC-SHA256"), auth)
	assert.Contains(t, auth, "/us-east-1/aps/")
	assert.NotContains(t, auth, "Bearer")
	assert.NotContains(t, auth, "super-secret-bearer")
	assert.Contains(t, strings.ToLower(auth), "x-scope-orgid")
	assert.NotContains(t, strings.ToLower(auth), "idempotency-key")
	for _, name := range signedHeaderNames(auth) {
		if strings.EqualFold(name, "host") {
			assert.NotEmpty(t, gotHost, "signed host missing on the wire")
			continue
		}
		_, present := got[http.CanonicalHeaderKey(name)]
		assert.True(t, present, "signed header %s missing on the wire", name)
	}
	assert.Equal(t, "tenant-a", got.Get("X-Scope-OrgID"))
	assert.Contains(t, rawQuery, "dedup=true")
	require.NotEmpty(t, body)
	sum := sha256.Sum256(body)
	wantHash := hex.EncodeToString(sum[:])
	assert.Equal(t, []string{wantHash}, signer.hashes)
	assert.NotEqual(t, emptyPayloadHash, wantHash)
}

func TestPrometheusCollector_SigV4HeadersAndQueryBeforeSign(t *testing.T) {
	signer := &recordingSigner{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedInstantResponse()))
	}))
	defer server.Close()

	opts := &CollectorOptions{
		Headers:         map[string]string{"X-Scope-OrgID": "tenant-a"},
		QueryParameters: map[string]string{"dedup": "true"},
		SigV4: &SigV4Options{
			Region:      "us-east-1",
			Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", "TOKEN"),
			Signer:      signer,
		},
	}
	collector, err := NewPrometheusCollectorWithOptions(server.URL, logr.Discard(), opts, server.Client().Transport)
	require.NoError(t, err)
	_, err = collector.Query(context.Background(), "up", time.Now())
	require.NoError(t, err)
	require.Equal(t, 1, signer.calls)
	require.NotEmpty(t, signer.headers)
	assert.Equal(t, "tenant-a", signer.headers[0].Get("X-Scope-OrgID"))
	assert.Contains(t, signer.queries[0], "dedup=true")
	assert.NotEqual(t, emptyPayloadHash, signer.hashes[0])
}

func TestPrometheusCollector_SigV4CredentialError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("request must not be sent when credentials fail")
	}))
	defer server.Close()

	opts := &CollectorOptions{
		SigV4: &SigV4Options{
			Region:      "us-east-1",
			Credentials: &countingProvider{err: errNoIRSA{}},
		},
	}
	collector, err := NewPrometheusCollectorWithOptions(server.URL, logr.Discard(), opts, server.Client().Transport)
	require.NoError(t, err)
	_, err = collector.Query(context.Background(), "up", time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prometheus sigv4 credentials")
	assert.Contains(t, err.Error(), "no IRSA token")
}

type errNoIRSA struct{}

func (errNoIRSA) Error() string { return "no IRSA token" }

func TestPrometheusCollector_SigV4BlocksMetadataIPBeforeCredentials(t *testing.T) {
	signer := &recordingSigner{}
	provider := &countingProvider{creds: aws.Credentials{AccessKeyID: "AKID", SecretAccessKey: "SECRET"}}
	opts := &CollectorOptions{
		SigV4: &SigV4Options{
			Region:      "us-east-1",
			Credentials: provider,
			Signer:      signer,
		},
	}
	collector, err := NewPrometheusCollectorWithOptions("http://169.254.169.254", logr.Discard(), opts)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = collector.Query(ctx, "up", time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SSRF blocked")
	assert.Equal(t, 0, signer.calls)
	assert.Equal(t, 0, provider.n)
}

func TestPrometheusCollector_SigV4BlocksResolvedLinkLocalBeforeCredentials(t *testing.T) {
	orig := prometheusLookupIP
	t.Cleanup(func() { prometheusLookupIP = orig })
	prometheusLookupIP = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("169.254.169.254")}}, nil
	}

	signer := &recordingSigner{}
	provider := &countingProvider{creds: aws.Credentials{AccessKeyID: "AKID", SecretAccessKey: "SECRET"}}
	opts := &CollectorOptions{
		SigV4: &SigV4Options{
			Region:      "us-east-1",
			Credentials: provider,
			Signer:      signer,
		},
	}
	collector, err := NewPrometheusCollectorWithOptions("http://amp.example", logr.Discard(), opts)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = collector.Query(ctx, "up", time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SSRF blocked")
	assert.Equal(t, 0, signer.calls)
	assert.Equal(t, 0, provider.n)
}

func TestSigV4Transport_CrossOriginStripsAuthHeaders(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	origin, err := url.Parse("https://aps-workspaces.us-east-1.amazonaws.com/workspaces/ws-example")
	require.NoError(t, err)
	signer := &recordingSigner{}
	provider := &countingProvider{creds: aws.Credentials{AccessKeyID: "AKID", SecretAccessKey: "SECRET", SessionToken: "TOKEN"}}
	rt := newSigV4Transport(server.Client().Transport, origin, &SigV4Options{
		Region:      "us-east-1",
		Credentials: provider,
		Signer:      signer,
	}, false, logr.Discard())

	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/query", strings.NewReader("query=up"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer leaked")
	req.Header.Set("X-Amz-Security-Token", "leaked-token")
	req.Header.Set("X-Custom", "keep")
	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 0, signer.calls)
	assert.Equal(t, 0, provider.n)
	assert.Empty(t, got.Get("Authorization"))
	assert.Empty(t, got.Get("X-Amz-Security-Token"))
	assert.Equal(t, "keep", got.Get("X-Custom"))
}

func TestSigV4Transport_NilIdempotencyKeyIsNotSigned(t *testing.T) {
	signer := &recordingSigner{}
	var forwarded http.Header
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		forwarded = r.Header.Clone()
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})
	origin := &url.URL{Scheme: "https", Host: "aps-workspaces.us-east-1.amazonaws.com"}
	rt := newSigV4Transport(base, origin, &SigV4Options{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", "TOKEN"),
		Signer:      signer,
	}, false, logr.Discard())

	req, err := http.NewRequest(http.MethodPost, origin.String()+"/api/v1/query", strings.NewReader("query=up"))
	require.NoError(t, err)
	req.Header["Idempotency-Key"] = nil
	req.Header["X-Empty-Value"] = []string{""}
	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, 1, signer.calls)
	require.NotEmpty(t, signer.headers)
	_, signedNil := signer.headers[0]["Idempotency-Key"]
	assert.False(t, signedNil, "nil Idempotency-Key must not be part of the canonical request")
	assert.Equal(t, []string{""}, signer.headers[0].Values("X-Empty-Value"))
	forwardedNil, ok := forwarded["Idempotency-Key"]
	assert.True(t, ok, "nil Idempotency-Key must still be on the request net/http retries")
	assert.Nil(t, forwardedNil)
	assert.Equal(t, []string{""}, forwarded.Values("X-Empty-Value"))
}

func TestSigV4Transport_SignErrorRestoresNilIdempotencyKey(t *testing.T) {
	signer := &errSigner{}
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("request must not be sent when signing fails")
		return nil, nil
	})
	origin := &url.URL{Scheme: "https", Host: "aps-workspaces.us-east-1.amazonaws.com"}
	rt := newSigV4Transport(base, origin, &SigV4Options{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", "TOKEN"),
		Signer:      signer,
	}, false, logr.Discard())

	req, err := http.NewRequest(http.MethodPost, origin.String()+"/api/v1/query", strings.NewReader("query=up"))
	require.NoError(t, err)
	req.Header["Idempotency-Key"] = nil
	_, err = rt.RoundTrip(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prometheus sigv4 sign")
	require.NotEmpty(t, signer.headers)
	_, signedNil := signer.headers[0]["Idempotency-Key"]
	assert.False(t, signedNil)
	restored, ok := req.Header["Idempotency-Key"]
	assert.True(t, ok)
	assert.Nil(t, restored)
}

func TestPrometheusCollector_SigV4RedirectIsUnsigned(t *testing.T) {
	var followHeaders http.Header
	var followHits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followHits++
		followHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedInstantResponse()))
	}))
	defer target.Close()

	signer := &recordingSigner{}
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/api/v1/query", http.StatusFound)
	}))
	defer source.Close()

	opts := &CollectorOptions{
		SigV4: &SigV4Options{
			Region:      "us-east-1",
			Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", "TOKEN"),
			Signer:      signer,
		},
	}
	collector, err := NewPrometheusCollectorWithOptions(source.URL, logr.Discard(), opts, http.DefaultTransport)
	require.NoError(t, err)
	_, err = collector.Query(context.Background(), "up", time.Now())
	require.NoError(t, err)
	require.Equal(t, 1, followHits)
	require.Equal(t, 1, signer.calls)
	require.NotEmpty(t, signer.hosts)
	assert.NotEqual(t, signer.hosts[0], target.Listener.Addr().String())
	assert.Empty(t, followHeaders.Get("Authorization"))
	assert.Empty(t, followHeaders.Get("X-Amz-Security-Token"))
}

func TestPrometheusCollector_SigV4AssumeRoleOnQuery(t *testing.T) {
	const roleARN = "arn:aws:iam::123456789012:role/attune-amp"
	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedInstantResponse()))
	}))
	defer server.Close()

	stub := &stubAssumeRole{}
	opts := &CollectorOptions{
		SigV4: &SigV4Options{
			Region:    "us-west-2",
			RoleARN:   roleARN,
			STSClient: stub,
		},
	}
	collector, err := NewPrometheusCollectorWithOptions(server.URL, logr.Discard(), opts, server.Client().Transport)
	require.NoError(t, err)
	assert.Equal(t, 0, stub.calls)
	_, err = collector.Query(context.Background(), "up", time.Now())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, stub.calls, 1)
	assert.Equal(t, roleARN, stub.arn)
	assert.True(t, strings.HasPrefix(auth, "AWS4-HMAC-SHA256"), auth)
	assert.Contains(t, auth, "/us-west-2/aps/")
}

func TestPrometheusCollector_SigV4FailureLogOmitsSecrets(t *testing.T) {
	const secret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	var lines []string
	logger := funcr.New(func(_, args string) {
		lines = append(lines, args)
	}, funcr.Options{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"status":"error","error":"denied"}`))
	}))
	defer server.Close()

	opts := &CollectorOptions{
		SigV4: &SigV4Options{
			Region: "us-east-1",
			Credentials: credentials.NewStaticCredentialsProvider(
				"AKIDEXAMPLE", secret, "session-token",
			),
		},
	}
	collector, err := NewPrometheusCollectorWithOptions(server.URL, logger, opts, server.Client().Transport)
	require.NoError(t, err)
	_, err = collector.Query(context.Background(), "up", time.Now())
	require.Error(t, err)
	logged := strings.Join(lines, "\n")
	assert.Contains(t, logged, "403")
	parsed, parseErr := url.Parse(server.URL)
	require.NoError(t, parseErr)
	assert.Contains(t, logged, parsed.Host)
	assert.NotContains(t, logged, secret)
	assert.NotContains(t, logged, "AWS4-HMAC-SHA256")
	assert.NotContains(t, logged, "session-token")
}
