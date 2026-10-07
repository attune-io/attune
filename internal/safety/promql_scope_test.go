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

package safety

import (
	"testing"

	"github.com/prometheus/prometheus/promql/parser"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnforcePromQLNamespace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		query   string
		wantErr bool
	}{
		{name: "aggregation", query: "sum(foo)"},
		{name: "rate", query: "rate(foo[5m])"},
		{name: "subquery", query: "max_over_time(foo[5m:1m])"},
		{name: "name matcher", query: `{__name__=~".+"}`},
		{name: "same namespace", query: `foo{namespace="team-a"}`},
		{name: "other namespace", query: `foo{namespace="team-b"}`, wantErr: true},
		{name: "namespace regex", query: `foo{namespace=~"team-a|team-b"}`, wantErr: true},
		{name: "negative namespace", query: `foo{namespace!="team-a"}`, wantErr: true},
		{name: "not promql", query: "this is not promql !!!", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := EnforcePromQLNamespace(tt.query, "team-a")
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.NotContains(t, got, "team-b")
			expr, err := parser.ParseExpr(got)
			require.NoError(t, err)
			counter := &countNamespace{namespace: "team-a"}
			require.NoError(t, parser.Walk(counter, expr, nil))
			assert.Greater(t, counter.seen, 0, got)
			assert.Equal(t, counter.seen, counter.matched, got)
		})
	}
}

type countNamespace struct {
	namespace string
	seen      int
	matched   int
}

func (c *countNamespace) Visit(node parser.Node, _ []parser.Node) (parser.Visitor, error) {
	selector, ok := node.(*parser.VectorSelector)
	if !ok || selector == nil {
		return c, nil
	}
	c.seen++
	for _, matcher := range selector.LabelMatchers {
		if matcher != nil && matcher.Name == "namespace" && matcher.Value == c.namespace {
			c.matched++
			break
		}
	}
	return c, nil
}

func TestPromQLQueryMayRun(t *testing.T) {
	t.Parallel()
	assert.True(t, PromQLQueryMayRun("sum(foo)", "team-a"))
	assert.False(t, PromQLQueryMayRun(`foo{namespace="other"}`, "team-a"))
	assert.True(t, PromQLQueryMayRun(`sum(foo{pod="{{ .PodName }}"})`, "team-a"))
}

func TestEnforcePromQLNamespace_SumFooShape(t *testing.T) {
	t.Parallel()
	got, err := EnforcePromQLNamespace("sum(foo)", "team-a")
	require.NoError(t, err)
	assert.Contains(t, got, `namespace="team-a"`)
	assert.Contains(t, got, "foo")
}
