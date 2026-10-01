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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

// Not parallel: ValidateCreate and mutateContainer increment metrics.
//
// Percentile 75 is rejected by the ResourceConfig CRD enum
// Enum=0;50;90;95;99. A container minAllowed above maxAllowed is
// webhook-only. The CRD quantity rule stays on policy and defaults
// spec.cpu and spec.memory.

func TestValidateCreate_ContainerPolicyWideFields(t *testing.T) {
	validator := &AttunePolicyValidator{}
	ctx := context.Background()
	boost := &attunev1alpha1.StartupBoost{
		Multiplier: "2",
		Duration:   metav1.Duration{Duration: time.Minute},
	}
	empty := ""
	margin := int32(10)
	mult := "2"
	oom := &attunev1alpha1.OOMBump{}
	surge := &attunev1alpha1.Surge{TriggerRatio: "2"}

	cases := []struct {
		name    string
		cpu     *attunev1alpha1.ResourceConfig
		memory  *attunev1alpha1.ResourceConfig
		field   string
		wantSub string
	}{
		{
			name:    "cpu startupBoost",
			cpu:     &attunev1alpha1.ResourceConfig{StartupBoost: boost},
			field:   "startupBoost",
			wantSub: "containerPolicies[0].cpu.startupBoost",
		},
		{
			name:    "memory startupBoost",
			memory:  &attunev1alpha1.ResourceConfig{StartupBoost: boost},
			field:   "startupBoost",
			wantSub: "containerPolicies[0].memory.startupBoost",
		},
		{
			name:    "memoryFromCpuRatio",
			memory:  &attunev1alpha1.ResourceConfig{MemoryFromCPURatio: &empty},
			field:   "memoryFromCpuRatio",
			wantSub: "containerPolicies[0].memory.memoryFromCpuRatio",
		},
		{
			name:    "decreaseUsageMarginPercent",
			memory:  &attunev1alpha1.ResourceConfig{DecreaseUsageMarginPercent: &margin},
			field:   "decreaseUsageMarginPercent",
			wantSub: "containerPolicies[0].memory.decreaseUsageMarginPercent",
		},
		{
			name:    "cpu limitMultiplier",
			cpu:     &attunev1alpha1.ResourceConfig{LimitMultiplier: &mult},
			field:   "limitMultiplier",
			wantSub: "containerPolicies[0].cpu.limitMultiplier",
		},
		{
			name:    "memory limitMultiplier",
			memory:  &attunev1alpha1.ResourceConfig{LimitMultiplier: &mult},
			field:   "limitMultiplier",
			wantSub: "containerPolicies[0].memory.limitMultiplier",
		},
		{
			name:    "cpu oomBump",
			cpu:     &attunev1alpha1.ResourceConfig{OOMBump: oom},
			field:   "oomBump",
			wantSub: "containerPolicies[0].cpu.oomBump",
		},
		{
			name:    "memory oomBump",
			memory:  &attunev1alpha1.ResourceConfig{OOMBump: oom},
			field:   "oomBump",
			wantSub: "containerPolicies[0].memory.oomBump",
		},
		{
			name:    "cpu surge",
			cpu:     &attunev1alpha1.ResourceConfig{Surge: surge},
			field:   "surge",
			wantSub: "containerPolicies[0].cpu.surge",
		},
		{
			name:    "memory surge",
			memory:  &attunev1alpha1.ResourceConfig{Surge: surge},
			field:   "surge",
			wantSub: "containerPolicies[0].memory.surge",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			policy := validPolicy()
			policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
				ContainerName: "app",
				CPU:           tt.cpu,
				Memory:        tt.memory,
			}}
			_, err := validator.ValidateCreate(ctx, policy)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.field)
			assert.Contains(t, err.Error(), "policy-wide")
			assert.Contains(t, err.Error(), tt.wantSub)
		})
	}
}

