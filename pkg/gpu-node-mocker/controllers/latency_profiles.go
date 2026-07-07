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

package controllers

import (
	"regexp"
	"strconv"
	"strings"
)

// latencyProfile bundles the full set of llm-d-inference-sim latency knobs for
// both calculators. Each profile mirrors one of the "Suggested Default
// Profiles" in llm-d-inference-sim/docs/latency-profiles.md, so mocked
// endpoints behave like a specific model/hardware combination.
type latencyProfile struct {
	// Common to both calculators.
	InterTokenLatency       string
	InterTokenLatencyStdDev string
	TimeFactorUnderLoad     string

	// constant calculator.
	TimeToFirstToken       string
	TimeToFirstTokenStdDev string
	KVCacheTransferLatency string
	KVCacheTransferStdDev  string

	// per-token calculator.
	PrefillOverhead             string
	PrefillTimePerToken         string
	PrefillTimeStdDev           string
	KVCacheTransferTimePerToken string
	KVCacheTransferTimeStdDev   string
}

const (
	// LatencyProfileAuto selects a profile automatically from the model size
	// parsed out of the served model name (e.g. "...-8B..." ⇒ 8 billion
	// parameters). It is the default.
	LatencyProfileAuto = "auto"

	// LatencyProfile* are the named profiles from latency-profiles.md and its
	// reference tables. Set one explicitly to force it regardless of model
	// size. Buckets are keyed by parameter count: small (1–3B), 8B (7–8B, the
	// fallback), 13B, 30B (TP=2), 70B (TP=8), and 405B (TP=8).
	LatencyProfileSmallL40S = "small-l40s"
	LatencyProfile8BH100    = "8b-h100"
	LatencyProfile13B       = "13b"
	LatencyProfile30BTP2    = "30b-tp2"
	LatencyProfile70BTP8    = "70b-tp8"
	LatencyProfile405BTP8   = "405b-tp8"

	// DefaultLatencyProfile is the profile-selection mode used when none is set.
	DefaultLatencyProfile = LatencyProfileAuto
)

// profile8BH100 mirrors "Profile 1: 8B-class model on H100, balanced load". It
// is also the fallback for models whose size cannot be parsed.
var profile8BH100 = latencyProfile{
	InterTokenLatency:           "12ms",
	InterTokenLatencyStdDev:     "2ms",
	TimeFactorUnderLoad:         "2.0",
	TimeToFirstToken:            "100ms",
	TimeToFirstTokenStdDev:      "20ms",
	KVCacheTransferLatency:      "2ms",
	KVCacheTransferStdDev:       "400us",
	PrefillOverhead:             "30ms",
	PrefillTimePerToken:         "250us",
	PrefillTimeStdDev:           "50us",
	KVCacheTransferTimePerToken: "3us",
	KVCacheTransferTimeStdDev:   "600ns",
}

// profile13B mirrors the 13B reference row (H100-class, balanced load).
var profile13B = latencyProfile{
	InterTokenLatency:           "22ms",
	InterTokenLatencyStdDev:     "3ms",
	TimeFactorUnderLoad:         "2.2",
	TimeToFirstToken:            "180ms",
	TimeToFirstTokenStdDev:      "30ms",
	KVCacheTransferLatency:      "2ms",
	KVCacheTransferStdDev:       "400us",
	PrefillOverhead:             "45ms",
	PrefillTimePerToken:         "320us",
	PrefillTimeStdDev:           "64us",
	KVCacheTransferTimePerToken: "4us",
	KVCacheTransferTimeStdDev:   "800ns",
}

// profile30BTP2 mirrors the 30–34B (TP=2) reference row.
var profile30BTP2 = latencyProfile{
	InterTokenLatency:           "30ms",
	InterTokenLatencyStdDev:     "5ms",
	TimeFactorUnderLoad:         "2.5",
	TimeToFirstToken:            "250ms",
	TimeToFirstTokenStdDev:      "50ms",
	KVCacheTransferLatency:      "2ms",
	KVCacheTransferStdDev:       "400us",
	PrefillOverhead:             "60ms",
	PrefillTimePerToken:         "400us",
	PrefillTimeStdDev:           "80us",
	KVCacheTransferTimePerToken: "6us",
	KVCacheTransferTimeStdDev:   "1200ns",
}

// profile70BTP8 mirrors "Profile 2: 70B model on 8×H100 (TP=8),
// throughput-optimized".
var profile70BTP8 = latencyProfile{
	InterTokenLatency:           "25ms",
	InterTokenLatencyStdDev:     "4ms",
	TimeFactorUnderLoad:         "3.0",
	TimeToFirstToken:            "200ms",
	TimeToFirstTokenStdDev:      "40ms",
	KVCacheTransferLatency:      "2ms",
	KVCacheTransferStdDev:       "400us",
	PrefillOverhead:             "80ms",
	PrefillTimePerToken:         "500us",
	PrefillTimeStdDev:           "100us",
	KVCacheTransferTimePerToken: "8us",
	KVCacheTransferTimeStdDev:   "1600ns",
}

