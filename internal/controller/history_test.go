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

package controller

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	rightsizev1alpha1 "github.com/SebTardif/kube-rightsize/api/v1alpha1"
)

// makeHistoryEntry creates a ResizeHistoryEntry for testing.
func makeHistoryEntry(workload string, ts time.Time) rightsizev1alpha1.ResizeHistoryEntry {
	return rightsizev1alpha1.ResizeHistoryEntry{
		Timestamp: metav1.Time{Time: ts},
		Workload:  workload,
		Container: "main",
		Resource:  "cpu",
		From:      "500m",
		To:        "250m",
		Method:    "InPlace",
		Result:    "Success",
	}
}

func TestAppendHistory_EmptyExisting(t *testing.T) {
	newEntries := []rightsizev1alpha1.ResizeHistoryEntry{
		makeHistoryEntry("api-server", time.Now()),
	}
	result := appendHistory(nil, newEntries, 20)
	assert.Len(t, result, 1)
	assert.Equal(t, "api-server", result[0].Workload)
}

func TestAppendHistory_ExistingPlusNew(t *testing.T) {
	existing := []rightsizev1alpha1.ResizeHistoryEntry{
		makeHistoryEntry("api-1", time.Now().Add(-1*time.Hour)),
	}
	newEntries := []rightsizev1alpha1.ResizeHistoryEntry{
		makeHistoryEntry("api-2", time.Now()),
	}
	result := appendHistory(existing, newEntries, 20)
	assert.Len(t, result, 2)
	assert.Equal(t, "api-1", result[0].Workload)
	assert.Equal(t, "api-2", result[1].Workload)
}

func TestAppendHistory_Truncation(t *testing.T) {
	existing := make([]rightsizev1alpha1.ResizeHistoryEntry, 20)
	for i := range existing {
		existing[i] = makeHistoryEntry(
			fmt.Sprintf("workload-%d", i),
			time.Now().Add(-time.Duration(20-i)*time.Minute),
		)
	}

	newEntries := make([]rightsizev1alpha1.ResizeHistoryEntry, 5)
	for i := range newEntries {
		newEntries[i] = makeHistoryEntry(
			fmt.Sprintf("new-workload-%d", i),
			time.Now().Add(time.Duration(i)*time.Minute),
		)
	}

	result := appendHistory(existing, newEntries, 20)
	assert.Len(t, result, 20)

	// The last 5 entries should be the newly appended ones.
	for i := 0; i < 5; i++ {
		assert.Equal(t, fmt.Sprintf("new-workload-%d", i), result[15+i].Workload)
	}
}

func TestAppendHistory_NoTruncation(t *testing.T) {
	existing := make([]rightsizev1alpha1.ResizeHistoryEntry, 3)
	for i := range existing {
		existing[i] = makeHistoryEntry(
			fmt.Sprintf("workload-%d", i),
			time.Now().Add(-time.Duration(3-i)*time.Minute),
		)
	}

	newEntries := make([]rightsizev1alpha1.ResizeHistoryEntry, 2)
	for i := range newEntries {
		newEntries[i] = makeHistoryEntry(
			fmt.Sprintf("new-workload-%d", i),
			time.Now(),
		)
	}

	result := appendHistory(existing, newEntries, 20)
	assert.Len(t, result, 5)
}
