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
	"fmt"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

// promQL parses guardrail queries with the default PromQL feature set.
var promQL = parser.NewParser(parser.Options{})

// EnforcePromQLNamespace adds namespace="<namespace>" to every vector
// selector. A selector that already has that exact matcher is unchanged.
// A different namespace matcher, including regex and negative matchers,
// is rejected so the query is not sent.
func EnforcePromQLNamespace(query, namespace string) (string, error) {
	if namespace == "" {
		return "", fmt.Errorf("policy namespace is empty")
	}
	expr, err := promQL.ParseExpr(query)
	if err != nil {
		return "", fmt.Errorf("parsing SLO query: %w", err)
	}
	walker := &namespaceScope{namespace: namespace}
	if err := parser.Walk(walker, expr, nil); err != nil {
		return "", err
	}
	if walker.err != nil {
		return "", walker.err
	}
	return expr.String(), nil
}

// PromQLQueryMayRun reports whether query can still be sent after namespace
// enforcement. A Go template is not PromQL until interpolation, so it may
// run. A query that parses and cannot be scoped will not be sent.
func PromQLQueryMayRun(query, namespace string) bool {
	if _, err := promQL.ParseExpr(query); err != nil {
		return true
	}
	_, err := EnforcePromQLNamespace(query, namespace)
	return err == nil
}

type namespaceScope struct {
	namespace string
	err       error
}

func (s *namespaceScope) Visit(node parser.Node, _ []parser.Node) (parser.Visitor, error) {
	if s.err != nil {
		return nil, s.err
	}
	if node == nil {
		return s, nil
	}
	selector, ok := node.(*parser.VectorSelector)
	if !ok {
		return s, nil
	}
	if err := scopeVectorSelector(selector, s.namespace); err != nil {
		s.err = err
		return nil, err
	}
	return s, nil
}

func scopeVectorSelector(selector *parser.VectorSelector, namespace string) error {
	matcher, err := labels.NewMatcher(labels.MatchEqual, "namespace", namespace)
	if err != nil {
		return fmt.Errorf("namespace matcher: %w", err)
	}
	exact := false
	for _, existing := range selector.LabelMatchers {
		if existing == nil || existing.Name != "namespace" {
			continue
		}
		if existing.Type == labels.MatchEqual && existing.Value == namespace {
			exact = true
			continue
		}
		return fmt.Errorf("SLO query selector matches namespace %s, not %q", existing, namespace)
	}
	if !exact {
		selector.LabelMatchers = append(selector.LabelMatchers, matcher)
	}
	return nil
}
