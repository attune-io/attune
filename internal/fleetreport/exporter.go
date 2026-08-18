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

package fleetreport

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/operatormetrics"
)

// Exporter periodically writes a fleet report ConfigMap.
// Implements manager.Runnable (Start blocks until context is cancelled).
type Exporter struct {
	Client    client.Client
	Log       logr.Logger
	Namespace string
	Name      string
	ClusterID string
	Interval  time.Duration
	// Now is optional; defaults to time.Now.
	Now func() time.Time
}

// Start runs the export loop until ctx is done.
func (e *Exporter) Start(ctx context.Context) error {
	if e.Interval <= 0 {
		e.Interval = 5 * time.Minute
	}
	if e.Name == "" {
		e.Name = "attune-fleet-report"
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	// Initial export soon after start (leader election already held when started).
	e.ExportOnce(ctx)

	t := time.NewTicker(e.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			e.ExportOnce(ctx)
		}
	}
}

// NeedLeaderElection makes this runnable run only on the elected leader.
func (e *Exporter) NeedLeaderElection() bool { return true }

// ExportOnce lists policies and writes/updates the fleet report ConfigMap.
func (e *Exporter) ExportOnce(ctx context.Context) {
	log := e.Log
	var list attunev1alpha1.AttunePolicyList
	if err := e.Client.List(ctx, &list); err != nil {
		log.Error(err, "fleet report: list AttunePolicies failed")
		operatormetrics.FleetReportExportTotal.WithLabelValues("failed").Inc()
		return
	}
	report := Build(list.Items, e.ClusterID, e.Now())
	cm, err := ConfigMapFromReport(e.Namespace, e.Name, report)
	if err != nil {
		log.Error(err, "fleet report: build ConfigMap failed")
		operatormetrics.FleetReportExportTotal.WithLabelValues("failed").Inc()
		return
	}
	if err := e.applyConfigMap(ctx, cm); err != nil {
		log.Error(err, "fleet report: write ConfigMap failed", "namespace", e.Namespace, "name", e.Name)
		operatormetrics.FleetReportExportTotal.WithLabelValues("failed").Inc()
		return
	}
	operatormetrics.FleetReportExportTotal.WithLabelValues("success").Inc()
	if report.UnparseableSavings > 0 {
		log.Info("fleet report dropped unparseable savings strings",
			"count", report.UnparseableSavings,
			"estimatedMonthlySavingsUSD", report.EstimatedMonthlySavingsUSD)
	}
	log.V(1).Info("fleet report exported",
		"policies", report.PolicyCount,
		"workloads", report.WorkloadsDiscovered,
		"unparseableSavings", report.UnparseableSavings,
		"schema", report.SchemaVersion)
}

func (e *Exporter) applyConfigMap(ctx context.Context, desired *corev1.ConfigMap) error {
	var existing corev1.ConfigMap
	err := e.Client.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, &existing)
	if apierrors.IsNotFound(err) {
		if createErr := e.Client.Create(ctx, desired); createErr != nil {
			return fmt.Errorf("create: %w", createErr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get: %w", err)
	}
	// Merge patch avoids 409 when concurrent writers touch labels/annotations
	// (same pattern as recommendation ConfigMap export).
	base := existing.DeepCopy()
	existing.Data = desired.Data
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	for k, v := range desired.Labels {
		existing.Labels[k] = v
	}
	if err := e.Client.Patch(ctx, &existing, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patch: %w", err)
	}
	return nil
}
