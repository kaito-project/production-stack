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
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
)

const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 15 * time.Second
	idleTimeout       = 60 * time.Second
	maxHeaderBytes    = 1 << 16

	shutdownTimeout = 10 * time.Second

	// crdWaitTimeout / crdWaitInterval bound the wait for the KAITO CRDs, which
	// may land moments after this chart. Past the timeout the process exits so
	// the kubelet's restart backoff owns the retry.
	crdWaitTimeout  = time.Minute
	crdWaitInterval = 5 * time.Second
)

// SetupWithManager registers the model discovery server on mgr.
//
// The informers are requested here, before mgr.Start, for two reasons: the
// cache is lazy and would otherwise build them inside the first request, and
// registering up front puts them in the manager's cache-sync phase — which
// completes before any non-leader-election Runnable starts. The server can
// therefore serve from a fully populated cache on its very first request.
func SetupWithManager(mgr ctrl.Manager, addr string, log logr.Logger) error {
	ctx := context.Background()

	if _, err := mgr.GetCache().GetInformer(ctx, &corev1.Namespace{},
		cache.BlockUntilSynced(false)); err != nil {
		return fmt.Errorf("watch Namespaces for the models API: %w", err)
	}

	if err := watchInferenceSets(ctx, mgr.GetCache(), log); err != nil {
		return fmt.Errorf("watch InferenceSets for the models API: %w", err)
	}

	return mgr.Add(NewServer(mgr.GetCache(), addr, log))
}

// watchInferenceSets registers the InferenceSet informer, retrying for
// crdWaitTimeout while the CRD is merely absent so a KAITO install that lands
// just after this one is not a startup failure. A CRD still missing past the
// timeout is fatal on purpose: exiting hands the retry to the kubelet's restart
// backoff instead of keeping a retry loop alive inside the process.
func watchInferenceSets(ctx context.Context, c cache.Cache, log logr.Logger) error {
	inferenceSets := &unstructured.Unstructured{}
	inferenceSets.SetGroupVersionKind(inferenceSetGVK)

	var lastErr error
	err := wait.PollUntilContextTimeout(ctx, crdWaitInterval, crdWaitTimeout, true,
		func(ctx context.Context) (bool, error) {
			_, lastErr = c.GetInformer(ctx, inferenceSets, cache.BlockUntilSynced(false))
			switch {
			case lastErr == nil:
				return true, nil
			case meta.IsNoMatchError(lastErr):
				log.Info("waiting for the InferenceSet CRD to be installed",
					"groupVersionKind", inferenceSetGVK.String())
				return false, nil
			default:
				return false, lastErr
			}
		})
	if err != nil {
		return lastErr
	}
	return nil
}

// Server exposes the model discovery endpoints as a manager Runnable.
type Server struct {
	addr  string
	cache cache.Cache
	log   logr.Logger
}

// NewServer builds the model discovery server. Queries are served from c, an
// informer-backed cache, so no request ever reaches the API server.
func NewServer(c cache.Cache, addr string, log logr.Logger) *Server {
	return &Server{addr: addr, cache: c, log: log}
}

// NeedLeaderElection reports false so every replica serves the endpoint. Unlike
// the event reporter, this is a read-only query surface fronted by a Service:
// gating it on leadership would leave non-leader pods as black-hole endpoints.
func (s *Server) NeedLeaderElection() bool { return false }

// Start serves until ctx is cancelled. It satisfies manager.Runnable.
func (s *Server) Start(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.addr,
		Handler:           NewHandler(NewLister(s.cache), s.log),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}

	serveErr := make(chan error, 1)
	go func() {
		s.log.Info("starting models API", "address", s.addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
