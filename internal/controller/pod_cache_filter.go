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
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/transform"
)

// refreshPodCacheFilterThrottle limits how often we rebuild dynamic selectors.
const refreshPodCacheFilterThrottle = 30 * time.Second

// lastPodFilterRefresh is package-level so all reconciler instances share the
// throttle (single manager). Protected by PodCacheFilter's own mutex for the
// selector set; the timestamp uses atomic store via sync.Map on reconciler.
func (r *AttunePolicyReconciler) refreshPodCacheFilter(ctx context.Context) {
	if r.PodCacheFilter == nil {
		return
	}
	// Throttle using lastBlockerRefresh-style map key.
	const key = "__pod_cache_filter__"
	if v, ok := r.lastBlockerRefresh.Load(key); ok {
		if last, ok := v.(time.Time); ok && r.now().Sub(last) < refreshPodCacheFilterThrottle {
			return
		}
	}
	r.lastBlockerRefresh.Store(key, r.now())

	logger := log.FromContext(ctx)
	var policies attunev1alpha1.AttunePolicyList
	if err := r.List(ctx, &policies); err != nil {
		logger.V(1).Info("Pod cache filter refresh: list policies failed", "err", err)
		return
	}

	// Collect unique label selectors from each policy's target workloads.
	seen := map[string]labels.Selector{}
	for i := range policies.Items {
		p := &policies.Items[i]
		workloads, err := r.discoverWorkloads(ctx, p)
		if err != nil {
			continue
		}
		for _, w := range workloads {
			m := r.getPodSelectorLabels(w)
			if len(m) == 0 {
				continue
			}
			// Stable key for dedup.
			sel := transform.SelectorFromMap(m)
			if sel == nil {
				continue
			}
			seen[sel.String()] = sel
		}
	}
	sels := make([]labels.Selector, 0, len(seen))
	for _, s := range seen {
		sels = append(sels, s)
	}
	r.PodCacheFilter.UpdateDynamic(sels)
	logger.V(1).Info("Refreshed pod cache filter selectors", "count", len(sels))
}

// liveReader returns APIReader when set (bypasses cache), else Client.
func (r *AttunePolicyReconciler) liveReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}