// profile405BTP8 mirrors the 405B (TP=8) reference row.
var profile405BTP8 = latencyProfile{
	InterTokenLatency:           "80ms",
	InterTokenLatencyStdDev:     "12ms",
	TimeFactorUnderLoad:         "3.5",
	TimeToFirstToken:            "900ms",
	TimeToFirstTokenStdDev:      "150ms",
	KVCacheTransferLatency:      "3ms",
	KVCacheTransferStdDev:       "600us",
	PrefillOverhead:             "200ms",
	PrefillTimePerToken:         "1200us",
	PrefillTimeStdDev:           "240us",
	KVCacheTransferTimePerToken: "20us",
	KVCacheTransferTimeStdDev:   "4us",
}

// profileSmallL40S mirrors "Profile 3: Small model (1–3B) on L40S,
// low-latency edge".
var profileSmallL40S = latencyProfile{
	InterTokenLatency:           "15ms",
	InterTokenLatencyStdDev:     "2ms",
	TimeFactorUnderLoad:         "1.5",
	TimeToFirstToken:            "110ms",
	TimeToFirstTokenStdDev:      "15ms",
	KVCacheTransferLatency:      "5ms",
	KVCacheTransferStdDev:       "1ms",
	PrefillOverhead:             "20ms",
	PrefillTimePerToken:         "350us",
	PrefillTimeStdDev:           "70us",
	KVCacheTransferTimePerToken: "12us",
	KVCacheTransferTimeStdDev:   "2400ns",
}

// Model-size bucket boundaries (in billions of parameters) used by auto
// selection. A model with a parsed size falls into the first bucket whose upper
// bound it is below; models at/above the largest bound use the 405B profile.
// Models whose size cannot be parsed fall back to the 8B profile.
const (
	modelSizeSmallMaxB = 5.0   // < 5B      ⇒ small-l40s (1–3B)
	modelSize8BMaxB    = 10.0  // 5–10B     ⇒ 8b-h100 (7–8B)
	modelSize13BMaxB   = 20.0  // 10–20B    ⇒ 13b
	modelSize30BMaxB   = 50.0  // 20–50B    ⇒ 30b-tp2
	modelSize70BMaxB   = 150.0 // 50–150B   ⇒ 70b-tp8; ≥150B ⇒ 405b-tp8
)

// modelSizeRe matches a parameter count immediately followed by a "b"/"B"
// suffix inside a model name, e.g. "8B", "0.5b", "70B", "405B".
var modelSizeRe = regexp.MustCompile(`(\d+(?:\.\d+)?)[bB]`)

// parseModelSizeB extracts the parameter count in billions from a model name.
// It returns the largest "<number>B" token found (so "Llama-3.1-8B" ⇒ 8) and
// false when no size token is present.
func parseModelSizeB(modelName string) (float64, bool) {
	matches := modelSizeRe.FindAllStringSubmatch(modelName, -1)
	if len(matches) == 0 {
		return 0, false
	}
	var maxB float64
	var found bool
	for _, m := range matches {
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			continue
		}
		if !found || v > maxB {
			maxB, found = v, true
		}
	}
	return maxB, found
}

// profileForModelSize maps a parameter count (billions) to a profile.
func profileForModelSize(sizeB float64) latencyProfile {
	switch {
	case sizeB < modelSizeSmallMaxB:
		return profileSmallL40S
	case sizeB < modelSize8BMaxB:
		return profile8BH100
	case sizeB < modelSize13BMaxB:
		return profile13B
	case sizeB < modelSize30BMaxB:
		return profile30BTP2
	case sizeB < modelSize70BMaxB:
		return profile70BTP8
	default:
		return profile405BTP8
	}
}

// selectLatencyProfile resolves the baseline latency profile for a shadow pod.
// The profile mode ("auto" or an explicit name) picks the baseline; individual
// non-empty Config knobs still override the corresponding profile value in
// ensureSimConfigMap. Unrecognized modes and models without a parseable size
// fall back to the balanced 8B profile (the historical default).
func selectLatencyProfile(mode, modelName string) latencyProfile {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case LatencyProfileSmallL40S:
		return profileSmallL40S
	case LatencyProfile8BH100:
		return profile8BH100
	case LatencyProfile13B:
		return profile13B
	case LatencyProfile30BTP2:
		return profile30BTP2
	case LatencyProfile70BTP8:
		return profile70BTP8
	case LatencyProfile405BTP8:
		return profile405BTP8
	default: // auto / empty / unknown
		if sizeB, ok := parseModelSizeB(modelName); ok {
			return profileForModelSize(sizeB)
		}
		return profile8BH100
	}
}

// resolveLatencyCalculator picks the simulator latency model. A per-deployment
// annotation value takes precedence, then the operator-wide Config value, and
// finally DefaultLatencyCalculator ("per-token"). The second return value is
// true when the annotation was non-empty but unrecognized, so the caller can
// log a warning; in that case the resolved value falls back to the Config /
// default value rather than silently disabling serving.
func resolveLatencyCalculator(annotation, configValue string) (calculator string, annotationInvalid bool) {
	switch strings.ToLower(strings.TrimSpace(annotation)) {
	case LatencyCalculatorConstant:
		return LatencyCalculatorConstant, false
	case LatencyCalculatorPerToken:
		return LatencyCalculatorPerToken, false
	case "":
		// Absent/empty annotation: fall through to the Config/default chain.
	default:
		annotationInvalid = true
	}
	if configValue == LatencyCalculatorConstant || configValue == LatencyCalculatorPerToken {
		return configValue, annotationInvalid
	}
	return DefaultLatencyCalculator, annotationInvalid
}
