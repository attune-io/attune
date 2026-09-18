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

package resize

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func qty(t *testing.T, s string) resource.Quantity {
	t.Helper()
	q, err := resource.ParseQuantity(s)
	require.NoError(t, err)
	return q
}

func envelopePod(cpuReq, memReq, cpuLim, memLim string, env *corev1.ResourceRequirements) *corev1.Pod {
	pod := newTestPod("web-0", "default", "app", cpuReq, memReq, cpuLim, memLim)
	pod.Spec.Resources = env
	return pod
}

func TestRaiseToCover(t *testing.T) {
	tests := []struct {
		name         string
		pod          *corev1.Pod
		wantNil      bool
		wantCPUReq   string
		wantMemReq   string
		wantCPULim   string
		wantMemLim   string
		wantNoCPULim bool
		wantNoMemLim bool
	}{
		{
			name:    "nil envelope is not invented",
			pod:     newTestPod("web-0", "default", "app", "100m", "128Mi", "200m", "256Mi"),
			wantNil: true,
		},
		{
			name: "raise requests to cover container sum",
			pod: envelopePod("300m", "256Mi", "300m", "256Mi", &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("200m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
			}),
			wantCPUReq:   "300m",
			wantMemReq:   "256Mi",
			wantNoCPULim: true,
			wantNoMemLim: true,
		},
		{
			name: "never shrink envelope requests",
			pod: envelopePod("100m", "64Mi", "200m", "128Mi", &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			}),
			wantCPUReq:   "500m",
			wantMemReq:   "1Gi",
			wantNoCPULim: true,
			wantNoMemLim: true,
		},
		{
			name: "limits are max of current, needed request, and max container limit",
			pod: func() *corev1.Pod {
				pod := newTestPod("web-0", "default", "app", "400m", "256Mi", "800m", "512Mi")
				pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
					Name: "sidecar",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("64Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("200m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				})
				pod.Spec.Resources = &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("200m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("300m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				}
				return pod
			}(),
			// needed CPU = max(200m, 400+100=500m) = 500m
			// limit = max(300m, 500m, max(800m,200m)=800m) = 800m
			wantCPUReq: "500m",
			wantMemReq: "320Mi",
			wantCPULim: "800m",
			wantMemLim: "512Mi",
		},
		{
			name: "after raise limit is at least request",
			pod: envelopePod("400m", "256Mi", "400m", "256Mi", &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("200m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				},
			}),
			wantCPUReq: "400m",
			wantMemReq: "256Mi",
			wantCPULim: "400m",
			wantMemLim: "256Mi",
		},
		{
			name: "missing envelope limit is not invented",
			pod: envelopePod("300m", "256Mi", "600m", "512Mi", &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("200m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
			}),
			wantCPUReq:   "300m",
			wantMemReq:   "256Mi",
			wantNoCPULim: true,
			wantNoMemLim: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RaiseToCover(tt.pod)
			if tt.wantNil {
				assert.Nil(t, got)
				assert.Nil(t, tt.pod.Spec.Resources)
				return
			}
			require.NotNil(t, got)
			if tt.wantCPUReq != "" {
				assert.True(t, got.Requests.Cpu().Equal(qty(t, tt.wantCPUReq)),
					"cpu request: want %s got %s", tt.wantCPUReq, got.Requests.Cpu().String())
			}
			if tt.wantMemReq != "" {
				assert.True(t, got.Requests.Memory().Equal(qty(t, tt.wantMemReq)),
					"memory request: want %s got %s", tt.wantMemReq, got.Requests.Memory().String())
			}
			if tt.wantNoCPULim {
				_, ok := got.Limits[corev1.ResourceCPU]
				assert.False(t, ok, "cpu limit must not be invented")
			} else if tt.wantCPULim != "" {
				assert.True(t, got.Limits.Cpu().Equal(qty(t, tt.wantCPULim)),
					"cpu limit: want %s got %s", tt.wantCPULim, got.Limits.Cpu().String())
			}
			if tt.wantNoMemLim {
				_, ok := got.Limits[corev1.ResourceMemory]
				assert.False(t, ok, "memory limit must not be invented")
			} else if tt.wantMemLim != "" {
				assert.True(t, got.Limits.Memory().Equal(qty(t, tt.wantMemLim)),
					"memory limit: want %s got %s", tt.wantMemLim, got.Limits.Memory().String())
			}
			if req, ok := got.Requests[corev1.ResourceCPU]; ok {
				if lim, lok := got.Limits[corev1.ResourceCPU]; lok {
					assert.GreaterOrEqual(t, lim.Cmp(req), 0, "cpu limit must be >= request")
				}
			}
			if req, ok := got.Requests[corev1.ResourceMemory]; ok {
				if lim, lok := got.Limits[corev1.ResourceMemory]; lok {
					assert.GreaterOrEqual(t, lim.Cmp(req), 0, "memory limit must be >= request")
				}
			}
		})
	}
}

