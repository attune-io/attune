//go:build integration

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

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/attune-io/attune/internal/weeklyreport"
)

type countingPoster struct {
	calls int
	body  string
}

func (p *countingPoster) Post(_ context.Context, _ string, body []byte) (int, error) {
	p.calls++
	p.body = string(body)
	return 200, nil
}

func TestWeeklyReportEnvtestPostsAndStamps(t *testing.T) {
	ns := "weekly-report-" + time.Now().UTC().Format("150405")
	require.NoError(t, k8sClient.Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	})

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "attune-weekly-report"},
		Data: map[string][]byte{
			"slack-url": []byte("https://hooks.slack.com/services/T/B/envtest"),
		},
	}
	require.NoError(t, k8sClient.Create(context.Background(), secret))

	policy := newTestPolicy("checkout", ns, "checkout")
	require.NoError(t, k8sClient.Create(context.Background(), policy))

	poster := &countingPoster{}
	sender := &weeklyreport.Sender{
		Client:    k8sClient,
		APIReader: k8sClient,
		Log:       logr.Discard(),
		Namespace: ns,
		Endpoints: []weeklyreport.Endpoint{{
			Name: "slack", SecretName: "attune-weekly-report", SecretKey: "slack-url",
		}},
		Poster: poster,
		Now:    func() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) },
	}
	sender.Once(context.Background())
	require.Equal(t, 1, poster.calls)
	require.Contains(t, poster.body, "Policies:")

	var cm corev1.ConfigMap
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "attune-weekly-report"}, &cm))
	require.Equal(t, "2026-W43", cm.Annotations["attune.io/last-report-week"])

	sender.Once(context.Background())
	require.Equal(t, 1, poster.calls, "second tick in the same week must not post")
	if strings.Contains(poster.body, "https://hooks.slack.com") {
		t.Fatal("webhook URL was copied into the message")
	}
}
