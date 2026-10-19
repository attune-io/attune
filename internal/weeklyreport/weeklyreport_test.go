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
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func TestWeekIDBoundaries(t *testing.T) {
	t.Parallel()
	sunday := time.Date(2026, 10, 18, 23, 59, 0, 0, time.UTC)
	monday := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	if got := WeekID(sunday); got != "2026-W42" {
		t.Fatalf("sunday week %s", got)
	}
	if got := WeekID(monday); got != "2026-W43" {
		t.Fatalf("monday week %s", got)
	}
	late := time.Date(2026, 12, 31, 12, 0, 0, 0, time.UTC)
	next := time.Date(2027, 1, 4, 0, 0, 0, 0, time.UTC)
	if got := WeekID(late); got != "2026-W53" {
		t.Fatalf("late 2026 week %s", got)
	}
	if got := WeekID(next); got != "2027-W01" {
		t.Fatalf("early 2027 week %s", got)
	}
	if !stampAlreadySent("2026-W44", "2026-W43") {
		t.Fatal("future stamp should wait")
	}
	if stampAlreadySent("not-a-week", "2026-W43") {
		t.Fatal("malformed stamp should send")
	}
	if !stampAlreadySent("2026-W43", "2026-W43") {
		t.Fatal("current stamp should skip")
	}
}

func TestParseEndpoints(t *testing.T) {
	t.Parallel()
	got, err := ParseEndpoints([]string{"slack/attune-weekly-report/slack-url"})
	if err != nil || len(got) != 1 || got[0].Name != "slack" {
		t.Fatalf("got %+v err %v", got, err)
	}
	if _, err := ParseEndpoints([]string{"Slack/ns/key"}); err == nil {
		t.Fatal("uppercase name should fail")
	}
	if _, err := ParseEndpoints([]string{"a/b/c/d"}); err == nil {
		t.Fatal("extra slash should fail")
	}
	if _, err := ParseEndpoints([]string{"a/sec/k", "b/sec/k"}); err == nil {
		t.Fatal("duplicate secret should fail")
	}
	if _, err := ParseEndpoints([]string{"a/s/k", "b/s/k2", "c/s/k3"}); err == nil {
		t.Fatal("third endpoint should fail")
	}
	if _, err := ParseEndpoints(nil); err != nil {
		t.Fatal(err)
	}
}

func TestMessageTextLayout(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	old := metav1.NewTime(now.Add(-8 * 24 * time.Hour))
	recent := metav1.NewTime(now.Add(-time.Hour))
	hist := make([]attunev1alpha1.ResizeHistoryEntry, attunev1alpha1.MaxResizeHistoryEntries)
	for i := range hist {
		hist[i] = attunev1alpha1.ResizeHistoryEntry{
			Timestamp: metav1.NewTime(now.Add(-time.Duration(i) * time.Hour)),
			Workload:  "checkout",
			Container: "app",
			Result:    attunev1alpha1.ResizeResultSuccess,
		}
	}
	hist[0] = attunev1alpha1.ResizeHistoryEntry{
		Timestamp: recent,
		Workload:  "checkout",
		Container: "app",
		Result:    attunev1alpha1.ResizeResultFailed,
		Reason:    "slo:<!channel>",
	}
	policies := []attunev1alpha1.AttunePolicy{
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "checkout"},
			Spec: attunev1alpha1.AttunePolicySpec{
				UpdateStrategy: &attunev1alpha1.UpdateStrategy{Type: attunev1alpha1.UpdateTypeAuto},
			},
			Status: attunev1alpha1.AttunePolicyStatus{
				Savings: attunev1alpha1.SavingsStatus{
					EstimatedMonthlySavings:      "$8.00",
					EstimatedMonthlyCostIncrease: "$1.25",
					ReclaimedCPURequest:          "1500m",
					ReclaimedMemoryRequest:       "512Mi",
				},
				Workloads:      attunev1alpha1.WorkloadStatus{Discovered: 2, WithRecommendations: 1},
				ResizeHistory:  hist,
				WorkloadErrors: []attunev1alpha1.WorkloadError{{Workload: "checkout", Error: "http://secret"}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "idle"},
			Status: attunev1alpha1.AttunePolicyStatus{
				ResizeHistory: []attunev1alpha1.ResizeHistoryEntry{{
					Timestamp: old,
					Result:    attunev1alpha1.ResizeResultSuccess,
					Workload:  "old",
				}},
			},
		},
	}
	text := MessageText(policies, "prod-us", "2026-W43", now)
	for _, want := range []string{
		"Attune weekly report 2026-W43 (prod-us)",
		"Policies: 2 (Auto 1, Recommend 1)",
		"Monthly savings: $8.00",
		"Monthly cost increase: $1.25",
		"Reclaimable: 1500m CPU, 512Mi memory",
		"Not a cloud bill",
		"Last 7 days: Success",
		"Counts are a lower bound",
		"- prod/checkout $8.00",
		"slo: !channel ",
		"Current reconcile errors",
		"- prod/checkout checkout",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in\n%s", want, text)
		}
	}
	if strings.Contains(text, "http://secret") {
		t.Fatal("error string leaked")
	}
	if strings.Contains(text, "<!channel>") {
		t.Fatal("angle brackets leaked")
	}
	body, err := JSONBody(text)
	if err != nil || !strings.Contains(string(body), `"text"`) {
		t.Fatalf("json %s err %v", body, err)
	}
	long := capRunes(strings.Repeat("a\n", 4000), textRuneCap)
	if !strings.HasSuffix(long, "\ntruncated") {
		t.Fatalf("cap suffix %q", long[len(long)-20:])
	}
}