func TestEvaluateEnvelope(t *testing.T) {
	increaseTarget := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("400m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	decreaseTarget := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
	}

	tests := []struct {
		name          string
		in            EnvelopeInput
		wantSkip      bool
		wantRaisedCPU string
	}{
		{
			name: "nil envelope does not skip or raise",
			in: EnvelopeInput{
				Pod:       newTestPod("web-0", "default", "app", "100m", "128Mi", "200m", "256Mi"),
				Container: "app",
				Target:    increaseTarget,
				InPlace:   true,
			},
		},
		{
			name: "in-place off increase that exceeds envelope is skipped",
			in: EnvelopeInput{
				Pod: envelopePod("100m", "128Mi", "200m", "256Mi", &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("150m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				}),
				Container: "app",
				Target:    increaseTarget,
				InPlace:   false,
			},
			wantSkip: true,
		},
		{
			name: "in-place off decrease under envelope is not skipped",
			in: EnvelopeInput{
				Pod: envelopePod("100m", "128Mi", "200m", "256Mi", &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("150m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				}),
				Container: "app",
				Target:    decreaseTarget,
				InPlace:   false,
			},
		},
		{
			name: "in-place on raises envelope to cover sum",
			in: EnvelopeInput{
				Pod: envelopePod("100m", "128Mi", "1", "1Gi", &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("150m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
				}),
				Container: "app",
				Target:    increaseTarget,
				InPlace:   true,
			},
			wantRaisedCPU: "400m",
		},
		{
			name: "RequestsOnly Burstable skips when raise would lift envelope limit",
			in: EnvelopeInput{
				Pod: func() *corev1.Pod {
					p := envelopePod("100m", "128Mi", "1", "1Gi", &corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("150m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("200m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					})
					p.Status.QOSClass = corev1.PodQOSBurstable
					return p
				}(),
				Container:       "app",
				Target:          increaseTarget,
				InPlace:         true,
				RequestsOnlyCPU: true,
				RequestsOnlyMem: true,
			},
			wantSkip: true,
		},
		{
			name: "RequestsOnly Burstable proceeds when existing limit already covers",
			in: EnvelopeInput{
				Pod: func() *corev1.Pod {
					p := envelopePod("100m", "128Mi", "1", "1Gi", &corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("150m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1"),
							corev1.ResourceMemory: resource.MustParse("1Gi"),
						},
					})
					p.Status.QOSClass = corev1.PodQOSBurstable
					return p
				}(),
				Container:       "app",
				Target:          increaseTarget,
				InPlace:         true,
				RequestsOnlyCPU: true,
				RequestsOnlyMem: true,
			},
			wantRaisedCPU: "400m",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateEnvelope(tt.in)
			assert.Equal(t, tt.wantSkip, got.Skip)
			if tt.wantSkip {
				assert.Equal(t, ReasonEnvelopeConstraint, got.Reason)
				return
			}
			assert.Empty(t, got.Reason)
			if tt.wantRaisedCPU == "" {
				if tt.in.Pod.Spec.Resources == nil {
					assert.Nil(t, got.Raised)
				}
				return
			}
			require.NotNil(t, got.Raised)
			assert.True(t, got.Raised.Requests.Cpu().Equal(qty(t, tt.wantRaisedCPU)),
				"raised cpu request: want %s got %s", tt.wantRaisedCPU, got.Raised.Requests.Cpu().String())
		})
	}
}

func TestPreservesQoS_UsesEnvelopeWhenSet(t *testing.T) {
	pod := envelopePod("100m", "128Mi", "200m", "256Mi", &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	})
	pod.Status.QOSClass = corev1.PodQOSGuaranteed
	// Container target is not request==limit. Envelope stays Guaranteed.
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("400m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}
	assert.True(t, PreservesQoS(pod, "app", target),
		"Guaranteed QoS is defined by the envelope, not container equality")

	// Raising a container limit above the envelope limit makes limit > request.
	breakTarget := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		},
	}
	assert.False(t, PreservesQoS(pod, "app", breakTarget),
		"raised envelope limit above request must not keep Guaranteed")
}

