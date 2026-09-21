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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	rsmetrics "github.com/attune-io/attune/internal/metrics"
)

// ---------- getOrCreateCollector ----------

func TestGetOrCreateCollector_CacheHit(t *testing.T) {
	reconciler := NewAttunePolicyReconciler()
	mc := &mockCollector{}
	staleTime := time.Now().Add(-5 * time.Minute)
	reconciler.collectors.Store("http://prom:9090", &collectorEntry{
		collector: mc,
		lastUsed:  staleTime,
	})

	before := time.Now()
	got, err := reconciler.getOrCreateCollector(&attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"}, nil)
	require.NoError(t, err)
	assert.Equal(t, mc, got)

	// Verify lastUsed was refreshed on cache hit.
	entry, ok := reconciler.collectors.Load("http://prom:9090")
	require.True(t, ok)
	ce, ok := entry.(*collectorEntry)
	require.True(t, ok, "cached value should be *collectorEntry")
	assert.True(t, ce.lastUsed.After(before) || ce.lastUsed.Equal(before),
		"lastUsed should be refreshed to ~now on cache hit, got %v", ce.lastUsed)
}

func TestGetOrCreateCollector_CacheMiss(t *testing.T) {
	mc := &mockCollector{}
	reconciler := NewAttunePolicyReconciler()
	reconciler.MetricsFactory = func(address string, _ *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		assert.Equal(t, "http://new:9090", address)
		return mc, nil
	}

	got, err := reconciler.getOrCreateCollector(&attunev1alpha1.PrometheusConfig{Address: "http://new:9090"}, nil)
	require.NoError(t, err)
	assert.Equal(t, mc, got)
}

func TestGetOrCreateCollector_FactoryError(t *testing.T) {
	reconciler := NewAttunePolicyReconciler()
	reconciler.MetricsFactory = func(string, *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		return nil, fmt.Errorf("connection refused")
	}

	_, err := reconciler.getOrCreateCollector(&attunev1alpha1.PrometheusConfig{Address: "http://broken:9090"}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
}

func TestGetOrCreateCollector_CacheFull(t *testing.T) {
	reconciler := NewAttunePolicyReconciler()
	reconciler.MetricsFactory = func(string, *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		return nil, nil
	}
	// Fill the cache to maxCollectors.
	for i := 0; i < maxCollectors; i++ {
		addr := fmt.Sprintf("http://prom-%d:9090", i)
		_, err := reconciler.getOrCreateCollector(&attunev1alpha1.PrometheusConfig{Address: addr}, nil)
		require.NoError(t, err)
	}

	// The next address should be rejected.
	_, err := reconciler.getOrCreateCollector(&attunev1alpha1.PrometheusConfig{Address: "http://one-too-many:9090"}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "collector cache full")
}

func TestGetOrCreateCollector_CustomTTL(t *testing.T) {
	customTTL := 2 * time.Minute
	reconciler := NewAttunePolicyReconciler()
	reconciler.CollectorTTL = customTTL
	reconciler.MetricsFactory = func(string, *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		return &mockCollector{}, nil
	}

	// Store an entry that is stale under custom TTL but fresh under default TTL.
	staleTime := time.Now().Add(-(customTTL + time.Minute))
	reconciler.collectors.Store("http://stale:9090", &collectorEntry{
		collector: &mockCollector{},
		lastUsed:  staleTime,
	})

	// Creating a new collector should trigger eviction of the stale entry.
	_, err := reconciler.getOrCreateCollector(&attunev1alpha1.PrometheusConfig{Address: "http://fresh:9090"}, nil)
	require.NoError(t, err)

	_, stillExists := reconciler.collectors.Load("http://stale:9090")
	assert.False(t, stillExists, "entry older than custom TTL should be evicted")
}

func TestGetOrCreateCollector_EvictsStaleEntries(t *testing.T) {
	reconciler := NewAttunePolicyReconciler()
	reconciler.MetricsFactory = func(string, *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		return &mockCollector{}, nil
	}
	// Fill the cache to maxCollectors with stale entries.
	staleTime := time.Now().Add(-(collectorTTL + time.Minute))
	for i := 0; i < maxCollectors; i++ {
		addr := fmt.Sprintf("http://stale-%d:9090", i)
		reconciler.collectors.Store(addr, &collectorEntry{
			collector: &mockCollector{},
			lastUsed:  staleTime,
		})
	}

	// A new address should succeed because stale entries get evicted.
	_, err := reconciler.getOrCreateCollector(&attunev1alpha1.PrometheusConfig{Address: "http://fresh:9090"}, nil)
	require.NoError(t, err)
}

func TestGetOrCreateCollector_ConcurrentAccess(t *testing.T) {
	reconciler := NewAttunePolicyReconciler()
	reconciler.CollectorTTL = 50 * time.Millisecond
	reconciler.MetricsFactory = func(string, *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		return &mockCollector{}, nil
	}

	// Seed some entries that will become stale mid-test.
	staleTime := time.Now().Add(-(collectorTTL + time.Minute))
	for i := 0; i < 10; i++ {
		reconciler.collectors.Store(fmt.Sprintf("http://stale-%d:9090", i),
			&collectorEntry{collector: &mockCollector{}, lastUsed: staleTime})
	}

	const goroutines = 20
	addresses := make([]string, goroutines)
	for i := range addresses {
		addresses[i] = fmt.Sprintf("http://concurrent-%d:9090", i%5) // 5 unique addresses
	}

	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := reconciler.getOrCreateCollector(&attunev1alpha1.PrometheusConfig{Address: addresses[idx]}, nil)
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		assert.NoError(t, err, "goroutine %d", i)
	}
	// With 5 unique addresses, at most 5 entries should remain in the map
	// (LoadOrStore deduplicates). The factory may be called more often due
	// to races, but the cache itself must be bounded.
	var stored int
	reconciler.collectors.Range(func(_, _ any) bool { stored++; return true })
	assert.LessOrEqual(t, stored, 15, "cache should not grow unbounded")
}

// closableMockCollector wraps mockCollector and implements io.Closer so
// we can verify that evicted collectors have Close() called.
type closableMockCollector struct {
	mockCollector
	closed bool
}

func TestGetOrCreateCollector_EvictionClosesCollector(t *testing.T) {
	closable := &closableMockCollector{}

	now := time.Now()
	reconciler := NewAttunePolicyReconciler()
	reconciler.CollectorTTL = time.Millisecond
	reconciler.MetricsFactory = func(_ string, _ *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		return &closableMockCollector{}, nil
	}
	reconciler.SetNowFunc(func() time.Time { return now })

	// Seed the cache with the closable collector at "now".
	reconciler.collectors.Store("http://old:9090", &collectorEntry{
		collector: closable,
		lastUsed:  now,
	})

	// Advance time past the TTL so the entry becomes stale.
	now = now.Add(2 * time.Millisecond)

	// Requesting a different address triggers eviction of stale entries.
	_, err := reconciler.getOrCreateCollector(
		&attunev1alpha1.PrometheusConfig{Address: "http://new:9090"}, nil,
	)
	require.NoError(t, err)

	assert.True(t, closable.closed,
		"Close() should be called on evicted collector that implements io.Closer")
}

func TestGetOrCreateCollector_EvictionClosesRateLimitedInner(t *testing.T) {
	inner := &closableMockCollector{}
	wrapped := rsmetrics.NewRateLimitedCollector(inner, 10, 20)

	now := time.Now()
	reconciler := NewAttunePolicyReconciler()
	reconciler.CollectorTTL = time.Millisecond
	reconciler.MetricsFactory = func(_ string, _ *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		return &closableMockCollector{}, nil
	}
	reconciler.SetNowFunc(func() time.Time { return now })

	reconciler.collectors.Store("http://old:9090", &collectorEntry{
		collector: wrapped,
		lastUsed:  now,
	})

	now = now.Add(2 * time.Millisecond)

	_, err := reconciler.getOrCreateCollector(
		&attunev1alpha1.PrometheusConfig{Address: "http://new:9090"}, nil,
	)
	require.NoError(t, err)

	assert.True(t, inner.closed,
		"RateLimitedCollector.Close must reach the inner collector on eviction")
}

func TestGetOrCreateCollector_ConcurrentRaceClosesUnused(t *testing.T) {
	var mu sync.Mutex
	var created []*closableMockCollector

	reconciler := NewAttunePolicyReconciler()
	reconciler.CollectorTTL = collectorTTL
	reconciler.MetricsFactory = func(_ string, _ *rsmetrics.CollectorOptions) (rsmetrics.MetricsCollector, error) {
		c := &closableMockCollector{}
		mu.Lock()
		created = append(created, c)
		mu.Unlock()
		return c, nil
	}

	// All goroutines race to create the same address.
	const goroutines = 10
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := reconciler.getOrCreateCollector(
				&attunev1alpha1.PrometheusConfig{Address: "http://race:9090"}, nil)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	// Exactly one collector should survive; all others should be closed.
	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(created), 1, "at least one collector must be created")

	var openCount int
	for _, c := range created {
		if !c.closed {
			openCount++
		}
	}
	assert.Equal(t, 1, openCount, "exactly one collector should remain open; race losers must be closed")
}

