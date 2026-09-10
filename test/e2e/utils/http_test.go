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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

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
	var gotPath, gotHost, gotAuth, gotMethod string
	var gotBody ChatCompletionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotHost, gotAuth = r.URL.Path, r.Host, r.Header.Get("Authorization")
		gotMethod = r.Method
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()

	gateway := &fakeEndpoint{urls: []string{server.URL + "/v1"}, host: "ns.gw.example.com"}
	optionCalls := 0
	resp, err := SendChat(gateway, "md1",
		WithAuth(deploy.AuthHeader{Name: "Authorization", Value: "Bearer k"}),
		WithPrompt("custom prompt"), WithMethod(http.MethodPut),
		func(request *gatewayRequest) { optionCalls++ })
	if err != nil {
		t.Fatalf("SendChat: %v", err)
	}
	resp.Body.Close()

	if optionCalls != 1 {
		t.Fatalf("request option executed %d times, want 1", optionCalls)
	}
	if gotMethod != http.MethodPut || gotBody.Model != "md1" || len(gotBody.Messages) != 1 || gotBody.Messages[0].Content != "custom prompt" {
		t.Fatalf("unexpected method or chat payload: method=%q body=%+v", gotMethod, gotBody)
	}
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

	gateway := &credentialEndpoint{
		fakeEndpoint: fakeEndpoint{urls: []string{deadURL + "/v1", live.URL + "/v1"}},
		headers:      []deploy.AuthHeader{{Name: "X-API-Key", Value: "valid-key"}},
	}
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

type credentialEndpoint struct {
	fakeEndpoint
	headers []deploy.AuthHeader
	err     error
}

func (endpoint *credentialEndpoint) authHeaders(context.Context) ([]deploy.AuthHeader, error) {
	return endpoint.headers, endpoint.err
}

