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

package config

import (
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("default config must be valid: %v", err)
	}
	cases := map[string]func(*Config){
		"refresh not below half the event TTL": func(c *Config) { c.EventRefreshInterval = 30 * time.Minute },
		"zero refresh":                         func(c *Config) { c.EventRefreshInterval = 0 },
		"zero window":                          func(c *Config) { c.WeightDownload.WindowDuration = 0 },
		"zero threshold":                       func(c *Config) { c.WeightDownload.MinMBps = 0 },
		"zero resync":                          func(c *Config) { c.ResyncInterval = 0 },
	}
	for name, mutate := range cases {
		c := DefaultConfig()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: want a validation error", name)
		}
	}
}