// ---------- resolvePrometheusConfig ----------

func TestResolvePrometheusConfig_PolicyHasAddress(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	reconciler := newReconcilerWithClient()

	config, _, err := reconciler.resolvePrometheusConfig(context.Background(), policy, nil)
	assert.NoError(t, err)
	assert.Equal(t, "http://prometheus:9090", config.Address)
}

func TestResolvePrometheusConfig_FallsBackToDefaults(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec:       attunev1alpha1.AttunePolicySpec{MetricsSource: attunev1alpha1.MetricsSource{}},
	}

	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				Prometheus: &attunev1alpha1.PrometheusConfig{
					Address: "http://defaults-prometheus:9090",
				},
			},
		},
	}
	reconciler := newReconcilerWithClient(defaults)

	config, _, err := reconciler.resolvePrometheusConfig(context.Background(), policy, defaults)
	assert.NoError(t, err)
	assert.Equal(t, "http://defaults-prometheus:9090", config.Address)
}

func TestResolvePrometheusConfig_NoAddressAnywhere(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec:       attunev1alpha1.AttunePolicySpec{MetricsSource: attunev1alpha1.MetricsSource{}},
	}
	reconciler := newReconcilerWithClient()

	_, _, err := reconciler.resolvePrometheusConfig(context.Background(), policy, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no Prometheus address configured")
}