func TestValidateCreate_ContainerPolicyNames(t *testing.T) {
	validator := &AttunePolicyValidator{}
	ctx := context.Background()

	t.Run("duplicate", func(t *testing.T) {
		policy := validPolicy()
		policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{
			{ContainerName: "app"},
			{ContainerName: "app"},
		}
		_, err := validator.ValidateCreate(ctx, policy)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `duplicate containerName "app"`)
	})

	t.Run("two stars", func(t *testing.T) {
		policy := validPolicy()
		policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{
			{ContainerName: "*"},
			{ContainerName: "*"},
		}
		_, err := validator.ValidateCreate(ctx, policy)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `only one "*" entry is allowed`)
	})

	t.Run("empty name", func(t *testing.T) {
		policy := validPolicy()
		policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
			ContainerName: "",
		}}
		_, err := validator.ValidateCreate(ctx, policy)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "containerPolicies[0].containerName must not be empty")
	})
}

func TestValidateCreate_ContainerPolicyPercentileAndBounds(t *testing.T) {
	validator := &AttunePolicyValidator{}
	ctx := context.Background()

	t.Run("percentile 75", func(t *testing.T) {
		policy := validPolicy()
		policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
			ContainerName: "app",
			CPU:           &attunev1alpha1.ResourceConfig{Percentile: 75},
		}}
		_, err := validator.ValidateCreate(ctx, policy)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "containerPolicies[0].cpu.percentile 75 is not supported")
	})

	t.Run("min greater than max", func(t *testing.T) {
		policy := validPolicy()
		minAllowed := mustQty(t, "500m")
		maxAllowed := mustQty(t, "100m")
		policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
			ContainerName: "app",
			CPU: &attunev1alpha1.ResourceConfig{
				MinAllowed: &minAllowed,
				MaxAllowed: &maxAllowed,
			},
		}}
		_, err := validator.ValidateCreate(ctx, policy)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "minAllowed")
		assert.Contains(t, err.Error(), "maxAllowed")
	})

	t.Run("valid entry and star", func(t *testing.T) {
		policy := validPolicy()
		maxAllowed := mustQty(t, "200m")
		policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
			ContainerName: "app",
			CPU: &attunev1alpha1.ResourceConfig{
				Percentile: 95,
				MaxAllowed: &maxAllowed,
			},
		}}
		_, err := validator.ValidateCreate(ctx, policy)
		require.NoError(t, err)

		policy = validPolicy()
		policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
			ContainerName: "*",
			CPU:           &attunev1alpha1.ResourceConfig{Percentile: 95},
		}}
		_, err = validator.ValidateCreate(ctx, policy)
		require.NoError(t, err)
	})

	t.Run("controlled values are allowed", func(t *testing.T) {
		policy := validPolicy()
		both := attunev1alpha1.ControlledRequestsAndLimits
		policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
			ContainerName: "app",
			CPU: &attunev1alpha1.ResourceConfig{
				ControlledValues: &both,
			},
		}}
		_, err := validator.ValidateCreate(ctx, policy)
		require.NoError(t, err)
	})
}

func TestMutateContainer_PerContainerControlledValues(t *testing.T) {
	both := attunev1alpha1.ControlledRequestsAndLimits
	policy := validPolicy()
	policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
		ContainerName: "app",
		CPU:           &attunev1alpha1.ResourceConfig{ControlledValues: &both},
	}, {
		ContainerName: "sidecar",
	}}
	recLimit := mustQty(t, "1600m")
	recReq := mustQty(t, "800m")
	liveLimit := mustQty(t, "1000m")
	rec := &attunev1alpha1.WorkloadRecommendation{
		Containers: []attunev1alpha1.ContainerRecommendation{
			{
				Name: "app",
				Recommended: attunev1alpha1.ResourceValues{
					CPURequest: recReq,
					CPULimit:   recLimit,
				},
			},
			{
				Name: "sidecar",
				Recommended: attunev1alpha1.ResourceValues{
					CPURequest: recReq,
					CPULimit:   recLimit,
				},
			},
		},
	}
	handler := &PodMutatingHandler{}
	app := &corev1.Container{
		Name: "app",
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceCPU: liveLimit.DeepCopy()},
		},
	}
	_, _ = handler.mutateContainer(app, rec, policy, "Deployment")
	assert.Equal(t, int64(1600), app.Resources.Limits.Cpu().MilliValue())

	sidecar := &corev1.Container{
		Name: "sidecar",
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceCPU: liveLimit.DeepCopy()},
		},
	}
	_, _ = handler.mutateContainer(sidecar, rec, policy, "Deployment")
	assert.Equal(t, int64(1000), sidecar.Resources.Limits.Cpu().MilliValue())
}

