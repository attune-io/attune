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
	"sync"
	"time"
)

// oomBumpPendingKey identifies one container on one workload for one policy.
// workloadKind keeps a Deployment and a Rollout that share a name apart.
// The pod annotation attune.io/oom-bump.<container> is a different key.
type oomBumpPendingKey struct {
	policyUID       string
	policyNamespace string
	policyName      string
	workloadNS      string
	workloadName    string
	workloadKind    string
	container       string
}

// oomBumpPendingEntry keeps planned stamps and the in-hold records that were
// already stored before this cycle. Dropping a stamp must not drop baseHeld.
type oomBumpPendingEntry struct {
	stamps   []oomBumpPodStamp
	baseHeld []oomBumpRecord
}

// oomBumpClear is one pod whose bump annotation a full revert removed.
// oomAt and restart stay until a strictly newer OOM, including across
// ResetUID. The informer can still show the key after the delete.
type oomBumpClear struct {
	policyNamespace string
	policyName      string
	namespace       string
	podName         string
	container       string
	raw             string
	oomAt           time.Time
	restart         int32
}

// oomBumpPending holds this reconcile's planned pod stamps.
// Recommend mode must not Put. Resize success Marks applied.
// A skip Drops the pod so the workload annotation cannot count it.
// cleared keeps a consumed OOM signal until the newer stamp is stored.
type oomBumpPending struct {
	mu      sync.Mutex
	items   map[oomBumpPendingKey]oomBumpPendingEntry
	cleared map[string][]oomBumpClear
}

func newOOMBumpPending() *oomBumpPending {
	return &oomBumpPending{
		items:   map[oomBumpPendingKey]oomBumpPendingEntry{},
		cleared: map[string][]oomBumpClear{},
	}
}

func (p *oomBumpPending) ResetUID(policyUID string) {
	if p == nil || policyUID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.items {
		if k.policyUID == policyUID {
			delete(p.items, k)
		}
	}
}

func (p *oomBumpPending) ForgetPolicy(namespace, name string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.items {
		if k.policyNamespace == namespace && k.policyName == name {
			delete(p.items, k)
		}
	}
	for uid, list := range p.cleared {
		kept := make([]oomBumpClear, 0, len(list))
		for _, c := range list {
			if c.policyNamespace == namespace && c.policyName == name {
				continue
			}
			kept = append(kept, c)
		}
		if len(kept) == 0 {
			delete(p.cleared, uid)
			continue
		}
		p.cleared[uid] = kept
	}
}

// NoteClear records an OOM signal a full revert already consumed.
// One pod keeps one signal. A repeat is ignored. A newer finishedAt,
// or a higher restart when finishedAt is zero, replaces the older one.
func (p *oomBumpPending) NoteClear(policyUID string, c oomBumpClear) {
	if p == nil || policyUID == "" || c.podName == "" || c.container == "" {
		return
	}
	if rec, ok := parseOOMBumpRecord(c.raw); ok {
		c.oomAt = rec.OOMAt
		c.restart = rec.Restart
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cleared == nil {
		p.cleared = map[string][]oomBumpClear{}
	}
	list := p.cleared[policyUID]
	for i, prev := range list {
		if prev.namespace != c.namespace || prev.podName != c.podName || prev.container != c.container {
			continue
		}
		if !oomSignalIsNew(c.oomAt, c.restart, &oomBumpRecord{OOMAt: prev.oomAt, Restart: prev.restart}) {
			return
		}
		list[i] = c
		p.cleared[policyUID] = list
		return
	}
	p.cleared[policyUID] = append(list, c)
}

// dropOlderClears forgets a consumed signal once the newer stamp is stored
// for that pod. The newer OOM starts at count 1 from the live request.
func (p *oomBumpPending) dropOlderClears(policyUID, namespace, podName, container string, finished time.Time, restart int32) {
	if p == nil || policyUID == "" || podName == "" || container == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	list := p.cleared[policyUID]
	if len(list) == 0 {
		return
	}
	kept := make([]oomBumpClear, 0, len(list))
	for _, prev := range list {
		if prev.namespace == namespace && prev.podName == podName && prev.container == container &&
			oomSignalIsNew(finished, restart, &oomBumpRecord{OOMAt: prev.oomAt, Restart: prev.restart}) {
			continue
		}
		kept = append(kept, prev)
	}
	if len(kept) == 0 {
		delete(p.cleared, policyUID)
		return
	}
	p.cleared[policyUID] = kept
}

// oomBumpAnnotationOnly is one planned annotation-only stamp.
// Map iteration order is not stable.
type oomBumpAnnotationOnly struct {
	container string
	stamp     oomBumpPodStamp
}

// annotationOnlyStamps copies planned annotation-only stamps for one workload.
func (p *oomBumpPending) annotationOnlyStamps(policyUID, policyNamespace, policyName, workloadNS, workloadName, workloadKind string) []oomBumpAnnotationOnly {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []oomBumpAnnotationOnly
	for k, entry := range p.items {
		if k.policyUID != policyUID || k.policyNamespace != policyNamespace || k.policyName != policyName ||
			k.workloadNS != workloadNS || k.workloadName != workloadName || k.workloadKind != workloadKind {
			continue
		}
		for _, stamp := range entry.stamps {
			if !stamp.AnnotationOnly {
				continue
			}
			cp := stamp
			out = append(out, oomBumpAnnotationOnly{container: k.container, stamp: cp})
		}
	}
	return out
}

// Clears returns consumed full-revert records for one policy.
func (p *oomBumpPending) Clears(policyUID string) []oomBumpClear {
	if p == nil || policyUID == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]oomBumpClear(nil), p.cleared[policyUID]...)
}