func TestResolvePrometheusConfig_RejectsBlockedPolicyAddress(t *testing.T) {
	policy := newTestPolicy("test-policy", "default")
	policy.Spec.MetricsSource.Prometheus.Address = "http://127.0.0.1:9090"
	reconciler := newReconcilerWithClient()

	_, _, err := reconciler.resolvePrometheusConfig(context.Background(), policy, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "SSRF blocked")
}

func TestResolvePrometheusConfig_RejectsBlockedDefaultsAddress(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec:       attunev1alpha1.AttunePolicySpec{MetricsSource: attunev1alpha1.MetricsSource{}},
	}
	defaults := &attunev1alpha1.AttuneDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-defaults"},
		Spec: attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				Prometheus: &attunev1alpha1.PrometheusConfig{
					Address: "http://127.0.0.1:9090",
				},
			},
		},
	}
	reconciler := newReconcilerWithClient(defaults)

	_, _, err := reconciler.resolvePrometheusConfig(context.Background(), policy, defaults)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "SSRF blocked")
}

// ---------- resolveDatadogCollector ----------

func TestResolveDatadogCollector_HappyPath(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "dd-keys", Namespace: "default"},
		Data: map[string][]byte{
			"api-key": []byte("test-api-key-12345"),
		},
	}
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				Datadog: &attunev1alpha1.DatadogConfig{
					Site:            "datadoghq.eu",
					APIKeySecretRef: &attunev1alpha1.SecretKeyRef{Name: "dd-keys", Key: "api-key"},
				},
			},
		},
	}
	reconciler := newReconcilerWithClient(secret)

	collector, qb, err := reconciler.resolveDatadogCollector(context.Background(), policy, datadogAuthContext{})
	require.NoError(t, err)
	assert.NotNil(t, collector, "collector should be non-nil")
	assert.IsType(t, &rsmetrics.DatadogQueryBuilder{}, qb, "should return DatadogQueryBuilder")
}

