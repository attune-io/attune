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

// Package argorollout holds the small local Rollout type Attune reads and
// patches. It is not the Argo Rollouts module and does not start an informer.
//
// kubebuilder:skip keeps controller-gen from treating Rollout as a CRD.
// The type embeds TypeMeta and ObjectMeta, which is otherwise enough for
// CRD generation. This package has no group name, so that output would be
// an empty CRD at config/crd/bases/_.yaml, and make build-crds would
// prepend it to dist/crds.yaml. Attune does not ship the Rollout CRD.
//
// +kubebuilder:skip
package argorollout
