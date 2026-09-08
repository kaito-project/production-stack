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

package modelsapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator/util"
)

const (
	testNamespace  = "team-a"
	otherNamespace = "team-b"
)

var createdAt = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	s.AddKnownTypeWithName(inferenceSetGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(inferenceSetListGVK, &unstructured.UnstructuredList{})
	return s
}

func inferenceSet(namespace, name string, created time.Time) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(inferenceSetGVK)
	u.SetNamespace(namespace)
	u.SetName(name)
	u.SetCreationTimestamp(metav1.NewTime(created))
	return u
}

func namespaceObj(name string, managed bool) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if managed {
		ns.Labels = map[string]string{util.ManagedByLabel: util.ManagedByValue}
	}
	return ns
}

func newHandler(t *testing.T, objs ...client.Object) *Handler {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).Build()
	return NewHandler(NewLister(c), logr.Discard())
}

// defaultHandler serves two models in testNamespace plus an unrelated model in
// another managed namespace, which must never leak across.
func defaultHandler(t *testing.T) *Handler {
	t.Helper()
	return newHandler(t,
		namespaceObj(testNamespace, true),
		namespaceObj(otherNamespace, true),
		inferenceSet(testNamespace, "chat-phi", createdAt),
		inferenceSet(testNamespace, "code-ministral", createdAt.Add(time.Hour)),
		inferenceSet(otherNamespace, "secret-model", createdAt),
	)
}

func do(h *Handler, method, path, namespace string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if namespace != "" {
		req.Header.Set(NamespaceHeader, namespace)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeList(t *testing.T, rec *httptest.ResponseRecorder) ModelList {
	t.Helper()
	var got ModelList
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode list: %v (body=%s)", err, rec.Body.String())
	}
	return got
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) errorBody {
	t.Helper()
	var got errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode error: %v (body=%s)", err, rec.Body.String())
	}
	return got
}

func TestListReturnsEveryRegisteredModelSortedByID(t *testing.T) {
	rec := do(defaultHandler(t), http.MethodGet, ListPath, testNamespace)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	got := decodeList(t, rec)
	if got.Object != "list" {
		t.Errorf("object = %q, want list", got.Object)
	}
	if len(got.Data) != 2 {
		t.Fatalf("got %d models, want 2: %+v", len(got.Data), got.Data)
	}
	if got.Data[0].ID != "chat-phi" || got.Data[1].ID != "code-ministral" {
		t.Errorf("models not sorted by id: %+v", got.Data)
	}
	first := got.Data[0]
	if first.Object != "model" || first.OwnedBy != "kaito" {
		t.Errorf("unexpected object/owned_by: %+v", first)
	}
	if first.Created != createdAt.Unix() {
		t.Errorf("created = %d, want %d", first.Created, createdAt.Unix())
	}
}

// A model that is not serving yet must still be listed: the endpoint is a
// registry of what is routable by name, not a health check.
func TestListIncludesModelsWithoutReadyEndpoints(t *testing.T) {
	h := newHandler(t,
		namespaceObj(testNamespace, true),
		inferenceSet(testNamespace, "cold-model", createdAt),
	)
	rec := do(h, http.MethodGet, ListPath, testNamespace)

	got := decodeList(t, rec)
	if len(got.Data) != 1 || got.Data[0].ID != "cold-model" {
		t.Fatalf("got %+v, want the not-yet-ready model to be listed", got.Data)
	}
}

func TestListEmptyNamespaceReturnsEmptyArrayNotNull(t *testing.T) {
	h := newHandler(t, namespaceObj(testNamespace, true))
	rec := do(h, http.MethodGet, ListPath, testNamespace)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := strings.TrimSpace(rec.Body.String()); !strings.Contains(body, `"data":[]`) {
		t.Errorf("body = %s, want an empty JSON array for data", body)
	}
}