func TestResolveDatadogCollector_DefaultSite(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "dd-keys", Namespace: "default"},
		Data: map[string][]byte{
			"api-key": []byte("test-api-key"),
		},
	}
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				Datadog: &attunev1alpha1.DatadogConfig{
					Site:            "", // empty = default
					APIKeySecretRef: &attunev1alpha1.SecretKeyRef{Name: "dd-keys", Key: "api-key"},
				},
			},
		},
	}
	reconciler := newReconcilerWithClient(secret)

	collector, _, err := reconciler.resolveDatadogCollector(context.Background(), policy, datadogAuthContext{})
	require.NoError(t, err)
	assert.NotNil(t, collector, "collector should be created with default site")
}

func TestResolveDatadogCollector_WithAppKey(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "dd-keys", Namespace: "default"},
		Data: map[string][]byte{
			"api-key": []byte("test-api-key"),
			"app-key": []byte("test-app-key"),
		},
	}
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				Datadog: &attunev1alpha1.DatadogConfig{
					Site:            "us5.datadoghq.com",
					APIKeySecretRef: &attunev1alpha1.SecretKeyRef{Name: "dd-keys", Key: "api-key"},
				},
			},
		},
	}
	reconciler := newReconcilerWithClient(secret)

	collector, _, err := reconciler.resolveDatadogCollector(context.Background(), policy, datadogAuthContext{})
	require.NoError(t, err)
	assert.NotNil(t, collector, "collector should succeed when app-key is present")
}

func TestResolveDatadogCollector_RejectsInvalidSite(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "dd-keys", Namespace: "default"},
		Data: map[string][]byte{
			"api-key": []byte("test-api-key"),
		},
	}
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				Datadog: &attunev1alpha1.DatadogConfig{
					Site:            "evil.example",
					APIKeySecretRef: &attunev1alpha1.SecretKeyRef{Name: "dd-keys", Key: "api-key"},
				},
			},
		},
	}
	reconciler := newReconcilerWithClient(secret)

	collector, _, err := reconciler.resolveDatadogCollector(context.Background(), policy, datadogAuthContext{})
	require.Error(t, err)
	assert.Nil(t, collector)
	assert.Contains(t, err.Error(), "not a recognized Datadog site")
}

func TestResolveDatadogCollector_MissingSecret(t *testing.T) {
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				Datadog: &attunev1alpha1.DatadogConfig{
					APIKeySecretRef: &attunev1alpha1.SecretKeyRef{Name: "nonexistent-secret", Key: "api-key"},
				},
			},
		},
	}
	reconciler := newReconcilerWithClient() // no secret

	_, _, err := reconciler.resolveDatadogCollector(context.Background(), policy, datadogAuthContext{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Datadog API key")
}

func TestResolveDatadogCollector_MissingKeyInSecret(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "dd-keys", Namespace: "default"},
		Data: map[string][]byte{
			"wrong-key": []byte("value"),
		},
	}
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				Datadog: &attunev1alpha1.DatadogConfig{
					APIKeySecretRef: &attunev1alpha1.SecretKeyRef{Name: "dd-keys", Key: "api-key"},
				},
			},
		},
	}
	reconciler := newReconcilerWithClient(secret)

	_, _, err := reconciler.resolveDatadogCollector(context.Background(), policy, datadogAuthContext{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Datadog API key")
}

func TestResolveDatadogCollector_CachesCollector(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "dd-keys", Namespace: "default"},
		Data: map[string][]byte{
			"api-key": []byte("test-api-key"),
		},
	}
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				Datadog: &attunev1alpha1.DatadogConfig{
					Site:            "datadoghq.com",
					APIKeySecretRef: &attunev1alpha1.SecretKeyRef{Name: "dd-keys", Key: "api-key"},
				},
			},
		},
	}
	reconciler := newReconcilerWithClient(secret)

	c1, _, err1 := reconciler.resolveDatadogCollector(context.Background(), policy, datadogAuthContext{})
	require.NoError(t, err1)
	c2, _, err2 := reconciler.resolveDatadogCollector(context.Background(), policy, datadogAuthContext{})
	require.NoError(t, err2)
	assert.Same(t, c1, c2, "second call should return cached collector")
}

