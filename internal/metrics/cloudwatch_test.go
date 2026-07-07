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
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockCloudWatchClient implements CloudWatchAPI for testing.
type mockCloudWatchClient struct {
	getMetricDataFn func(ctx context.Context, params *cloudwatch.GetMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error)
}

func (m *mockCloudWatchClient) GetMetricData(ctx context.Context, params *cloudwatch.GetMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
	return m.getMetricDataFn(ctx, params, optFns...)
}

func TestCloudWatchCollector_QueryRangeGrouped_CPU(t *testing.T) {
	ts1 := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	ts2 := time.Date(2024, 1, 15, 10, 5, 0, 0, time.UTC)

	mock := &mockCloudWatchClient{
		getMetricDataFn: func(_ context.Context, _ *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
			return &cloudwatch.GetMetricDataOutput{
				MetricDataResults: []cwtypes.MetricDataResult{
					{
						Label:      aws.String("container_cpu_usage_total api-server-abc web"),
						Timestamps: []time.Time{ts1, ts2},
						Values:     []float64{500000000, 1000000000}, // nanocores
					},
					{
						Label:      aws.String("container_cpu_usage_total api-server-abc sidecar"),
						Timestamps: []time.Time{ts1},
						Values:     []float64{100000000},
					},
				},
			}, nil
		},
	}

	c := NewCloudWatchCollectorWithClient(mock, "my-cluster", logr.Discard())

	spec := CloudWatchQuerySpec{
		Metric:      "container_cpu_usage_total",
		ClusterName: "my-cluster",
		Namespace:   "default",
		PodPrefix:   "api-server-",
		Period:      300,
		Stat:        "Average",
	}
	query, _ := json.Marshal(spec)

	grouped, err := c.QueryRangeGrouped(context.Background(), string(query),
		ts1.Add(-time.Hour), ts2, 5*time.Minute)
	require.NoError(t, err)

	assert.Len(t, grouped, 2)
	assert.Len(t, grouped["web"], 2)
	assert.Len(t, grouped["sidecar"], 1)

	// Verify nanocores -> cores conversion.
	assert.InDelta(t, 0.5, grouped["web"][0].Value, 0.001)
	assert.InDelta(t, 1.0, grouped["web"][1].Value, 0.001)
	assert.InDelta(t, 0.1, grouped["sidecar"][0].Value, 0.001)
}

func TestCloudWatchCollector_QueryRangeGrouped_Memory(t *testing.T) {
	ts1 := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	mock := &mockCloudWatchClient{
		getMetricDataFn: func(_ context.Context, _ *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
			return &cloudwatch.GetMetricDataOutput{
				MetricDataResults: []cwtypes.MetricDataResult{
					{
						Label:      aws.String("container_memory_working_set pod-abc main"),
						Timestamps: []time.Time{ts1},
						Values:     []float64{536870912}, // 512 MiB in bytes
					},
				},
			}, nil
		},
	}

	c := NewCloudWatchCollectorWithClient(mock, "prod", logr.Discard())

	spec := CloudWatchQuerySpec{
		Metric:      "container_memory_working_set",
		ClusterName: "prod",
		Namespace:   "default",
		PodPrefix:   "pod-",
		Period:      300,
		Stat:        "Average",
	}
	query, _ := json.Marshal(spec)

	grouped, err := c.QueryRangeGrouped(context.Background(), string(query),
		ts1.Add(-time.Hour), ts1, 5*time.Minute)
	require.NoError(t, err)

	// Memory should NOT be converted.
	assert.InDelta(t, 536870912, grouped["main"][0].Value, 1)
}

func TestCloudWatchCollector_PodPrefixFiltering(t *testing.T) {
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	mock := &mockCloudWatchClient{
		getMetricDataFn: func(_ context.Context, _ *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
			return &cloudwatch.GetMetricDataOutput{
				MetricDataResults: []cwtypes.MetricDataResult{
					{
						Label:      aws.String("metric api-server-abc main"),
						Timestamps: []time.Time{ts},
						Values:     []float64{100},
					},
					{
						Label:      aws.String("metric other-app-xyz main"),
						Timestamps: []time.Time{ts},
						Values:     []float64{200},
					},
				},
			}, nil
		},
	}

	c := NewCloudWatchCollectorWithClient(mock, "c", logr.Discard())

	spec := CloudWatchQuerySpec{
		Metric:      "container_memory_working_set",
		ClusterName: "c",
		Namespace:   "default",
		PodPrefix:   "api-server-", // Should filter out "other-app-xyz"
		Period:      300,
		Stat:        "Average",
	}
	query, _ := json.Marshal(spec)

	grouped, err := c.QueryRangeGrouped(context.Background(), string(query),
		ts.Add(-time.Hour), ts, 5*time.Minute)
	require.NoError(t, err)

	// Only api-server-abc should match.
	assert.Len(t, grouped, 1)
	assert.Contains(t, grouped, "main")
	assert.InDelta(t, 100, grouped["main"][0].Value, 0.001)
}