type recordingPoster struct {
	calls int
	body  []byte
	code  int
	err   error
}

func (p *recordingPoster) Post(_ context.Context, _ string, body []byte) (int, error) {
	p.calls++
	p.body = append([]byte(nil), body...)
	if p.err != nil {
		return 0, p.err
	}
	if p.code == 0 {
		return 200, nil
	}
	return p.code, nil
}

func testSender(t *testing.T, objects ...client.Object) (*Sender, *recordingPoster, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := attunev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	poster := &recordingPoster{}
	s := &Sender{
		Client:    c,
		APIReader: c,
		Log:       logr.Discard(),
		Namespace: "attune-system",
		Endpoints: []Endpoint{{Name: "slack", SecretName: "attune-weekly-report", SecretKey: "slack-url"}},
		Poster:    poster,
		Now:       func() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) },
	}
	return s, poster, c
}

func slackSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "attune-system", Name: "attune-weekly-report"},
		Data:       map[string][]byte{"slack-url": []byte(" https://hooks.slack.com/services/T/B/x \n")},
	}
}

func TestSenderZeroPoliciesStampsWithoutPost(t *testing.T) {
	t.Parallel()
	s, poster, c := testSender(t, slackSecret())
	s.tick(context.Background())
	if poster.calls != 0 {
		t.Fatalf("posted %d", poster.calls)
	}
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "attune-system", Name: stampConfigMap}, &cm); err != nil {
		t.Fatal(err)
	}
	if cm.Annotations[stampAnnotation] != "2026-W43" {
		t.Fatalf("stamp %v", cm.Annotations)
	}
}

func TestSenderPostsOncePerWeek(t *testing.T) {
	t.Parallel()
	policy := &attunev1alpha1.AttunePolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "checkout"}}
	s, poster, _ := testSender(t, slackSecret(), policy)
	s.tick(context.Background())
	s.tick(context.Background())
	if poster.calls != 1 {
		t.Fatalf("calls %d", poster.calls)
	}
	if !strings.Contains(string(poster.body), "prod/checkout") && !strings.Contains(string(poster.body), "Policies: 1") {
		t.Fatalf("body %s", poster.body)
	}
}

func TestSenderRejectedURLDoesNotStamp(t *testing.T) {
	t.Parallel()
	secret := slackSecret()
	secret.Data["slack-url"] = []byte("https://example.com/nope")
	policy := &attunev1alpha1.AttunePolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "checkout"}}
	s, poster, c := testSender(t, secret, policy)
	s.tick(context.Background())
	if poster.calls != 0 {
		t.Fatal("posted a rejected URL")
	}
	var cm corev1.ConfigMap
	err := c.Get(context.Background(), client.ObjectKey{Namespace: "attune-system", Name: stampConfigMap}, &cm)
	if err == nil {
		t.Fatal("stamped a rejected URL")
	}
}

func TestSenderMissingSecretDoesNotStamp(t *testing.T) {
	t.Parallel()
	policy := &attunev1alpha1.AttunePolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "checkout"}}
	s, _, c := testSender(t, policy)
	s.tick(context.Background())
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "attune-system", Name: stampConfigMap}, &cm); err == nil {
		t.Fatal("stamped without a secret")
	}
}

func TestProductionClient(t *testing.T) {
	c := NewClient()
	hc := c.HTTPClient()
	if hc.Timeout != httpTimeout {
		t.Fatalf("timeout %s", hc.Timeout)
	}
	if err := hc.CheckRedirect(nil, nil); !errors.Is(err, ErrRedirect) {
		t.Fatalf("redirect %v", err)
	}
	t.Setenv("HTTPS_PROXY", "http://10.1.2.3:8888")
	req, err := http.NewRequest(http.MethodGet, "https://hooks.slack.com/services/a", nil)
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := hc.Transport.(*http.Transport)
	if !ok {
		t.Fatal("transport type")
	}
	got, err := transport.Proxy(req)
	if err != nil {
		t.Fatal(err)
	}
	want, err := http.ProxyFromEnvironment(req)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != want.String() {
		t.Fatalf("proxy got %s want %s", got, want)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr == nil {
			_, _ = io.Copy(io.Discard, conn)
			_ = conn.Close()
		}
		close(done)
	}()
	ctx := context.WithValue(context.Background(), requestHostKey{}, "hooks.slack.com")
	conn, err := transport.DialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	<-done
	if _, err := allowedIPs(context.Background(), "127.0.0.1"); err == nil {
		t.Fatal("loopback host was allowed")
	}
}
