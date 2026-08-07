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
	"encoding/json"
	"fmt"
	"io"
	"syscall"
	"testing"
)

func TestChatCompletionRequestSerializesMaxTokens(t *testing.T) {
	data, err := json.Marshal(ChatCompletionRequest{Model: "model", MaxTokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), `{"model":"model","messages":null,"max_tokens":1}`; got != want {
		t.Fatalf("json.Marshal() = %s, want %s", got, want)
	}
}

func TestIsRecoverablePortForwardError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "wrapped EOF", err: fmt.Errorf("post request: %w", io.EOF), want: true},
		{name: "connection reset", err: syscall.ECONNRESET, want: true},
		{name: "request deadline", err: context.DeadlineExceeded, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isRecoverablePortForwardError(test.err); got != test.want {
				t.Fatalf("isRecoverablePortForwardError() = %v, want %v", got, test.want)
			}
		})
	}
}
