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

// Package gitops implements opt-in GitOps pull request automation for
// recommendation export (Phase B of durable recommendations).
package gitops

import (
	"fmt"
	"math"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

// ContainerDrift describes one container resource that differs from the template.
type ContainerDrift struct {
	Workload  string
	Kind      string
	Container string
	Resource  string // cpu or memory
	Template  string
	Recommended string
	ChangePercent float64
}

// ComputeDrift compares recommended requests to workload pod template requests.
// Returns entries whose absolute change percent is >= minChangePercent.
func ComputeDrift(
	workloads []client.Object,
	recs []attunev1alpha1.WorkloadRecommendation,
	minChangePercent float64,
) []ContainerDrift {
	byKey := map[string]attunev1alpha1.WorkloadRecommendation{}
	for _, r := range recs {
		byKey[r.Kind+"/"+r.Workload] = r
	}
	var out []ContainerDrift
	for _, w := range workloads {
		kind := workloadKind(w)
		name := w.GetName()
		rec, ok := byKey[kind+"/"+name]
		if !ok {
			// try match on workload name only
			for _, r := range recs {
				if r.Workload == name {
					rec = r
					ok = true
					break
				}
			}
		}
		if !ok {
			continue
		}
		tpl := podTemplateSpec(w)
		if tpl == nil {
			continue
		}
		for _, cRec := range rec.Containers {
			c := findContainer(tpl.Spec.Containers, cRec.Name)
			if c == nil {
				continue
			}
			if d, ok := driftOne(name, kind, cRec.Name, "cpu", c.Resources.Requests.Cpu(), &cRec.Recommended.CPURequest, minChangePercent); ok {
				out = append(out, d)
			}
			if d, ok := driftOne(name, kind, cRec.Name, "memory", c.Resources.Requests.Memory(), &cRec.Recommended.MemoryRequest, minChangePercent); ok {
				out = append(out, d)
			}
		}
	}
	return out
}

func driftOne(workload, kind, container, resName string, template, recommended *resource.Quantity, minPct float64) (ContainerDrift, bool) {
	if recommended == nil || recommended.IsZero() {
		return ContainerDrift{}, false
	}
	tplVal := float64(0)
	if template != nil && !template.IsZero() {
		if resName == "cpu" {
			tplVal = float64(template.MilliValue())
		} else {
			tplVal = float64(template.Value())
		}
	}
	var recVal float64
	if resName == "cpu" {
		recVal = float64(recommended.MilliValue())
	} else {
		recVal = float64(recommended.Value())
	}
	if tplVal <= 0 {
		// No template request: treat any positive recommendation as 100% drift.
		if recVal <= 0 {
			return ContainerDrift{}, false
		}
		return ContainerDrift{
			Workload: workload, Kind: kind, Container: container, Resource: resName,
			Template: "0", Recommended: recommended.String(), ChangePercent: 100,
		}, minPct <= 100
	}
	pct := math.Abs(recVal-tplVal) / tplVal * 100.0
	if math.IsNaN(pct) || math.IsInf(pct, 0) {
		return ContainerDrift{}, false
	}
	if pct+1e-9 < minPct {
		return ContainerDrift{}, false
	}
	tplStr := "0"
	if template != nil {
		tplStr = template.String()
	}
	return ContainerDrift{
		Workload: workload, Kind: kind, Container: container, Resource: resName,
		Template: tplStr, Recommended: recommended.String(), ChangePercent: pct,
	}, true
}

// FormatPRBody builds a markdown PR description (no secrets).
func FormatPRBody(policyNS, policyName string, drifts []ContainerDrift) string {
	var b strings.Builder
	b.WriteString("## Attune recommendation drift\n\n")
	fmt.Fprintf(&b, "Policy: `%s/%s`\n\n", policyNS, policyName)
	b.WriteString("Recommendations differ from workload **pod templates** beyond the configured threshold.\n\n")
	b.WriteString("| Workload | Kind | Container | Resource | Template | Recommended | Change |\n")
	b.WriteString("|----------|------|-----------|----------|----------|-------------|--------|\n")
	for _, d := range drifts {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %.1f%% |\n",
			d.Workload, d.Kind, d.Container, d.Resource, d.Template, d.Recommended, d.ChangePercent)
	}
	b.WriteString("\n### Next steps\n\n")
	b.WriteString("1. Review recommended requests against production risk.\n")
	b.WriteString("2. Apply via `kubectl attune diff -o yaml` / your patch pipeline, or edit templates below.\n")
	b.WriteString("3. Merge so the next deploy starts near recommended sizes.\n\n")
	b.WriteString("_Opened by Attune GitOps pull request automation (opt-in)._\n")
	return b.String()
}

// BranchName returns a stable branch for the policy.
func BranchName(policyNS, policyName string) string {
	safe := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, policyNS+"-"+policyName)
	return "attune/recommendations-" + safe
}

func podTemplateSpec(w client.Object) *corev1.PodTemplateSpec {
	switch o := w.(type) {
	case *appsv1.Deployment:
		return &o.Spec.Template
	case *appsv1.StatefulSet:
		return &o.Spec.Template
	case *appsv1.DaemonSet:
		return &o.Spec.Template
	default:
		return nil
	}
}

func workloadKind(w client.Object) string {
	gvk := w.GetObjectKind().GroupVersionKind()
	if gvk.Kind != "" {
		return gvk.Kind
	}
	switch w.(type) {
	case *appsv1.Deployment:
		return "Deployment"
	case *appsv1.StatefulSet:
		return "StatefulSet"
	case *appsv1.DaemonSet:
		return "DaemonSet"
	default:
		return "Workload"
	}
}

func findContainer(containers []corev1.Container, name string) *corev1.Container {
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}
