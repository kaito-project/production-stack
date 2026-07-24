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
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Agentic-trace replay driver.
//
// The prefix-cache perf spec drives load from real multi-turn agent sessions
// extracted from the HuggingFace dataset sammshen/lmcache-agentic-traces
// (Option A: a small trimmed fixture is committed under test/e2e/testdata and
// regenerated offline by hack/e2e/scripts/extract_agentic_traces.py — the full
// 2.37 GB dataset is never fetched at test time).
//
// Each fixture line is one LLM iteration of a session. Its `input` field is the
// full cumulative OpenAI-format messages array for that turn, and iteration N's
// input is a strict prefix-superset of iteration N-1's — exactly the
// prefix-sharing pattern the EPP prefix-cache-scorer is meant to exploit.
//
// Replay contract (mirrors the dataset's AIPerf guidance):
//   - Turns WITHIN a session are sent strictly sequentially so the shared
//     prefix accumulates and hits the KV cache.
//   - Sessions run CONCURRENTLY (Concurrency workers) to create sustained load
//     and fill the queue.

// traceRow is one line of the JSONL fixture. It matches the dataset schema;
// fields the replayer does not need are ignored.
type traceRow struct {
	SessionID    string        `json:"session_id"`
	Model        string        `json:"model"`
	Input        []ChatMessage `json:"input"`
	PreGap       float64       `json:"pre_gap"`
	OutputLength int           `json:"output_length"`
}

// ReplaySession is one agent task: an ordered list of turns, where each turn is
// the full cumulative messages array for that iteration.
type ReplaySession struct {
	SessionID string
	Turns     [][]ChatMessage
	// PreGaps[i] is the client-side think/tool time (seconds) before turn i.
	PreGaps []float64
}

// ReplayStats captures the aggregate outcome of a replay run.
type ReplayStats struct {
	Success      int64 // 2xx responses
	Errors5xx    int64
	OtherNon2xx  int64 // 4xx (incl. 429) and non-5xx failures like 503
	TransportErr int64 // connection errors, timeouts
	Total        int64
}

// BlockSizeTokens mirrors the simulator's prefix-cache block size (the shadow
// pod config sets block-size: 16). A prompt shorter than one full block yields
// zero prefix hashes, so the prefix-cache-scorer can neither index nor match it
// — such turns are dropped at load time (issue #109 block-size floor).
const BlockSizeTokens = 16

// estimateTokens approximates the token count of a messages array closely
// enough to enforce the one-block floor. It takes the larger of the
// whitespace-word count and chars/4 (a common rough bytes-per-token ratio):
// it matches neither tokenizer exactly but is comfortably conservative for a
// 16-token floor, so it never drops a turn that would actually span a block.
func estimateTokens(msgs []ChatMessage) int {
	words, chars := 0, 0
	for _, m := range msgs {
		chars += len(m.Content)
		words += len(strings.Fields(m.Content))
	}
	if approx := chars / 4; approx > words {
		return approx
	}
	return words
}

// LoadTraceSessions reads a JSONL trace fixture and groups its rows into
// sessions, preserving both session order and intra-session turn order.
func LoadTraceSessions(path string) ([]ReplaySession, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening trace fixture %q: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Agentic contexts can be large; allow long lines (up to 32 MiB).
	scanner.Buffer(make([]byte, 1024*1024), 32*1024*1024)

	idx := make(map[string]int)
	var sessions []ReplaySession
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row traceRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("parsing trace fixture %q line %d: %w", path, lineNo, err)
		}
		if row.SessionID == "" || len(row.Input) == 0 {
			continue
		}
		// Block-size floor (issue #109): a prompt shorter than one 16-token
		// block produces no prefix hashes, so the prefix-cache-scorer can't
		// act on it. Drop such turns. Because every turn is a prefix-superset
		// of the previous one, dropping short (necessarily leading) turns
		// preserves the monotonic shared-prefix chain of what remains.
		if estimateTokens(row.Input) < BlockSizeTokens {
			continue
		}
		i, ok := idx[row.SessionID]
		if !ok {
			i = len(sessions)
			idx[row.SessionID] = i
			sessions = append(sessions, ReplaySession{SessionID: row.SessionID})
		}
		sessions[i].Turns = append(sessions[i].Turns, row.Input)
		sessions[i].PreGaps = append(sessions[i].PreGaps, row.PreGap)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading trace fixture %q: %w", path, err)
	}
	if len(sessions) == 0 {
		return nil, fmt.Errorf("trace fixture %q contained no usable sessions", path)
	}
	return sessions, nil
}

// ReplaySessionsConcurrent replays the given sessions against the gateway.
// Sessions are distributed across `concurrency` workers; the turns of any one
// session are always sent sequentially by a single worker. When honorTiming is
// true the recorded pre_gap delay is applied before each turn (realistic
// think/tool time); when false turns fire back-to-back for maximum cache
// pressure. All requests target `model` (the deployment name / X-Gateway-Model-Name),
// overriding the model recorded in the trace.
func ReplaySessionsConcurrent(ctx context.Context, gatewayURL, model string, sessions []ReplaySession, concurrency int, honorTiming bool) ReplayStats {
	if concurrency <= 0 {
		concurrency = 1
	}

	var (
		success      atomic.Int64
		errors5xx    atomic.Int64
		otherNon2xx  atomic.Int64
		transportErr atomic.Int64
		total        atomic.Int64
	)

	sessionCh := make(chan ReplaySession)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range sessionCh {
				for turnIdx, turn := range s.Turns {
					select {
					case <-ctx.Done():
						return
					default:
					}
					if honorTiming && turnIdx < len(s.PreGaps) && s.PreGaps[turnIdx] > 0 {
						time.Sleep(time.Duration(s.PreGaps[turnIdx] * float64(time.Second)))
					}
					total.Add(1)
					resp, err := SendChatCompletionRaw(gatewayURL, ChatCompletionRequest{
						Model:    model,
						Messages: turn,
					})
					if err != nil {
						transportErr.Add(1)
						continue
					}
					_, _ = ReadResponseBody(resp)
					switch {
					case resp.StatusCode >= 200 && resp.StatusCode < 300:
						success.Add(1)
					case resp.StatusCode >= 500 && resp.StatusCode < 600:
						errors5xx.Add(1)
					default:
						otherNon2xx.Add(1)
					}
				}
			}
		}()
	}

	for _, s := range sessions {
		select {
		case <-ctx.Done():
			goto drain
		case sessionCh <- s:
		}
	}
drain:
	close(sessionCh)
	wg.Wait()

	return ReplayStats{
		Success:      success.Load(),
		Errors5xx:    errors5xx.Load(),
		OtherNon2xx:  otherNon2xx.Load(),
		TransportErr: transportErr.Load(),
		Total:        total.Load(),
	}
}
