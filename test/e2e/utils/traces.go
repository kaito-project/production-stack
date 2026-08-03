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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNoUsableSessions is returned by LoadTraceSessions when the fixture path
// resolved and parsed cleanly but yielded zero usable sessions (e.g. an empty
// or fully-filtered shard file). It is distinct from a hard failure (missing
// path, parse error) so callers such as the perf spec can treat an
// under-populated fixture as a skip rather than a failure.
var ErrNoUsableSessions = errors.New("trace fixture contained no usable sessions")

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
	Errors5xx    int64 // 5xx responses, incl. 503 (ext_proc fail-closed) and 504 (ext_proc timeout)
	OtherNon2xx  int64 // non-2xx, non-5xx: 4xx incl. 400 (context overflow) and 429 (queue full)
	TransportErr int64 // connection errors, timeouts before any HTTP status
	Total        int64
	// StatusCounts breaks the outcome down by exact HTTP status code so the
	// 5xx / OtherNon2xx buckets can be disambiguated (e.g. 503 vs 504, 400 vs
	// 429). Transport errors (no HTTP status) are recorded under key 0.
	StatusCounts map[int]int64
	// SampleErrors holds a bounded sample of non-2xx responses (status + a
	// truncated body) so the origin of failures can be read from the run
	// itself (e.g. an OpenAI-format vLLM error vs an Envoy/ext_proc local reply)
	// instead of hunting through pod logs.
	SampleErrors []ReplaySample
}

// ReplaySample is one captured non-2xx response used for diagnostics.
type ReplaySample struct {
	Status int
	Body   string
}

// BlockSizeTokens mirrors the simulator's prefix-cache block size (the shadow
// pod config sets block-size: 16). A prompt shorter than one full block yields
// zero prefix hashes, so the prefix-cache-scorer can neither index nor match it
// — such turns are dropped at load time (block-size floor).
const BlockSizeTokens = 16

// MaxModelLenTokens mirrors the simulator's max-model-len (the shadow pod
// config sets max-model-len: 32768). A turn whose prompt exceeds the model's
// context window is unservable — the backend rejects it with HTTP 400
// "maximum context length" — and a well-behaved client would truncate or split
// rather than send it. Such turns are dropped at load time (context ceiling),
// symmetric to the block-size floor. Because turns grow monotonically within a
// session, dropping an over-length turn truncates the session at that point and
// preserves the shared-prefix chain of what remains. The estimator below is
// >= the sim's (roughly word-based) dummy tokenizer count, so the ceiling
// conservatively catches every turn the sim would reject.
const MaxModelLenTokens = 32768

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

// decodeTraceLine parses one JSONL line into a trace row. ok is false when the
// line is blank, missing required fields, or filtered by the block-size floor
// or context ceiling; err is non-nil only for a hard JSON parse failure. It is
// the single decode+filter point used by LoadTraceSessions.
func decodeTraceLine(text string) (row traceRow, ok bool, err error) {
	line := strings.TrimSpace(text)
	if line == "" {
		return traceRow{}, false, nil
	}
	if err := json.Unmarshal([]byte(line), &row); err != nil {
		return traceRow{}, false, err
	}
	if row.SessionID == "" || len(row.Input) == 0 {
		return traceRow{}, false, nil
	}
	// Block-size floor: a prompt shorter than one 16-token block produces no
	// prefix hashes, so the prefix-cache-scorer can't act on it. Drop such
	// turns. Because every turn is a prefix-superset of the previous one,
	// dropping short (necessarily leading) turns preserves the monotonic
	// shared-prefix chain of what remains.
	if estimateTokens(row.Input) < BlockSizeTokens {
		return traceRow{}, false, nil
	}
	// Context ceiling: a prompt exceeding max-model-len is unservable (HTTP 400
	// "maximum context length"). Drop it — the replay should only send requests
	// a real client could serve. Dropping an over-length (necessarily trailing)
	// turn truncates the session there and preserves the prefix chain of the
	// earlier turns.
	if estimateTokens(row.Input) > MaxModelLenTokens {
		return traceRow{}, false, nil
	}
	return row, true, nil
}

