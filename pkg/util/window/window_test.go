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

package window

import (
	"testing"
	"time"
)

func newSlowConfig() Config {
	return Config{WindowDuration: 60 * time.Second, MinMBps: 20}
}

func sample(base time.Time, offset time.Duration, mbps float64) Sample {
	return Sample{Timestamp: base.Add(offset), MBps: mbps, SourcePod: "qwen-0"}
}

func TestWindowSustainedSlowRaises(t *testing.T) {
	ws := NewSet(newSlowConfig())
	base := time.Now()
	key := "ns/iset/llm-0"

	ws.Add(key, sample(base, 0, 10))
	ws.Add(key, sample(base, 30*time.Second, 8))
	v := ws.Add(key, sample(base, 60*time.Second, 12))

	if !v.Ready {
		t.Fatalf("window should be ready after a full %s span", newSlowConfig().WindowDuration)
	}
	if !v.Slow {
		t.Fatalf("every sample below threshold must yield Slow=true")
	}
	if v.WorstMBps != 8 {
		t.Fatalf("WorstMBps=%v, want 8", v.WorstMBps)
	}
}

func TestWindowTransientDipDoesNotRaise(t *testing.T) {
	ws := NewSet(newSlowConfig())
	base := time.Now()
	key := "ns/iset/llm-0"

	ws.Add(key, sample(base, 0, 10))
	ws.Add(key, sample(base, 30*time.Second, 25)) // one healthy sample clears it
	v := ws.Add(key, sample(base, 60*time.Second, 10))

	if !v.Ready {
		t.Fatalf("window should be ready")
	}
	if v.Slow {
		t.Fatalf("a single in-threshold sample must clear Slow (transient dip)")
	}
}

func TestWindowPartialNotReady(t *testing.T) {
	ws := NewSet(newSlowConfig())
	base := time.Now()
	key := "ns/iset/llm-0"

	ws.Add(key, sample(base, 0, 5))
	v := ws.Add(key, sample(base, 30*time.Second, 5)) // span 30s < 60s

	if v.Ready {
		t.Fatalf("window must not be ready before a full span")
	}
	if v.Slow {
		t.Fatalf("verdict must not be Slow before ready")
	}
}

func TestWindowEvictsOldSamples(t *testing.T) {
	ws := NewSet(newSlowConfig())
	base := time.Now()
	key := "ns/iset/llm-0"

	ws.Add(key, sample(base, 0, 50))             // healthy, will be evicted
	ws.Add(key, sample(base, 70*time.Second, 5)) // evicts the first sample
	v := ws.Add(key, sample(base, 130*time.Second, 5))

	if !v.Ready {
		t.Fatalf("window should be ready")
	}
	if !v.Slow {
		t.Fatalf("after evicting the healthy sample only slow samples remain")
	}
}

func TestWindowForget(t *testing.T) {
	ws := NewSet(newSlowConfig())
	base := time.Now()
	key := "ns/iset/llm-0"

	ws.Add(key, sample(base, 0, 5))
	ws.Add(key, sample(base, 30*time.Second, 5))
	ws.Forget(key)

	// After forgetting, a single new sample must not be Ready.
	v := ws.Add(key, sample(base, 60*time.Second, 5))
	if v.Ready {
		t.Fatalf("forgotten window must restart evaluation from scratch")
	}
}
