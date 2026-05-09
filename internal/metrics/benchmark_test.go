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

package metrics

import (
	"math/rand"
	"testing"
	"time"
)

func BenchmarkBuildProfile_1000Samples(b *testing.B) {
	samples := generateBenchSamples(1000)
	b.ResetTimer()
	for b.Loop() {
		BuildProfile(samples)
	}
}

func BenchmarkBuildProfile_10000Samples(b *testing.B) {
	samples := generateBenchSamples(10000)
	b.ResetTimer()
	for b.Loop() {
		BuildProfile(samples)
	}
}

func BenchmarkBuildProfile_100000Samples(b *testing.B) {
	samples := generateBenchSamples(100000)
	b.ResetTimer()
	for b.Loop() {
		BuildProfile(samples)
	}
}

func generateBenchSamples(count int) []Sample {
	rng := rand.New(rand.NewSource(42))
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	samples := make([]Sample, count)
	for i := range samples {
		samples[i] = Sample{
			Timestamp: base.Add(time.Duration(i) * 5 * time.Minute),
			Value:     0.1 + rng.Float64()*0.3,
		}
	}
	return samples
}
