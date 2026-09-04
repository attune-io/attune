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
	"math"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/go-logr/logr"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/attune-io/attune/internal/operatormetrics"
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

	spec := CloudWatchQuerySpec{Metric: "container_memory_working_set", ClusterName: "c", Namespace: "ns", Period: 300, Stat: "Average"}
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

func TestCloudWatchCollector_QueryRangeGrouped_NaNInfFiltered(t *testing.T) {
	ts1 := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	ts2 := ts1.Add(1 * time.Minute)
	ts3 := ts1.Add(2 * time.Minute)
	ts4 := ts1.Add(3 * time.Minute)
	ts5 := ts1.Add(4 * time.Minute)

	mock := &mockCloudWatchClient{
		getMetricDataFn: func(_ context.Context, _ *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
			return &cloudwatch.GetMetricDataOutput{
				MetricDataResults: []cwtypes.MetricDataResult{
					{
						Label:      aws.String("metric pod-a main"),
						Timestamps: []time.Time{ts1, ts2, ts3, ts4, ts5},
						Values:     []float64{0.25, math.NaN(), math.Inf(1), math.Inf(-1), 0.75},
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
		Period:      300,
		Stat:        "Average",
	}
	query, err := json.Marshal(spec)
	require.NoError(t, err)

	ctx := WithNanInfLabels(context.Background(), "cw-ns", "cw-policy", "memory")
	before := promtestutil.ToFloat64(operatormetrics.NanInfSamplesTotal.WithLabelValues("cw-ns", "cw-policy", "untracked", "memory"))
	grouped, err := c.QueryRangeGrouped(ctx, string(query),
		ts1.Add(-time.Hour), ts5, time.Minute)
	require.NoError(t, err)
	require.Len(t, grouped["main"], 2, "NaN, +Inf, and -Inf samples should be filtered out")
	assert.InDelta(t, 0.25, grouped["main"][0].Value, 0.001)
	assert.InDelta(t, 0.75, grouped["main"][1].Value, 0.001)
	after := promtestutil.ToFloat64(operatormetrics.NanInfSamplesTotal.WithLabelValues("cw-ns", "cw-policy", "untracked", "memory"))
	assert.Equal(t, before, after, "mixed finite+NaN series must not increment the unusable-series counter")
}

func TestCloudWatchCollector_QueryRangeGrouped_AllNonFiniteIncrementsOnce(t *testing.T) {
	ts1 := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	ts2 := ts1.Add(1 * time.Minute)
	ts3 := ts1.Add(2 * time.Minute)

	mock := &mockCloudWatchClient{
		getMetricDataFn: func(_ context.Context, _ *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
			return &cloudwatch.GetMetricDataOutput{
				MetricDataResults: []cwtypes.MetricDataResult{
					{
						Label:      aws.String("metric pod-a main"),
						Timestamps: []time.Time{ts1, ts2, ts3},
						Values:     []float64{math.NaN(), math.Inf(1), math.Inf(-1)},
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
		Period:      300,
		Stat:        "Average",
	}
	query, err := json.Marshal(spec)
	require.NoError(t, err)

	ctx := WithNanInfLabels(context.Background(), "cw-ns", "cw-policy", "memory")
	before := promtestutil.ToFloat64(operatormetrics.NanInfSamplesTotal.WithLabelValues("cw-ns", "cw-policy", "untracked", "memory"))
	grouped, err := c.QueryRangeGrouped(ctx, string(query),
		ts1.Add(-time.Hour), ts3, time.Minute)
	require.NoError(t, err)
	assert.Empty(t, grouped["main"], "all-non-finite series keeps no samples")
	after := promtestutil.ToFloat64(operatormetrics.NanInfSamplesTotal.WithLabelValues("cw-ns", "cw-policy", "untracked", "memory"))
	assert.Equal(t, before+1, after, "all-non-finite series increments the counter once")
}

func TestCloudWatchCollector_QueryRangeGrouped_ShortValuesNoPanic(t *testing.T) {
	ts1 := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	ts2 := ts1.Add(1 * time.Minute)
	ts3 := ts1.Add(2 * time.Minute)

	mock := &mockCloudWatchClient{
		getMetricDataFn: func(_ context.Context, _ *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
			return &cloudwatch.GetMetricDataOutput{
				MetricDataResults: []cwtypes.MetricDataResult{
					{
						Label:      aws.String("metric pod-a main"),
						Timestamps: []time.Time{ts1, ts2, ts3},
						Values:     []float64{42.0},
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
		Period:      300,
		Stat:        "Average",
	}
	query, err := json.Marshal(spec)
	require.NoError(t, err)

	grouped, err := c.QueryRangeGrouped(context.Background(), string(query),
		ts1.Add(-time.Hour), ts3, time.Minute)
	require.NoError(t, err)
	require.Len(t, grouped["main"], 1)
	assert.InDelta(t, 42.0, grouped["main"][0].Value, 0.001)
	assert.Equal(t, ts1, grouped["main"][0].Timestamp)
}

func TestNewCloudWatchCollector_RejectsHostileInputs(t *testing.T) {
	t.Parallel()
	_, err := NewCloudWatchCollector(context.Background(), "us-east-1",
		`x" Namespace="kube-system" MetricName="container_memory_working_set`, "", logr.Discard())
	require.Error(t, err)
	_, err = NewCloudWatchCollector(context.Background(), "us-east-1", "prod",
		`arn:aws:iam::123456789012:role/x" extra`, logr.Discard())
	require.Error(t, err)
}

func TestCloudWatchCollector_HostileClusterNameNotInSEARCH(t *testing.T) {
	t.Parallel()
	var gotExpr string
	called := false
	mock := &mockCloudWatchClient{
		getMetricDataFn: func(_ context.Context, params *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
			called = true
			if len(params.MetricDataQueries) > 0 {
				gotExpr = aws.ToString(params.MetricDataQueries[0].Expression)
			}
			return &cloudwatch.GetMetricDataOutput{}, nil
		},
	}
	c := NewCloudWatchCollectorWithClient(mock, "c", logr.Discard())
	spec := CloudWatchQuerySpec{
		Metric:      "container_memory_working_set",
		ClusterName: `x" Namespace="kube-system" MetricName="container_memory_working_set`,
		Namespace:   "ns",
		Period:      300,
		Stat:        "Average",
	}
	query, err := json.Marshal(spec)
	require.NoError(t, err)

	_, err = c.QueryRangeGrouped(context.Background(), string(query),
		time.Now().Add(-time.Hour), time.Now(), time.Minute)
	require.Error(t, err)
	assert.False(t, called, "must not send GetMetricData when clusterName is hostile")
	assert.NotContains(t, gotExpr, `Namespace="kube-system"`)
	assert.NotContains(t, gotExpr, `x"`)
}

func TestCloudWatchCollector_ValidClusterNameQuotedInSEARCH(t *testing.T) {
	t.Parallel()
	var gotExpr string
	mock := &mockCloudWatchClient{
		getMetricDataFn: func(_ context.Context, params *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
			if len(params.MetricDataQueries) > 0 {
				gotExpr = aws.ToString(params.MetricDataQueries[0].Expression)
			}
			return &cloudwatch.GetMetricDataOutput{}, nil
		},
	}
	c := NewCloudWatchCollectorWithClient(mock, "my-eks-cluster", logr.Discard())
	spec := CloudWatchQuerySpec{
		Metric:      "container_memory_working_set",
		ClusterName: "my-eks-cluster",
		Namespace:   "default",
		Period:      300,
		Stat:        "Average",
	}
	query, err := json.Marshal(spec)
	require.NoError(t, err)

	_, err = c.QueryRangeGrouped(context.Background(), string(query),
		time.Now().Add(-time.Hour), time.Now(), time.Minute)
	require.NoError(t, err)
	assert.Contains(t, gotExpr, `ClusterName="my-eks-cluster"`)
	assert.Contains(t, gotExpr, `MetricName="container_memory_working_set"`)
	assert.Contains(t, gotExpr, `'Average'`)
}

func TestCloudWatchCollector_PinsMetricAndStat(t *testing.T) {
	t.Parallel()
	called := false
	mock := &mockCloudWatchClient{
		getMetricDataFn: func(_ context.Context, _ *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
			called = true
			return &cloudwatch.GetMetricDataOutput{}, nil
		},
	}
	c := NewCloudWatchCollectorWithClient(mock, "c", logr.Discard())

	for _, spec := range []CloudWatchQuerySpec{
		{Metric: `x" ClusterName="other`, ClusterName: "c", Namespace: "ns", Period: 300, Stat: "Average"},
		{Metric: "container_memory_working_set", ClusterName: "c", Namespace: "ns", Period: 300, Stat: `Average' MetricName="evil`},
		{Metric: "AWS/EC2 CPUUtilization", ClusterName: "c", Namespace: "ns", Period: 300, Stat: "Average"},
	} {
		query, err := json.Marshal(spec)
		require.NoError(t, err)
		_, err = c.QueryRangeGrouped(context.Background(), string(query),
			time.Now().Add(-time.Hour), time.Now(), time.Minute)
		require.Error(t, err, "spec=%+v", spec)
	}
	assert.False(t, called, "must not send GetMetricData for unpinned Metric/Stat")
}

func TestCloudWatchCollector_PodPrefixInSEARCH(t *testing.T) {
	t.Parallel()
	var gotExpr string
	mock := &mockCloudWatchClient{
		getMetricDataFn: func(_ context.Context, params *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
			if len(params.MetricDataQueries) > 0 {
				gotExpr = aws.ToString(params.MetricDataQueries[0].Expression)
			}
			return &cloudwatch.GetMetricDataOutput{}, nil
		},
	}
	c := NewCloudWatchCollectorWithClient(mock, "c", logr.Discard())
	spec := CloudWatchQuerySpec{
		Metric:      "container_memory_working_set",
		ClusterName: "c",
		Namespace:   "default",
		PodPrefix:   "api-server-",
		Period:      300,
		Stat:        "Average",
	}
	query, err := json.Marshal(spec)
	require.NoError(t, err)

	_, err = c.QueryRangeGrouped(context.Background(), string(query),
		time.Now().Add(-time.Hour), time.Now(), time.Minute)
	require.NoError(t, err)
	assert.NotContains(t, gotExpr, `PodName=`, "quoted SEARCH has no prefix wildcard; filter client-side")
	assert.Contains(t, gotExpr, `MetricName="container_memory_working_set"`)
}

func TestCloudWatchCollector_HostilePodPrefixNotInSEARCH(t *testing.T) {
	t.Parallel()
	called := false
	mock := &mockCloudWatchClient{
		getMetricDataFn: func(_ context.Context, _ *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
			called = true
			return &cloudwatch.GetMetricDataOutput{}, nil
		},
	}
	c := NewCloudWatchCollectorWithClient(mock, "c", logr.Discard())
	spec := CloudWatchQuerySpec{
		Metric:      "container_memory_working_set",
		ClusterName: "c",
		Namespace:   "default",
		PodPrefix:   `api" ClusterName="other`,
		Period:      300,
		Stat:        "Average",
	}
	query, err := json.Marshal(spec)
	require.NoError(t, err)
	_, err = c.QueryRangeGrouped(context.Background(), string(query),
		time.Now().Add(-time.Hour), time.Now(), time.Minute)
	require.Error(t, err)
	assert.False(t, called, "must not send GetMetricData when podPrefix contains a quote")
}

func TestCloudWatchCollector_HostileNamespaceNotInSEARCH(t *testing.T) {
	t.Parallel()
	called := false
	mock := &mockCloudWatchClient{
		getMetricDataFn: func(_ context.Context, _ *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
			called = true
			return &cloudwatch.GetMetricDataOutput{}, nil
		},
	}
	c := NewCloudWatchCollectorWithClient(mock, "c", logr.Discard())
	spec := CloudWatchQuerySpec{
		Metric:      "container_memory_working_set",
		ClusterName: "c",
		Namespace:   `ns" MetricName="container_cpu_usage_total`,
		Period:      300,
		Stat:        "Average",
	}
	query, err := json.Marshal(spec)
	require.NoError(t, err)
	_, err = c.QueryRangeGrouped(context.Background(), string(query),
		time.Now().Add(-time.Hour), time.Now(), time.Minute)
	require.Error(t, err)
	assert.False(t, called)
}

// Verify CloudWatchCollector implements MetricsCollector.
var _ MetricsCollector = &CloudWatchCollector{}
