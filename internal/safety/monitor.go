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

// Package safety monitors resized pods for safety violations and handles
// automatic reverts when problems are detected.
package safety

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"text/template"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
	"github.com/attune-io/attune/internal/resize"
	"github.com/attune-io/attune/internal/throttle"
)

// ThrottleChecker is an alias for throttle.Checker for backward compatibility.
type ThrottleChecker = throttle.Checker

// DefaultThrottleThreshold is the fraction of CPU periods that are throttled
// above which the safety monitor triggers a revert (50%).
const DefaultThrottleThreshold = 0.5

// DefaultSLOEvaluationWindow is the default duration after resize during which
// SLO guardrail queries are evaluated.
const DefaultSLOEvaluationWindow = 5 * time.Minute

// SLOQuerier executes an instant PromQL query and returns a scalar value.
// MetricsCollector satisfies this interface.
type SLOQuerier interface {
	Query(ctx context.Context, query string, ts time.Time) (float64, error)
}

// sloTemplateData holds the variables available for interpolation in SLO queries.
type sloTemplateData struct {
	Namespace    string
	WorkloadName string
	PodName      string
}

// ResizeRecord tracks a resize operation for safety monitoring.
type ResizeRecord struct {
	PodName           string
	Namespace         string
	Container         string
	OriginalResources corev1.ResourceRequirements
	NewResources      corev1.ResourceRequirements
	ResizedAt         time.Time
	ObservationEnd    time.Time
	// RestartCount holds the container restart count recorded at the time of
	// the resize so that CheckPod can detect increases.
	RestartCount int32
	// WorkloadName is the name of the owning workload (Deployment, StatefulSet, etc.).
	// Used for SLO guardrail query template interpolation.
	WorkloadName string
}

// SafetyVerdict is the result of checking a resized pod for problems.
type SafetyVerdict struct {
	Safe    bool
	Reason  string // "oomkill", "throttle", "restart", "notready", ""
	Message string
	// ThrottleDeferred is true when the throttle check was skipped because the
	// resize happened less than 5 minutes ago (the Prometheus rate window still
	// contains pre-resize data). The caller should keep observing the pod so the
	// throttle check runs on a subsequent reconciliation.
	ThrottleDeferred bool
}

// Monitor watches resized pods for safety violations.
type Monitor struct {
	client            kubernetes.Interface
	logger            logr.Logger
	throttleChecker   ThrottleChecker
	throttleThreshold float64
	sloQuerier        SLOQuerier
	sloGuardrails     []attunev1alpha1.SLOGuardrail
	sloTemplates      map[string]*template.Template // cached parsed templates keyed by query string
}

// NewMonitor creates a Monitor backed by the given Kubernetes client.
func NewMonitor(client kubernetes.Interface, logger logr.Logger) *Monitor {
	return &Monitor{
		client:            client,
		logger:            logger,
		throttleThreshold: DefaultThrottleThreshold,
	}
}

// WithThrottleChecker adds CPU throttle checking via Prometheus queries.
func (m *Monitor) WithThrottleChecker(checker ThrottleChecker, threshold float64) *Monitor {
	m.throttleChecker = checker
	if threshold > 0 {
		m.throttleThreshold = threshold
	}
	return m
}

// WithSLOChecker adds application-level SLO guardrail checking. The querier
// is used to execute instant PromQL queries for each guardrail. Templates are
// parsed once and cached for the lifetime of the Monitor.
func (m *Monitor) WithSLOChecker(querier SLOQuerier, guardrails []attunev1alpha1.SLOGuardrail) *Monitor {
	m.sloQuerier = querier
	m.sloGuardrails = guardrails
	m.sloTemplates = make(map[string]*template.Template, len(guardrails))
	for _, g := range guardrails {
		tmpl, err := template.New(g.Name).Parse(g.Query)
		if err != nil {
			m.logger.Error(err, "Failed to parse SLO query template, will retry per-invocation",
				"guardrail", g.Name)
			continue
		}
		m.sloTemplates[g.Query] = tmpl
	}
	return m
}

// CheckCriticalStatuses checks a pod's container statuses for critical safety
// events (OOMKill and excessive restarts) that warrant an immediate revert.
// Returns a non-nil SafetyVerdict if a critical issue is found.
// This is used both by CheckPod (full observation) and for early detection
// during the observation period.
func CheckCriticalStatuses(pod *corev1.Pod, record ResizeRecord) *SafetyVerdict {
	for _, cs := range slices.Concat(pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses) {
		if cs.Name != record.Container {
			continue
		}

		// Check for OOMKill that happened after the resize.
		if cs.LastTerminationState.Terminated != nil &&
			cs.LastTerminationState.Terminated.Reason == "OOMKilled" &&
			cs.LastTerminationState.Terminated.FinishedAt.After(record.ResizedAt) {
			return &SafetyVerdict{
				Safe:    false,
				Reason:  "oomkill",
				Message: fmt.Sprintf("container %s in pod %s/%s was OOMKilled after resize", record.Container, record.Namespace, record.PodName),
			}
		}

		// Check for excessive restarts since the resize.
		if cs.RestartCount >= record.RestartCount+2 {
			return &SafetyVerdict{
				Safe:    false,
				Reason:  "restart",
				Message: fmt.Sprintf("container %s in pod %s/%s restarted %d times since resize (was %d)", record.Container, record.Namespace, record.PodName, cs.RestartCount, record.RestartCount),
			}
		}
	}
	return nil
}

