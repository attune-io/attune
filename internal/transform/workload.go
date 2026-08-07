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

package transform

import (
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// StripDeploymentFields drops fields Attune never reads from Deployments,
// reducing informer cache memory at scale.
func StripDeploymentFields(obj any) (any, error) {
	d, ok := obj.(*appsv1.Deployment)
	if !ok {
		return obj, nil
	}
	d.ManagedFields = nil
	stripPodTemplate(&d.Spec.Template)
	// Keep selector, replicas, and rollout-relevant status fields.
	d.Spec.Strategy = appsv1.DeploymentStrategy{}
	d.Spec.MinReadySeconds = 0
	d.Spec.RevisionHistoryLimit = nil
	d.Spec.Paused = false
	d.Spec.ProgressDeadlineSeconds = nil
	// Status: keep replica counts used by IsRollingOut.
	d.Status.Conditions = nil
	d.Status.CollisionCount = nil
	return d, nil
}

// StripStatefulSetFields drops unused StatefulSet fields from the cache.
func StripStatefulSetFields(obj any) (any, error) {
	s, ok := obj.(*appsv1.StatefulSet)
	if !ok {
		return obj, nil
	}
	s.ManagedFields = nil
	stripPodTemplate(&s.Spec.Template)
	s.Spec.VolumeClaimTemplates = nil
	s.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{}
	s.Spec.RevisionHistoryLimit = nil
	s.Spec.MinReadySeconds = 0
	s.Spec.PersistentVolumeClaimRetentionPolicy = nil
	s.Spec.Ordinals = nil
	s.Status.Conditions = nil
	s.Status.CollisionCount = nil
	return s, nil
}

// StripDaemonSetFields drops unused DaemonSet fields from the cache.
func StripDaemonSetFields(obj any) (any, error) {
	d, ok := obj.(*appsv1.DaemonSet)
	if !ok {
		return obj, nil
	}
	d.ManagedFields = nil
	stripPodTemplate(&d.Spec.Template)
	d.Spec.UpdateStrategy = appsv1.DaemonSetUpdateStrategy{}
	d.Spec.MinReadySeconds = 0
	d.Spec.RevisionHistoryLimit = nil
	d.Status.Conditions = nil
	return d, nil
}

// StripReplicaSetFields drops unused ReplicaSet fields from the cache.
func StripReplicaSetFields(obj any) (any, error) {
	rs, ok := obj.(*appsv1.ReplicaSet)
	if !ok {
		return obj, nil
	}
	rs.ManagedFields = nil
	stripPodTemplate(&rs.Spec.Template)
	rs.Spec.MinReadySeconds = 0
	rs.Status.Conditions = nil
	return rs, nil
}

// StripHPAFields drops unused HPA fields from the cache while keeping
// scale target, metrics, and annotations used for auto-tune.
func StripHPAFields(obj any) (any, error) {
	h, ok := obj.(*autoscalingv2.HorizontalPodAutoscaler)
	if !ok {
		return obj, nil
	}
	h.ManagedFields = nil
	h.Status.Conditions = nil
	h.Status.CurrentMetrics = nil
	return h, nil
}

// StripJobFields drops unused Job fields from the cache.
func StripJobFields(obj any) (any, error) {
	j, ok := obj.(*batchv1.Job)
	if !ok {
		return obj, nil
	}
	j.ManagedFields = nil
	stripPodTemplate(&j.Spec.Template)
	j.Status.Conditions = nil
	return j, nil
}

// StripCronJobFields drops unused CronJob fields from the cache.
func StripCronJobFields(obj any) (any, error) {
	c, ok := obj.(*batchv1.CronJob)
	if !ok {
		return obj, nil
	}
	c.ManagedFields = nil
	stripPodTemplate(&c.Spec.JobTemplate.Spec.Template)
	c.Status.Active = nil
	return c, nil
}

func stripPodTemplate(t *corev1.PodTemplateSpec) {
	if t == nil {
		return
	}
	t.ManagedFields = nil
	// Reuse pod stripping logic for the embedded template.
	pod := &corev1.Pod{ObjectMeta: t.ObjectMeta, Spec: t.Spec}
	_, _ = StripPodFields(pod)
	t.ObjectMeta = pod.ObjectMeta
	t.Spec = pod.Spec
}