func TestEvaluateEnvelope_TwoContainerSumExceeds(t *testing.T) {
	pod := newTestPod("web-0", "default", "app", "100m", "128Mi", "200m", "256Mi")
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
		Name: "sidecar",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
	})
	pod.Spec.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("150m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}
	got := EvaluateEnvelope(EnvelopeInput{
		Pod:       pod,
		Container: "app",
		Target:    target,
		InPlace:   false,
	})
	assert.True(t, got.Skip, "sum 250m exceeds envelope request 200m")
	assert.Equal(t, ReasonEnvelopeConstraint, got.Reason)
}

func TestRaiseToCover_EmptyResourcesPointer(t *testing.T) {
	pod := newTestPod("web-0", "default", "app", "100m", "128Mi", "200m", "256Mi")
	pod.Spec.Resources = &corev1.ResourceRequirements{}
	got := RaiseToCover(pod)
	require.NotNil(t, got)
	assert.True(t, got.Requests.Cpu().Equal(qty(t, "100m")),
		"empty envelope still covers container sum, got %s", got.Requests.Cpu().String())
	_, hasLim := got.Limits[corev1.ResourceCPU]
	assert.False(t, hasLim)
}

func TestRaiseToCover_DoesNotMutateInput(t *testing.T) {
	pod := envelopePod("300m", "256Mi", "300m", "256Mi", &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	})
	orig := pod.Spec.Resources.Requests[corev1.ResourceCPU].DeepCopy()
	_ = RaiseToCover(pod)
	assert.True(t, pod.Spec.Resources.Requests[corev1.ResourceCPU].Equal(orig))
}

func TestEnvelopeDecisionReasonConstant(t *testing.T) {
	assert.Equal(t, "envelope_constraint", ReasonEnvelopeConstraint)
}

func TestContainersExceedEnvelope_NilInputs(t *testing.T) {
	assert.False(t, containersExceedEnvelope(nil, &corev1.ResourceRequirements{}))
	pod := newTestPod("web-0", "default", "app", "100m", "128Mi", "200m", "256Mi")
	assert.False(t, containersExceedEnvelope(pod, nil))
}

func TestContainersExceedEnvelope_LimitsOnlySum(t *testing.T) {
	pod := envelopePod("300m", "256Mi", "300m", "256Mi", &corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	})
	assert.True(t, containersExceedEnvelope(pod, pod.Spec.Resources),
		"container request sum 300m exceeds envelope limit 200m")
}

func TestContainersExceedEnvelope_MaxContainerLimit(t *testing.T) {
	pod := envelopePod("100m", "128Mi", "800m", "256Mi", &corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	})
	assert.True(t, containersExceedEnvelope(pod, pod.Spec.Resources),
		"container limit 800m exceeds envelope limit 500m")
}

func TestIncreaseExceedsCurrentEnvelope_PlannedLimitAboveEnvelope(t *testing.T) {
	pod := envelopePod("100m", "128Mi", "200m", "256Mi", &corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	})
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("800m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	planned := applyPlannedContainer(pod, "app", target)
	assert.True(t, increaseExceedsCurrentEnvelope(pod, planned, "app", target),
		"planned container limit 800m exceeds envelope limit 500m")
	lim, ok := containerLimit(planned, "app", corev1.ResourceCPU)
	require.True(t, ok)
	assert.True(t, lim.Equal(qty(t, "800m")))
}

func TestIncreaseExceedsCurrentEnvelope_InitContainerLimit(t *testing.T) {
	pod := newTestPod("web-0", "default", "app", "100m", "128Mi", "200m", "256Mi")
	pod.Spec.InitContainers = []corev1.Container{{
		Name: "init",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
	}}
	pod.Spec.Resources = &corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("400m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}
	planned := applyPlannedContainer(pod, "init", target)
	assert.True(t, increaseExceedsCurrentEnvelope(pod, planned, "init", target),
		"planned init limit 400m exceeds envelope limit 200m")
	initRes := containerResources(planned, "init")
	assert.True(t, initRes.Limits.Cpu().Equal(qty(t, "400m")))
}

func TestSumContainerRequests_RegularInitDoesNotInflateRunningSum(t *testing.T) {
	pod := newTestPod("web-0", "default", "app", "200m", "128Mi", "200m", "256Mi")
	pod.Spec.InitContainers = []corev1.Container{{
		Name: "migrate",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("500m"),
			},
		},
	}}
	sum := sumContainerRequests(pod, corev1.ResourceCPU)
	assert.True(t, sum.Equal(resource.MustParse("500m")),
		"CREATE/template raise uses max(running, init)=500m, not 700m; got %s", sum.String())

	pod.Spec.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
	}
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
	}
	planned := applyPlannedContainer(pod, "app", target)
	assert.False(t, increaseExceedsCurrentEnvelope(pod, planned, "app", target),
		"app staying at 200m must not skip because of a regular init")
}