func TestGatewayRequestAuthentication(t *testing.T) {
	for _, test := range []struct {
		name    string
		opts    []RequestOption
		err     error
		wantKey string
		wantErr bool
	}{
		{name: "automatic", wantKey: "valid-key"},
		{name: "explicitly unauthenticated", opts: []RequestOption{WithoutAuth()}, err: errors.New("must not fetch credentials")},
		{name: "invalid key replaces default", opts: []RequestOption{WithAuth(deploy.AuthHeader{Name: "X-API-Key", Value: "invalid"})}, wantKey: "invalid"},
		{name: "alternate header replaces default", opts: []RequestOption{WithAuth(deploy.AuthHeader{Name: "Authorization", Value: "Bearer alternate"})}},
		{name: "empty explicit credentials", opts: []RequestOption{WithAuth()}, wantErr: true},
		{name: "empty header value", opts: []RequestOption{WithAuth(deploy.AuthHeader{Name: "X-API-Key"})}, wantErr: true},
		{name: "credential lookup fails", err: errors.New("credential unavailable"), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			endpoint := &credentialEndpoint{
				fakeEndpoint: fakeEndpoint{urls: []string{"http://gateway/v1"}},
				headers: []deploy.AuthHeader{
					{Name: "X-API-Key", Value: "valid-key"},
					{Name: "Authorization", Value: "Bearer valid-key"},
				},
				err: test.err,
			}
			req, err := newGatewayRequest(http.MethodGet, test.opts).request(context.Background(), endpoint, ModelsPath, nil)
			if test.wantErr {
				if err == nil {
					t.Fatal("expected authentication error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := req.Header.Get("X-API-Key"); got != test.wantKey {
				t.Fatalf("X-API-Key = %q, want %q", got, test.wantKey)
			}
			wantAuth := ""
			if test.name == "alternate header replaces default" {
				wantAuth = "Bearer alternate"
			}
			if got := req.Header.Get("Authorization"); got != wantAuth {
				t.Fatalf("Authorization = %q, want %q", got, wantAuth)
			}
		})
	}
}

func TestGatewayRequestRejectsMissingAuthentication(t *testing.T) {
	for _, endpoint := range []deploy.GatewayEndpoint{
		&fakeEndpoint{urls: []string{"http://gateway/v1"}},
		&credentialEndpoint{fakeEndpoint: fakeEndpoint{urls: []string{"http://gateway/v1"}}},
	} {
		if _, err := newGatewayRequest(http.MethodGet, nil).request(context.Background(), endpoint, ModelsPath, nil); err == nil {
			t.Fatal("request without authentication context or credentials must fail")
		}
	}
}

func TestGatewayTrafficAutomaticallyAuthenticates(t *testing.T) {
	var accepted atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-API-Key") != "valid-key" || request.Header.Get("Authorization") != "" || request.Host != "tenant.example.com" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		accepted.Add(1)
	}))
	defer server.Close()
	endpoint := &credentialEndpoint{
		fakeEndpoint: fakeEndpoint{urls: []string{server.URL + "/v1"}, host: "tenant.example.com"},
		headers:      []deploy.AuthHeader{{Name: "X-API-Key", Value: "valid-key"}},
	}
	ctx := context.Background()
	for _, test := range []struct {
		name string
		send func() (*http.Response, error)
	}{
		{name: "chat", send: func() (*http.Response, error) { return SendChat(endpoint, "model") }},
		{name: "models", send: func() (*http.Response, error) { return SendModels(endpoint, ModelsPath) }},
		{name: "raw chat", send: func() (*http.Response, error) {
			return SendGatewayRequest(ctx, endpoint, http.MethodPost, ChatCompletionsPath, []byte(`{"model":"model"}`), WithTimeout(time.Second))
		}},
		{name: "malformed body", send: func() (*http.Response, error) {
			return SendGatewayRequest(ctx, endpoint, http.MethodPost, ChatCompletionsPath, []byte("not JSON"))
		}},
		{name: "origin path", send: func() (*http.Response, error) {
			return SendGatewayRequest(ctx, endpoint, http.MethodGet, "/healthz", nil, WithOriginPath())
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			response, err := test.send()
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", response.StatusCode)
			}
		})
	}
	load := &LoadGenerator{Gateway: endpoint, Model: "model"}
	load.sendOnce(ctx)
	if stats := load.Stats(); stats.Success != 1 {
		t.Fatalf("load generator stats = %+v, want one success", stats)
	}
	session := ReplaySession{Turns: [][]ChatMessage{{{Role: "user", Content: "hello"}}}}
	stats := ReplaySessionsConcurrent(ctx, endpoint, "model", []ReplaySession{session, session}, 2, false)
	if stats.Success != 2 {
		t.Fatalf("replay stats = %+v, want two successes", stats)
	}
	if got := accepted.Load(); got != 8 {
		t.Fatalf("authenticated requests = %d, want 8", got)
	}
}

func TestNegativeAuthRequestsAreNotRetried(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Header.Get("X-API-Key") == "valid-key" {
			writer.WriteHeader(http.StatusOK)
			return
		}
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	endpoint := &credentialEndpoint{
		fakeEndpoint: fakeEndpoint{urls: []string{server.URL + "/v1"}},
		headers:      []deploy.AuthHeader{{Name: "X-API-Key", Value: "valid-key"}},
	}
	for _, option := range []RequestOption{WithoutAuth(), WithAuth(deploy.AuthHeader{Name: "Authorization", Value: "Bearer invalid"})} {
		response, err := SendChat(endpoint, "model", option, WithTransportRetry())
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", response.StatusCode)
		}
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2 without retries", got)
	}
}

