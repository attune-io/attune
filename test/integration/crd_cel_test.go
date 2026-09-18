//go:build integration

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

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

func createCELNamespace(t *testing.T, cl client.Client, name string) string {
	t.Helper()
	require.NoError(t, cl.Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}))
	return name
}

// These cases use startIsolatedEnvtest (CRDs only, no webhook) so the API
// server is the only admission gate.
func TestCRDCEL_RejectsStartupBoostDurationOutsideRange(t *testing.T) {
	_, cl := startIsolatedEnvtest(t)
	ctx := context.Background()
	ns := createCELNamespace(t, cl, "cel-boost")

	cases := []struct {
		name string
		d    time.Duration
	}{
		{name: "boost-short", d: 5 * time.Second},
		{name: "boost-long", d: 2 * time.Hour},
	}
	for _, tc := range cases {
		policy := newTestPolicy(tc.name, ns, "api-server")
		policy.Spec.CPU.StartupBoost = &attunev1alpha1.StartupBoost{
			Multiplier: "2.0",
			Duration:   metav1.Duration{Duration: tc.d},
		}
		err := cl.Create(ctx, policy)
		require.True(t, apierrors.IsInvalid(err), "duration %s: want Invalid, got %v", tc.d, err)
		require.Contains(t, err.Error(), "startupBoost.duration must be between 10s and 1h")
	}
}

func TestCRDCEL_AcceptsStartupBoostDurationInRange(t *testing.T) {
	_, cl := startIsolatedEnvtest(t)
	ctx := context.Background()
	ns := createCELNamespace(t, cl, "cel-boost-ok")

	policy := newTestPolicy("boost-ok", ns, "api-server")
	policy.Spec.CPU.StartupBoost = &attunev1alpha1.StartupBoost{
		Multiplier: "2.0",
		Duration:   metav1.Duration{Duration: 10 * time.Second},
	}
	require.NoError(t, cl.Create(ctx, policy))
}

func TestCRDCEL_RejectsMinAllowedGreaterThanMaxAllowed(t *testing.T) {
	_, cl := startIsolatedEnvtest(t)
	ctx := context.Background()
	ns := createCELNamespace(t, cl, "cel-bounds")

	policy := newTestPolicy("bounds-bad", ns, "api-server")
	policy.Spec.CPU.MinAllowed = quantityPtr("4000m")
	policy.Spec.CPU.MaxAllowed = quantityPtr("50m")
	err := cl.Create(ctx, policy)
	require.True(t, apierrors.IsInvalid(err), "want Invalid, got %v", err)
	require.Contains(t, err.Error(), "minAllowed must be less than or equal to maxAllowed")
}
