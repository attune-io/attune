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

// Package fleetreport builds and publishes a per-cluster fleet summary for
// multi-cluster observability (issue #369 Phase B).
package fleetreport

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

// SchemaVersion is the stable version of the fleet report JSON document.
// Breaking changes require a version bump and docs update.
const SchemaVersion = "v1"

// ConfigMapLabel marks fleet report ConfigMaps for collectors.
const ConfigMapLabel = "attune.io/fleet-report"

// Report is the versioned cluster summary written to a ConfigMap.
//
// Schema stability: additive fields only within a major schemaVersion.
// Collectors must ignore unknown fields.
type Report struct {
	// SchemaVersion identifies the document shape (currently "v1").
	SchemaVersion string `json:"schemaVersion"`
	// ClusterID is an optional stable cluster name (from flag or empty).
	ClusterID string `json:"clusterId,omitempty"`
	// GeneratedAt is when this report was produced (UTC RFC3339 in JSON).
	GeneratedAt time.Time `json:"generatedAt"`
	// PolicyCount is the number of AttunePolicy objects in the cluster.
	PolicyCount int `json:"policyCount"`
	// PoliciesByMode counts policies by updateStrategy.type from stored Spec
	// (not AttuneDefaults merge). Empty type is counted as "Recommend" to match
	// the built-in default when unset.
	PoliciesByMode map[string]int `json:"policiesByMode"`
	// ReadyTrue / ReadyFalse count policies by Ready condition status.
	ReadyTrue  int `json:"readyTrue"`
	ReadyFalse int `json:"readyFalse"`
	// InsufficientData counts policies with Ready reason InsufficientData.
	InsufficientData int `json:"insufficientData"`
	// WorkloadsDiscovered sums status.workloads.discovered across policies.
	WorkloadsDiscovered int `json:"workloadsDiscovered"`
	// WorkloadsWithRecommendations sums status.workloads.withRecommendations.
	WorkloadsWithRecommendations int `json:"workloadsWithRecommendations"`
	// WorkloadsResized sums status.workloads.resized.
	WorkloadsResized int `json:"workloadsResized"`
	// EstimatedMonthlySavingsUSD is the sum of parseable estimatedMonthlySavings
	// values (approximate; do not over-claim precision in rollups).
	EstimatedMonthlySavingsUSD float64 `json:"estimatedMonthlySavingsUSD"`
	// ReclaimedCPURequestMilli sums freeable CPU millicores when parseable from status.
	ReclaimedCPURequestMilli int64 `json:"reclaimedCpuRequestMilli,omitempty"`
	// ReclaimedMemoryRequestBytes sums freeable memory bytes when parseable.
	ReclaimedMemoryRequestBytes int64 `json:"reclaimedMemoryRequestBytes,omitempty"`
}

// Build constructs a Report from the given policies.
func Build(policies []attunev1alpha1.AttunePolicy, clusterID string, now time.Time) Report {
	r := Report{
		SchemaVersion:  SchemaVersion,
		ClusterID:      clusterID,
		GeneratedAt:    now.UTC(),
		PolicyCount:    len(policies),
		PoliciesByMode: map[string]int{},
	}
	for i := range policies {
		p := &policies[i]
		mode := string(attunev1alpha1.DefaultUpdateType) // Recommend when unset on Spec
		if p.Spec.UpdateStrategy != nil && p.Spec.UpdateStrategy.Type != "" {
			mode = string(p.Spec.UpdateStrategy.Type)
		}
		r.PoliciesByMode[mode]++

		ready := metav1.ConditionUnknown
		reason := ""
		for _, c := range p.Status.Conditions {
			if c.Type == attunev1alpha1.ConditionReady {
				ready = c.Status
				reason = c.Reason
				break
			}
		}
		switch ready {
		case metav1.ConditionTrue:
			r.ReadyTrue++
		case metav1.ConditionFalse:
			r.ReadyFalse++
		}
		if reason == attunev1alpha1.ReasonInsufficientData {
			r.InsufficientData++
		}

		r.WorkloadsDiscovered += int(p.Status.Workloads.Discovered)
		r.WorkloadsWithRecommendations += int(p.Status.Workloads.WithRecommendations)
		r.WorkloadsResized += int(p.Status.Workloads.Resized)

		r.EstimatedMonthlySavingsUSD += parseUSD(p.Status.Savings.EstimatedMonthlySavings)
		if milli, ok := parseCPUMilli(firstNonEmpty(p.Status.Savings.ReclaimedCPURequest, p.Status.Savings.CPURequestReduction)); ok {
			r.ReclaimedCPURequestMilli += milli
		}
		if bytes, ok := parseMemoryBytes(firstNonEmpty(p.Status.Savings.ReclaimedMemoryRequest, p.Status.Savings.MemoryRequestReduction)); ok {
			r.ReclaimedMemoryRequestBytes += bytes
		}
	}
	return r
}

// MarshalJSONDocument returns indented JSON for ConfigMap storage.
func MarshalJSONDocument(r Report) ([]byte, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal fleet report: %w", err)
	}
	return append(b, '\n'), nil
}

// ConfigMapFromReport builds a ConfigMap holding the report under key "report.json".
func ConfigMapFromReport(namespace, name string, r Report) (*corev1.ConfigMap, error) {
	body, err := MarshalJSONDocument(r)
	if err != nil {
		return nil, err
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				ConfigMapLabel:              "true",
				"app.kubernetes.io/name":    "attune",
				"app.kubernetes.io/part-of": "attune",
			},
		},
		Data: map[string]string{
			"report.json":    string(body),
			"schema-version": r.SchemaVersion,
			"cluster-id":     r.ClusterID,
			"generated-at":   r.GeneratedAt.UTC().Format(time.RFC3339),
		},
	}, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func parseUSD(s string) float64 {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "$")
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

func parseCPUMilli(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0, false
	}
	return q.MilliValue(), true
}

func parseMemoryBytes(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0, false
	}
	return q.Value(), true
}
