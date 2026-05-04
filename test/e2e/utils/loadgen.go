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

package utils

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// LoadGenStats captures the running counters of a load generator.
type LoadGenStats struct {
	Success      int64 // 2xx responses
	Errors5xx    int64
	OtherNon2xx  int64
	TransportErr int64 // connection errors, timeouts, etc.
	Total        int64
}

// LoadGenerator drives concurrent chat-completion requests against the
// gateway. Two modes are supported:
//
//   - Concurrency mode (Rate == 0): N goroutines loop forward, issuing a new
//     request as soon as the previous one returns — used to saturate serving
//     capacity and push vllm:num_requests_waiting above the KEDA threshold.
//   - Rate mode (Rate > 0): a single goroutine fires one request every
//     1/Rate seconds — used for the "no 5xx during scale-down" low-rate
//     background stream.
//
// Start is idempotent per instance; it must be followed by exactly one Stop.
type LoadGenerator struct {
	GatewayURL  string
	Model       string
	Prompt      string
	Concurrency int
	Rate        float64 // requests per second; 0 means Concurrency mode

	cancel context.CancelFunc
	wg     sync.WaitGroup

	success      atomic.Int64
	errors5xx    atomic.Int64
	otherNon2xx  atomic.Int64
	transportErr atomic.Int64
	total        atomic.Int64
}

// Start begins issuing requests. It returns immediately.
func (lg *LoadGenerator) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	lg.cancel = cancel

	if lg.Prompt == "" {
		lg.Prompt = "hello"
	}

	if lg.Rate > 0 {
		lg.wg.Add(1)
		go lg.runRate(ctx)
		return
	}
	n := lg.Concurrency
	if n <= 0 {
		n = 1
	}
	for i := 0; i < n; i++ {
		lg.wg.Add(1)
		go lg.runWorker(ctx)
	}
}

// Stop signals the generator to halt and waits for all workers to return.
func (lg *LoadGenerator) Stop() {
	if lg.cancel == nil {
		return
	}
	lg.cancel()
	lg.wg.Wait()
	lg.cancel = nil
}

// Stats returns a snapshot of the current counters.
func (lg *LoadGenerator) Stats() LoadGenStats {
	return LoadGenStats{
		Success:      lg.success.Load(),
		Errors5xx:    lg.errors5xx.Load(),
		OtherNon2xx:  lg.otherNon2xx.Load(),
		TransportErr: lg.transportErr.Load(),
		Total:        lg.total.Load(),
	}
}

func (lg *LoadGenerator) runWorker(ctx context.Context) {
	defer lg.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		lg.sendOnce()
	}
}

func (lg *LoadGenerator) runRate(ctx context.Context) {
	defer lg.wg.Done()
	interval := time.Duration(float64(time.Second) / lg.Rate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			lg.sendOnce()
		}
	}
}

func (lg *LoadGenerator) sendOnce() {
	lg.total.Add(1)
	resp, err := SendChatCompletionWithPrompt(lg.GatewayURL, lg.Model, lg.Prompt)
	if err != nil {
		lg.transportErr.Add(1)
		return
	}
	// Drain + close body for connection reuse.
	_, _ = ReadResponseBody(resp)
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		lg.success.Add(1)
	case resp.StatusCode >= 500 && resp.StatusCode < 600:
		lg.errors5xx.Add(1)
	default:
		lg.otherNon2xx.Add(1)
	}
	// Ensure 429/503 do not starve: brief breath.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		time.Sleep(100 * time.Millisecond)
	}
}