func TestRetrieveReturnsBareObjectMatchingTheListEntry(t *testing.T) {
	h := defaultHandler(t)

	listed := decodeList(t, do(h, http.MethodGet, ListPath, testNamespace)).Data[0]

	rec := do(h, http.MethodGet, ListPath+"/chat-phi", testNamespace)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"data"`) {
		t.Errorf("retrieve must not wrap the model in a list envelope: %s", rec.Body.String())
	}

	var got Model
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode model: %v", err)
	}
	if got != listed {
		t.Errorf("retrieve = %+v, list entry = %+v; they must agree", got, listed)
	}
}

func TestRetrieveUnknownModelReturnsModelNotFound(t *testing.T) {
	rec := do(defaultHandler(t), http.MethodGet, ListPath+"/nope", testNamespace)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if src := rec.Header().Get(errorSourceHeader); src != errorSourceValue {
		t.Errorf("%s = %q, want %q", errorSourceHeader, src, errorSourceValue)
	}
	got := decodeError(t, rec)
	if got.Error.Code != "model_not_found" || got.Error.Type != "invalid_request_error" {
		t.Errorf("unexpected error envelope: %+v", got.Error)
	}
	if got.Error.Param == nil || *got.Error.Param != "model" {
		t.Errorf("param = %v, want \"model\"", got.Error.Param)
	}
}

// Namespace isolation is the security boundary of this endpoint: a model in
// another namespace must be indistinguishable from one that does not exist.
func TestRetrieveCannotReachAnotherNamespace(t *testing.T) {
	rec := do(defaultHandler(t), http.MethodGet, ListPath+"/secret-model", testNamespace)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a model owned by another namespace", rec.Code)
	}
	if decodeError(t, rec).Error.Code != "model_not_found" {
		t.Errorf("cross-namespace lookup must not reveal a distinct error code")
	}
}

func TestListIsScopedToTheInjectedNamespace(t *testing.T) {
	rec := do(defaultHandler(t), http.MethodGet, ListPath, otherNamespace)

	got := decodeList(t, rec)
	if len(got.Data) != 1 || got.Data[0].ID != "secret-model" {
		t.Fatalf("got %+v, want only the models of %s", got.Data, otherNamespace)
	}
}

func TestRequestErrors(t *testing.T) {
	tests := []struct {
		name      string
		method    string
		path      string
		namespace string
		status    int
		code      string
	}{
		{"empty model id", http.MethodGet, ListPath + "/", testNamespace, http.StatusBadRequest, "invalid_model_id"},
		{"missing namespace header", http.MethodGet, ListPath, "", http.StatusBadRequest, "invalid_gateway_namespace"},
		{"malformed namespace header", http.MethodGet, ListPath, "../kube-system", http.StatusBadRequest, "invalid_gateway_namespace"},
		{"unmanaged namespace", http.MethodGet, ListPath, "kube-system", http.StatusNotFound, "namespace_not_found"},
		{"unknown namespace", http.MethodGet, ListPath, "ghost", http.StatusNotFound, "namespace_not_found"},
		{"sibling path is not swallowed", http.MethodGet, "/v1/modelsfoo", testNamespace, http.StatusNotFound, "unknown_url"},
		{"post to list", http.MethodPost, ListPath, testNamespace, http.StatusMethodNotAllowed, "method_not_allowed"},
		{"delete to retrieve", http.MethodDelete, ListPath + "/chat-phi", testNamespace, http.StatusMethodNotAllowed, "method_not_allowed"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHandler(t,
				namespaceObj(testNamespace, true),
				namespaceObj("kube-system", false),
				inferenceSet(testNamespace, "chat-phi", createdAt),
			)
			rec := do(h, tc.method, tc.path, tc.namespace)

			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.status, rec.Body.String())
			}
			if got := decodeError(t, rec).Error.Code; got != tc.code {
				t.Errorf("error code = %q, want %q", got, tc.code)
			}
		})
	}
}

func TestMethodNotAllowedAdvertisesGet(t *testing.T) {
	rec := do(defaultHandler(t), http.MethodPost, ListPath, testNamespace)

	if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
		t.Errorf("Allow = %q, want GET", allow)
	}
}

// Every replica must serve the endpoint: it sits behind a Service, so a
// leader-only runnable would turn non-leader pods into black-hole endpoints.
func TestServerDoesNotRequireLeaderElection(t *testing.T) {
	if (&Server{}).NeedLeaderElection() {
		t.Fatal("models API must run on every replica")
	}
}

// A failing cache read must surface as a retryable 503 rather than a 500, so
// clients back off instead of treating it as a permanent error.
func TestCacheReadFailureIsRetryable(t *testing.T) {
	base := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(namespaceObj(testNamespace, true)).Build()
	unreadable := listErrReader{
		Reader: base,
		err:    &meta.NoKindMatchError{GroupKind: inferenceSetGVK.GroupKind()},
	}
	h := NewHandler(NewLister(unreadable), logr.Discard())

	rec := do(h, http.MethodGet, ListPath, testNamespace)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := decodeError(t, rec).Error.Code; got != "models_api_unavailable" {
		t.Errorf("error code = %q, want models_api_unavailable", got)
	}
}

// listErrReader fails every List. The fake client cannot express this: it
// returns an empty list for a type its scheme does not know.
type listErrReader struct {
	client.Reader
	err error
}

func (r listErrReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return r.err
}