// CheckPod evaluates the current state of a pod that was previously resized
// and returns a SafetyVerdict. It checks, in order:
//  1. Pod existence (deleted pods are considered safe).
//  2. OOMKill events that occurred after the resize.
//  3. Restart count increases of 2 or more since the resize.
//  4. Pod Ready condition.
func (m *Monitor) CheckPod(ctx context.Context, record ResizeRecord, now time.Time) (SafetyVerdict, error) {
	pod, err := m.client.CoreV1().Pods(record.Namespace).Get(ctx, record.PodName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return SafetyVerdict{Safe: true}, nil
		}
		return SafetyVerdict{}, fmt.Errorf("getting pod %s/%s: %w", record.Namespace, record.PodName, err)
	}

	// Critical checks: OOMKill and excessive restarts.
	if v := CheckCriticalStatuses(pod, record); v != nil {
		return *v, nil
	}

	// Check for CPU throttling via Prometheus (if checker is configured).
	// Skip when the resize happened less than 5 minutes ago because the
	// Prometheus rate(…[5m]) window still contains 100% pre-resize data.
	// A false-positive throttle revert on a just-upscaled container would
	// create an infinite resize→revert loop for the pods most in need of
	// more CPU.
	var throttleDeferred bool
	throttleGrace := 5 * time.Minute
	if m.throttleChecker != nil {
		if now.Sub(record.ResizedAt) >= throttleGrace {
			ratio, err := m.throttleChecker.GetThrottleRatio(ctx, record.Namespace, record.PodName, record.Container, now)
			if err != nil {
				m.logger.Error(err, "Safety throttle check failed, skipping throttle detection",
					"pod", record.PodName, "namespace", record.Namespace, "container", record.Container)
			} else if ratio > m.throttleThreshold {
				return SafetyVerdict{
					Safe:    false,
					Reason:  "throttle",
					Message: fmt.Sprintf("container %s in pod %s/%s has %.0f%% CPU throttle ratio (threshold %.0f%%)", record.Container, record.Namespace, record.PodName, ratio*100, m.throttleThreshold*100),
				}, nil
			}
		} else {
			// Grace period not yet elapsed; signal the caller to keep observing
			// so the throttle check runs on a future reconciliation.
			throttleDeferred = true
		}
	}

	// Check application-level SLO guardrails via Prometheus.
	if m.sloQuerier != nil && len(m.sloGuardrails) > 0 {
		if v := m.checkSLOGuardrails(ctx, record, now); v != nil {
			return *v, nil
		}
	}

	// Check the pod Ready condition.
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			if condition.Status != corev1.ConditionTrue {
				return SafetyVerdict{
					Safe:    false,
					Reason:  "notready",
					Message: fmt.Sprintf("pod %s/%s is not ready", record.Namespace, record.PodName),
				}, nil
			}
			break
		}
	}

	return SafetyVerdict{Safe: true, ThrottleDeferred: throttleDeferred}, nil
}

// checkSLOGuardrails evaluates all configured SLO guardrail queries against
// Prometheus. Returns a non-nil SafetyVerdict if any guardrail is breached.
// Fails open: if a query errors, the guardrail is skipped with a log message.
func (m *Monitor) checkSLOGuardrails(ctx context.Context, record ResizeRecord, now time.Time) *SafetyVerdict {
	for _, g := range m.sloGuardrails {
		evalWindow := DefaultSLOEvaluationWindow
		if g.EvaluationWindow != nil && g.EvaluationWindow.Duration > 0 {
			evalWindow = g.EvaluationWindow.Duration
		}
		if now.Sub(record.ResizedAt) < evalWindow {
			continue // evaluation window not yet elapsed
		}

		query, err := interpolateSLOQuery(g.Query, record, m.sloTemplates[g.Query])
		if err != nil {
			m.logger.Error(err, "SLO guardrail query interpolation failed, skipping",
				"guardrail", g.Name, "pod", record.PodName, "namespace", record.Namespace)
			continue
		}

		value, err := m.sloQuerier.Query(ctx, query, now)
		if err != nil {
			m.logger.Error(err, "SLO guardrail query failed, skipping",
				"guardrail", g.Name, "pod", record.PodName, "namespace", record.Namespace)
			continue
		}
		if math.IsNaN(value) || math.IsInf(value, 0) {
			m.logger.Info("SLO guardrail query returned non-finite value, skipping",
				"guardrail", g.Name, "value", value, "pod", record.PodName, "namespace", record.Namespace)
			continue
		}

		threshold, err := strconv.ParseFloat(g.Threshold, 64)
		if err != nil || math.IsNaN(threshold) || math.IsInf(threshold, 0) {
			m.logger.Error(err, "SLO guardrail threshold parse failed, skipping",
				"guardrail", g.Name, "threshold", g.Threshold)
			continue
		}

		comparison := g.Comparison
		if comparison == "" {
			comparison = "above"
		}

		breached := false
		switch comparison {
		case "above":
			breached = value > threshold
		case "below":
			breached = value < threshold
		}

		if breached {
			return &SafetyVerdict{
				Safe:   false,
				Reason: "slo:" + g.Name,
				Message: fmt.Sprintf("SLO guardrail %q breached for pod %s/%s: value %.4f %s threshold %.4f",
					g.Name, record.Namespace, record.PodName, value, comparison, threshold),
			}
		}
	}
	return nil
}