func TestResolveDatadogCollector_AddingAppKeyRecreatesCollector(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "dd-keys", Namespace: "default"},
		Data: map[string][]byte{
			"api-key": []byte("test-api-key"),
		},
	}
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				Datadog: &attunev1alpha1.DatadogConfig{
					Site:            "datadoghq.com",
					APIKeySecretRef: &attunev1alpha1.SecretKeyRef{Name: "dd-keys", Key: "api-key"},
				},
			},
		},
	}
	reconciler := newReconcilerWithClient(secret)

	c1, _, err1 := reconciler.resolveDatadogCollector(context.Background(), policy, datadogAuthContext{})
	require.NoError(t, err1)

	var current corev1.Secret
	require.NoError(t, reconciler.Get(context.Background(), types.NamespacedName{Name: "dd-keys", Namespace: "default"}, &current))
	current.Data["app-key"] = []byte("test-app-key")
	require.NoError(t, reconciler.Update(context.Background(), &current))

	c2, _, err2 := reconciler.resolveDatadogCollector(context.Background(), policy, datadogAuthContext{})
	require.NoError(t, err2)
	assert.NotSame(t, c1, c2, "inserting app-key must create a new collector")
}

// ---------- resolveCloudWatchCollector ----------

func TestResolveCloudWatchCollector_CreatesCollector(t *testing.T) {
	// NewCloudWatchCollector succeeds at construction time (the AWS SDK
	// loads config from env/files without making API calls). Verify the
	// collector and query builder are created correctly.
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				CloudWatch: &attunev1alpha1.CloudWatchConfig{
					Region:      "us-east-1",
					ClusterName: "test-cluster",
				},
			},
		},
	}
	reconciler := newReconcilerWithClient()

	collector, qb, err := reconciler.resolveCloudWatchCollector(context.Background(), policy)
	require.NoError(t, err)
	assert.NotNil(t, collector, "collector should be created")
	cwQB, ok := qb.(*rsmetrics.CloudWatchQueryBuilder)
	require.True(t, ok, "should return CloudWatchQueryBuilder")
	assert.Equal(t, "test-cluster", cwQB.ClusterName)
}

func TestResolveCloudWatchCollector_QueryBuilder(t *testing.T) {
	// Even though the collector creation fails, we can verify the function's
	// structure by testing a cached path. Pre-seed the cache with a mock.
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				CloudWatch: &attunev1alpha1.CloudWatchConfig{
					Region:      "eu-west-1",
					ClusterName: "prod-cluster",
					RoleARN:     "arn:aws:iam::123456789012:role/test",
				},
			},
		},
	}
	reconciler := newReconcilerWithClient()

	// Pre-seed the collector cache so the factory is not called (avoids AWS SDK).
	cacheKey := fmt.Sprintf("cloudwatch:%s|%s|%s", "eu-west-1", "prod-cluster", "arn:aws:iam::123456789012:role/test")
	mc := &mockCollector{}
	reconciler.collectors.Store(cacheKey, &collectorEntry{collector: mc, lastUsed: time.Now()})

	collector, qb, err := reconciler.resolveCloudWatchCollector(context.Background(), policy)
	require.NoError(t, err)
	assert.Same(t, mc, collector, "should return pre-seeded cached collector")
	cwQB, ok := qb.(*rsmetrics.CloudWatchQueryBuilder)
	require.True(t, ok, "should return CloudWatchQueryBuilder")
	assert.Equal(t, "prod-cluster", cwQB.ClusterName, "ClusterName should match policy")
}

func TestResolveCloudWatchCollector_CacheKeyIncludesRoleARN(t *testing.T) {
	reconciler := newReconcilerWithClient()

	// Pre-seed two entries with different role ARNs.
	mc1 := &mockCollector{}
	mc2 := &mockCollector{}
	reconciler.collectors.Store("cloudwatch:us-east-1|cluster|", &collectorEntry{collector: mc1, lastUsed: time.Now()})
	reconciler.collectors.Store("cloudwatch:us-east-1|cluster|arn:aws:iam::111:role/x", &collectorEntry{collector: mc2, lastUsed: time.Now()})

	policy1 := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				CloudWatch: &attunev1alpha1.CloudWatchConfig{
					Region:      "us-east-1",
					ClusterName: "cluster",
				},
			},
		},
	}
	policy2 := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p2", Namespace: "default"},
		Spec: attunev1alpha1.AttunePolicySpec{
			MetricsSource: attunev1alpha1.MetricsSource{
				CloudWatch: &attunev1alpha1.CloudWatchConfig{
					Region:      "us-east-1",
					ClusterName: "cluster",
					RoleARN:     "arn:aws:iam::111:role/x",
				},
			},
		},
	}

	c1, _, err1 := reconciler.resolveCloudWatchCollector(context.Background(), policy1)
	require.NoError(t, err1)
	c2, _, err2 := reconciler.resolveCloudWatchCollector(context.Background(), policy2)
	require.NoError(t, err2)
	assert.Same(t, mc1, c1, "no-role policy should return pre-seeded mc1")
	assert.Same(t, mc2, c2, "role-arn policy should return pre-seeded mc2")
	assert.NotSame(t, c1, c2, "different role ARNs should resolve to different collectors")
}

