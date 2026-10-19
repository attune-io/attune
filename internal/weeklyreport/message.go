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

package weeklyreport

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/api/resource"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
	"github.com/attune-io/attune/internal/fleetreport"
)

const dollarSentence = "These dollars are the gap between current requests and the current non-stale recommendation, priced from costPricing or the built-in rates, times 730 hours. Not a cloud bill, not week-over-week spend, and not dollars saved this week."

// MessageText builds the chat body for policies that exist.
func MessageText(policies []attunev1alpha1.AttunePolicy, clusterID, week string, now time.Time) string {
	report := fleetreport.Build(policies, "", now)
	lines := make([]string, 0, 16)
	lines = append(lines, headerLine(week, clusterID))
	lines = append(lines, policyLines(report)...)
	lines = append(lines, savingsLines(report, policies)...)
	lines = append(lines, activityLines(policies, now)...)
	text := strings.Join(lines, "\n")
	return capRunes(text, textRuneCap)
}

func headerLine(week, clusterID string) string {
	clusterID = sanitizeAngles(clusterID)
	clusterID = strings.TrimSpace(clusterID)
	if clusterID == "" {
		return "Attune weekly report " + week
	}
	return "Attune weekly report " + week + " (" + clusterID + ")"
}

func policyLines(r fleetreport.Report) []string {
	var modes []string
	for name, n := range r.PoliciesByMode {
		if n > 0 {
			modes = append(modes, name)
		}
	}
	sort.Strings(modes)
	line := sprintf("Policies: %d", r.PolicyCount)
	if len(modes) > 0 {
		parts := make([]string, 0, len(modes))
		for _, name := range modes {
			parts = append(parts, sprintf("%s %d", name, r.PoliciesByMode[name]))
		}
		line += " (" + strings.Join(parts, ", ") + ")"
	}
	out := []string{
		line,
		sprintf("Ready: %d true, %d false", r.ReadyTrue, r.ReadyFalse),
	}
	if r.InsufficientData > 0 {
		out = append(out, sprintf("InsufficientData: %d", r.InsufficientData))
	}
	out = append(out, sprintf("Workloads: %d discovered, %d with recommendations", r.WorkloadsDiscovered, r.WorkloadsWithRecommendations))
	return out
}

func savingsLines(r fleetreport.Report, policies []attunev1alpha1.AttunePolicy) []string {
	var out []string
	if r.EstimatedMonthlySavingsUSD != 0 {
		out = append(out, "Monthly savings: "+formatDollars(r.EstimatedMonthlySavingsUSD))
	}
	increase := costIncrease(policies)
	if increase != 0 {
		out = append(out, "Monthly cost increase: "+formatDollars(increase))
	}
	if r.ReclaimedCPURequestMilli != 0 || r.ReclaimedMemoryRequestBytes != 0 {
		out = append(out, sprintf("Reclaimable: %s CPU, %s memory", formatCPU(r.ReclaimedCPURequestMilli), formatMemory(r.ReclaimedMemoryRequestBytes)))
	}
	if len(out) > 0 {
		out = append(out, dollarSentence)
	}
	if r.UnparseableSavings > 0 {
		out = append(out, sprintf("Unparseable savings: %d", r.UnparseableSavings))
	}
	top := topSavings(policies)
	if len(top) > 0 {
		out = append(out, "Largest monthly savings:")
		out = append(out, top...)
	}
	return out
}

func costIncrease(policies []attunev1alpha1.AttunePolicy) float64 {
	var sum float64
	for i := range policies {
		usd, _ := fleetreport.ParseUSD(policies[i].Status.Savings.EstimatedMonthlyCostIncrease)
		sum += usd
	}
	return sum
}

type ranked struct {
	ns, name string
	usd      float64
}

func topSavings(policies []attunev1alpha1.AttunePolicy) []string {
	var rows []ranked
	for i := range policies {
		usd, ok := fleetreport.ParseUSD(policies[i].Status.Savings.EstimatedMonthlySavings)
		if !ok || usd <= 0 {
			continue
		}
		rows = append(rows, ranked{ns: policies[i].Namespace, name: policies[i].Name, usd: usd})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].usd != rows[j].usd {
			return rows[i].usd > rows[j].usd
		}
		if rows[i].ns != rows[j].ns {
			return rows[i].ns < rows[j].ns
		}
		return rows[i].name < rows[j].name
	})
	if len(rows) > maxList {
		rows = rows[:maxList]
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, sprintf("- %s/%s %s", row.ns, row.name, formatDollars(row.usd)))
	}
	return out
}

