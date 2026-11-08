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
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestResolveCronJobOwner_ManualJobStaysAJob(t *testing.T) {
	yes := true
	scheduled := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nightly-28491000",
			Namespace: "ns",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1",
				Kind:       "CronJob",
				Name:       "nightly",
				Controller: &yes,
			}},
		},
	}
	manual := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "adhoc",
			Namespace: "ns",
			Annotations: map[string]string{
				"cronjob.kubernetes.io/instantiate": "manual",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1",
				Kind:       "CronJob",
				Name:       "nightly",
				Controller: &yes,
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(scheduled, manual).Build()
	kind, name, err := resolveCronJobOwner(context.Background(), c, "ns", "nightly-28491000")
	require.NoError(t, err)
	assert.Equal(t, "CronJob", kind)
	assert.Equal(t, "nightly", name)

	kind, name, err = resolveCronJobOwner(context.Background(), c, "ns", "adhoc")
	require.NoError(t, err)
	assert.Empty(t, kind)
	assert.Empty(t, name)
}
