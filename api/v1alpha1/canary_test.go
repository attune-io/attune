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

package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanaryStatus_AllowsCreateSizing(t *testing.T) {
	var none *CanaryStatus
	assert.False(t, none.AllowsCreateSizing("app", "pod-1"))

	inProgress := &CanaryStatus{
		Phase: CanaryPhaseInProgress,
		Pods:  []string{"canary-pod"},
		Workloads: []CanaryWorkloadStatus{
			{Workload: "app-a", Phase: CanaryPhaseInProgress, Pods: []string{"a-canary"}},
			{Workload: "app-b", Phase: CanaryPhaseFullRollout, Pods: []string{"b-1"}},
		},
	}
	assert.True(t, inProgress.AllowsCreateSizing("app-a", "a-canary"))
	assert.False(t, inProgress.AllowsCreateSizing("app-a", "a-new"))
	assert.True(t, inProgress.AllowsCreateSizing("app-b", "b-new"), "promoted app may CREATE-size")
	assert.False(t, inProgress.AllowsHPARetune("app-a"))
	assert.True(t, inProgress.AllowsHPARetune("app-b"))

	legacy := &CanaryStatus{Phase: CanaryPhaseInProgress, Pods: []string{"only-this"}}
	assert.True(t, legacy.AllowsCreateSizing("any", "only-this"))
	assert.False(t, legacy.AllowsCreateSizing("any", "other"))
	assert.False(t, legacy.AllowsHPARetune("any"))

	done := &CanaryStatus{Phase: CanaryPhaseFullRollout}
	assert.True(t, done.AllowsCreateSizing("any", "new-pod"))
	assert.True(t, done.AllowsHPARetune("any"))
}

func TestCanaryStatus_RollupPhase(t *testing.T) {
	cs := &CanaryStatus{
		Phase: CanaryPhaseInProgress,
		Workloads: []CanaryWorkloadStatus{
			{Workload: "a", Phase: CanaryPhaseFullRollout},
			{Workload: "b", Phase: CanaryPhaseInProgress},
		},
	}
	cs.RollupPhase()
	assert.Equal(t, CanaryPhaseInProgress, cs.Phase)

	cs.Workloads[1].Phase = CanaryPhaseFullRollout
	cs.RollupPhase()
	assert.Equal(t, CanaryPhaseFullRollout, cs.Phase)

	ws := cs.UpsertWorkload("c")
	require.NotNil(t, ws)
	assert.Equal(t, CanaryPhaseInProgress, ws.Phase)
	cs.RollupPhase()
	assert.Equal(t, CanaryPhaseInProgress, cs.Phase)
}