func TestApplyCreateStartupBoost_ContainerMaxCaps(t *testing.T) {
	maxFour := mustQty(t, "4")
	sideMax := mustQty(t, "200m")
	max600 := mustQty(t, "600m")
	policy := validPolicy()
	policy.Spec.CPU.MaxAllowed = &maxFour
	policy.Spec.CPU.StartupBoost = &attunev1alpha1.StartupBoost{
		Multiplier: "2",
		Duration:   metav1.Duration{Duration: time.Minute},
	}
	policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
		ContainerName: "sidecar",
		CPU:           &attunev1alpha1.ResourceConfig{MaxAllowed: &sideMax},
	}}
	request := mustQty(t, "500m")
	sidecar := &corev1.Container{
		Name: "sidecar",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: request.DeepCopy()},
		},
	}
	raised := applyCreateStartupBoost(sidecar, policy, resource.Quantity{})
	assert.False(t, raised)
	assert.Equal(t, int64(200), sidecar.Resources.Requests.Cpu().MilliValue())

	app := &corev1.Container{
		Name: "app",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: request.DeepCopy()},
		},
	}
	raised = applyCreateStartupBoost(app, policy, resource.Quantity{})
	assert.True(t, raised)
	assert.Equal(t, int64(1000), app.Resources.Requests.Cpu().MilliValue())

	policy.Spec.CPU.MaxAllowed = &max600
	capped := &corev1.Container{
		Name: "app",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: request.DeepCopy()},
		},
	}
	raised = applyCreateStartupBoost(capped, policy, resource.Quantity{})
	assert.True(t, raised)
	assert.Equal(t, int64(600), capped.Resources.Requests.Cpu().MilliValue())
}

