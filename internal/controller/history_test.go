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

	rightsizev1alpha1 "github.com/SebTardifLabs/kube-rightsize/api/v1alpha1"
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
		Result:    rightsizev1alpha1.ResizeResultSuccess,
	}
}

func TestAppendHistory_EmptyExisting(t *testing.T) {
	newEntries := []rightsizev1alpha1.ResizeHistoryEntry{
		makeHistoryEntry("api-server", time.Now()),
	}
	result := appendHistory(nil, newEntries, maxHistoryEntries)
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
	result := appendHistory(existing, newEntries, maxHistoryEntries)
	assert.Len(t, result, 2)
	assert.Equal(t, "api-1", result[0].Workload)
	assert.Equal(t, "api-2", result[1].Workload)
}

func TestAppendHistory_Truncation(t *testing.T) {
	existing := make([]rightsizev1alpha1.ResizeHistoryEntry, maxHistoryEntries)
	for i := range existing {
		existing[i] = makeHistoryEntry(
			fmt.Sprintf("workload-%d", i),
			time.Now().Add(-time.Duration(maxHistoryEntries-i)*time.Minute),
		)
	}

	newEntries := make([]rightsizev1alpha1.ResizeHistoryEntry, 5)
	for i := range newEntries {
		newEntries[i] = makeHistoryEntry(
			fmt.Sprintf("new-workload-%d", i),
			time.Now().Add(time.Duration(i)*time.Minute),
		)
	}

	result := appendHistory(existing, newEntries, maxHistoryEntries)
	assert.Len(t, result, maxHistoryEntries)

	// The last 5 entries should be the newly appended ones.
	for i := 0; i < 5; i++ {
		assert.Equal(t, fmt.Sprintf("new-workload-%d", i), result[maxHistoryEntries-5+i].Workload)
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

	result := appendHistory(existing, newEntries, maxHistoryEntries)
	assert.Len(t, result, 5)
}

func TestResizeHistoryMethod_LegacyDefaults(t *testing.T) {
	assert.Equal(t, "InPlace", resizeHistoryMethod(rightsizev1alpha1.ResizeHistoryEntry{
		Result: rightsizev1alpha1.ResizeResultSuccess,
	}))
	assert.Equal(t, "Eviction", resizeHistoryMethod(rightsizev1alpha1.ResizeHistoryEntry{
		Result: rightsizev1alpha1.ResizeResultEvicted,
	}))
}

func TestNormalizeResizeHistoryMethods_FillsMissingMethods(t *testing.T) {
	history := []rightsizev1alpha1.ResizeHistoryEntry{
		{Result: rightsizev1alpha1.ResizeResultSuccess},
		{Result: rightsizev1alpha1.ResizeResultEvicted},
		{Method: "InPlace", Result: rightsizev1alpha1.ResizeResultReverted},
	}

	changed := normalizeResizeHistoryMethods(history)

	assert.True(t, changed)
	assert.Equal(t, "InPlace", history[0].Method)
	assert.Equal(t, "Eviction", history[1].Method)
	assert.Equal(t, "InPlace", history[2].Method)
}

func TestNormalizeResizeHistoryMethods_IdempotentWhenAllPresent(t *testing.T) {
	history := []rightsizev1alpha1.ResizeHistoryEntry{
		{Method: "InPlace", Result: rightsizev1alpha1.ResizeResultSuccess},
		{Method: "Eviction", Result: rightsizev1alpha1.ResizeResultEvicted},
		{Method: "InPlace", Result: rightsizev1alpha1.ResizeResultReverted},
	}

	changed := normalizeResizeHistoryMethods(history)

	assert.False(t, changed, "should report no change when all methods are already set")
}

func TestResizeHistoryMethod_LegacyFailedDefaultsToInPlace(t *testing.T) {
	assert.Equal(t, "InPlace", resizeHistoryMethod(rightsizev1alpha1.ResizeHistoryEntry{
		Result: rightsizev1alpha1.ResizeResultFailed,
	}))
}

func TestIsSuccessfulInPlaceHistory_LegacySuccessCounts(t *testing.T) {
	assert.True(t, isSuccessfulInPlaceHistory(rightsizev1alpha1.ResizeHistoryEntry{
		Result: rightsizev1alpha1.ResizeResultSuccess,
	}))
	assert.False(t, isSuccessfulInPlaceHistory(rightsizev1alpha1.ResizeHistoryEntry{
		Result: rightsizev1alpha1.ResizeResultEvicted,
	}))
	assert.False(t, isSuccessfulInPlaceHistory(rightsizev1alpha1.ResizeHistoryEntry{
		Method: "InPlace",
		Result: rightsizev1alpha1.ResizeResultReverted,
	}))
}

func TestRemoveSuccessfulInPlaceHistory_UsesSharedSemantics(t *testing.T) {
	now := time.Now()
	entries := []rightsizev1alpha1.ResizeHistoryEntry{
		{Workload: "legacy-success", Result: rightsizev1alpha1.ResizeResultSuccess, Timestamp: metav1.NewTime(now)},
		{Workload: "explicit-success", Method: "InPlace", Result: rightsizev1alpha1.ResizeResultSuccess, Timestamp: metav1.NewTime(now)},
		{Workload: "eviction", Method: "Eviction", Result: rightsizev1alpha1.ResizeResultEvicted, Timestamp: metav1.NewTime(now)},
		{Workload: "reverted", Method: "InPlace", Result: rightsizev1alpha1.ResizeResultReverted, Timestamp: metav1.NewTime(now)},
	}

	filtered := removeSuccessfulInPlaceHistory(entries)

	assert.Len(t, filtered, 2)
	assert.Equal(t, "eviction", filtered[0].Workload)
	assert.Equal(t, "reverted", filtered[1].Workload)
}

func TestAppendUnique(t *testing.T) {
	result := appendUnique(nil, "pod-a")
	assert.Equal(t, []string{"pod-a"}, result)

	result = appendUnique(result, "pod-b")
	assert.Equal(t, []string{"pod-a", "pod-b"}, result)

	// Duplicate should be ignored.
	result = appendUnique(result, "pod-a")
	assert.Equal(t, []string{"pod-a", "pod-b"}, result)
}
