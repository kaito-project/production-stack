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

package reason

import (
	"fmt"
	"sort"
	"strings"
)

// suppressionTable encodes §1.4: each active upstream cluster reason
// suppresses the listed downstream reasons that have a strict definitional
// dependency on it. Reasons NOT present here are emitted independently of any
// active cluster reason because they represent local state the reporter can
// evaluate without consulting the cluster layer.
//
// clusterCRDMissing is handled specially (see SuppressedByCRD) because its
// suppression set depends on WHICH CRD is missing.
var suppressionTable = map[Reason][]Reason{
	ClusterIstioControlPlaneNotReady: {
		ModelharnessGatewayClassMissing,
		ModelharnessGatewayProgrammingFailed,
	},
	ClusterGatewayAuthNotReady: {
		ModelharnessExtAuthzProviderMissing,
		ModelharnessAPIKeyReconcileFailed,
	},
	ClusterKaitoControllerNotReady: {
		InferencesetInfraProvisioningFailed,
		InferencesetModelPodsNotReady,
	},
	ClusterNodeProvisionerNotReady: {
		InferencesetInfraProvisioningFailed,
	},
}

// crdSuppression maps a missing CRD resource (group/resource form) to the
// downstream reasons whose detection requires that CRD. Used by
// SuppressedByCRD to implement the clusterCRDMissing row of §1.4.
var crdSuppression = map[string][]Reason{
	"apikeys.kaito.sh": {
		ModelharnessAPIKeyReconcileFailed,
	},
	"inferencesets.kaito.sh": {
		InferencesetInfraProvisioningFailed,
		InferencesetModelPodsNotReady,
		InferencesetEPPNotReady,
		InferencesetRouteNotReady,
		InferencesetWeightDownloadSlow,
	},
	"gateways.gateway.networking.k8s.io": {
		ModelharnessGatewayClassMissing,
		ModelharnessGatewayProgrammingFailed,
	},
	"httproutes.gateway.networking.k8s.io": {
		InferencesetRouteNotReady,
	},
	"inferencepools.inference.networking.k8s.io": {
		InferencesetRouteNotReady,
	},
}

// namespacedSuppressionTable encodes the per-namespace §1.4 rows: an active
// modelharness reason suppresses the listed modeldeployment reasons ONLY within
// the same workload namespace. Unlike the cluster rows above (a single control
// plane is cluster-wide, so its suppression is global), a namespace Gateway is
// per-namespace: when it is not accepted / not programmed, every HTTPRoute
// attached to it in THAT namespace reports Accepted=False (NoMatchingParent),
// so the redundant per-InferenceSet route errors in that namespace are gated
// while the Gateway problem is surfaced once for the namespace.
var namespacedSuppressionTable = map[Reason][]Reason{
	ModelharnessGatewayClassMissing: {
		InferencesetRouteNotReady,
	},
	ModelharnessGatewayProgrammingFailed: {
		InferencesetRouteNotReady,
	},
	ModelharnessGatewayDataPlaneNotReady: {
		InferencesetRouteNotReady,
	},
}

// SuppressedBy returns the set of downstream reasons that an active upstream
// cluster reason suppresses (§1.4), for the non-CRD rows of the suppression
// table. Returns nil for reasons not present in the table (e.g.
// clusterBBRNotReady, clusterKedaNotReady never suppress anything).
func SuppressedBy(upstream Reason) []Reason {
	return suppressionTable[upstream]
}

// SuppressedByCRD returns the downstream reasons suppressed because the named
// CRD (in group/resource form, e.g. "inferencesets.kaito.sh") is missing.
func SuppressedByCRD(crdName string) []Reason {
	return crdSuppression[crdName]
}

// SuppressedWithinNamespaceBy returns the downstream reasons that an active
// modelharness reason suppresses within its OWN workload namespace (§1.4). The
// caller must scope the gating to the namespace of the active upstream finding.
func SuppressedWithinNamespaceBy(upstream Reason) []Reason {
	return namespacedSuppressionTable[upstream]
}

// HasSuppressionRow reports whether the upstream cluster reason participates
// in cross-layer suppression at all (either via the static table or, for
// clusterCRDMissing, via the per-CRD map). Only such reasons may ever carry
// the §1.4 transparency suffix.
func HasSuppressionRow(upstream Reason) bool {
	if upstream == ClusterCRDMissing {
		return true
	}
	_, ok := suppressionTable[upstream]
	return ok
}

// TransparencySuffix builds the deterministic §1.4 suffix appended to an
// upstream cluster Warning message when it is actively suppressing at least
// one downstream reason. The downstream reason names are sorted
// lexicographically so the suffix is stable across reconciles. nsCount is the
// number of distinct namespaces in which suppression is in effect.
//
// Returns the empty string when nothing is suppressed (the suffix MUST only
// appear in genuine cross-layer-dependency cases).
func TransparencySuffix(suppressed map[Reason]bool, nsCount int) string {
	if len(suppressed) == 0 || nsCount == 0 {
		return ""
	}
	names := make([]string, 0, len(suppressed))
	for r := range suppressed {
		names = append(names, string(r))
	}
	sort.Strings(names)
	return fmt.Sprintf(" (suppressing downstream reasons: %s in %d namespace(s))",
		strings.Join(names, ", "), nsCount)
}