func TestApplyCreateStartupBoost_MaxClampPreservesQoS(t *testing.T) {
	both := attunev1alpha1.ControlledRequestsAndLimits
	only := attunev1alpha1.ControlledRequestsOnly
	handler := &PodMutatingHandler{}
	makePolicy := func(cv string) *attunev1alpha1.AttunePolicy {
		maxAllowed := mustQty(t, "200m")
		mode := cv
		policy := validPolicy()
		policy.Spec.CPU.StartupBoost = &attunev1alpha1.StartupBoost{
			Multiplier: "2",
			Duration:   metav1.Duration{Duration: time.Minute},
		}
		policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
			ContainerName: "app",
			CPU: &attunev1alpha1.ResourceConfig{
				MaxAllowed:       &maxAllowed,
				ControlledValues: &mode,
			},
		}}
		return policy
	}
	rec := func(request, limit string) *attunev1alpha1.WorkloadRecommendation {
		return &attunev1alpha1.WorkloadRecommendation{
			Containers: []attunev1alpha1.ContainerRecommendation{{
				Name: "app",
				Recommended: attunev1alpha1.ResourceValues{
					CPURequest: mustQty(t, request),
					CPULimit:   mustQty(t, limit),
				},
			}},
		}
	}

	t.Run("guaranteed request and limit both drop to the container max", func(t *testing.T) {
		app := &corev1.Container{Name: "app"}
		_, boosted := handler.mutateContainer(app, rec("500m", "500m"), makePolicy(both), "Deployment")
		assert.False(t, boosted)
		assert.Equal(t, int64(200), app.Resources.Requests.Cpu().MilliValue())
		assert.Equal(t, int64(200), app.Resources.Limits.Cpu().MilliValue())
	})

	t.Run("limit already above the request stays above the container max", func(t *testing.T) {
		app := &corev1.Container{Name: "app"}
		_, boosted := handler.mutateContainer(app, rec("500m", "1"), makePolicy(both), "Deployment")
		assert.False(t, boosted)
		assert.Equal(t, int64(200), app.Resources.Requests.Cpu().MilliValue())
		assert.Equal(t, int64(1000), app.Resources.Limits.Cpu().MilliValue())
	})

	t.Run("requests only does not lower a higher user limit", func(t *testing.T) {
		app := &corev1.Container{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{corev1.ResourceCPU: mustQty(t, "1")},
			},
		}
		_, boosted := handler.mutateContainer(app, rec("500m", "500m"), makePolicy(only), "Deployment")
		assert.False(t, boosted)
		assert.Equal(t, int64(200), app.Resources.Requests.Cpu().MilliValue())
		assert.Equal(t, int64(1000), app.Resources.Limits.Cpu().MilliValue())
	})

	t.Run("requests only live limit below the max still caps the published request", func(t *testing.T) {
		policy := makePolicy(only)
		direct := &corev1.Container{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: mustQty(t, "500m")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: mustQty(t, "100m")},
			},
		}
		raised := applyCreateStartupBoost(direct, policy, mustQty(t, "500m"))
		assert.False(t, raised)
		assert.Equal(t, int64(200), direct.Resources.Requests.Cpu().MilliValue())
		assert.Equal(t, int64(100), direct.Resources.Limits.Cpu().MilliValue())

		app := &corev1.Container{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{corev1.ResourceCPU: mustQty(t, "100m")},
			},
		}
		_, boosted := handler.mutateContainer(app, rec("500m", "500m"), policy, "Deployment")
		assert.False(t, boosted)
		assert.Equal(t, int64(100), app.Resources.Requests.Cpu().MilliValue())
		assert.Equal(t, int64(100), app.Resources.Limits.Cpu().MilliValue())
	})
}

func TestValidateCreate_EffectiveContainerBoundsAndCaps(t *testing.T) {
	validator := &AttunePolicyValidator{}
	ctx := context.Background()

	t.Run("policy floor above container ceiling", func(t *testing.T) {
		policy := validPolicy()
		minAllowed := mustQty(t, "500m")
		maxAllowed := mustQty(t, "200m")
		policy.Spec.CPU.MinAllowed = &minAllowed
		policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
			ContainerName: "sidecar",
			CPU:           &attunev1alpha1.ResourceConfig{MaxAllowed: &maxAllowed},
		}}
		_, err := validator.ValidateCreate(ctx, policy)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "sidecar")
		assert.Contains(t, err.Error(), "cpu")
		assert.Contains(t, err.Error(), "500m")
		assert.Contains(t, err.Error(), "200m")
		assert.Contains(t, err.Error(), "must be <=")
	})

	t.Run("policy floor above star ceiling", func(t *testing.T) {
		policy := validPolicy()
		minAllowed := mustQty(t, "500m")
		maxAllowed := mustQty(t, "200m")
		policy.Spec.CPU.MinAllowed = &minAllowed
		policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
			ContainerName: "*",
			CPU:           &attunev1alpha1.ResourceConfig{MaxAllowed: &maxAllowed},
		}}
		_, err := validator.ValidateCreate(ctx, policy)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "*")
		assert.Contains(t, err.Error(), "500m")
		assert.Contains(t, err.Error(), "200m")
		assert.Contains(t, err.Error(), "must be <=")
	})

	t.Run("container cpu max above 256 cores", func(t *testing.T) {
		policy := validPolicy()
		maxAllowed := mustQty(t, "257")
		policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
			ContainerName: "app",
			CPU:           &attunev1alpha1.ResourceConfig{MaxAllowed: &maxAllowed},
		}}
		_, err := validator.ValidateCreate(ctx, policy)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "containerPolicies[0].cpu.maxAllowed")
		assert.Contains(t, err.Error(), "256 cores")
	})

	t.Run("container memory max above 16Ti", func(t *testing.T) {
		policy := validPolicy()
		maxAllowed := mustQty(t, "17Ti")
		policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
			ContainerName: "app",
			Memory:        &attunev1alpha1.ResourceConfig{MaxAllowed: &maxAllowed},
		}}
		_, err := validator.ValidateCreate(ctx, policy)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "containerPolicies[0].memory.maxAllowed")
		assert.Contains(t, err.Error(), "16Ti")
	})
}