func activityLines(policies []attunev1alpha1.AttunePolicy, now time.Time) []string {
	start := now.UTC().Add(-historyWindow)
	counts := map[string]int{
		string(attunev1alpha1.ResizeResultSuccess):         0,
		string(attunev1alpha1.ResizeResultFailed):          0,
		string(attunev1alpha1.ResizeResultReverted):        0,
		string(attunev1alpha1.ResizeResultEvicted):         0,
		string(attunev1alpha1.ResizeResultTemplatePatched): 0,
	}
	type failRow struct {
		ns, workload, container, reason string
		when                            time.Time
	}
	var fails []failRow
	lower := false
	for i := range policies {
		hist := policies[i].Status.ResizeHistory
		if len(hist) >= attunev1alpha1.MaxResizeHistoryEntries {
			if oldest, ok := oldestStamp(hist); ok && !oldest.Before(start) {
				lower = true
			}
		}
		for _, entry := range hist {
			when := entry.Timestamp.Time
			if when.IsZero() || when.Before(start) {
				continue
			}
			result := string(entry.Result)
			if _, ok := counts[result]; !ok {
				continue
			}
			counts[result]++
			if entry.Result == attunev1alpha1.ResizeResultFailed || entry.Result == attunev1alpha1.ResizeResultReverted {
				fails = append(fails, failRow{
					ns: policies[i].Namespace, workload: entry.Workload, container: entry.Container,
					reason: sanitizeAngles(entry.Reason), when: when.UTC(),
				})
			}
		}
	}
	line := sprintf("Last 7 days: Success %d, Failed %d, Reverted %d, Evicted %d, TemplatePatched %d",
		counts[string(attunev1alpha1.ResizeResultSuccess)],
		counts[string(attunev1alpha1.ResizeResultFailed)],
		counts[string(attunev1alpha1.ResizeResultReverted)],
		counts[string(attunev1alpha1.ResizeResultEvicted)],
		counts[string(attunev1alpha1.ResizeResultTemplatePatched)],
	)
	out := []string{line}
	if lower {
		out = append(out, "Counts are a lower bound. At least one policy filled its 50-row history inside the window.")
	}
	sort.Slice(fails, func(i, j int) bool {
		if !fails[i].when.Equal(fails[j].when) {
			return fails[i].when.After(fails[j].when)
		}
		if fails[i].ns != fails[j].ns {
			return fails[i].ns < fails[j].ns
		}
		if fails[i].workload != fails[j].workload {
			return fails[i].workload < fails[j].workload
		}
		return fails[i].container < fails[j].container
	})
	if len(fails) > maxList {
		fails = fails[:maxList]
	}
	if len(fails) > 0 {
		out = append(out, "Recent failures:")
		for _, row := range fails {
			out = append(out, sprintf("- %s/%s %s %s %s", row.ns, row.workload, row.container, row.reason, row.when.Format(time.RFC3339)))
		}
	}
	errs := reconcileErrors(policies)
	if len(errs) > 0 {
		out = append(out, "Current reconcile errors, latest reconcile, not this week:")
		out = append(out, errs...)
	}
	return out
}

func oldestStamp(hist []attunev1alpha1.ResizeHistoryEntry) (time.Time, bool) {
	var oldest time.Time
	found := false
	for _, entry := range hist {
		when := entry.Timestamp.Time
		if when.IsZero() {
			continue
		}
		if !found || when.Before(oldest) {
			oldest = when
			found = true
		}
	}
	return oldest, found
}

func reconcileErrors(policies []attunev1alpha1.AttunePolicy) []string {
	type row struct{ ns, policy, workload string }
	var rows []row
	for i := range policies {
		for _, errRow := range policies[i].Status.WorkloadErrors {
			if strings.TrimSpace(errRow.Workload) == "" && strings.TrimSpace(errRow.Error) == "" {
				continue
			}
			rows = append(rows, row{policies[i].Namespace, policies[i].Name, errRow.Workload})
			if len(rows) == maxList {
				break
			}
		}
		if len(rows) == maxList {
			break
		}
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, sprintf("- %s/%s %s", row.ns, row.policy, sanitizeAngles(row.workload)))
	}
	return out
}

func sanitizeAngles(s string) string {
	s = strings.ReplaceAll(s, "<", " ")
	s = strings.ReplaceAll(s, ">", " ")
	return s
}

func sprintf(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}

func formatDollars(v float64) string {
	return sprintf("$%.2f", v)
}

func formatCPU(milli int64) string {
	return resource.NewMilliQuantity(milli, resource.DecimalSI).String()
}

func formatMemory(bytes int64) string {
	return resource.NewQuantity(bytes, resource.BinarySI).String()
}

func capRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	const suffix = "\ntruncated"
	budget := limit - utf8.RuneCountInString(suffix)
	if budget < 1 {
		return string([]rune(text)[:limit])
	}
	prefix := string([]rune(text)[:budget])
	if i := strings.LastIndex(prefix, "\n"); i >= 0 {
		prefix = prefix[:i]
	}
	return prefix + suffix
}

// JSONBody is the single object posted to Slack and Teams.
func JSONBody(text string) ([]byte, error) {
	return json.Marshal(struct {
		Text string `json:"text"`
	}{Text: text})
}
