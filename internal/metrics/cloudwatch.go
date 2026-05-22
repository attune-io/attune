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
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/go-logr/logr"
)

// CloudWatchAPI is the subset of the CloudWatch client used by the collector.
// Defined as an interface for testability.
type CloudWatchAPI interface {
	GetMetricData(ctx context.Context, params *cloudwatch.GetMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error)
}

// CloudWatchCollector implements MetricsCollector by querying Amazon CloudWatch
// Container Insights metrics.
type CloudWatchCollector struct {
	client      CloudWatchAPI
	clusterName string
	logger      logr.Logger
}

// NewCloudWatchCollector creates a collector that queries CloudWatch Container
// Insights metrics. It uses the default AWS credential chain (IRSA, Pod Identity,
// instance profile) and optionally assumes a cross-account IAM role.
func NewCloudWatchCollector(ctx context.Context, region, clusterName, roleARN string, logger logr.Logger) (*CloudWatchCollector, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	if roleARN != "" {
		stsClient := sts.NewFromConfig(cfg)
		creds := stscreds.NewAssumeRoleProvider(stsClient, roleARN)
		cfg.Credentials = aws.NewCredentialsCache(creds)
	}

	return &CloudWatchCollector{
		client:      cloudwatch.NewFromConfig(cfg),
		clusterName: clusterName,
		logger:      logger,
	}, nil
}

// NewCloudWatchCollectorWithClient creates a collector with a pre-configured
// CloudWatch API client (used in tests).
func NewCloudWatchCollectorWithClient(client CloudWatchAPI, clusterName string, logger logr.Logger) *CloudWatchCollector {
	return &CloudWatchCollector{
		client:      client,
		clusterName: clusterName,
		logger:      logger,
	}
}

// QueryRange executes a CloudWatch query and returns flattened samples.
func (c *CloudWatchCollector) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]Sample, error) {
	grouped, err := c.QueryRangeGrouped(ctx, query, start, end, step)
	if err != nil {
		return nil, err
	}
	var samples []Sample
	for _, s := range grouped {
		samples = append(samples, s...)
	}
	return samples, nil
}

// QueryRangeGrouped parses the JSON query spec and executes a CloudWatch
// GetMetricData request using a SEARCH expression. Results are grouped by
// the ContainerName dimension.
func (c *CloudWatchCollector) QueryRangeGrouped(ctx context.Context, query string, start, end time.Time, _ time.Duration) (map[string][]Sample, error) {
	var spec CloudWatchQuerySpec
	if err := json.Unmarshal([]byte(query), &spec); err != nil {
		return nil, fmt.Errorf("parsing CloudWatch query spec: %w", err)
	}

	period := spec.Period
	if period < 60 {
		period = 60
	}
	period32 := int32(min(period, 86400)) //nolint:gosec // period is clamped to [60, 86400]

	// Build a SEARCH expression to find all matching Container Insights metrics.
	searchExpr := fmt.Sprintf(
		`SEARCH('{ContainerInsights,ClusterName,Namespace,PodName,ContainerName} MetricName="%s" ClusterName="%s" Namespace="%s"', '%s', %d)`,
		spec.Metric, spec.ClusterName, spec.Namespace, spec.Stat, period32,
	)

	input := &cloudwatch.GetMetricDataInput{
		StartTime: aws.Time(start),
		EndTime:   aws.Time(end),
		MetricDataQueries: []cwtypes.MetricDataQuery{
			{
				Id:         aws.String("search0"),
				Expression: aws.String(searchExpr),
				Period:     aws.Int32(period32),
				ReturnData: aws.Bool(true),
			},
		},
	}

	isCPU := spec.Metric == "container_cpu_usage_total"
	grouped := make(map[string][]Sample)

	// Paginate through results.
	for {
		output, err := c.client.GetMetricData(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("CloudWatch GetMetricData failed: %w", err)
		}

		for _, result := range output.MetricDataResults {
			container, podName := parseCloudWatchLabel(aws.ToString(result.Label))

			// Filter by pod prefix if specified.
			if spec.PodPrefix != "" && !strings.HasPrefix(podName, spec.PodPrefix) {
				continue
			}

			// Filter by container if specified.
			if spec.Container != "" && container != spec.Container {
				continue
			}

			for i, ts := range result.Timestamps {
				value := result.Values[i]
				// Container Insights CPU is in nanocores; convert to cores.
				if isCPU {
					value /= 1e9
				}
				grouped[container] = append(grouped[container], Sample{
					Timestamp: ts,
					Value:     value,
				})
			}
		}

		if output.NextToken == nil {
			break
		}
		input.NextToken = output.NextToken
	}

	c.logger.V(1).Info("CloudWatch query completed",
		"metric", spec.Metric,
		"namespace", spec.Namespace,
		"containers", len(grouped),
		"totalSamples", countSamples(grouped))

	return grouped, nil
}

// Query executes a CloudWatch instant query by querying a 10-minute window
// and returning the latest value.
func (c *CloudWatchCollector) Query(ctx context.Context, query string, ts time.Time) (float64, error) {
	start := ts.Add(-10 * time.Minute)
	samples, err := c.QueryRange(ctx, query, start, ts, time.Minute)
	if err != nil {
		return 0, err
	}
	if len(samples) == 0 {
		return 0, fmt.Errorf("empty result from CloudWatch instant query")
	}
	// Return latest sample.
	latest := samples[0]
	for _, s := range samples[1:] {
		if s.Timestamp.After(latest.Timestamp) {
			latest = s
		}
	}
	return latest.Value, nil
}

// Close is a no-op; the AWS SDK client does not need explicit cleanup.
func (c *CloudWatchCollector) Close() error {
	return nil
}

// parseCloudWatchLabel extracts the container name and pod name from a
// CloudWatch SEARCH result label. Labels from SEARCH expressions are
// formatted as "MetricName ContainerName PodName" or similar patterns
// based on the dimension order.
func parseCloudWatchLabel(label string) (container, podName string) {
	// CloudWatch SEARCH labels are space-separated dimension values.
	// The order follows the dimension list in the SEARCH expression:
	// {ContainerInsights,ClusterName,Namespace,PodName,ContainerName}
	// The label contains: PodName ContainerName (after the metric name).
	parts := strings.Fields(label)
	switch {
	case len(parts) >= 2:
		podName = parts[len(parts)-2]
		container = parts[len(parts)-1]
	case len(parts) == 1:
		container = parts[0]
	}
	return container, podName
}
