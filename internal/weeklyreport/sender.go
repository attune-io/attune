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
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/operatormetrics"
	"github.com/attune-io/attune/internal/validation"
)

// Sender posts one cluster report per ISO week.
type Sender struct {
	Client    client.Client
	APIReader client.Reader
	Log       logr.Logger
	Namespace string
	ClusterID string
	Endpoints []Endpoint
	// Poster is optional. Tests set it. Production uses NewClient.
	Poster Poster
	// Now is optional. Tests set it.
	Now func() time.Time

	mu        sync.Mutex
	sentWeek  string
	needStamp bool
	emptyLog  sync.Once
}

// Once runs a single check. Start calls it immediately, and tests call it
// instead of waiting for the 15 minute ticker.
func (s *Sender) Once(ctx context.Context) { s.tick(ctx) }

// Start checks once, then every 15 minutes, until ctx is cancelled.
func (s *Sender) Start(ctx context.Context) error {
	s.Once(ctx)
	ticker := time.NewTicker(reportInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// NeedLeaderElection keeps a second replica from sending the same week.
func (s *Sender) NeedLeaderElection() bool { return true }

func (s *Sender) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Sender) reader() client.Reader {
	if s.APIReader != nil {
		return s.APIReader
	}
	return s.Client
}

func (s *Sender) poster() Poster {
	if s.Poster != nil {
		return s.Poster
	}
	return NewClient()
}

func (s *Sender) tick(ctx context.Context) {
	if len(s.Endpoints) == 0 {
		s.emptyLog.Do(func() {
			s.Log.Error(errNoEndpoints, "weekly report enabled with no endpoints")
		})
		operatormetrics.WeeklyReportTotal.WithLabelValues("no_endpoints").Inc()
		return
	}
	week := WeekID(s.now())
	s.mu.Lock()
	sent, need := s.sentWeek, s.needStamp
	s.mu.Unlock()
	if sent == week && need {
		if err := s.writeStamp(ctx, week); err != nil {
			s.Log.Error(err, "weekly report stamp failed", "week", week)
			operatormetrics.WeeklyReportTotal.WithLabelValues("stamp_failed").Inc()
			return
		}
		s.mu.Lock()
		s.needStamp = false
		s.mu.Unlock()
		operatormetrics.WeeklyReportLastSuccess.Set(float64(s.now().Unix()))
		operatormetrics.WeeklyReportTotal.WithLabelValues("sent").Inc()
		return
	}
	if sent == week {
		return
	}
	stamp, err := s.readStamp(ctx)
	if err != nil {
		s.Log.Error(err, "weekly report stamp read failed")
		return
	}
	if stampAlreadySent(stamp, week) {
		s.mu.Lock()
		s.sentWeek = week
		s.needStamp = false
		s.mu.Unlock()
		return
	}

	urls, secretOK := s.readSecrets(ctx)
	if !secretOK {
		operatormetrics.WeeklyReportTotal.WithLabelValues("failed").Inc()
		return
	}
	policies, err := s.listPolicies(ctx)
	if err != nil {
		s.Log.Error(err, "weekly report list failed")
		operatormetrics.WeeklyReportTotal.WithLabelValues("failed").Inc()
		return
	}
	if len(policies) == 0 {
		if err := s.writeStamp(ctx, week); err != nil {
			s.Log.Error(err, "weekly report stamp failed", "week", week)
			operatormetrics.WeeklyReportTotal.WithLabelValues("stamp_failed").Inc()
			return
		}
		s.mu.Lock()
		s.sentWeek = week
		s.needStamp = false
		s.mu.Unlock()
		operatormetrics.WeeklyReportTotal.WithLabelValues("skipped_no_policies").Inc()
		return
	}
	body, err := JSONBody(MessageText(policies, s.ClusterID, week, s.now()))
	if err != nil {
		s.Log.Error(err, "weekly report encode failed")
		operatormetrics.WeeklyReportTotal.WithLabelValues("failed").Inc()
		return
	}
	anyOK := false
	for _, dest := range urls {
		if s.postOne(ctx, dest, body) {
			anyOK = true
		}
	}
	if !anyOK {
		operatormetrics.WeeklyReportTotal.WithLabelValues("failed").Inc()
		return
	}
	if err := s.writeStamp(ctx, week); err != nil {
		s.Log.Error(err, "weekly report stamp failed", "week", week)
		s.mu.Lock()
		s.sentWeek = week
		s.needStamp = true
		s.mu.Unlock()
		operatormetrics.WeeklyReportTotal.WithLabelValues("stamp_failed").Inc()
		return
	}
	s.mu.Lock()
	s.sentWeek = week
	s.needStamp = false
	s.mu.Unlock()
	operatormetrics.WeeklyReportLastSuccess.Set(float64(s.now().Unix()))
	operatormetrics.WeeklyReportTotal.WithLabelValues("sent").Inc()
}

type destination struct {
	name string
	url  string
}

func (s *Sender) readSecrets(ctx context.Context) ([]destination, bool) {
	var out []destination
	ok := false
	for _, ep := range s.Endpoints {
		raw, err := s.secretValue(ctx, ep)
		if err != nil {
			s.Log.Error(err, "weekly report secret failed", "endpoint", ep.Name, "secret", ep.SecretName, "key", ep.SecretKey)
			operatormetrics.WeeklyReportSendTotal.WithLabelValues(ep.Name, "secret_error").Inc()
			continue
		}
		ok = true
		if err := validation.ChatWebhookURL(raw); err != nil {
			s.Log.Error(err, "weekly report URL rejected", "endpoint", ep.Name)
			operatormetrics.WeeklyReportSendTotal.WithLabelValues(ep.Name, "rejected").Inc()
			continue
		}
		out = append(out, destination{name: ep.Name, url: raw})
	}
	return out, ok
}

func (s *Sender) secretValue(ctx context.Context, ep Endpoint) (string, error) {
	var secret corev1.Secret
	err := s.reader().Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: ep.SecretName}, &secret)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(secret.Data[ep.SecretKey]))
	if value == "" {
		return "", errEmptySecret
	}
	return value, nil
}

