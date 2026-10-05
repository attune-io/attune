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

package webhook

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func surgeValidator(t *testing.T, objs ...client.Object) *AttunePolicyValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, attunev1alpha1.AddToScheme(scheme))
	return &AttunePolicyValidator{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()}
}

func TestValidate_SurgeUsesCombinedHistory(t *testing.T) {
	window := func(h time.Duration) *metav1.Duration {
		return &metav1.Duration{Duration: h}
	}
	ns := func(h time.Duration) *attunev1alpha1.AttuneNamespaceDefaults {
		return &attunev1alpha1.AttuneNamespaceDefaults{
			ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "default"},
			Spec: attunev1alpha1.AttuneDefaultsSpec{
				MetricsSource: &attunev1alpha1.MetricsSource{HistoryWindow: window(h)},
			},
		}
	}
	cluster := func(h time.Duration) *attunev1alpha1.AttuneDefaults {
		return &attunev1alpha1.AttuneDefaults{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
			Spec: attunev1alpha1.AttuneDefaultsSpec{
				MetricsSource: &attunev1alpha1.MetricsSource{HistoryWindow: window(h)},
			},
		}
	}
	policy := func(surge time.Duration) *attunev1alpha1.AttunePolicy {
		p := validPolicy()
		p.Namespace = "default"
		p.Spec.CPU.Surge = &attunev1alpha1.Surge{Window: window(surge)}
		return p
	}

	_, err := surgeValidator(t, ns(24*time.Hour)).ValidateCreate(context.Background(), policy(48*time.Hour))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "historyWindow")

	_, err = surgeValidator(t, cluster(336*time.Hour)).ValidateCreate(context.Background(), policy(200*time.Hour))
	require.NoError(t, err)

	_, err = surgeValidator(t, cluster(336*time.Hour), ns(24*time.Hour)).ValidateCreate(context.Background(), policy(48*time.Hour))
	require.Error(t, err)

	explicit := policy(48 * time.Hour)
	explicit.Spec.MetricsSource.HistoryWindow = window(24 * time.Hour)
	_, err = surgeValidator(t, cluster(336*time.Hour)).ValidateCreate(context.Background(), explicit)
	require.Error(t, err)

	base := surgeValidator(t)
	_, err = (&AttunePolicyValidator{Client: errListReader{Reader: base.Client}}).ValidateCreate(context.Background(), policy(48*time.Hour))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listing AttuneDefaults")
}

type errListReader struct{ client.Reader }

func (errListReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return fmt.Errorf("listing AttuneDefaults: injected")
}

