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

package scenarios

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
)

// -- fakes shared by this package's unit tests. Ordinary Go (stdlib
// "testing" + these fakes) — no network calls, no live cluster, no
// Ginkgo/Gomega. --

// fakeLogger records every Logf call for later inspection instead of
// writing anywhere.
type fakeLogger struct {
	mu    sync.Mutex
	lines []string
}

func (f *fakeLogger) Logf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines = append(f.lines, fmt.Sprintf(format, args...))
}

func (f *fakeLogger) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.lines))
	copy(out, f.lines)
	return out
}

// fakeLifecycle is an in-memory Lifecycle implementation that records the
// order of calls it receives, so tests can assert on both behavior
// (namespace/deployment names actually used) and ordering (setup and
// cleanup sequencing).
type fakeLifecycle struct {
	mu    sync.Mutex
	calls []string

	access map[string]NamespaceAccess

	ensureNamespaceErr  error
	ensureDeploymentErr error
	deleteNamespaceErr  error
	deleteDeploymentErr error
	namespaceAccessErr  error
}

func newFakeLifecycle() *fakeLifecycle {
	return &fakeLifecycle{access: map[string]NamespaceAccess{}}
}

func (f *fakeLifecycle) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeLifecycle) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeLifecycle) EnsureNamespace(_ context.Context, namespace string, authEnabled bool) error {
	f.record(fmt.Sprintf("EnsureNamespace(%s,auth=%v)", namespace, authEnabled))
	return f.ensureNamespaceErr
}

func (f *fakeLifecycle) DeleteNamespace(_ context.Context, namespace string) error {
	f.record(fmt.Sprintf("DeleteNamespace(%s)", namespace))
	return f.deleteNamespaceErr
}

func (f *fakeLifecycle) EnsureModelDeployment(_ context.Context, spec DeploymentSpec) error {
	f.record(fmt.Sprintf("EnsureModelDeployment(%s/%s)", spec.Namespace, spec.Name))
	return f.ensureDeploymentErr
}

func (f *fakeLifecycle) DeleteModelDeployment(_ context.Context, namespace, name string) error {
	f.record(fmt.Sprintf("DeleteModelDeployment(%s/%s)", namespace, name))
	return f.deleteDeploymentErr
}

func (f *fakeLifecycle) NamespaceAccess(_ context.Context, namespace string) (NamespaceAccess, error) {
	f.record(fmt.Sprintf("NamespaceAccess(%s)", namespace))
	if f.namespaceAccessErr != nil {
		return NamespaceAccess{}, f.namespaceAccessErr
	}
	if a, ok := f.access[namespace]; ok {
		return a, nil
	}
	return NamespaceAccess{GatewayURL: "http://fake-gateway/" + namespace}, nil
}

// fakeChatRule is one canned response (or error) fakeChatClient returns,
// consumed in order per call.
type fakeChatRule struct {
	status int
	body   string
	header map[string]string
	err    error
}

// fakeChatClient returns canned responses/errors from an ordered queue
// (or, once the queue is empty, repeats a default rule) and records every
// request it was asked to send.
type fakeChatClient struct {
	mu       sync.Mutex
	rules    []fakeChatRule
	def      fakeChatRule
	requests []ChatRequest
}

func (f *fakeChatClient) SendChat(_ context.Context, _ string, req ChatRequest) (*ChatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)

	rule := f.def
	if len(f.rules) > 0 {
		rule = f.rules[0]
		f.rules = f.rules[1:]
	}
	if rule.err != nil {
		return nil, rule.err
	}
	hdr := make(http.Header, len(rule.header))
	for k, v := range rule.header {
		hdr.Set(k, v)
	}
	return &ChatResponse{StatusCode: rule.status, Header: hdr, Body: []byte(rule.body)}, nil
}

func (f *fakeChatClient) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeChatClient) lastRequest() ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

// fakeKubeOps is a minimal, in-memory KubeOps implementation.
type fakeKubeOps struct {
	mu sync.Mutex

	deployments   map[string]*appsv1.Deployment // key: namespace/name
	readyReplicas map[string]int32

	podLogs             map[string]string // key: namespace/pod
	aggregatePodLogLen  int
	firstRunningPodName string
	firstRunningPodErr  error
	execOutput          string
	execErr             error
	successSnapshot     MetricSnapshot
	eppPods             []string

	scaleCalls []string
}

func newFakeKubeOps() *fakeKubeOps {
	return &fakeKubeOps{
		deployments:   map[string]*appsv1.Deployment{},
		readyReplicas: map[string]int32{},
		podLogs:       map[string]string{},
	}
}

func key(namespace, name string) string { return namespace + "/" + name }

func (f *fakeKubeOps) GetDeployment(_ context.Context, namespace, name string) (*appsv1.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.deployments[key(namespace, name)]
	if !ok {
		return nil, fmt.Errorf("deployment %s/%s not found", namespace, name)
	}
	return d, nil
}

func (f *fakeKubeOps) ScaleDeployment(_ context.Context, namespace, name string, replicas int32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scaleCalls = append(f.scaleCalls, fmt.Sprintf("%s/%s=%d", namespace, name, replicas))
	f.readyReplicas[key(namespace, name)] = replicas
	return nil
}

// lastScaleCall returns the most recent "namespace/name=replicas" record
// left by ScaleDeployment, or "" if it was never called.
func (f *fakeKubeOps) lastScaleCall() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.scaleCalls) == 0 {
		return ""
	}
	return f.scaleCalls[len(f.scaleCalls)-1]
}

func (f *fakeKubeOps) ReadyReplicas(_ context.Context, namespace, name string) (int32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readyReplicas[key(namespace, name)], nil
}

func (f *fakeKubeOps) WaitForReadyReplicas(ctx context.Context, namespace, name string, want int32, _ time.Duration) error {
	ready, err := f.ReadyReplicas(ctx, namespace, name)
	if err != nil {
		return err
	}
	if ready < want {
		return fmt.Errorf("only %d ready, want %d", ready, want)
	}
	return nil
}

func (f *fakeKubeOps) FirstRunningPod(context.Context, string, string) (string, error) {
	return f.firstRunningPodName, f.firstRunningPodErr
}

func (f *fakeKubeOps) PodLogs(_ context.Context, namespace, pod, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.podLogs[key(namespace, pod)], nil
}

func (f *fakeKubeOps) AggregatePodSetLogLen(context.Context, string, string, string) (int, error) {
	return f.aggregatePodLogLen, nil
}

func (f *fakeKubeOps) ExecInPod(context.Context, string, string, ...string) (string, error) {
	return f.execOutput, f.execErr
}

func (f *fakeKubeOps) RequestSuccessTotalSnapshot(context.Context, string, string) (MetricSnapshot, error) {
	return f.successSnapshot, nil
}

func (f *fakeKubeOps) EPPPodNames(context.Context, string, string) ([]string, error) {
	return f.eppPods, nil
}