func TestSumContainerRequests_NativeSidecarCounts(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	pod := newTestPod("web-0", "default", "app", "200m", "128Mi", "200m", "256Mi")
	pod.Spec.InitContainers = []corev1.Container{{
		Name:          "mesh",
		RestartPolicy: &always,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("100m"),
			},
		},
	}}
	sum := sumContainerRequests(pod, corev1.ResourceCPU)
	assert.True(t, sum.Equal(resource.MustParse("300m")),
		"native sidecar must count in the running sum, got %s", sum.String())
}

func TestContainerResources_NilAndMissing(t *testing.T) {
	assert.Empty(t, containerResources(nil, "app").Requests)
	pod := newTestPod("web-0", "default", "app", "100m", "128Mi", "200m", "256Mi")
	assert.Empty(t, containerResources(pod, "missing").Requests)
}

func TestIncreaseExceedsCurrentEnvelope_NilEnvelopeAndRequestCap(t *testing.T) {
	bare := newTestPod("web-0", "default", "app", "100m", "128Mi", "200m", "256Mi")
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}
	planned := applyPlannedContainer(bare, "app", target)
	assert.False(t, increaseExceedsCurrentEnvelope(bare, planned, "app", target),
		"no pod envelope means the guard is a no-op")

	pod := envelopePod("100m", "128Mi", "200m", "256Mi", &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("150m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	})
	raise := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}
	plannedRaise := applyPlannedContainer(pod, "app", raise)
	assert.True(t, increaseExceedsCurrentEnvelope(pod, plannedRaise, "app", raise),
		"planned request 200m exceeds envelope request 150m")
}

func TestIncreaseExceedsCurrentEnvelope_FitsRequestsAndLimits(t *testing.T) {
	pod := envelopePod("100m", "128Mi", "200m", "256Mi", &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
	})
	target := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("300m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	planned := applyPlannedContainer(pod, "app", target)
	assert.False(t, increaseExceedsCurrentEnvelope(pod, planned, "app", target),
		"200m/300m fits under envelope 500m/1")
	assert.False(t, containersExceedEnvelope(planned, planned.Spec.Resources))
}

func TestDecideCreateEnvelope(t *testing.T) {
	t.Parallel()

	t.Run("nil envelope is not invented", func(t *testing.T) {
		t.Parallel()
		pod := newTestPod("web-0", "default", "app", "500m", "256Mi", "1", "1Gi")
		got := DecideCreateEnvelope(pod, true, true)
		assert.False(t, got.Skip)
		assert.Nil(t, got.Raised)
		assert.Nil(t, pod.Spec.Resources)
	})

	t.Run("raises requests to cover container sum", func(t *testing.T) {
		t.Parallel()
		pod := envelopePod("500m", "256Mi", "1", "1Gi", &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("200m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		})
		got := DecideCreateEnvelope(pod, true, true)
		require.False(t, got.Skip)
		require.NotNil(t, got.Raised)
		assert.True(t, got.Raised.Requests.Cpu().Equal(qty(t, "500m")),
			"want 500m got %s", got.Raised.Requests.Cpu().String())
	})

	t.Run("RequestsOnly Burstable skip when raise would lift limit", func(t *testing.T) {
		t.Parallel()
		pod := envelopePod("500m", "256Mi", "500m", "256Mi", &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("200m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("300m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		})
		pod.Spec.Containers[0].Resources.Limits = nil
		got := DecideCreateEnvelope(pod, true, true)
		assert.True(t, got.Skip)
		assert.Equal(t, ReasonEnvelopeConstraint, got.Reason)
		assert.Nil(t, got.Raised)
	})

	t.Run("RequestsOnly Burstable raises requests without lifting limit", func(t *testing.T) {
		t.Parallel()
		pod := envelopePod("250m", "128Mi", "250m", "128Mi", &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("200m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("300m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		})
		pod.Spec.Containers[0].Resources.Limits = nil
		got := DecideCreateEnvelope(pod, true, true)
		require.False(t, got.Skip)
		require.NotNil(t, got.Raised)
		assert.True(t, got.Raised.Requests.Cpu().Equal(qty(t, "250m")),
			"want 250m got %s", got.Raised.Requests.Cpu().String())
		assert.True(t, got.Raised.Limits.Cpu().Equal(qty(t, "300m")),
			"must not lift the 300m user cap, got %s", got.Raised.Limits.Cpu().String())
	})
}
