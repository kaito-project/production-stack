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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kaito-project/production-stack/test/e2e/deploy"
)

// fakeEndpoint records Reset calls and can hand out a different URL each time,
// standing in for a tunnel that is rebuilt on a fresh local port.
type fakeEndpoint struct {
	urls   []string
	host   string
	resets int
}

func (e *fakeEndpoint) BaseURL() (string, error) {
	url := e.urls[0]
	if len(e.urls) > 1 {
		e.urls = e.urls[1:]
	}
	return url, nil
}
func (e *fakeEndpoint) Host() string { return e.host }
func (e *fakeEndpoint) Reset()       { e.resets++ }

func TestChatCompletionRequestSerializesMaxTokens(t *testing.T) {
	data, err := json.Marshal(ChatCompletionRequest{Model: "model", MaxTokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), `{"model":"model","messages":null,"max_tokens":1}`; got != want {
		t.Fatalf("json.Marshal() = %s, want %s", got, want)
	}
}

// The endpoint hands out the API root, so helpers must append the relative
// path. Appending "/v1/..." to a base that already carries it was the failure
// mode this guards against.
func TestSendChatPostsToAPIRootWithHostAndAuth(t *testing.T) {
	var gotPath, gotHost, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotHost, gotAuth = r.URL.Path, r.Host, r.Header.Get("Authorization")
	}))
	defer server.Close()

	gateway := &fakeEndpoint{urls: []string{server.URL + "/v1"}, host: "ns.gw.example.com"}
	resp, err := SendChat(gateway, "md1", WithAuth(deploy.AuthHeader{Name: "Authorization", Value: "Bearer k"}))
	if err != nil {
		t.Fatalf("SendChat: %v", err)
	}
	resp.Body.Close()

	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", gotPath)
	}
	if gotHost != "ns.gw.example.com" {
		t.Errorf("Host = %q, want ns.gw.example.com", gotHost)
	}
	if gotAuth != "Bearer k" {
		t.Errorf("Authorization = %q, want Bearer k", gotAuth)
	}
}

// A tunnel can only be repaired by rebuilding it, so retrying against the same
// URL would fail identically however many attempts are allowed.
func TestSendChatWithTransportRetryResetsTheEndpoint(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	live := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer live.Close()

	gateway := &fakeEndpoint{urls: []string{deadURL + "/v1", live.URL + "/v1"}}
	resp, err := SendChat(gateway, "md1", WithTransportRetry())
	if err != nil {
		t.Fatalf("SendChat: %v", err)
	}
	resp.Body.Close()

	if gateway.resets != 1 {
		t.Fatalf("Reset called %d times, want 1", gateway.resets)
	}
}

func TestGatewayOriginDropsTheAPIRoot(t *testing.T) {
	got, err := GatewayOrigin(&fakeEndpoint{urls: []string{"https://ns.example.com/v1"}})
	if err != nil {
		t.Fatalf("GatewayOrigin: %v", err)
	}
	if want := "https://ns.example.com"; got != want {
		t.Fatalf("GatewayOrigin() = %q, want %q", got, want)
	}
}