func TestCreatePodRequestsOnly(t *testing.T) {
	both := attunev1alpha1.ControlledRequestsAndLimits
	only := attunev1alpha1.ControlledRequestsOnly
	always := corev1.ContainerRestartPolicyAlways

	t.Run("empty list uses the policy block", func(t *testing.T) {
		policy := validPolicy()
		policy.Spec.CPU.ControlledValues = &both
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}}
		assert.False(t, createPodRequestsOnly(policy, pod, corev1.ResourceCPU))
		assert.True(t, createPodRequestsOnly(policy, pod, corev1.ResourceMemory))
	})

	t.Run("one requests and limits container lifts the envelope", func(t *testing.T) {
		policy := validPolicy()
		policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
			ContainerName: "app",
			CPU:           &attunev1alpha1.ResourceConfig{ControlledValues: &both},
		}}
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "app"},
			{Name: "sidecar"},
		}}}
		assert.False(t, createPodRequestsOnly(policy, pod, corev1.ResourceCPU))
		assert.True(t, createPodRequestsOnly(policy, pod, corev1.ResourceMemory))
	})

	t.Run("every managed container requests only keeps the skip", func(t *testing.T) {
		policy := validPolicy()
		policy.Spec.CPU.ControlledValues = &both
		policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
			ContainerName: "app",
			CPU:           &attunev1alpha1.ResourceConfig{ControlledValues: &only},
		}, {
			ContainerName: "sidecar",
			CPU:           &attunev1alpha1.ResourceConfig{ControlledValues: &only},
		}}
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "app"},
			{Name: "sidecar"},
		}}}
		assert.True(t, createPodRequestsOnly(policy, pod, corev1.ResourceCPU))
	})

	t.Run("excluded sidecar does not force a lift", func(t *testing.T) {
		policy := validPolicy()
		policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
			ContainerName: "istio-proxy",
			CPU:           &attunev1alpha1.ResourceConfig{ControlledValues: &both},
		}}
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "app"},
			{Name: "istio-proxy"},
		}}}
		assert.True(t, createPodRequestsOnly(policy, pod, corev1.ResourceCPU))
	})

	t.Run("native sidecar counts", func(t *testing.T) {
		policy := validPolicy()
		policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
			ContainerName: "sidecar",
			CPU:           &attunev1alpha1.ResourceConfig{ControlledValues: &both},
		}}
		pod := &corev1.Pod{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
			InitContainers: []corev1.Container{{
				Name:          "sidecar",
				RestartPolicy: &always,
			}, {
				Name: "init",
			}},
		}}
		assert.False(t, createPodRequestsOnly(policy, pod, corev1.ResourceCPU))
	})

	t.Run("normal init is ignored", func(t *testing.T) {
		policy := validPolicy()
		policy.Spec.ContainerPolicies = []attunev1alpha1.ContainerResourcePolicy{{
			ContainerName: "init",
			CPU:           &attunev1alpha1.ResourceConfig{ControlledValues: &both},
		}}
		pod := &corev1.Pod{Spec: corev1.PodSpec{
			Containers:     []corev1.Container{{Name: "app"}},
			InitContainers: []corev1.Container{{Name: "init"}},
		}}
		assert.True(t, createPodRequestsOnly(policy, pod, corev1.ResourceCPU))
	})
}