func TestValidate_Surge(t *testing.T) {
	validator := &AttunePolicyValidator{}

	t.Run("omitted surge stays valid", func(t *testing.T) {
		_, err := validator.ValidateCreate(context.Background(), validPolicy())
		assert.NoError(t, err)
	})

	t.Run("empty block is valid on cpu and memory", func(t *testing.T) {
		policy := validPolicy()
		policy.Spec.CPU.Surge = &attunev1alpha1.Surge{}
		policy.Spec.Memory.Surge = &attunev1alpha1.Surge{}
		_, err := validator.ValidateCreate(context.Background(), policy)
		assert.NoError(t, err)
	})

	t.Run("window equal to the default history is accepted", func(t *testing.T) {
		policy := validPolicy()
		pct := int32(50)
		policy.Spec.CPU.Surge = &attunev1alpha1.Surge{
			TriggerRatio: "100",
			Percentile:   &pct,
			Window:       &metav1.Duration{Duration: 168 * time.Hour},
		}
		_, err := validator.ValidateCreate(context.Background(), policy)
		assert.NoError(t, err)
	})

	t.Run("window equal to a configured history is accepted", func(t *testing.T) {
		policy := validPolicy()
		policy.Spec.MetricsSource.HistoryWindow = &metav1.Duration{Duration: 2 * time.Hour}
		policy.Spec.CPU.Surge = &attunev1alpha1.Surge{
			Window: &metav1.Duration{Duration: 2 * time.Hour},
		}
		policy.Spec.Memory.Surge = &attunev1alpha1.Surge{
			TriggerRatio: "1.5",
			Window:       &metav1.Duration{Duration: 2 * time.Hour},
		}
		_, err := validator.ValidateCreate(context.Background(), policy)
		assert.NoError(t, err)
	})

	cases := []struct {
		name    string
		mutate  func(*attunev1alpha1.AttunePolicy)
		wantErr string
	}{
		{
			name: "trigger ratio 1",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.CPU.Surge = &attunev1alpha1.Surge{TriggerRatio: "1"}
			},
			wantErr: "cpu.surge.triggerRatio must be greater than 1",
		},
		{
			name: "trigger ratio 0",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.CPU.Surge = &attunev1alpha1.Surge{TriggerRatio: "0"}
			},
			wantErr: "cpu.surge.triggerRatio must be greater than 1",
		},
		{
			name: "trigger ratio NaN",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.Memory.Surge = &attunev1alpha1.Surge{TriggerRatio: "NaN"}
			},
			wantErr: "memory.surge.triggerRatio must be a finite number",
		},
		{
			name: "trigger ratio Inf",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.CPU.Surge = &attunev1alpha1.Surge{TriggerRatio: "Inf"}
			},
			wantErr: "cpu.surge.triggerRatio must be a finite number",
		},
		{
			name: "trigger ratio negative",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.CPU.Surge = &attunev1alpha1.Surge{TriggerRatio: "-1"}
			},
			wantErr: "cpu.surge.triggerRatio must be greater than 1",
		},
		{
			name: "trigger ratio above 100",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.CPU.Surge = &attunev1alpha1.Surge{TriggerRatio: "100.1"}
			},
			wantErr: "cpu.surge.triggerRatio must be <= 100",
		},
		{
			name: "percentile 0",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				zero := int32(0)
				p.Spec.CPU.Surge = &attunev1alpha1.Surge{Percentile: &zero}
			},
			wantErr: "cpu.surge.percentile 0",
		},
		{
			name: "percentile 75",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				bad := int32(75)
				p.Spec.Memory.Surge = &attunev1alpha1.Surge{Percentile: &bad}
			},
			wantErr: "memory.surge.percentile 75",
		},
		{
			name: "window 0",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.CPU.Surge = &attunev1alpha1.Surge{Window: &metav1.Duration{}}
			},
			wantErr: "cpu.surge.window must be at least 5m, or omit it",
		},
		{
			name: "window below 5m",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.CPU.Surge = &attunev1alpha1.Surge{Window: &metav1.Duration{Duration: 4 * time.Minute}}
			},
			wantErr: "cpu.surge.window must be at least 5m",
		},
		{
			name: "window longer than default history",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.CPU.Surge = &attunev1alpha1.Surge{Window: &metav1.Duration{Duration: 168*time.Hour + time.Minute}}
			},
			wantErr: "must not exceed historyWindow",
		},
		{
			name: "window longer than configured history",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.MetricsSource.HistoryWindow = &metav1.Duration{Duration: 2 * time.Hour}
				p.Spec.Memory.Surge = &attunev1alpha1.Surge{Window: &metav1.Duration{Duration: 2*time.Hour + time.Minute}}
			},
			wantErr: "memory.surge.window",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := validPolicy()
			tc.mutate(policy)
			_, err := validator.ValidateCreate(context.Background(), policy)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestValidateDefaults_Surge(t *testing.T) {
	t.Run("empty block is valid", func(t *testing.T) {
		_, err := validateDefaultsSpec(attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{Surge: &attunev1alpha1.Surge{}},
		})
		assert.NoError(t, err)
	})

	t.Run("rejects the same edges as a policy", func(t *testing.T) {
		zero := int32(0)
		_, err := validateDefaultsSpec(attunev1alpha1.AttuneDefaultsSpec{
			Memory: &attunev1alpha1.ResourceConfig{
				Surge: &attunev1alpha1.Surge{TriggerRatio: "NaN", Percentile: &zero},
			},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "memory.surge.triggerRatio")

		_, err = validateDefaultsSpec(attunev1alpha1.AttuneDefaultsSpec{
			MetricsSource: &attunev1alpha1.MetricsSource{
				HistoryWindow: &metav1.Duration{Duration: time.Hour},
			},
			CPU: &attunev1alpha1.ResourceConfig{
				Surge: &attunev1alpha1.Surge{Window: &metav1.Duration{Duration: 2 * time.Hour}},
			},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cpu.surge.window")
		assert.Contains(t, err.Error(), "must not exceed historyWindow")

		_, err = validateDefaultsSpec(attunev1alpha1.AttuneDefaultsSpec{
			CPU: &attunev1alpha1.ResourceConfig{
				Surge: &attunev1alpha1.Surge{Window: &metav1.Duration{Duration: 168 * time.Hour}},
			},
		})
		assert.NoError(t, err)
	})
}

// noListReader fails the test on any List call, proving admission did not
// read defaults.
type noListReader struct {
	client.Reader
	t *testing.T
}

func (r noListReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	r.t.Helper()
	r.t.Fatalf("admission listed defaults")
	return nil
}

// clusterListErrReader serves the namespace defaults list and fails only the
// cluster defaults list, so a test can tell the two reads apart.
type clusterListErrReader struct{ client.Reader }

func (r clusterListErrReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*attunev1alpha1.AttuneDefaultsList); ok {
		return fmt.Errorf("cluster list injected")
	}
	return r.Reader.List(ctx, list, opts...)
}

func TestValidate_SurgeHistoryListOnlyWhenNeeded(t *testing.T) {
	window := func(h time.Duration) *metav1.Duration {
		return &metav1.Duration{Duration: h}
	}
	base := func() *attunev1alpha1.AttunePolicy {
		p := validPolicy()
		p.Namespace = "default"
		return p
	}
	fakeReader := surgeValidator(t).Client
	errReader := func(*testing.T) client.Reader { return errListReader{Reader: fakeReader} }
	noList := func(t *testing.T) client.Reader { return noListReader{Reader: fakeReader, t: t} }
	clusterErr := func(*testing.T) client.Reader { return clusterListErrReader{Reader: fakeReader} }

	cases := []struct {
		name    string
		mutate  func(*attunev1alpha1.AttunePolicy)
		reader  func(*testing.T) client.Reader
		wantErr []string
	}{
		{
			name:   "no surge, no history, list error ignored",
			mutate: func(*attunev1alpha1.AttunePolicy) {},
			reader: errReader,
		},
		{
			name:   "no surge, no history, no list",
			mutate: func(*attunev1alpha1.AttunePolicy) {},
			reader: noList,
		},
		{
			name: "empty cpu surge block, no history, list error ignored",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.CPU.Surge = &attunev1alpha1.Surge{}
			},
			reader: errReader,
		},
		{
			name: "policy history covers the surge window without a list",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.MetricsSource.HistoryWindow = window(24 * time.Hour)
				p.Spec.CPU.Surge = &attunev1alpha1.Surge{Window: window(2 * time.Hour)}
			},
			reader: errReader,
		},
		{
			name: "policy history still bounds the surge window",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.MetricsSource.HistoryWindow = window(24 * time.Hour)
				p.Spec.CPU.Surge = &attunev1alpha1.Surge{Window: window(48 * time.Hour)}
			},
			reader:  errReader,
			wantErr: []string{"must not exceed historyWindow (24h0m0s)"},
		},
		{
			name: "cpu surge window without history rejects on a list error",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.CPU.Surge = &attunev1alpha1.Surge{Window: window(48 * time.Hour)}
			},
			reader:  errReader,
			wantErr: []string{"listing AttuneNamespaceDefaults in default", "injected"},
		},
		{
			name: "memory surge window without history rejects on a list error",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.Memory.Surge = &attunev1alpha1.Surge{Window: window(2 * time.Hour)}
			},
			reader:  errReader,
			wantErr: []string{"listing AttuneNamespaceDefaults in default", "injected"},
		},
		{
			name: "surge window without history rejects on a cluster list error",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.CPU.Surge = &attunev1alpha1.Surge{Window: window(2 * time.Hour)}
			},
			reader:  clusterErr,
			wantErr: []string{"listing AttuneDefaults: cluster list injected"},
		},
		{
			name: "container surge is rejected as policy-wide without a list",
			mutate: func(p *attunev1alpha1.AttunePolicy) {
				p.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
					ContainerName: "app",
					CPU:           &attunev1alpha1.ResourceConfig{Surge: &attunev1alpha1.Surge{Window: window(2 * time.Hour)}},
				}}
			},
			reader:  noList,
			wantErr: []string{"surge is policy-wide"},
		},
	}
	for _, tc := range cases {
		check := func(t *testing.T, err error) {
			t.Helper()
			if len(tc.wantErr) == 0 {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			for _, sub := range tc.wantErr {
				assert.Contains(t, err.Error(), sub)
			}
		}
		t.Run(tc.name+"/create", func(t *testing.T) {
			policy := base()
			tc.mutate(policy)
			v := &AttunePolicyValidator{Client: tc.reader(t)}
			_, err := v.ValidateCreate(context.Background(), policy)
			check(t, err)
		})
		t.Run(tc.name+"/update", func(t *testing.T) {
			policy := base()
			tc.mutate(policy)
			v := &AttunePolicyValidator{Client: tc.reader(t)}
			_, err := v.ValidateUpdate(context.Background(), base(), policy)
			check(t, err)
		})
	}
}