func (s *Sender) listPolicies(ctx context.Context) ([]attunev1alpha1.AttunePolicy, error) {
	var list attunev1alpha1.AttunePolicyList
	if err := s.Client.List(ctx, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (s *Sender) postOne(ctx context.Context, dest destination, body []byte) bool {
	status, err := s.poster().Post(ctx, dest.url, body)
	switch {
	case errors.Is(err, ErrRejected):
		s.Log.Error(err, "weekly report URL rejected", "endpoint", dest.name)
		operatormetrics.WeeklyReportSendTotal.WithLabelValues(dest.name, "rejected").Inc()
		return false
	case err != nil:
		s.Log.Error(errPostForLog(err), "weekly report post failed", "endpoint", dest.name)
		operatormetrics.WeeklyReportSendTotal.WithLabelValues(dest.name, "failed").Inc()
		return false
	case status >= 200 && status < 300:
		s.Log.Info("weekly report posted", "endpoint", dest.name, "status", status, "bytes", len(body))
		operatormetrics.WeeklyReportSendTotal.WithLabelValues(dest.name, "success").Inc()
		return true
	default:
		s.Log.Error(ErrPostFailed, "weekly report post failed", "endpoint", dest.name, "status", status)
		operatormetrics.WeeklyReportSendTotal.WithLabelValues(dest.name, "failed").Inc()
		return false
	}
}

func (s *Sender) readStamp(ctx context.Context) (string, error) {
	var cm corev1.ConfigMap
	err := s.reader().Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: stampConfigMap}, &cm)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if cm.Annotations == nil {
		return "", nil
	}
	return cm.Annotations[stampAnnotation], nil
}

func (s *Sender) writeStamp(ctx context.Context, week string) error {
	var current corev1.ConfigMap
	err := s.reader().Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: stampConfigMap}, &current)
	if apierrors.IsNotFound(err) {
		fresh := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   s.Namespace,
				Name:        stampConfigMap,
				Annotations: map[string]string{stampAnnotation: week},
			},
		}
		createErr := s.Client.Create(ctx, fresh)
		if createErr == nil {
			return nil
		}
		if !apierrors.IsAlreadyExists(createErr) {
			return createErr
		}
	} else if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{stampAnnotation: week},
		},
	})
	if err != nil {
		return err
	}
	base := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: stampConfigMap}}
	return s.Client.Patch(ctx, base, client.RawPatch(types.MergePatchType, patch))
}

var (
	errEmptySecret = emptySecretError("weekly report secret value is empty")
	errNoEndpoints = emptySecretError("weekly report has no endpoints")
)

type emptySecretError string

func (e emptySecretError) Error() string { return string(e) }