// interpolateSLOQuery renders Go template variables in a PromQL query string.
// Supported variables: {{ .Namespace }}, {{ .WorkloadName }}, {{ .PodName }}.
// If a pre-parsed template is provided (from the cache), it is used directly;
// otherwise the template is parsed on the fly.
func interpolateSLOQuery(queryTemplate string, record ResizeRecord, cached *template.Template) (string, error) {
	tmpl := cached
	if tmpl == nil {
		var err error
		tmpl, err = template.New("slo").Parse(queryTemplate)
		if err != nil {
			return "", fmt.Errorf("parsing SLO query template: %w", err)
		}
	}
	data := sloTemplateData{
		Namespace:    rsmetrics.EscapePromQL(record.Namespace),
		WorkloadName: rsmetrics.EscapePromQL(record.WorkloadName),
		PodName:      rsmetrics.EscapePromQL(record.PodName),
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing SLO query template: %w", err)
	}
	return buf.String(), nil
}

// RevertPod resizes the pod back to its original resources using the /resize
// subresource. This is the undo path for a resize that caused problems.
func (m *Monitor) RevertPod(ctx context.Context, record ResizeRecord) error {
	// Retry loop handles 409 Conflict errors that occur when the kubelet
	// updates pod status between our Get and UpdateResize, bumping
	// resourceVersion. This mirrors the retry logic in ResizePod.
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		pod, err := m.client.CoreV1().Pods(record.Namespace).Get(ctx, record.PodName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("getting pod for revert %s/%s: %w", record.Namespace, record.PodName, err)
		}

		updated := pod.DeepCopy()
		// Apply the K8s v1.33 memory limit clamp: memory limits cannot be
		// decreased in-place when the resize policy is NotRequired. Without
		// this, reverts that lower the memory limit are rejected by the API
		// server on v1.33 clusters.
		revertTarget := resize.ClampMemoryLimitForPolicy(pod, record.Container, record.OriginalResources)
		// For Guaranteed QoS pods, the memory limit clamp may cause
		// requests != limits, which K8s rejects ("Pod QOS Class may not
		// change as a result of resizing"). Raise the memory request to
		// match the clamped limit, mirroring the controller's resize path
		// in internal/controller/resize.go.
		if pod.Status.QOSClass == corev1.PodQOSGuaranteed {
			if memLim, ok := revertTarget.Limits[corev1.ResourceMemory]; ok {
				if memReq, rok := revertTarget.Requests[corev1.ResourceMemory]; rok && memReq.Cmp(memLim) < 0 {
					revertTarget.Requests[corev1.ResourceMemory] = memLim.DeepCopy()
					m.logger.Info("Memory request raised to match clamped limit for Guaranteed QoS revert",
						"pod", record.PodName, "namespace", record.Namespace,
						"container", record.Container, "request", memLim.String())
				}
			}
		}
		found := false
		for i, c := range updated.Spec.InitContainers {
			if c.Name == record.Container {
				updated.Spec.InitContainers[i].Resources = revertTarget
				found = true
				break
			}
		}
		if !found {
			for i, c := range updated.Spec.Containers {
				if c.Name == record.Container {
					updated.Spec.Containers[i].Resources = revertTarget
					found = true
					break
				}
			}
		}
		if !found {
			m.logger.Info("container not found in pod, skipping revert",
				"pod", record.PodName, "namespace", record.Namespace,
				"container", record.Container)
			return nil
		}

		logFields := []any{
			"pod", record.PodName,
			"namespace", record.Namespace,
			"container", record.Container,
			"toCPU", record.OriginalResources.Requests.Cpu().String(),
			"toMemory", record.OriginalResources.Requests.Memory().String(),
		}
		if len(record.NewResources.Requests) > 0 {
			logFields = append(logFields,
				"fromCPU", record.NewResources.Requests.Cpu().String(),
				"fromMemory", record.NewResources.Requests.Memory().String(),
			)
		}
		m.logger.Info("reverting pod resize", logFields...)

		_, err = m.client.CoreV1().Pods(record.Namespace).UpdateResize(ctx, record.PodName, updated, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("reverting resize for pod %s/%s: %w", record.Namespace, record.PodName, err)
		}

		return nil
	})
}