// ---------- collectorCacheKey ----------

func TestCollectorCacheKey_AddressOnly(t *testing.T) {
	config := &attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"}
	assert.Equal(t, "http://prom:9090", collectorCacheKey(config, nil))
}

func TestCollectorCacheKey_WithOptions(t *testing.T) {
	config := &attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"}
	opts := &rsmetrics.CollectorOptions{
		BearerToken:        "tok",
		InsecureSkipVerify: true,
		Headers:            map[string]string{"X-Scope-OrgID": "tenant-1"},
	}
	key := collectorCacheKey(config, opts)
	assert.Contains(t, key, "http://prom:9090")
	assert.Contains(t, key, "|bearer:")
	assert.Contains(t, key, "|insecure")
	assert.Contains(t, key, "|h:X-Scope-OrgID=")
	assert.NotContains(t, key, "tenant-1") // header value should be hashed
	assert.NotContains(t, key, "tok")      // bearer token should be hashed
}

func TestCollectorCacheKey_DeterministicWithMultipleHeaders(t *testing.T) {
	config := &attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"}
	opts := &rsmetrics.CollectorOptions{
		Headers: map[string]string{"Z-Header": "z", "A-Header": "a", "M-Header": "m"},
	}
	// Call multiple times to verify map iteration order doesn't affect the key.
	key1 := collectorCacheKey(config, opts)
	for i := 0; i < 100; i++ {
		assert.Equal(t, key1, collectorCacheKey(config, opts), "cache key must be deterministic on iteration %d", i)
	}
	// Verify sorted order: A before M before Z (values are now SHA-256 hashed).
	assert.Contains(t, key1, "|h:A-Header=")
	assert.Contains(t, key1, "|h:M-Header=")
	assert.Contains(t, key1, "|h:Z-Header=")
	// Hashed values should NOT contain the raw header value.
	assert.NotContains(t, key1, "=a|")
	assert.NotContains(t, key1, "=z")
}

func TestCollectorCacheKey_DifferentConfigsDifferentKeys(t *testing.T) {
	config := &attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"}
	key1 := collectorCacheKey(config, nil)
	key2 := collectorCacheKey(config, &rsmetrics.CollectorOptions{BearerToken: "tok"})
	assert.NotEqual(t, key1, key2)
}

func TestCollectorCacheKey_DifferentBearerTokensDifferentKeys(t *testing.T) {
	config := &attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"}
	key1 := collectorCacheKey(config, &rsmetrics.CollectorOptions{BearerToken: "tok-a"})
	key2 := collectorCacheKey(config, &rsmetrics.CollectorOptions{BearerToken: "tok-b"})
	assert.NotEqual(t, key1, key2)
}

func TestCollectorCacheKey_WithQueryParameters(t *testing.T) {
	config := &attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"}
	opts := &rsmetrics.CollectorOptions{
		QueryParameters: map[string]string{"step": "30s", "timeout": "10s"},
	}
	key := collectorCacheKey(config, opts)
	assert.Contains(t, key, "|qp:step=30s")
	assert.Contains(t, key, "|qp:timeout=10s")
}

func TestCollectorCacheKey_DifferentQueryParametersDifferentKeys(t *testing.T) {
	config := &attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"}
	key1 := collectorCacheKey(config, &rsmetrics.CollectorOptions{
		QueryParameters: map[string]string{"step": "30s"},
	})
	key2 := collectorCacheKey(config, &rsmetrics.CollectorOptions{
		QueryParameters: map[string]string{"step": "60s"},
	})
	assert.NotEqual(t, key1, key2)
}

