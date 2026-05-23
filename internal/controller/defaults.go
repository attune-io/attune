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

package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
	pkgdefaults "github.com/SebTardifLabs/kube-rightsize/pkg/defaults"
)

// fetchDefaults returns the effective defaults for the given namespace, checking
// namespace-scoped RightSizeNamespaceDefaults first, then falling back to
// cluster-scoped RightSizeDefaults. Returns nil if neither exists.
//
// If multiple defaults objects exist at the same scope, selection is
// deterministic: the lexicographically smallest metadata.name wins.
func (r *RightSizePolicyReconciler) fetchDefaults(ctx context.Context, namespace string) (*rightsizev1alpha1.RightSizeDefaults, error) {
	// Check namespace-scoped defaults first.
	var nsList rightsizev1alpha1.RightSizeNamespaceDefaultsList
	if err := r.List(ctx, &nsList, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing RightSizeNamespaceDefaults in %s: %w", namespace, err)
	}
	if len(nsList.Items) > 0 {
		nsDefaults := nsList.Items[0]
		for i := 1; i < len(nsList.Items); i++ {
			if nsList.Items[i].Name < nsDefaults.Name {
				nsDefaults = nsList.Items[i]
			}
		}
		// Convert to RightSizeDefaults so callers don't need to know the source.
		return &rightsizev1alpha1.RightSizeDefaults{
			ObjectMeta: nsDefaults.ObjectMeta,
			Spec:       nsDefaults.Spec,
		}, nil
	}

	// Fall back to cluster-scoped defaults.
	var clusterList rightsizev1alpha1.RightSizeDefaultsList
	if err := r.List(ctx, &clusterList); err != nil {
		return nil, fmt.Errorf("listing RightSizeDefaults: %w", err)
	}
	if len(clusterList.Items) == 0 {
		return nil, nil
	}
	clusterDefaults := &clusterList.Items[0]
	for i := 1; i < len(clusterList.Items); i++ {
		if clusterList.Items[i].Name < clusterDefaults.Name {
			clusterDefaults = &clusterList.Items[i]
		}
	}
	return clusterDefaults, nil
}

// applyBuiltInDefaults fills strategy and metrics fields still unset after
// mergeDefaults with the operator's built-in default values. This runs AFTER
// mergeDefaults so that cluster-wide RightSizeDefaults take precedence.
//
// Per-resource fields (Percentile, Overhead, MinAllowed/MaxAllowed, BurstSensitivity)
// are NOT set here; they are handled defensively at their usage sites in
// buildRecommendationEngines.
func (r *RightSizePolicyReconciler) applyBuiltInDefaults(policy *rightsizev1alpha1.RightSizePolicy) {
	pkgdefaults.ApplyBuiltInDefaults(policy)
}

// mergeDefaults delegates to the shared defaults package and logs inherited fields.
func (r *RightSizePolicyReconciler) mergeDefaults(policy *rightsizev1alpha1.RightSizePolicy, defaults *rightsizev1alpha1.RightSizeDefaults) {
	if defaults == nil {
		ctrl.Log.V(1).Info("No cluster defaults configured, using built-in values only")
		return
	}
	inherited := pkgdefaults.MergeDefaults(policy, defaults)
	if len(inherited) > 0 {
		ctrl.Log.V(1).Info("Merged cluster defaults into policy",
			"defaultsName", defaults.Name,
			"fieldsInherited", inherited)
	} else {
		ctrl.Log.V(1).Info("All policy fields already set, no defaults applied",
			"defaultsName", defaults.Name)
	}
}

// isWithinResizeWindow returns true if the current time falls within the
// configured resize schedule. Returns true if no schedule is configured.
func isWithinResizeWindow(schedule *rightsizev1alpha1.ResizeSchedule, now time.Time) bool {
	if schedule == nil {
		return true
	}

	// Load timezone.
	tz := schedule.Timezone
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		// Invalid timezone: fail open (allow resize) rather than silently
		// blocking resizes with an undetectable config error.
		return true
	}
	localNow := now.In(loc)

	// Check day-of-week constraint.
	if len(schedule.DaysOfWeek) > 0 {
		dayName := localNow.Weekday().String()
		dayAllowed := false
		for _, d := range schedule.DaysOfWeek {
			if strings.EqualFold(d, dayName) {
				dayAllowed = true
				break
			}
		}
		if !dayAllowed {
			return false
		}
	}

	// Check time windows. If no windows are specified, all times are allowed.
	if len(schedule.Windows) == 0 {
		return true
	}
	nowMinutes := localNow.Hour()*60 + localNow.Minute()
	for _, w := range schedule.Windows {
		start := parseHHMM(w.Start)
		end := parseHHMM(w.End)
		if start < 0 || end < 0 {
			continue
		}
		if start <= end {
			// Normal window: e.g. 02:00-06:00
			if nowMinutes >= start && nowMinutes < end {
				return true
			}
		} else {
			// Overnight window: e.g. 22:00-06:00
			if nowMinutes >= start || nowMinutes < end {
				return true
			}
		}
	}
	return false
}

// parseHHMM parses "HH:MM" into minutes since midnight. Returns -1 on error.
func parseHHMM(s string) int {
	if len(s) != 5 || s[2] != ':' {
		return -1
	}
	h, err1 := strconv.Atoi(s[:2])
	m, err2 := strconv.Atoi(s[3:])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return -1
	}
	return h*60 + m
}