func TestSendGatewayRequestPreservesRequest(t *testing.T) {
	for _, test := range []struct {
		name        string
		path        string
		body        string
		contentType string
		wantPath    string
		opts        []RequestOption
	}{
		{name: "missing model", path: ChatCompletionsPath, body: `{"messages":[]}`, contentType: "application/json", wantPath: "/v1/chat/completions"},
		{name: "non-string model", path: ChatCompletionsPath, body: `{"model":42}`, contentType: "application/json", wantPath: "/v1/chat/completions"},
		{name: "non JSON", path: ChatCompletionsPath, body: "not JSON", contentType: "text/plain", wantPath: "/v1/chat/completions", opts: []RequestOption{WithHeader("Content-Type", "text/plain")}},
		{name: "origin", path: "/healthz", wantPath: "/healthz", opts: []RequestOption{WithOriginPath(), WithMethod(http.MethodGet)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Error(err)
				}
				if string(body) != test.body || request.URL.Path != test.wantPath || request.Header.Get("Content-Type") != test.contentType {
					t.Errorf("unexpected request: path=%q body=%q content-type=%q", request.URL.Path, body, request.Header.Get("Content-Type"))
				}
				wantMethod := http.MethodPost
				if test.name == "origin" {
					wantMethod = http.MethodGet
				}
				if request.Method != wantMethod || request.Header.Get("X-API-Key") != "valid-key" {
					t.Error("request must preserve method and authentication")
				}
				writer.WriteHeader(http.StatusBadRequest)
			}))
			defer server.Close()
			gateway := &credentialEndpoint{
				fakeEndpoint: fakeEndpoint{urls: []string{server.URL + "/v1"}},
				headers:      []deploy.AuthHeader{{Name: "X-API-Key", Value: "valid-key"}},
			}
			var body []byte
			if test.body != "" {
				body = []byte(test.body)
			}
			response, err := SendGatewayRequest(context.Background(), gateway, http.MethodPost, test.path, body,
				append(test.opts, WithTransportRetry())...)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusBadRequest || requests.Load() != 1 || gateway.resets != 0 {
				t.Fatal("HTTP errors must be returned unchanged without retries")
			}
		})
	}
}

func TestSendGatewayRequestHonorsContextAndTimeout(t *testing.T) {
	for _, cancelContext := range []bool{false, true} {
		name := "timeout"
		if cancelContext {
			name = "context cancellation"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if cancelContext {
					cancel()
				}
				<-request.Context().Done()
			}))
			defer server.Close()
			gateway := &credentialEndpoint{
				fakeEndpoint: fakeEndpoint{urls: []string{server.URL + "/v1"}},
				headers:      []deploy.AuthHeader{{Name: "X-API-Key", Value: "valid-key"}},
			}
			opts := []RequestOption{WithTimeout(100 * time.Millisecond)}
			if cancelContext {
				opts = []RequestOption{WithTimeout(5 * time.Second), WithTransportRetry()}
			}
			response, err := SendGatewayRequest(ctx, gateway, http.MethodGet, ModelsPath, nil, opts...)
			if response != nil {
				response.Body.Close()
			}
			want := context.DeadlineExceeded
			if cancelContext {
				want = context.Canceled
			}
			if !errors.Is(err, want) || gateway.resets != 0 {
				t.Fatalf("error = %v, want %v without resetting the endpoint", err, want)
			}
		})
	}
}

func TestReplayPreservesFullChatRequest(t *testing.T) {
	turn := []ChatMessage{
		{Role: "user", Content: "weather"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call-1", Type: "function", Function: ToolCallFunction{Name: "weather", Arguments: `{"city":"Sydney"}`}}}},
		{Role: "tool", Content: "sunny", ToolCallID: "call-1"},
	}
	received := make(chan ChatCompletionRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body ChatCompletionRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		received <- body
	}))
	defer server.Close()
	gateway := &credentialEndpoint{
		fakeEndpoint: fakeEndpoint{urls: []string{server.URL + "/v1"}},
		headers:      []deploy.AuthHeader{{Name: "X-API-Key", Value: "valid-key"}},
	}
	stats := ReplaySessionsConcurrent(context.Background(), gateway, "model", []ReplaySession{{Turns: [][]ChatMessage{turn}}}, 1, false)
	if stats.Success != 1 {
		t.Fatalf("replay failed: %+v", stats)
	}
	body := <-received
	if body.Model != "model" || body.MaxTokens != 1 || !reflect.DeepEqual(body.Messages, turn) {
		t.Fatalf("replay changed the full chat request: %+v", body)
	}
}

