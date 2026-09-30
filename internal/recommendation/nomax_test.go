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

package recommendation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/attune-io/attune/internal/metrics"
)

func tenCPUProfile() metrics.UsageProfile {
	return metrics.UsageProfile{
		OverallPercentiles: metrics.PercentileSet{P95: 10},
		DataPoints:         100,
		Confidence:         1,
	}
}

func TestNewEngine_NoMaxDoesNotClampTenCPU(t *testing.T) {
	engine := NewEngine(95, 0, resource.MustParse("1m"), resource.MustParse("4000m"), 100, 100,
		EngineOpts{IsCPU: true, NoMax: true, BurstSensitivity: ptrFloat(0)})
	got, expl, _ := engine.RecommendWithExplanation(tenCPUProfile(), resource.MustParse("10"))
	assert.True(t, resource.MustParse("10").Equal(got), got.String())
	assert.Nil(t, expl.MaxBound)
	assert.Empty(t, expl.BoundsApplied)
}

func TestNewEngine_ExplicitMaxStillClamps(t *testing.T) {
	engine := NewEngine(95, 0, resource.MustParse("1m"), resource.MustParse("2"), 100, 100,
		EngineOpts{IsCPU: true, BurstSensitivity: ptrFloat(0)})
	got, expl, _ := engine.RecommendWithExplanation(tenCPUProfile(), resource.MustParse("10"))
	assert.True(t, resource.MustParse("2").Equal(got), got.String())
	assert.Equal(t, "max", expl.BoundsApplied)
	require.NotNil(t, expl.MaxBound)
	assert.True(t, resource.MustParse("2").Equal(*expl.MaxBound))
}