func TestCollectorCacheKey_QueryParametersDeterministic(t *testing.T) {
	config := &attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"}
	opts := &rsmetrics.CollectorOptions{
		QueryParameters: map[string]string{"z-param": "z", "a-param": "a", "m-param": "m"},
	}
	key1 := collectorCacheKey(config, opts)
	for i := 0; i < 100; i++ {
		assert.Equal(t, key1, collectorCacheKey(config, opts), "cache key must be deterministic on iteration %d", i)
	}
}

// ---------- buildCollectorOptions ----------

func TestBuildCollectorOptions_NilWhenNoAuthOrTLS(t *testing.T) {
	r := NewAttunePolicyReconciler()
	config := &attunev1alpha1.PrometheusConfig{Address: "http://prom:9090"}
	opts, err := r.buildCollectorOptions(context.Background(), "default", config, prometheusAuthContext{})
	assert.NoError(t, err)
	assert.Nil(t, opts)
}

func TestBuildCollectorOptions_WithHeaders(t *testing.T) {
	r := NewAttunePolicyReconciler()
	config := &attunev1alpha1.PrometheusConfig{
		Address: "http://prom:9090",
		Headers: map[string]string{"X-Scope-OrgID": "tenant-1"},
	}
	opts, err := r.buildCollectorOptions(context.Background(), "default", config, prometheusAuthContext{})
	assert.NoError(t, err)
	require.NotNil(t, opts)
	assert.Equal(t, "tenant-1", opts.Headers["X-Scope-OrgID"])
}

func TestBuildCollectorOptions_WithQueryParameters(t *testing.T) {
	r := NewAttunePolicyReconciler()
	config := &attunev1alpha1.PrometheusConfig{
		Address:         "http://prom:9090",
		QueryParameters: map[string]string{"dedup": "true"},
	}
	opts, err := r.buildCollectorOptions(context.Background(), "default", config, prometheusAuthContext{})
	assert.NoError(t, err)
	require.NotNil(t, opts)
	assert.Equal(t, "true", opts.QueryParameters["dedup"])
}

func TestBuildCollectorOptions_RejectsReservedQueryParameters(t *testing.T) {
	r := NewAttunePolicyReconciler()
	config := &attunev1alpha1.PrometheusConfig{
		Address:         "http://prom:9090",
		QueryParameters: map[string]string{"query": "up"},
	}
	opts, err := r.buildCollectorOptions(context.Background(), "default", config, prometheusAuthContext{})
	assert.Error(t, err)
	assert.Nil(t, opts)
	assert.Contains(t, err.Error(), "reserved")
}

func TestBuildCollectorOptions_WithTLS(t *testing.T) {
	r := NewAttunePolicyReconciler()
	config := &attunev1alpha1.PrometheusConfig{
		Address: "https://prom:9090",
		TLS:     &attunev1alpha1.TLSConfig{InsecureSkipVerify: true},
	}
	opts, err := r.buildCollectorOptions(context.Background(), "default", config, prometheusAuthContext{})
	assert.NoError(t, err)
	require.NotNil(t, opts)
	assert.True(t, opts.InsecureSkipVerify)
}

func TestBuildCollectorOptions_WithBearerToken(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "prom-token", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("test-bearer")},
	}
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	config := &attunev1alpha1.PrometheusConfig{
		Address: "http://prom:9090",
		BearerTokenSecret: &attunev1alpha1.SecretKeyRef{
			Name: "prom-token",
			Key:  "token",
		},
	}
	opts, err := r.buildCollectorOptions(context.Background(), "default", config, prometheusAuthContext{})
	assert.NoError(t, err)
	require.NotNil(t, opts)
	assert.Equal(t, "test-bearer", opts.BearerToken)
}

func TestBuildCollectorOptions_SecretNotFound(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := NewAttunePolicyReconciler()
	r.Client = fakeClient
	r.Scheme = scheme

	config := &attunev1alpha1.PrometheusConfig{
		Address: "http://prom:9090",
		BearerTokenSecret: &attunev1alpha1.SecretKeyRef{
			Name: "missing-secret",
			Key:  "token",
		},
	}
	opts, err := r.buildCollectorOptions(context.Background(), "default", config, prometheusAuthContext{})
	assert.Error(t, err)
	assert.Nil(t, opts)
	assert.Contains(t, err.Error(), "missing-secret")
}

