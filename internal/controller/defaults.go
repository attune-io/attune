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

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	pkgdefaults "github.com/attune-io/attune/pkg/defaults"
)

// fetchDefaults returns the effective defaults for the given namespace.
//
// Precedence (per field): built-in < cluster AttuneDefaults < namespace
// AttuneNamespaceDefaults < policy (policy applied later via mergeDefaults).
// When both cluster and namespace defaults exist, namespace values win for
// fields they set; cluster values fill fields left unset on the namespace
// object (3-tier merge).
//
// If multiple defaults objects exist at the same scope, selection is
// deterministic: the lexicographically smallest metadata.name wins.
func (r *AttunePolicyReconciler) fetchDefaults(ctx context.Context, namespace string) (*attunev1alpha1.AttuneDefaults, error) {
	var nsList attunev1alpha1.AttuneNamespaceDefaultsList
	if err := r.List(ctx, &nsList, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing AttuneNamespaceDefaults in %s: %w", namespace, err)
	}
	var nsDefaults *attunev1alpha1.AttuneDefaults
	if len(nsList.Items) > 0 {
		picked := nsList.Items[0]
		for i := 1; i < len(nsList.Items); i++ {
			if nsList.Items[i].Name < picked.Name {
				picked = nsList.Items[i]
			}
		}
		nsDefaults = &attunev1alpha1.AttuneDefaults{
			ObjectMeta: picked.ObjectMeta,
			Spec:       picked.Spec,
		}
	}

	var clusterList attunev1alpha1.AttuneDefaultsList
	if err := r.List(ctx, &clusterList); err != nil {
		return nil, fmt.Errorf("listing AttuneDefaults: %w", err)
	}
	var clusterDefaults *attunev1alpha1.AttuneDefaults
	if len(clusterList.Items) > 0 {
		clusterDefaults = &clusterList.Items[0]
		for i := 1; i < len(clusterList.Items); i++ {
			if clusterList.Items[i].Name < clusterDefaults.Name {
				clusterDefaults = &clusterList.Items[i]
			}
		}
	}

	return pkgdefaults.CombineDefaultsLayers(clusterDefaults, nsDefaults), nil
}

// applyBuiltInDefaults fills strategy and metrics fields still unset after
// mergeDefaults with the operator's built-in default values. This runs AFTER
// mergeDefaults so that cluster-wide AttuneDefaults take precedence.
//
// Per-resource fields (Percentile, Overhead, MinAllowed/MaxAllowed, BurstSensitivity)
// are NOT set here; they are handled defensively at their usage sites in
// buildRecommendationEngines.
func (r *AttunePolicyReconciler) applyBuiltInDefaults(policy *attunev1alpha1.AttunePolicy) {
	pkgdefaults.ApplyBuiltInDefaults(policy)
}

// mergeDefaults delegates to the shared defaults package and logs inherited fields.
func (r *AttunePolicyReconciler) mergeDefaults(policy *attunev1alpha1.AttunePolicy, defaults *attunev1alpha1.AttuneDefaults) {
	if defaults == nil {
		ctrl.Log.V(1).Info("No AttuneDefaults configured, using built-in values only")
		return
	}
	inherited := pkgdefaults.MergeDefaults(policy, defaults)
	if len(inherited) > 0 {
		ctrl.Log.V(1).Info("Merged effective defaults into policy",
			"defaultsName", defaults.Name,
			"fieldsInherited", inherited)
	} else {
		ctrl.Log.V(1).Info("All policy fields already set, no defaults applied",
			"defaultsName", defaults.Name)
	}
}

// isWithinResizeWindow returns true if the current time falls within the
// configured resize schedule. Returns true if no schedule is configured.
func isWithinResizeWindow(schedule *attunev1alpha1.ResizeSchedule, now time.Time) bool {
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

	// Check time windows first, then validate day-of-week (which may need
	// to check the previous day for overnight windows).
	if len(schedule.Windows) == 0 {
		// No windows: all times are allowed, just check day-of-week.
		if len(schedule.DaysOfWeek) > 0 {
			return isDayAllowed(localNow.Weekday(), schedule.DaysOfWeek)
		}
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
				if len(schedule.DaysOfWeek) > 0 && !isDayAllowed(localNow.Weekday(), schedule.DaysOfWeek) {
					continue
				}
				return true
			}
		} else {
			// Overnight window: e.g. 22:00-06:00
			if nowMinutes >= start {
				// Pre-midnight portion: check today's day-of-week.
				if len(schedule.DaysOfWeek) > 0 && !isDayAllowed(localNow.Weekday(), schedule.DaysOfWeek) {
					continue
				}
				return true
			}
			if nowMinutes < end {
				// Post-midnight portion: the window opened yesterday,
				// so check yesterday's day-of-week.
				if len(schedule.DaysOfWeek) > 0 && !isDayAllowed(localNow.Add(-24*time.Hour).Weekday(), schedule.DaysOfWeek) {
					continue
				}
				return true
			}
		}
	}
	return false
}

// isDayAllowed checks whether the given weekday is in the allowed list
// (case-insensitive comparison).
func isDayAllowed(day time.Weekday, allowed []string) bool {
	dayName := day.String()
	for _, d := range allowed {
		if strings.EqualFold(d, dayName) {
			return true
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
