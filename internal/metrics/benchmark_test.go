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

// BenchmarkBuildProfile_HistoryWindows benchmarks BuildProfile with sample
// counts matching real production history windows (queryStep=5m).
func BenchmarkBuildProfile_HistoryWindows(b *testing.B) {
	cases := []struct {
		name    string
		samples int
	}{
		{"1d_288", 288},    // 24h / 5m
		{"7d_2016", 2016},  // default history window
		{"14d_4032", 4032}, // 2-week window
		{"30d_8640", 8640}, // 30d max window
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			samples := generateBenchSamples(tc.samples)
			b.ResetTimer()
			b.ReportAllocs()
			for b.Loop() {
				BuildProfile(samples)
			}
		})
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