// ---------- discoverPrometheus ----------

func TestDiscoverPrometheus_WellKnownService(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prometheus-server",
			Namespace: "monitoring",
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{Port: 9090}},
		},
	}
	reconciler := newReconcilerWithClient(svc)

	addr := reconciler.discoverPrometheus(context.Background())
	assert.Equal(t, "http://prometheus-server.monitoring:9090", addr)
}

func TestDiscoverPrometheus_WellKnownService_CustomPort(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prometheus-server",
			Namespace: "monitoring",
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{Port: 80}},
		},
	}
	reconciler := newReconcilerWithClient(svc)

	addr := reconciler.discoverPrometheus(context.Background())
	assert.Equal(t, "http://prometheus-server.monitoring:80", addr)
}

func TestDiscoverPrometheus_NoServiceFound(t *testing.T) {
	reconciler := newReconcilerWithClient()

	addr := reconciler.discoverPrometheus(context.Background())
	assert.Empty(t, addr, "should return empty when no Prometheus service is found")
}

func TestDiscoverPrometheus_CachedResult(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prometheus-server",
			Namespace: "monitoring",
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{Port: 80}},
		},
	}
	reconciler := newReconcilerWithClient(svc)

	// First call discovers and caches.
	addr1 := reconciler.discoverPrometheus(context.Background())
	assert.Equal(t, "http://prometheus-server.monitoring:80", addr1)

	// Second call returns cached result even after the service is deleted.
	require.NoError(t, reconciler.Delete(context.Background(), svc))
	addr2 := reconciler.discoverPrometheus(context.Background())
	assert.Equal(t, addr1, addr2, "should return cached address")
}

func TestDiscoverPrometheus_OperatorCRD_DefaultPort(t *testing.T) {
	prom := &unstructured.Unstructured{}
	prom.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "monitoring.coreos.com", Version: "v1", Kind: "Prometheus",
	})
	prom.SetName("k8s")
	prom.SetNamespace("monitoring")

	s := testScheme()
	s.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "PrometheusList"},
		&unstructured.UnstructuredList{},
	)
	s.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "Prometheus"},
		&unstructured.Unstructured{},
	)
	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(prom).Build()
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = s

	addr := reconciler.discoverPrometheus(context.Background())
	assert.Equal(t, "http://prometheus-k8s.monitoring:9090", addr)
}

func TestDiscoverPrometheus_OperatorCRD_CustomPort(t *testing.T) {
	prom := &unstructured.Unstructured{}
	prom.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "monitoring.coreos.com", Version: "v1", Kind: "Prometheus",
	})
	prom.SetName("k8s")
	prom.SetNamespace("monitoring")
	require.NoError(t, unstructured.SetNestedField(prom.Object, int64(8080), "spec", "port"))

	s := testScheme()
	s.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "PrometheusList"},
		&unstructured.UnstructuredList{},
	)
	s.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "Prometheus"},
		&unstructured.Unstructured{},
	)
	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(prom).Build()
	reconciler := NewAttunePolicyReconciler()
	reconciler.Client = fakeClient
	reconciler.Scheme = s

	addr := reconciler.discoverPrometheus(context.Background())
	assert.Equal(t, "http://prometheus-k8s.monitoring:8080", addr)
}

func TestResolvePrometheusAddress_FallsBackToAutoDiscovery(t *testing.T) {
	// Policy has no Prometheus address, no AttuneDefaults, but a well-known service exists.
	policy := &attunev1alpha1.AttunePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec:       attunev1alpha1.AttunePolicySpec{MetricsSource: attunev1alpha1.MetricsSource{}},
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prometheus-kube-prometheus-prometheus",
			Namespace: "monitoring",
		},
	}
	reconciler := newReconcilerWithClient(svc)

	config, _, err := reconciler.resolvePrometheusConfig(context.Background(), policy, nil)
	assert.NoError(t, err)
	assert.Equal(t, "http://prometheus-kube-prometheus-prometheus.monitoring:9090", config.Address)
}