func (p *oomBumpPending) Put(policyUID, policyNamespace, policyName, workloadNS, workloadName, workloadKind, container string, stamps []oomBumpPodStamp, baseHeld []oomBumpRecord) {
	if p == nil || len(stamps) == 0 {
		return
	}
	cp := append([]oomBumpPodStamp(nil), stamps...)
	held := append([]oomBumpRecord(nil), baseHeld...)
	p.mu.Lock()
	defer p.mu.Unlock()
	k := oomBumpPendingKey{policyUID, policyNamespace, policyName, workloadNS, workloadName, workloadKind, container}
	p.items[k] = oomBumpPendingEntry{stamps: cp, baseHeld: held}
}

func (p *oomBumpPending) peek(k oomBumpPendingKey, podNS, podName string) (oomBumpPodStamp, int, bool) {
	entry := p.items[k]
	for i := range entry.stamps {
		if entry.stamps[i].Namespace == podNS && entry.stamps[i].PodName == podName {
			return entry.stamps[i], i, true
		}
	}
	return oomBumpPodStamp{}, 0, false
}

// Stamps returns a copy of the planned stamps for one container.
func (p *oomBumpPending) Stamps(policyUID, policyNamespace, policyName, workloadNS, workloadName, workloadKind, container string) []oomBumpPodStamp {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := p.items[oomBumpPendingKey{policyUID, policyNamespace, policyName, workloadNS, workloadName, workloadKind, container}]
	return append([]oomBumpPodStamp(nil), entry.stamps...)
}

// PeekPod returns a copy of the planned stamp. It does not remove it.
func (p *oomBumpPending) PeekPod(policyUID, policyNamespace, policyName, workloadNS, workloadName, workloadKind, container, podNS, podName string) (oomBumpPodStamp, bool) {
	if p == nil {
		return oomBumpPodStamp{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	stamp, _, ok := p.peek(oomBumpPendingKey{policyUID, policyNamespace, policyName, workloadNS, workloadName, workloadKind, container}, podNS, podName)
	return stamp, ok
}

// DropPod removes a stamp that must not increment count (budget, QoS, pod not selected).
// baseHeld stays so a later Applied call can still publish the stored floor.
func (p *oomBumpPending) DropPod(policyUID, policyNamespace, policyName, workloadNS, workloadName, workloadKind, container, podNS, podName string) (oomBumpPodStamp, bool) {
	if p == nil {
		return oomBumpPodStamp{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	k := oomBumpPendingKey{policyUID, policyNamespace, policyName, workloadNS, workloadName, workloadKind, container}
	stamp, i, ok := p.peek(k, podNS, podName)
	if !ok {
		return oomBumpPodStamp{}, false
	}
	entry := p.items[k]
	entry.stamps = append(entry.stamps[:i], entry.stamps[i+1:]...)
	if len(entry.stamps) == 0 && len(entry.baseHeld) == 0 {
		delete(p.items, k)
	} else {
		p.items[k] = entry
	}
	return stamp, true
}

// MarkApplied keeps the stamp in the list and records that the pod annotation was stored.
func (p *oomBumpPending) MarkApplied(policyUID, policyNamespace, policyName, workloadNS, workloadName, workloadKind, container, podNS, podName string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	k := oomBumpPendingKey{policyUID, policyNamespace, policyName, workloadNS, workloadName, workloadKind, container}
	entry := p.items[k]
	for i := range entry.stamps {
		if entry.stamps[i].Namespace == podNS && entry.stamps[i].PodName == podName {
			entry.stamps[i].applied = true
			p.items[k] = entry
			return
		}
	}
}

// Applied returns stamps whose pod annotation was stored, plus the in-hold
// records from before this cycle, then deletes the key.
func (p *oomBumpPending) Applied(policyUID, policyNamespace, policyName, workloadNS, workloadName, workloadKind, container string) ([]oomBumpPodStamp, []oomBumpRecord) {
	if p == nil {
		return nil, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	k := oomBumpPendingKey{policyUID, policyNamespace, policyName, workloadNS, workloadName, workloadKind, container}
	entry, ok := p.items[k]
	if !ok {
		return nil, nil
	}
	delete(p.items, k)
	out := make([]oomBumpPodStamp, 0, len(entry.stamps))
	for _, stamp := range entry.stamps {
		if stamp.applied {
			out = append(out, stamp)
		}
	}
	held := append([]oomBumpRecord(nil), entry.baseHeld...)
	return out, held
}
