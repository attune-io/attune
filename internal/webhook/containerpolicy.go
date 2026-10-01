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

package webhook

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	attunev1alpha1 "github.com/attune-io/attune/api/v1alpha1"
)

// validateContainerPolicies rejects duplicate names, a second "*", an empty
// containerName, policy-wide fields v1 does not thread, and an effective
// minAllowed above maxAllowed after the field-wise overlay. Other
// ResourceConfig checks reuse validateResourceConfigFields.
// MaxItems=100 is a CRD limit, not a webhook check.
func validateContainerPolicies(policy *attunev1alpha1.AttunePolicy, history *metav1.Duration) error {
	if policy == nil || len(policy.Spec.ContainerPolicies) == 0 {
		return nil
	}
	policies := policy.Spec.ContainerPolicies
	seen := map[string]struct{}{}
	stars := 0
	for i := range policies {
		entry := policies[i]
		if entry.ContainerName == "" {
			return fmt.Errorf("containerPolicies[%d].containerName must not be empty", i)
		}
		if entry.ContainerName == attunev1alpha1.ContainerPolicyWildcard {
			stars++
			if stars > 1 {
				return fmt.Errorf("containerPolicies: only one \"*\" entry is allowed")
			}
		} else if _, ok := seen[entry.ContainerName]; ok {
			return fmt.Errorf("containerPolicies: duplicate containerName %q", entry.ContainerName)
		} else {
			seen[entry.ContainerName] = struct{}{}
		}
		if err := rejectPolicyWideContainerFields(i, "cpu", entry.CPU); err != nil {
			return err
		}
		if err := rejectPolicyWideContainerFields(i, "memory", entry.Memory); err != nil {
			return err
		}
		if entry.CPU != nil {
			if err := validateResourceConfigFields(fmt.Sprintf("containerPolicies[%d].cpu", i), entry.CPU, history); err != nil {
				return err
			}
		}
		if entry.Memory != nil {
			if err := validateResourceConfigFields(fmt.Sprintf("containerPolicies[%d].memory", i), entry.Memory, history); err != nil {
				return err
			}
		}
	}
	return validateEffectiveContainerBounds(policy)
}

// validateEffectiveContainerBounds rejects a policy floor that sits above a
// container or "*" ceiling. Each block can pass min<=max on its own.
func validateEffectiveContainerBounds(policy *attunev1alpha1.AttunePolicy) error {
	if policy == nil {
		return nil
	}
	seen := map[string]struct{}{}
	for i := range policy.Spec.ContainerPolicies {
		name := policy.Spec.ContainerPolicies[i].ContainerName
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		cpu, mem := attunev1alpha1.EffectiveContainerResources(policy, name)
		if err := effectiveMinMax(name, "cpu", &cpu); err != nil {
			return err
		}
		if err := effectiveMinMax(name, "memory", &mem); err != nil {
			return err
		}
	}
	return nil
}

func effectiveMinMax(container, side string, rc *attunev1alpha1.ResourceConfig) error {
	if rc == nil || rc.MinAllowed == nil || rc.MaxAllowed == nil {
		return nil
	}
	if rc.MinAllowed.Cmp(*rc.MaxAllowed) > 0 {
		return fmt.Errorf("containerPolicies %q %s minAllowed (%s) must be <= maxAllowed (%s)",
			container, side, rc.MinAllowed.String(), rc.MaxAllowed.String())
	}
	return nil
}

func rejectPolicyWideContainerFields(index int, side string, rc *attunev1alpha1.ResourceConfig) error {
	if rc == nil {
		return nil
	}
	base := fmt.Sprintf("containerPolicies[%d].%s", index, side)
	if rc.StartupBoost != nil {
		return fmt.Errorf("%s.startupBoost is policy-wide in v1 and cannot be set on a container policy", base)
	}
	if rc.MemoryFromCPURatio != nil {
		return fmt.Errorf("%s.memoryFromCpuRatio is policy-wide in v1 and cannot be set on a container policy", base)
	}
	if rc.DecreaseUsageMarginPercent != nil {
		return fmt.Errorf("%s.decreaseUsageMarginPercent is policy-wide in v1 and cannot be set on a container policy", base)
	}
	if rc.LimitMultiplier != nil {
		return fmt.Errorf("%s.limitMultiplier is policy-wide in v1 and cannot be set on a container policy", base)
	}
	if rc.OOMBump != nil {
		return fmt.Errorf("%s.oomBump is policy-wide in v1 and cannot be set on a container policy", base)
	}
	if rc.Surge != nil {
		return fmt.Errorf("%s.surge is policy-wide in v1 and cannot be set on a container policy", base)
	}
	return nil
}
