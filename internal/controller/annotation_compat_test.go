/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or applied to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/attune-io/attune/internal/lifecycle"
)

type annotationFixture struct {
	Version           string            `json:"version"`
	ObservationPeriod string            `json:"observationPeriod"`
	Now               string            `json:"now"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	WantPhase         string            `json:"wantPhase"`
	WantContainers    []string          `json:"wantContainers"`
}

func TestReleasedAnnotationFixtures(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "annotations", "v*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "expected versioned annotation fixtures")

	for _, path := range files {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			var fx annotationFixture
			require.NoError(t, json.Unmarshal(raw, &fx))

			period, err := time.ParseDuration(fx.ObservationPeriod)
			require.NoError(t, err)
			now, err := time.Parse(time.RFC3339, fx.Now)
			require.NoError(t, err)

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "compat-pod",
					Namespace:   "default",
					Labels:      fx.Labels,
					Annotations: fx.Annotations,
				},
			}
			records, err := buildResizeRecords(pod, period)
			require.NoError(t, err, "current parser must accept %s annotations", fx.Version)
			gotNames := make([]string, 0, len(records))
			for _, rec := range records {
				gotNames = append(gotNames, rec.Container)
			}
			assert.Equal(t, fx.WantContainers, gotNames)

			tracked := fx.Labels[labelTracked] == "true"
			in := lifecycle.InputFromAnnotations(tracked, fx.Annotations[annotationResizedAt], now, period)
			assert.Equal(t, lifecycle.Phase(fx.WantPhase), lifecycle.Classify(in))
		})
	}
}