func TestCloudWatchCollector_APIError(t *testing.T) {
	mock := &mockCloudWatchClient{
		getMetricDataFn: func(_ context.Context, _ *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
			return nil, fmt.Errorf("access denied")
		},
	}

	c := NewCloudWatchCollectorWithClient(mock, "c", logr.Discard())

	spec := CloudWatchQuerySpec{Metric: "x", ClusterName: "c", Namespace: "ns", Period: 300, Stat: "Average"}
	query, _ := json.Marshal(spec)

	_, err := c.QueryRangeGrouped(context.Background(), string(query),
		time.Now().Add(-time.Hour), time.Now(), time.Minute)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "access denied")
}

func TestCloudWatchCollector_Pagination(t *testing.T) {
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	callCount := 0

	mock := &mockCloudWatchClient{
		getMetricDataFn: func(_ context.Context, params *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
			callCount++
			if callCount == 1 {
				return &cloudwatch.GetMetricDataOutput{
					MetricDataResults: []cwtypes.MetricDataResult{
						{
							Label:      aws.String("metric pod-a main"),
							Timestamps: []time.Time{ts},
							Values:     []float64{100},
						},
					},
					NextToken: aws.String("page2"),
				}, nil
			}
			return &cloudwatch.GetMetricDataOutput{
				MetricDataResults: []cwtypes.MetricDataResult{
					{
						Label:      aws.String("metric pod-b main"),
						Timestamps: []time.Time{ts},
						Values:     []float64{200},
					},
				},
			}, nil
		},
	}

	c := NewCloudWatchCollectorWithClient(mock, "c", logr.Discard())

	spec := CloudWatchQuerySpec{Metric: "container_memory_working_set", ClusterName: "c", Namespace: "ns", PodPrefix: "pod-", Period: 300, Stat: "Average"}
	query, _ := json.Marshal(spec)

	grouped, err := c.QueryRangeGrouped(context.Background(), string(query),
		ts.Add(-time.Hour), ts, 5*time.Minute)
	require.NoError(t, err)
	assert.Equal(t, 2, callCount, "should have made 2 API calls for pagination")
	assert.Len(t, grouped["main"], 2)
}

func TestCloudWatchCollector_Query_Instant(t *testing.T) {
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	earlier := ts.Add(-3 * time.Minute)

	mock := &mockCloudWatchClient{
		getMetricDataFn: func(_ context.Context, _ *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
			return &cloudwatch.GetMetricDataOutput{
				MetricDataResults: []cwtypes.MetricDataResult{
					{
						Label:      aws.String("metric pod main"),
						Timestamps: []time.Time{earlier, ts},
						Values:     []float64{1.0, 2.0},
					},
				},
			}, nil
		},
	}

	c := NewCloudWatchCollectorWithClient(mock, "c", logr.Discard())

	spec := CloudWatchQuerySpec{Metric: "container_memory_working_set", ClusterName: "c", Namespace: "ns", Period: 300, Stat: "Average"}
	query, _ := json.Marshal(spec)

	val, err := c.Query(context.Background(), string(query), ts)
	require.NoError(t, err)
	assert.InDelta(t, 2.0, val, 0.001, "should return the latest sample")
}

func TestCloudWatchCollector_ContainerFiltering(t *testing.T) {
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	mock := &mockCloudWatchClient{
		getMetricDataFn: func(_ context.Context, _ *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
			return &cloudwatch.GetMetricDataOutput{
				MetricDataResults: []cwtypes.MetricDataResult{
					{
						Label:      aws.String("metric pod-a main"),
						Timestamps: []time.Time{ts},
						Values:     []float64{100},
					},
					{
						Label:      aws.String("metric pod-a sidecar"),
						Timestamps: []time.Time{ts},
						Values:     []float64{200},
					},
				},
			}, nil
		},
	}

	c := NewCloudWatchCollectorWithClient(mock, "c", logr.Discard())

	spec := CloudWatchQuerySpec{
		Metric:      "container_memory_working_set",
		ClusterName: "c",
		Namespace:   "ns",
		PodPrefix:   "pod-",
		Container:   "main", // Should filter out "sidecar"
		Period:      300,
		Stat:        "Average",
	}
	query, _ := json.Marshal(spec)

	grouped, err := c.QueryRangeGrouped(context.Background(), string(query),
		ts.Add(-time.Hour), ts, 5*time.Minute)
	require.NoError(t, err)
	assert.Len(t, grouped, 1, "should only contain the 'main' container")
	assert.Contains(t, grouped, "main")
	assert.InDelta(t, 100, grouped["main"][0].Value, 0.001)
}

func TestParseCloudWatchLabel(t *testing.T) {
	tests := []struct {
		label         string
		wantContainer string
		wantPod       string
	}{
		{"container_cpu_usage_total api-server-abc web", "web", "api-server-abc"},
		{"single", "single", ""},
		{"", "", ""},
		{"a b c d", "d", "c"},
	}
	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			container, pod := parseCloudWatchLabel(tt.label)
			assert.Equal(t, tt.wantContainer, container)
			assert.Equal(t, tt.wantPod, pod)
		})
	}
}

func TestCloudWatchCollector_Close(t *testing.T) {
	c := &CloudWatchCollector{}
	require.NoError(t, c.Close(), "Close is a no-op and must succeed")
	require.NoError(t, c.Close(), "Close must be idempotent")
}

// Verify CloudWatchCollector implements MetricsCollector.
var _ MetricsCollector = &CloudWatchCollector{}
