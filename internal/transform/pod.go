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

// Package transform provides informer cache transform functions that strip
// unused fields from Kubernetes objects to reduce memory consumption at scale.
package transform

import (
	corev1 "k8s.io/api/core/v1"
)

// StripPodFields removes fields from a Pod that the attune operator
// never reads. This reduces the per-Pod memory footprint in the informer cache
// by dropping large fields like env vars, volumes, probes, and command args.
//
// Preserved fields (used by the controller):
//   - metadata (name, namespace, labels, annotations, resourceVersion, uid, deletionTimestamp)
//   - spec.nodeName
//   - spec.containers[].name, .resources, .resizePolicy
//   - spec.initContainers[].name, .resources, .restartPolicy (native sidecars)
//   - status.phase, .conditions, .containerStatuses, .initContainerStatuses, .qosClass
func StripPodFields(obj any) (any, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return obj, nil
	}

	// Strip metadata.managedFields (can be very large with server-side apply).
	pod.ManagedFields = nil

	// Strip unused spec fields.
	pod.Spec.Volumes = nil
	pod.Spec.ImagePullSecrets = nil
	pod.Spec.HostAliases = nil
	pod.Spec.Overhead = nil
	pod.Spec.TopologySpreadConstraints = nil

	// Strip unused fields from each container, keeping only name, resources,
	// and resizePolicy.
	for i := range pod.Spec.Containers {
		stripContainer(&pod.Spec.Containers[i])
	}
	for i := range pod.Spec.InitContainers {
		stripInitContainer(&pod.Spec.InitContainers[i])
	}

	// Strip ephemeral containers entirely (never used by the operator).
	pod.Spec.EphemeralContainers = nil

	return pod, nil
}

// stripContainer zeroes out fields the operator does not read from a regular
// container, preserving Name, Resources, and ResizePolicy.
func stripContainer(c *corev1.Container) {
	c.Image = ""
	c.Command = nil
	c.Args = nil
	c.WorkingDir = ""
	c.Ports = nil
	c.EnvFrom = nil
	c.Env = nil
	c.VolumeMounts = nil
	c.VolumeDevices = nil
	c.LivenessProbe = nil
	c.ReadinessProbe = nil
	c.StartupProbe = nil
	c.Lifecycle = nil
	c.SecurityContext = nil
	c.TerminationMessagePath = ""
	c.TerminationMessagePolicy = ""
	c.ImagePullPolicy = ""
}

// stripInitContainer zeroes out fields from an init container, preserving
// Name, Resources, and RestartPolicy (needed for native sidecar detection).
func stripInitContainer(c *corev1.Container) {
	stripContainer(c)
}
