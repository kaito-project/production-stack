/*
Copyright 2026 The KAITO Authors.

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

// Package window provides a per-key sliding-window evaluator over throughput
// samples. Each key (e.g. a pod) accumulates timestamped MB/s samples bounded
// by a time window; once the window is fully populated the evaluator reports
// whether every sample stayed below a minimum throughput (a sustained-slow
// verdict). A single in-threshold sample inside the window clears it, so
// transient dips never raise the verdict. A Set is safe for concurrent use.
package window

import (
	"sync"
	"time"
)

// Config configures the sliding-window evaluation.
type Config struct {
	// WindowDuration is the sliding window length (e.g. 60s).
	WindowDuration time.Duration
	// MinMBps is the throughput threshold in MB/s. A sample strictly below this
	// is "slow".
	MinMBps float64
}

// Sample is one observed throughput reading fed into a window.
type Sample struct {
	// Timestamp is when the sample was taken.
	Timestamp time.Time
	// MBps is the observed throughput in MB/s.
	MBps float64
	// SourcePod is the pod the sample was scraped from.
	SourcePod string
}

// Verdict is the result of evaluating one key's throughput window.
type Verdict struct {
	// Ready reports whether the window is fully populated (span >=
	// WindowDuration and >= 2 samples) and therefore eligible for a decision.
	Ready bool
	// Slow is true only when Ready and EVERY sample in the window is strictly
	// below MinMBps. A single in-threshold sample inside the window clears it,
	// so transient dips never raise the verdict.
	Slow bool
	// WorstMBps is the lowest throughput observed across the window.
	WorstMBps float64
	// SourcePod is the pod the window's samples were scraped from.
	SourcePod string
}

// bucket is a per-key ring buffer of throughput samples bounded by
// WindowDuration. Samples older than the window are evicted on every add and
// evaluate.
type bucket struct {
	cfg     Config
	samples []Sample
}

func (b *bucket) add(s Sample) {
	b.samples = append(b.samples, s)
	b.evict(s.Timestamp)
}

// evict drops samples whose timestamp is older than now-WindowDuration.
func (b *bucket) evict(now time.Time) {
	cutoff := now.Add(-b.cfg.WindowDuration)
	i := 0
	for i < len(b.samples) && b.samples[i].Timestamp.Before(cutoff) {
		i++
	}
	if i > 0 {
		b.samples = append(b.samples[:0:0], b.samples[i:]...)
	}
}

func (b *bucket) evaluate(now time.Time) Verdict {
	b.evict(now)
	if len(b.samples) < 2 {
		return Verdict{Ready: false}
	}
	span := b.samples[len(b.samples)-1].Timestamp.Sub(b.samples[0].Timestamp)
	if span < b.cfg.WindowDuration {
		return Verdict{Ready: false}
	}
	worst := b.samples[0].MBps
	allBelow := true
	for _, s := range b.samples {
		if s.MBps < worst {
			worst = s.MBps
		}
		if s.MBps >= b.cfg.MinMBps {
			allBelow = false
		}
	}
	return Verdict{
		Ready:     true,
		Slow:      allBelow,
		WorstMBps: worst,
		SourcePod: b.samples[len(b.samples)-1].SourcePod,
	}
}

// Set maintains one throughput window per key and is safe for concurrent use.
type Set struct {
	cfg     Config
	mu      sync.Mutex
	buckets map[string]*bucket
}

// NewSet creates a Set with the given configuration.
func NewSet(cfg Config) *Set {
	return &Set{cfg: cfg, buckets: map[string]*bucket{}}
}

// Add records a throughput sample for key and returns the current verdict.
func (s *Set) Add(key string, sample Sample) Verdict {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.buckets[key]
	if b == nil {
		b = &bucket{cfg: s.cfg}
		s.buckets[key] = b
	}
	b.add(sample)
	return b.evaluate(sample.Timestamp)
}

// Forget drops the window for key so a subsequent sample re-evaluates from
// scratch.
func (s *Set) Forget(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.buckets, key)
}
