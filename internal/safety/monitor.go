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
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ThrottleChecker queries Prometheus for CPU throttle ratio.
type ThrottleChecker interface {
	// GetThrottleRatio returns the CPU throttle ratio (0.0-1.0) for a container.
	// Returns 0.0 if data is unavailable.
	GetThrottleRatio(ctx context.Context, namespace, pod, container string) (float64, error)
}

// DefaultThrottleThreshold is the fraction of CPU periods that are throttled
// above which the safety monitor triggers a revert (50%).
const DefaultThrottleThreshold = 0.5

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
}

// SafetyVerdict is the result of checking a resized pod for problems.
type SafetyVerdict struct {
	Safe    bool
	Reason  string // "oomkill", "throttle", "restart", "notready", ""
	Message string
}

// Monitor watches resized pods for safety violations.
type Monitor struct {
	client            kubernetes.Interface
	logger            logr.Logger
	throttleChecker   ThrottleChecker
	throttleThreshold float64
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

// CheckPod evaluates the current state of a pod that was previously resized
// and returns a SafetyVerdict. It checks, in order:
//  1. Pod existence (deleted pods are considered safe).
//  2. OOMKill events that occurred after the resize.
//  3. Restart count increases of 2 or more since the resize.
//  4. Pod Ready condition.
func (m *Monitor) CheckPod(ctx context.Context, record ResizeRecord) (SafetyVerdict, error) {
	pod, err := m.client.CoreV1().Pods(record.Namespace).Get(ctx, record.PodName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return SafetyVerdict{Safe: true}, nil
		}
		return SafetyVerdict{}, fmt.Errorf("getting pod %s/%s: %w", record.Namespace, record.PodName, err)
	}

	// Search both regular and init container statuses (native sidecars
	// report status in InitContainerStatuses).
	allStatuses := append(pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses...)
	for _, cs := range allStatuses {
		if cs.Name != record.Container {
			continue
		}

		// Check for OOMKill that happened after the resize.
		if cs.LastTerminationState.Terminated != nil &&
			cs.LastTerminationState.Terminated.Reason == "OOMKilled" &&
			cs.LastTerminationState.Terminated.FinishedAt.After(record.ResizedAt) {
			return SafetyVerdict{
				Safe:    false,
				Reason:  "oomkill",
				Message: fmt.Sprintf("container %s in pod %s/%s was OOMKilled after resize", record.Container, record.Namespace, record.PodName),
			}, nil
		}

		// Check for excessive restarts since the resize.
		if cs.RestartCount >= record.RestartCount+2 {
			return SafetyVerdict{
				Safe:    false,
				Reason:  "restart",
				Message: fmt.Sprintf("container %s in pod %s/%s restarted %d times since resize (was %d)", record.Container, record.Namespace, record.PodName, cs.RestartCount, record.RestartCount),
			}, nil
		}
	}

	// Check for CPU throttling via Prometheus (if checker is configured).
	if m.throttleChecker != nil {
		ratio, err := m.throttleChecker.GetThrottleRatio(ctx, record.Namespace, record.PodName, record.Container)
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

	return SafetyVerdict{Safe: true}, nil
}

// RevertPod resizes the pod back to its original resources using the /resize
// subresource. This is the undo path for a resize that caused problems.
func (m *Monitor) RevertPod(ctx context.Context, record ResizeRecord) error {
	pod, err := m.client.CoreV1().Pods(record.Namespace).Get(ctx, record.PodName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting pod for revert %s/%s: %w", record.Namespace, record.PodName, err)
	}

	updated := pod.DeepCopy()
	found := false
	for i, c := range updated.Spec.InitContainers {
		if c.Name == record.Container {
			updated.Spec.InitContainers[i].Resources = record.OriginalResources
			found = true
			break
		}
	}
	if !found {
		for i, c := range updated.Spec.Containers {
			if c.Name == record.Container {
				updated.Spec.Containers[i].Resources = record.OriginalResources
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

	m.logger.Info("reverting pod resize", "pod", record.PodName,
		"namespace", record.Namespace, "container", record.Container)

	_, err = m.client.CoreV1().Pods(record.Namespace).UpdateResize(ctx, record.PodName, updated, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("reverting resize for pod %s/%s: %w", record.Namespace, record.PodName, err)
	}

	return nil
}