// LoadTraceSessions reads a JSONL trace fixture and groups its rows into
// sessions, preserving both session order and intra-session turn order. It
// materializes every session in memory and so is intended for the small
// committed fixture and for callers that need random access (the A/B and
// sticky specs).
//
// The path may be a single JSONL file or a directory (all *.jsonl children are
// read in sorted order). This is what lets the perf spec point
// E2E_TRACE_FIXTURE at a whole directory of shards so their sessions merge
// into one corpus — a single shard produced by the extract
// script's --shards mode may hold too few distinct sessions to exercise
// cross-pod prefix routing. Rows are grouped by session_id across all files, so
// merging shards is safe even if a session's rows were somehow split across
// files.
func LoadTraceSessions(path string) ([]ReplaySession, error) {
	files, err := resolveFixtureFiles(path)
	if err != nil {
		return nil, err
	}

	// Grouping is global across every file so shards combine into one corpus.
	idx := make(map[string]int)
	var sessions []ReplaySession
	for _, file := range files {
		if err := appendTraceSessions(file, idx, &sessions); err != nil {
			return nil, err
		}
	}
	if len(sessions) == 0 {
		return nil, fmt.Errorf("trace fixture %q: %w", path, ErrNoUsableSessions)
	}
	return sessions, nil
}

// resolveFixtureFiles expands a fixture path into the concrete JSONL files to
// read. A plain file yields itself; a directory yields its *.jsonl children
// sorted so shard order is stable and reproducible.
func resolveFixtureFiles(path string) ([]string, error) {
	info, statErr := os.Stat(path)
	if statErr == nil && info.IsDir() {
		matches, err := filepath.Glob(filepath.Join(path, "*.jsonl"))
		if err != nil {
			return nil, fmt.Errorf("globbing trace fixture dir %q: %w", path, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("no *.jsonl files under trace fixture dir %q", path)
		}
		sort.Strings(matches)
		return matches, nil
	}
	if statErr == nil {
		return []string{path}, nil
	}
	return nil, fmt.Errorf("resolving trace fixture %q: %w", path, statErr)
}

// appendTraceSessions reads one JSONL file and merges its rows into the shared
// idx/sessions accumulator, preserving intra-session turn order. Grouping is by
// session_id via the caller-owned idx map, so calling it across multiple shard
// files combines them into a single session corpus.
func appendTraceSessions(path string, idx map[string]int, sessions *[]ReplaySession) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening trace fixture %q: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Agentic contexts can be large; allow long lines (up to 32 MiB).
	scanner.Buffer(make([]byte, 1024*1024), 32*1024*1024)

	lineNo := 0
	for scanner.Scan() {
		lineNo++
		row, ok, err := decodeTraceLine(scanner.Text())
		if err != nil {
			return fmt.Errorf("parsing trace fixture %q line %d: %w", path, lineNo, err)
		}
		if !ok {
			continue
		}
		i, seen := idx[row.SessionID]
		if !seen {
			i = len(*sessions)
			idx[row.SessionID] = i
			*sessions = append(*sessions, ReplaySession{SessionID: row.SessionID})
		}
		(*sessions)[i].Turns = append((*sessions)[i].Turns, row.Input)
		(*sessions)[i].PreGaps = append((*sessions)[i].PreGaps, row.PreGap)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading trace fixture %q: %w", path, err)
	}
	return nil
}

// TraceShard is one shard's worth of sessions, loaded from a single fixture
// file. Path is the source file (for diagnostics).
type TraceShard struct {
	Path     string
	Sessions []ReplaySession
}

