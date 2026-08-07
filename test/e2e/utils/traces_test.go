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
	"encoding/json"
	"strings"
	"testing"
)

func TestEstimateTokensMirrorsDummyTokenizer(t *testing.T) {
	messages := []ChatMessage{{Role: "user", Content: "hello, world!"}}
	if got, want := estimateTokens(messages), 9; got != want {
		t.Fatalf("estimateTokens() = %d, want %d", got, want)
	}

	messages = []ChatMessage{{
		Role: "assistant",
		ToolCalls: []ToolCall{{
			Function: ToolCallFunction{Name: "lookup", Arguments: `{"x":1}`},
		}},
	}}
	if got, want := estimateTokens(messages), 17; got != want {
		t.Fatalf("estimateTokens() with tool call = %d, want %d", got, want)
	}
}

func TestFitsModelContextAtDummyTokenizerBoundary(t *testing.T) {
	overheadTokens := 5 // "###", role, and colon.
	if !FitsModelContext([]ChatMessage{{
		Role:    "user",
		Content: strings.Repeat("word ", MaxModelLenTokens-overheadTokens),
	}}) {
		t.Fatal("prompt at max-model-len should fit")
	}
	if FitsModelContext([]ChatMessage{{
		Role:    "user",
		Content: strings.Repeat("word ", MaxModelLenTokens-overheadTokens+1),
	}}) {
		t.Fatal("prompt above max-model-len should not fit")
	}
}

func TestDecodeTraceLineRejectsPunctuationHeavyOverflow(t *testing.T) {
	row := traceRow{
		SessionID: "code-heavy",
		Input: []ChatMessage{{
			Role:    "user",
			Content: strings.Repeat("x,", (MaxModelLenTokens/2)+1),
		}},
	}
	data, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := decodeTraceLine(string(data)); err != nil || ok {
		t.Fatalf("decodeTraceLine() = ok %v, err %v; want filtered without error", ok, err)
	}
}
