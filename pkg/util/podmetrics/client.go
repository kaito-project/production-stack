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

// Package podmetrics scrapes Prometheus metrics from workload pods: it
// discovers pods by label selector, reads their /metrics endpoint through the
// API-server proxy, and parses the text-format response into a queryable
// snapshot. It is workload-agnostic — callers pick which pods to scrape and
// interpret the resulting metrics themselves.
package podmetrics

import (
	"context"
	"fmt"
	"time"

	"k8s.io/client-go/kubernetes"
)

// Client discovers source pods by label selector and scrapes their Prometheus
// /metrics endpoint through the API-server proxy. It is the shared cluster
// access for pod-based scrapers, so the generic mechanics of "find the pods,
// scrape them, build a snapshot" live in one place and callers own only their
// workload-specific pod selection and metric interpretation.
type Client struct {
	clientset kubernetes.Interface
	// port is the pod port serving the Prometheus /metrics endpoint. It is fixed
	// for a Client's lifetime, so it is configured once here rather than passed
	// on every scrape.
	port int

	// ScrapeRawFunc and NowFunc override the clientset-backed cluster access and
	// the wall clock. They are test seams: nil in production, in which case the
	// clientset-backed implementation and time.Now are used.
	ScrapeRawFunc func(ctx context.Context, namespace, pod string, port int) (string, error)
	NowFunc       func() time.Time
}

// NewClient returns a Client backed by clientset that scrapes /metrics on port.
func NewClient(clientset kubernetes.Interface, port int) *Client {
	return &Client{clientset: clientset, port: port}
}

// Snapshot scrapes each pod's /metrics endpoint and merges every metric into a
// single Snapshot — tagging each sample with the pod it came from. Callers pass
// the pod names to scrape, already filtered to the ones they care about (e.g.
// only still-starting pods). found is false when pods is empty or none served
// parseable metrics (e.g. the workload is starting or already gone). The
// returned error wraps the first scrape failure and is left for the caller to
// annotate with context.
func (c *Client) Snapshot(ctx context.Context, namespace string, pods []string) (Snapshot, bool, error) {
	if len(pods) == 0 {
		return Snapshot{}, false, nil
	}

	snap := Snapshot{
		ScrapedAt: c.now(),
		Metrics:   make(map[string][]Sample),
	}
	var firstErr error
	scraped := false
	for _, pod := range pods {
		text, err := c.scrapeRaw(ctx, namespace, pod)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		families, err := parseMetricFamilies(text)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		scraped = true
		for name, samples := range families {
			for i := range samples {
				samples[i].SourcePod = pod
			}
			snap.Metrics[name] = append(snap.Metrics[name], samples...)
		}
	}
	if !scraped {
		// Pods exist but none served parseable metrics (e.g. download finished).
		return Snapshot{}, false, firstErr
	}
	return snap, true, nil
}

// now returns the current time via NowFunc when installed (test seam) or the
// real wall clock otherwise.
func (c *Client) now() time.Time {
	if c.NowFunc != nil {
		return c.NowFunc()
	}
	return time.Now()
}

// scrapeRaw fetches the raw Prometheus text from pod:port/metrics through the
// API-server proxy (read-only, no direct pod network access required), via the
// clientset unless a test seam is installed.
func (c *Client) scrapeRaw(ctx context.Context, namespace, pod string) (string, error) {
	if c.ScrapeRawFunc != nil {
		return c.ScrapeRawFunc(ctx, namespace, pod, c.port)
	}
	raw, err := c.clientset.CoreV1().RESTClient().Get().
		Namespace(namespace).
		Resource("pods").
		SubResource("proxy").
		Name(fmt.Sprintf("%s:%d", pod, c.port)).
		Suffix("metrics").
		Do(ctx).
		Raw()
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