// StreamTraceShards resolves a fixture path (single file or directory)
// into its concrete shard files and calls fn once per non-empty shard, with
// that shard's sessions loaded into memory. Crucially, only ONE shard is
// resident at a time: fn is invoked, then the slice goes out of scope and is
// freed before the next file is read, so peak memory is bounded by the largest
// single shard rather than the whole corpus. This is what lets the perf spec
// replay a whole directory of shards against a small box without OOM (contrast
// LoadTraceSessions, which materializes every shard at once).
//
// Grouping is PER FILE here (unlike LoadTraceSessions' global grouping): each
// file is an independent shard. The extract script's round-robin --shards mode
// keeps every session's rows whole within one file, so per-file grouping loses
// nothing. Empty / fully-filtered shards are skipped (fn is not called for
// them). If NO file yields any usable session, ErrNoUsableSessions is returned.
func StreamTraceShards(path string, fn func(TraceShard) error) error {
	files, err := resolveFixtureFiles(path)
	if err != nil {
		return err
	}
	usable := false
	for _, file := range files {
		// Fresh accumulators each iteration so the previous shard is GC'd.
		idx := make(map[string]int)
		var sessions []ReplaySession
		if err := appendTraceSessions(file, idx, &sessions); err != nil {
			return err
		}
		if len(sessions) == 0 {
			continue
		}
		usable = true
		if err := fn(TraceShard{Path: file, Sessions: sessions}); err != nil {
			return err
		}
	}
	if !usable {
		return fmt.Errorf("trace fixture %q: %w", path, ErrNoUsableSessions)
	}
	return nil
}

// replayFromChannel is the shared worker-pool core. It drains sessions from
// `in` across `concurrency` workers; the turns of any one session are always
// sent sequentially by a single worker. When honorTiming is true the recorded
// pre_gap delay is applied before each turn (realistic think/tool time); when
// false turns fire back-to-back for maximum cache pressure. All requests target
// `model` (the deployment name / X-Gateway-Model-Name), overriding the model
// recorded in the trace. The caller owns `in` and must close it (or cancel ctx)
// to terminate the pool.
func replayFromChannel(ctx context.Context, gatewayURL, model string, in <-chan ReplaySession, concurrency int, honorTiming bool) ReplayStats {
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
	// Per-status-code tally. HTTP calls are the bottleneck, so a single mutex
	// here is negligible; transport errors are recorded under key 0.
	var statusMu sync.Mutex
	statusCounts := make(map[int]int64)
	recordStatus := func(code int) {
		statusMu.Lock()
		statusCounts[code]++
		statusMu.Unlock()
	}
	// Capture a bounded sample of non-2xx response bodies (truncated) so the
	// failure origin is visible in the run output.
	const maxSamples = 5
	const maxSampleBody = 400
	var sampleMu sync.Mutex
	var samples []ReplaySample
	captureSample := func(status int, body []byte) {
		sampleMu.Lock()
		defer sampleMu.Unlock()
		if len(samples) >= maxSamples {
			return
		}
		b := strings.TrimSpace(string(body))
		if len(b) > maxSampleBody {
			b = b[:maxSampleBody] + "…"
		}
		samples = append(samples, ReplaySample{Status: status, Body: b})
	}

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range in {
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
						recordStatus(0)
						continue
					}
					body, _ := ReadResponseBody(resp)
					recordStatus(resp.StatusCode)
					switch {
					case resp.StatusCode >= 200 && resp.StatusCode < 300:
						success.Add(1)
					case resp.StatusCode >= 500 && resp.StatusCode < 600:
						errors5xx.Add(1)
						captureSample(resp.StatusCode, body)
					default:
						otherNon2xx.Add(1)
						captureSample(resp.StatusCode, body)
					}
				}
			}
		}()
	}
	wg.Wait()

	return ReplayStats{
		Success:      success.Load(),
		Errors5xx:    errors5xx.Load(),
		OtherNon2xx:  otherNon2xx.Load(),
		TransportErr: transportErr.Load(),
		Total:        total.Load(),
		StatusCounts: statusCounts,
		SampleErrors: samples,
	}
}

// ReplaySessionsConcurrent replays a materialized slice of sessions against the
// gateway using the shared worker pool. See replayFromChannel for the semantics
// of concurrency and honorTiming.
func ReplaySessionsConcurrent(ctx context.Context, gatewayURL, model string, sessions []ReplaySession, concurrency int, honorTiming bool) ReplayStats {
	in := make(chan ReplaySession)
	go func() {
		defer close(in)
		for _, s := range sessions {
			select {
			case <-ctx.Done():
				return
			case in <- s:
			}
		}
	}()
	return replayFromChannel(ctx, gatewayURL, model, in, concurrency, honorTiming)
}