func TestLoadGeneratorStopCancelsRequests(t *testing.T) {
	for _, test := range []struct {
		name string
		rate float64
	}{{name: "concurrency"}, {name: "rate", rate: 100}} {
		t.Run(test.name, func(t *testing.T) {
			started := make(chan struct{}, 1)
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				_, _ = io.Copy(io.Discard, request.Body)
				started <- struct{}{}
				select {
				case <-request.Context().Done():
				case <-release:
				}
			}))
			defer server.Close()
			defer close(release)
			gateway := &credentialEndpoint{
				fakeEndpoint: fakeEndpoint{urls: []string{server.URL + "/v1"}},
				headers:      []deploy.AuthHeader{{Name: "X-API-Key", Value: "key"}},
			}
			load := &LoadGenerator{Gateway: gateway, Model: "model", Concurrency: 1, Rate: test.rate}
			load.Start(context.Background())
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				load.cancel()
				t.Fatal("request did not reach the server")
			}
			stopped := make(chan struct{})
			go func() { load.Stop(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("Stop did not cancel the in-flight request")
			}
			if gateway.resets != 0 || load.Stats().Total != 1 {
				t.Fatalf("canceled request retried: resets=%d stats=%+v", gateway.resets, load.Stats())
			}
		})
	}
}

type trackedResponseBody struct {
	io.Reader
	closes int
}

func (body *trackedResponseBody) Close() error {
	body.closes++
	return nil
}

func TestResponseParsers(t *testing.T) {
	parsers := map[string]func(*http.Response) error{
		"chat":   func(response *http.Response) error { _, err := ParseChatCompletionResponse(response); return err },
		"error":  func(response *http.Response) error { _, err := ParseErrorResponse(response); return err },
		"models": func(response *http.Response) error { _, err := ParseModelList(response); return err },
		"model":  func(response *http.Response) error { _, err := ParseModel(response); return err },
	}
	readErr := errors.New("broken stream")
	for name, parse := range parsers {
		for _, test := range []struct {
			name      string
			reader    io.Reader
			wantError string
		}{
			{name: "valid", reader: strings.NewReader(`{}`)},
			{name: "invalid", reader: strings.NewReader(`not-json`), wantError: "body: not-json"},
			{name: "read failure", reader: iotest.ErrReader(readErr), wantError: readErr.Error()},
		} {
			t.Run(name+"/"+test.name, func(t *testing.T) {
				body := &trackedResponseBody{Reader: test.reader}
				err := parse(&http.Response{Body: body})
				if body.closes != 1 {
					t.Fatalf("body closed %d times", body.closes)
				}
				if test.wantError == "" {
					if err != nil {
						t.Fatal(err)
					}
				} else if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error=%v, want %q", err, test.wantError)
				}
			})
		}
	}
}

func TestContextRequestHelpers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, send := range map[string]func() (*http.Response, error){
		"chat":   func() (*http.Response, error) { return SendChatContext(ctx, nil, "model") },
		"models": func() (*http.Response, error) { return SendModelsContext(ctx, nil, ModelsPath) },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := send(); !errors.Is(err, context.Canceled) {
				t.Fatalf("expected cancellation before accessing gateway, got %v", err)
			}
		})
	}
}

func TestCheckChatSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusBadGateway)
		_, _ = writer.Write([]byte("epp unavailable"))
	}))
	defer server.Close()
	gateway := &fakeEndpoint{urls: []string{server.URL}}
	err := CheckChatSuccess(context.Background(), gateway, "model", WithoutAuth())
	if err == nil || !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "epp unavailable") {
		t.Fatalf("probe lost response details: %v", err)
	}
}
