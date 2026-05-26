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

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

const (
	probeTimeout = 10 * time.Second

	// probeImage is the agnhost image used by the upstream Kubernetes
	// NetworkPolicy conformance suite. We use it instead of busybox
	// because its `connect` subcommand returns documented, stable exit
	// codes that distinguish "blocked" (silent drop / timeout, exit 1)
	// from "refused" (TCP RST, exit 2) — letting the deny tests assert
	// the *right* failure mode rather than treating any non-zero exit
	// as success and silently passing if the EPP Service is misrouted.
	//
	// Pinned to the registry.k8s.io path (the k8s.gcr.io alias has been
	// frozen since 2023) and a specific tag for reproducibility — the
	// `latest` tag does not exist for this image.
	probeImage = "registry.k8s.io/e2e-test-images/agnhost:2.47"

	// probeConnectTimeout is the per-probe TCP connect timeout passed
	// to `agnhost connect`. Bumped from busybox `nc -w 3` to absorb
	// Cilium identity-propagation lag on 3-node AKS clusters where the
	// source pod and destination pod land on different nodes — the
	// destination node may not have the source identity programmed
	// into its policy map for several seconds after the source pod
	// reports Ready.
	probeConnectTimeout = "5s"
)

var _ = Describe("Network Policy", utils.GinkgoLabelNetworkPolicy, Ordered, func() {
	var (
		ctx          context.Context
		clientset    *kubernetes.Clientset
		namespace    string
		namespaceB   string
		netpolModelA string
		netpolModelB string
		serverIP     string
		serverPort   int32
		serverIPB    string
		serverPortB  int32
		// EPP pod endpoints — used as the policy-enforcement sentinel for
		// the deny / cross-namespace tests. The model pod's IP is patched
		// by gpu-node-mocker to its shadow pod's IP and the original is
		// bound to a fake node, which confuses Cilium identity allocation
		// and makes K8s NetworkPolicy enforcement against the model pod
		// IP unreliable on AKS. EPP is a regular Deployment pod in the
		// same workload namespace, lacks the gateway label, and is
		// therefore selected by `default-deny-ingress` identically to the
		// model pod — so probing it asserts the same policy contract on a
		// pod whose identity Cilium actually programs.
		eppIP           string
		eppPort         int32
		eppIPB          string
		eppPortB        int32
		probeNamespaces []string
	)

	BeforeAll(func() {
		ctx = context.Background()
		utils.GetClusterClient(utils.TestingCluster)

		var err error
		clientset, err = utils.GetK8sClientset()
		Expect(err).NotTo(HaveOccurred(), "failed to create k8s clientset")

		// Install both workload namespaces (each with default-deny + allow-inference
		// NetworkPolicy pair) via the shared case framework. InstallCase handles
		// the per-namespace Gateway, modeldeployment Helm release, EPP / shadow
		// pod readiness, and gateway routing warmup.
		InstallCase(CaseNetworkPolicyA)
		InstallCase(CaseNetworkPolicyB)

		namespace = CaseNamespace(CaseNetworkPolicyA)
		namespaceB = CaseNamespace(CaseNetworkPolicyB)
		netpolModelA = CaseDeployments[CaseNetworkPolicyA][0].Name
		netpolModelB = CaseDeployments[CaseNetworkPolicyB][0].Name

		// Fail fast if the modelharness chart did not actually render the
		// per-namespace NetworkPolicies. Without this assertion, a chart
		// regression would surface only as the (much slower) 5-minute
		// canary-enforcement timeout below, with the dump-cluster-state
		// snapshot taken AFTER teardown — at which point the policies
		// would be gone regardless and the failure mode is ambiguous.
		expectNetworkPoliciesPresent(ctx, clientset, namespace)
		expectNetworkPoliciesPresent(ctx, clientset, namespaceB)

		// Resolve the model pod IP + port for namespace A.
		serverIP, serverPort = readyModelPodEndpoint(ctx, clientset, namespace, netpolModelA)
		Expect(serverIP).NotTo(BeEmpty(), "could not find a ready model pod IP in %s", namespace)
		Expect(serverPort).To(BeNumerically(">", 0), "could not determine model pod serving port in %s", namespace)

		// Resolve the model pod IP + port for namespace B.
		serverIPB, serverPortB = readyModelPodEndpoint(ctx, clientset, namespaceB, netpolModelB)
		Expect(serverIPB).NotTo(BeEmpty(), "could not find a ready model pod IP in %s", namespaceB)
		Expect(serverPortB).To(BeNumerically(">", 0), "could not determine model pod serving port in %s", namespaceB)

		// Resolve the EPP pod IP + port for both workload namespaces. EPP
		// is the policy-enforcement sentinel for this suite (see the
		// `eppIP`/`eppIPB` declaration above for why model-pod IPs are
		// unreliable under gpu-node-mocker's patched-IP arrangement).
		// The BeforeAll canary loop below uses the namespace-A EPP
		// endpoint to wait for enforcement to come up; the deny / cross-
		// namespace `It`s reuse the same endpoints so they assert against
		// pod identities Cilium has actually programmed.
		eppIP, eppPort = readyEPPPodEndpoint(ctx, clientset, namespace, netpolModelA)
		Expect(eppIP).NotTo(BeEmpty(), "could not find a ready EPP pod IP in %s", namespace)
		Expect(eppPort).To(BeNumerically(">", 0), "could not determine EPP pod port in %s", namespace)

		eppIPB, eppPortB = readyEPPPodEndpoint(ctx, clientset, namespaceB, netpolModelB)
		Expect(eppIPB).NotTo(BeEmpty(), "could not find a ready EPP pod IP in %s", namespaceB)
		Expect(eppPortB).To(BeNumerically(">", 0), "could not determine EPP pod port in %s", namespaceB)

		// AKS Cilium overlay race guard: poll the EPP pod's CiliumEndpoint
		// until status.policy.ingress.enforcing flips to true. Without this
		// gate, an EPP pod that was created in the narrow window between
		// the NetworkPolicy apply and cilium-agent's policy compile lands
		// with enforcing=false and never recovers — the canary probe loop
		// below then spins for 5 minutes and fails with a generic
		// "enforcement not active" message that hides the actual cause.
		// Failing here surfaces a specific, actionable Cilium message
		// instead, and short-circuits 4+ minutes of useless polling.
		waitForCiliumEndpointEnforcing(ctx, clientset, namespace, eppIP)
		waitForCiliumEndpointEnforcing(ctx, clientset, namespaceB, eppIPB)

		// Wait for NetworkPolicy enforcement to actually take effect on this
		// cluster. On freshly created Cilium clusters the policy maps may take
		// a few seconds to load even after pods report Ready. Use a single
		// long-lived canary pod in an external namespace and probe with
		// `agnhost connect`, whose documented exit codes (1=blocked,
		// 2=refused) let us distinguish enforced-deny from a misrouted
		// destination — something busybox `nc -z` cannot do.
		canaryNS := fmt.Sprintf("e2e-netpol-canary-%d", rand.Intn(900000)+100000)
		probeNamespaces = append(probeNamespaces, canaryNS)
		_, _ = clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: canaryNS},
		}, metav1.CreateOptions{})

		// Wait for the namespace's `default` ServiceAccount to be
		// created by the SA admission controller. Without this, an
		// immediate Pod create races the controller and fails with
		// `error looking up service account <ns>/default: serviceaccount
		// "default" not found` on freshly-created namespaces.
		Eventually(func() error {
			_, err := clientset.CoreV1().ServiceAccounts(canaryNS).Get(ctx, "default", metav1.GetOptions{})
			return err
		}, 30*time.Second, time.Second).Should(Succeed(),
			"default ServiceAccount in %s did not appear", canaryNS)

		canaryPodName := fmt.Sprintf("canary-probe-%d", rand.Intn(900000)+100000)
		_, err = clientset.CoreV1().Pods(canaryNS).Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: canaryPodName, Namespace: canaryNS},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  "probe",
					Image: probeImage,
					// `agnhost pause` keeps the container alive
					// indefinitely (equivalent to `sleep infinity`)
					// without a shell wrapper, so the probe binary is
					// PID 1 and ready to be exec'd against immediately.
					Args: []string{"pause"},
				}},
			},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "failed to create canary probe pod")
		Expect(utils.WaitForPodReady(ctx, clientset, canaryNS, canaryPodName, utils.PollTimeout)).
			To(Succeed(), "canary probe pod did not become ready")

		restCfg, err := utils.GetK8sConfig()
		Expect(err).NotTo(HaveOccurred())

		var (
			lastCanaryOut     string
			lastCanaryExecErr string
			pollAttempts      int
			connectedCount    int
		)
		Eventually(func() bool {
			pollAttempts++
			// `agnhost connect` is a pure TCP probe with documented exit
			// codes: 0 = connected, 1 = TIMEOUT (silent drop — what
			// NetworkPolicy default-deny produces), 2 = REFUSED (TCP RST
			// — destination unreachable but NOT blocked), 3 = DNS_ERROR,
			// 4 = OTHER. Echo the exit code to stdout because client-go's
			// StreamWithContext returns nil even when the remote command
			// exits non-zero, so we cannot rely on `err != nil` to mean
			// "blocked".
			cmd := []string{"sh", "-c", fmt.Sprintf(
				"/agnhost connect %s:%d --timeout=%s --protocol=tcp 2>&1; echo EXIT=$?",
				eppIP, eppPort, probeConnectTimeout,
			)}
			req := clientset.CoreV1().RESTClient().Post().
				Resource("pods").Name(canaryPodName).Namespace(canaryNS).
				SubResource("exec").
				VersionedParams(&corev1.PodExecOptions{
					Command: cmd, Stdout: true, Stderr: true,
				}, scheme.ParameterCodec)

			exec, err := remotecommand.NewSPDYExecutor(restCfg, "POST", req.URL())
			if err != nil {
				lastCanaryExecErr = "newSPDYExecutor: " + err.Error()
				return false
			}

			var stdout, stderr bytes.Buffer
			execCtx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			streamErr := exec.StreamWithContext(execCtx, remotecommand.StreamOptions{
				Stdout: &stdout, Stderr: &stderr,
			})
			out := stdout.String() + stderr.String()
			lastCanaryOut = out
			if streamErr != nil {
				lastCanaryExecErr = "stream: " + streamErr.Error()
			} else {
				lastCanaryExecErr = ""
			}
			if bytes.Contains([]byte(out), []byte("EXIT=0")) {
				connectedCount++
				// One-shot mid-poll diagnostic dump after ~1min of
				// consecutive successful canary connections. Surfaces the
				// state needed to root-cause the failure into the test
				// log BEFORE the AfterAll teardown wipes it. Using
				// `==12` (12 polls × 5s ≈ 1min) ensures the dump is
				// printed exactly once, not on every iteration.
				if connectedCount == 12 {
					AddReportEntry("netpol-enforcement-diag",
						networkPolicyEnforcementDiagnostics(ctx, clientset, namespace, eppIP, canaryNS))
				}
				return false
			}
			// Empty output (no EXIT marker at all) usually means the SPDY
			// stream was torn down before the remote shell printed anything.
			// That is NOT evidence of NetworkPolicy enforcement, so don't
			// treat it as success — keep polling for a real EXIT=N signal.
			if !bytes.Contains([]byte(out), []byte("EXIT=")) {
				return false
			}
			// EXIT=N (N != 0): nc could not establish the TCP handshake from
			// the external canary namespace. Enforcement is active.
			return true
		}, 5*time.Minute, 5*time.Second).Should(BeTrue(),
			// Use a func() string so Gomega evaluates the diagnostic message
			// lazily, after the polling loop has updated the captured
			// variables. Passing `lastCanaryOut`/`lastCanaryExecErr` as
			// fmt.Sprintf args would snapshot their (initial empty) values
			// at Should() call time — Gomega only invokes the format
			// string lazily, but the variadic string args have already
			// been copied into the []any slice by then.
			func() string {
				// Capture diagnostic snapshot lazily so the data reflects the
				// state at the moment the timeout fires, not at Should() call
				// time. Helps distinguish "policies missing" from "policies
				// present but unenforced by Cilium".
				diag := networkPolicyEnforcementDiagnostics(ctx, clientset, namespace, eppIP, canaryNS)
				return fmt.Sprintf(
					"timed out waiting for NetworkPolicy enforcement to become active — "+
						"Cilium may not be enforcing policies on this cluster, or the "+
						"allow-inference-traffic rule is too permissive\n"+
						"polled %d times; %d probes saw EXIT=0 (canary reached the EPP pod)\n"+
						"last agnhost connect output: %q\nlast exec error: %q\n"+
						"eppIP=%s eppPort=%d canaryNS=%s\n"+
						"--- diagnostics ---\n%s",
					pollAttempts, connectedCount,
					lastCanaryOut, lastCanaryExecErr,
					eppIP, eppPort, canaryNS,
					diag,
				)
			})
	})

	AfterAll(func() {
		// Clean up probe namespaces (canary + any It-block-created probe ns).
		for _, ns := range probeNamespaces {
			_ = clientset.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
		}
		UninstallCase(CaseNetworkPolicyA)
		UninstallCase(CaseNetworkPolicyB)
	})

	// probeTarget launches an agnhost pod in probeNS and execs the given
	// command. It returns the stdout output and any exec error. The caller
	// decides how to interpret the result (e.g. EXIT=0 in stdout means
	// connectivity, EXIT=2 means the destination refused the connection).
	// Optional labels can be applied to the probe pod.
	probeTarget := func(probeNS string, cmd []string, timeout time.Duration, labels map[string]string) (string, error) {
		// Track probe namespaces for cleanup (skip pre-existing ones).
		if probeNS != namespace && probeNS != "istio-system" && probeNS != "kube-system" && probeNS != "default" {
			probeNamespaces = append(probeNamespaces, probeNS)
		}

		// Ensure namespace exists.
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: probeNS}}
		_, _ = clientset.CoreV1().Namespaces().Create(ctx, nsObj, metav1.CreateOptions{})

		// Wait for the namespace's `default` ServiceAccount to be
		// created by the SA admission controller. Without this, an
		// immediate Pod create races the controller and fails with
		// `error looking up service account <ns>/default: serviceaccount
		// "default" not found` — most visible on freshly-created
		// namespaces (e.g. `random-ns`).
		Eventually(func() error {
			_, err := clientset.CoreV1().ServiceAccounts(probeNS).Get(ctx, "default", metav1.GetOptions{})
			return err
		}, 30*time.Second, time.Second).Should(Succeed(),
			"default ServiceAccount in %s did not appear", probeNS)

		probePodName := fmt.Sprintf("netpol-probe-%d", rand.Intn(900000)+100000)
		probePod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      probePodName,
				Namespace: probeNS,
				Labels:    labels,
				// Disable Istio sidecar injection on the probe pod. Without
				// this, a probe created in a meshed namespace (e.g.
				// `istio-system`, or any namespace the llm-gateway-auth
				// operator has touched via its mesh-config patch) gets an
				// istio-proxy sidecar. The sidecar wraps outbound traffic
				// in mTLS and the EPP pod sees the source as 127.0.0.1
				// (the local proxy), trivially matching
				// `allow-inference-traffic`'s intra-pod selector and
				// bypassing NetworkPolicy entirely. A bare agnhost probe
				// is the only way to assert L3/L4 policy honestly.
				Annotations: map[string]string{
					"sidecar.istio.io/inject": "false",
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  "probe",
					Image: probeImage,
					// `agnhost pause` keeps the container alive
					// indefinitely without a shell wrapper, so the probe
					// binary is PID 1 and ready to be exec'd against
					// immediately.
					Args: []string{"pause"},
				}},
			},
		}
		_, err := clientset.CoreV1().Pods(probeNS).Create(ctx, probePod, metav1.CreateOptions{})
		if err != nil {
			GinkgoLogr.Info("probe pod create", "err", err)
		}
		Expect(utils.WaitForPodReady(ctx, clientset, probeNS, probePodName, utils.PollTimeout)).
			To(Succeed(), "probe pod in %s did not become ready", probeNS)

		defer func() {
			_ = clientset.CoreV1().Pods(probeNS).Delete(ctx, probePodName, metav1.DeleteOptions{})
		}()

		restCfg, err := utils.GetK8sConfig()
		Expect(err).NotTo(HaveOccurred())

		req := clientset.CoreV1().RESTClient().Post().
			Resource("pods").
			Name(probePodName).
			Namespace(probeNS).
			SubResource("exec").
			VersionedParams(&corev1.PodExecOptions{
				Command: cmd,
				Stdout:  true,
				Stderr:  true,
			}, scheme.ParameterCodec)

		exec, err := remotecommand.NewSPDYExecutor(restCfg, "POST", req.URL())
		Expect(err).NotTo(HaveOccurred())

		var stdout, stderr bytes.Buffer
		execCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		err = exec.StreamWithContext(execCtx, remotecommand.StreamOptions{
			Stdout: &stdout,
			Stderr: &stderr,
		})

		return stdout.String(), err
	}

	// connectCmd builds a TCP connectivity check using `agnhost connect`,
	// the same probe binary the upstream Kubernetes NetworkPolicy
	// conformance suite uses. Unlike busybox `nc -z`, agnhost has
	// documented, stable exit codes that distinguish *why* a probe
	// failed:
	//   0 = connected
	//   1 = TIMEOUT  (silent drop — what NetworkPolicy default-deny does)
	//   2 = REFUSED  (TCP RST — destination is reachable but nothing is
	//                 listening; means policy let the probe through and
	//                 the test passing on this would be a silent bug)
	//   3 = DNS_ERROR
	//   4 = OTHER
	//
	// We MUST echo `EXIT=$?` because client-go's StreamWithContext
	// returns nil even when the remote command exits non-zero, so the
	// caller cannot rely on `streamErr != nil` to mean "blocked".
	// Callers should inspect stdout via `classify()` instead of trusting
	// the returned err.
	connectCmd := func(targetIP string, targetPort int32) []string {
		return []string{"sh", "-c", fmt.Sprintf(
			"/agnhost connect %s:%d --timeout=%s --protocol=tcp 2>&1; echo EXIT=$?",
			targetIP, targetPort, probeConnectTimeout,
		)}
	}

	// classifyResult interprets the stdout/stderr of a connectCmd run via
	// probeTarget into one of four states. "blocked" is what the deny
	// tests expect — a NetworkPolicy default-deny silently drops the SYN
	// and agnhost reports TIMEOUT (exit 1). "refused" means the SYN
	// reached the destination's host but no listener answered, which is
	// NOT a NetworkPolicy success and should fail the deny tests loud.
	// "indeterminate" covers cases where the SPDY stream got torn down
	// before agnhost printed an exit marker (rare; treated as
	// not-connected by `connected()` but exposed separately so callers
	// can distinguish "couldn't tell" from "definitely blocked").
	classifyResult := func(out string) string {
		switch {
		case strings.Contains(out, "EXIT=0"):
			return "connected"
		case strings.Contains(out, "EXIT=1"):
			return "blocked"
		case strings.Contains(out, "EXIT=2"):
			return "refused"
		case strings.Contains(out, "EXIT="):
			return "other"
		default:
			return "indeterminate"
		}
	}

	// connected returns true iff the probe completed a TCP handshake.
	// Backwards-compatible shim for assertions that only care about the
	// connected/not-connected boolean — prefer classifyResult() in new
	// code so the deny tests can distinguish "blocked by policy"
	// (the wanted outcome) from "refused by an empty endpoint"
	// (a bug that policy did NOT block).
	connected := func(out string) bool {
		return classifyResult(out) == "connected"
	}

	// probeClassified is a convenience wrapper that checks TCP connectivity
	// to the namespace-A EPP pod (see the `eppIP` declaration for why EPP,
	// not the model pod, is the policy-enforcement sentinel under
	// gpu-node-mocker). Returns the classified result directly so callers
	// can distinguish "blocked" (the wanted deny outcome) from "refused"
	// (a bug where the destination is reachable but unlistening — policy
	// did NOT block the probe).
	probeClassified := func(probeNS string) (string, string) {
		out, _ := probeTarget(probeNS, connectCmd(eppIP, eppPort), probeTimeout, nil)
		return classifyResult(out), out
	}

	// expectDenied probes from probeNS to the namespace-A EPP pod and
	// asserts the probe is silently dropped. On any non-"blocked" outcome,
	// it attaches a full enforcement-diagnostic snapshot (rendered policy
	// `from:` peers, EPP pod identity, probe namespace labels, probe pod
	// container list + Istio injection markers) to the spec report
	// BEFORE failing — otherwise the AfterAll teardown wipes the
	// probe-side state and the failing CI log shows only the one-line
	// "expected connected to equal blocked" with no way to root-cause.
	expectDenied := func(probeNS, msg string) {
		state, out := probeClassified(probeNS)
		if state != "blocked" {
			AddReportEntry("netpol-deny-diag",
				networkPolicyEnforcementDiagnostics(ctx, clientset, namespace, eppIP, probeNS))
		}
		Expect(state).To(Equal("blocked"),
			"%s. state=%q output=%q", msg, state, out)
	}

	// expectDeniedLabeled is the labeled-probe variant of expectDenied,
	// used by tests that need to assert that label-only spoofing across
	// namespaces does NOT grant access (the X1 regression guard).
	expectDeniedLabeled := func(probeNS string, labels map[string]string, targetIP string, targetPort int32, msg string) {
		out, _ := probeTarget(probeNS, connectCmd(targetIP, targetPort), probeTimeout, labels)
		state := classifyResult(out)
		if state != "blocked" {
			AddReportEntry("netpol-deny-diag",
				networkPolicyEnforcementDiagnostics(ctx, clientset, namespace, targetIP, probeNS))
		}
		Expect(state).To(Equal("blocked"),
			"%s. state=%q output=%q", msg, state, out)
	}

	// ── Enforcement baseline ──────────────────────────────────────────────
	// These two tests run first (suite is Ordered) and together prove that
	// NetworkPolicy enforcement is active. If intra-namespace connectivity
	// works but an external namespace is NOT blocked, enforcement is off and
	// every subsequent deny assertion would be a false positive.
	It("baseline: should ALLOW ingress from within the model namespace", func() {
		// NetworkPolicy is L3/L4, so a TCP-level reachability check is the
		// correct signal. Hitting the model pod directly with an
		// HTTP/`/v1/chat/completions` POST would test the EPP+Gateway
		// pipeline (which only the Gateway pod can reach), not the policy.
		// We probe the EPP pod (not the model pod) for the same reason the
		// deny tests do — see `eppIP` declaration.
		out, _ := probeTarget(namespace, connectCmd(eppIP, eppPort), probeTimeout, nil)
		Expect(connected(out)).To(BeTrue(),
			"intra-namespace TCP reach to EPP pod should succeed — if this fails, NetworkPolicy is over-blocking. agnhost output: %q", out)
	})

	It("baseline: should DENY ingress from an external namespace (proves enforcement is active)", func() {
		state, out := probeClassified("external-ns")
		Expect(state).To(Equal("blocked"),
			"external namespace should be silently dropped by default-deny-ingress (TIMEOUT/EXIT=1). "+
				"Got state=%q output=%q. If state=='connected', NetworkPolicy enforcement is NOT active and "+
				"remaining deny tests are unreliable. If state=='refused', the EPP Service is misconfigured — "+
				"policy let the probe through but no listener answered.", state, out)
	})

	// ── Deny tests ────────────────────────────────────────────────────────
	It("should DENY ingress from a non-gateway pod in default namespace", func() {
		expectDenied("default", "non-gateway pod in default should be silently dropped")
	})

	It("should DENY ingress from istio-system namespace", func() {
		expectDenied("istio-system",
			"istio-system should be silently dropped — only the gateway pod in default is allowed")
	})

	It("should DENY ingress from a random namespace", func() {
		expectDenied("random-ns",
			"random-ns should be silently dropped by default-deny-ingress")
	})

	It("should ALLOW ingress from kube-system namespace (chart allowlist)", func() {
		// kube-system is included in modelharness's
		// `allowedIngressNamespaces` defaults so cluster-control-plane
		// components (kube-proxy health probes, konnectivity agent,
		// kubelet exec proxy, etc.) can reach EPP / vLLM. If this
		// assertion fails, the chart's `allow-inference-traffic` rule
		// has regressed — verify charts/modelharness/values.yaml still
		// lists "kube-system" under networkPolicy.allowedIngressNamespaces.
		state, out := probeClassified("kube-system")
		Expect(state).To(Equal("connected"),
			"kube-system should be allowed by allow-inference-traffic — "+
				"see networkPolicy.allowedIngressNamespaces in modelharness values.yaml. "+
				"state=%q output=%q", state, out)
	})

	// ── Allow tests ───────────────────────────────────────────────────────
	It("should DENY ingress via gateway-labeled pod in default namespace", func() {
		// Each workload namespace only trusts its own in-namespace gateway
		// pod. A pod in `default` carrying the inference-gateway label
		// (including the cluster-wide gateway pod itself) must NOT be
		// allowed to reach EPP / vLLM in a workload namespace — otherwise a
		// compromised or misconfigured cross-namespace gateway could bypass
		// per-namespace isolation.
		gatewayLabels := map[string]string{
			"gateway.networking.k8s.io/gateway-name": "inference-gateway",
		}
		expectDeniedLabeled("default", gatewayLabels, eppIP, eppPort,
			"gateway-labeled pod in default should be silently dropped — only the in-namespace "+
				"gateway pod is a trusted ingress source")
	})

	// ── Cross-namespace isolation ─────────────────────────────────────────
	It("should DENY ingress from workload namespace A to workload namespace B", func() {
		expectDeniedLabeled(namespace, nil, eppIPB, eppPortB,
			"workload namespace A should be silently dropped by namespace B's default-deny")
	})

	It("should DENY ingress from workload namespace B to workload namespace A", func() {
		expectDeniedLabeled(namespaceB, nil, eppIP, eppPort,
			"workload namespace B should be silently dropped by namespace A's default-deny")
	})

	// Regression guard: a pod in namespace A that *spoofs* namespace B's
	// gateway label must still be denied. NetworkPolicy `podSelector`
	// without a `namespaceSelector` is namespace-scoped by construction,
	// so labels alone cannot grant cross-namespace access. This test
	// locks that invariant in: if anyone reintroduces a cross-namespace
	// allow rule keyed only on a pod label (the X1 regression), this
	// case will fail while the existing unlabeled X2 tests would not.
	It("should DENY cross-namespace ingress even when probe pod spoofs the target's gateway label", func() {
		spoofedLabels := map[string]string{
			"gateway.networking.k8s.io/gateway-name": fmt.Sprintf("%s-gateway", netpolModelB),
		}
		expectDeniedLabeled(namespace, spoofedLabels, eppIPB, eppPortB,
			"a pod in namespace A carrying namespace B's gateway label must be silently dropped — "+
				"labels do not grant cross-namespace trust under the post-X1 policy")
	})

	// ── North-South positive path ─────────────────────────────────────────
	// The gateway pod is the namespace's only N/S entry point and must
	// be reachable from outside the workload namespace via real
	// pod-to-pod traffic over the CNI dataplane. Probing through
	// `kubectl port-forward` (as the rest of the suite does for
	// convenience) goes through the apiserver→kubelet host-side path
	// and bypasses NetworkPolicy entirely — so port-forward "works"
	// would be a false positive if the policy were over-restrictive.
	// This test probes the gateway's Service ClusterIP from a probe pod
	// in an external namespace, which exercises the real CNI path the
	// production N/S traffic would use.
	It("should ALLOW external-namespace ingress to the gateway pod via Service ClusterIP", func() {
		gwSvcName := utils.IstioGatewayServiceName(CaseGatewayName(CaseNetworkPolicyA))
		svc, err := clientset.CoreV1().Services(namespace).Get(ctx, gwSvcName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "could not look up gateway Service %s/%s", namespace, gwSvcName)
		Expect(svc.Spec.ClusterIP).NotTo(BeEmpty(), "gateway Service has no ClusterIP")

		var gwPort int32
		for _, p := range svc.Spec.Ports {
			if p.Port == 80 {
				gwPort = p.Port
				break
			}
		}
		Expect(gwPort).To(BeNumerically(">", 0), "gateway Service does not expose port 80")

		out, _ := probeTarget("e2e-netpol-external-client",
			connectCmd(svc.Spec.ClusterIP, gwPort), probeTimeout, nil)
		Expect(connected(out)).To(BeTrue(),
			"external-namespace pod should be allowed to TCP-connect to the gateway pod via Service ClusterIP — "+
				"if this fails, the workload-namespace NetworkPolicy is over-restrictive and is silently relying on "+
				"apiserver-mediated paths (port-forward / kubelet) for reachability. agnhost output: %q", out)
	})
})

// readyModelPodEndpoint returns the PodIP and first containerPort of a Ready
// pod owned by the given InferenceSet name in the given namespace. Fails the
// current Ginkgo spec if no Ready pod has appeared within InferenceSetReadyTimeout.
func readyModelPodEndpoint(ctx context.Context, clientset *kubernetes.Clientset, ns, deploymentName string) (string, int32) {
	selector := fmt.Sprintf("inferenceset.kaito.sh/created-by=%s", deploymentName)

	Eventually(func() (string, error) {
		pods, err := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return "", err
		}
		for _, pod := range pods.Items {
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					return pod.Status.PodIP, nil
				}
			}
		}
		return "", fmt.Errorf("no ready model pods found for %s in %s", deploymentName, ns)
	}, utils.InferenceSetReadyTimeout, utils.PollInterval).ShouldNot(BeEmpty(),
		"model pod did not become ready in %s", ns)

	pods, err := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	Expect(err).NotTo(HaveOccurred())

	var (
		ip   string
		port int32
	)
	for _, pod := range pods.Items {
		ready := false
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				ready = true
				break
			}
		}
		if !ready {
			continue
		}
		ip = pod.Status.PodIP
		for _, c := range pod.Spec.Containers {
			for _, p := range c.Ports {
				if p.ContainerPort > 0 {
					port = p.ContainerPort
					break
				}
			}
			if port > 0 {
				break
			}
		}
		if ip != "" && port > 0 {
			break
		}
	}
	return ip, port
}

// readyEPPPodEndpoint returns the PodIP and first containerPort of a Ready
// EPP pod owned by the InferencePool of the given deployment in the given
// namespace. EPP pods are normal Deployment pods (no gpu-node-mocker shadow
// hackery), making them a deterministic probe target for verifying that
// Cilium has actually loaded the namespace's NetworkPolicy maps. Fails the
// current Ginkgo spec if no Ready pod has appeared within
// InferenceSetReadyTimeout.
func readyEPPPodEndpoint(ctx context.Context, clientset *kubernetes.Clientset, ns, deploymentName string) (string, int32) {
	// Matches charts/modeldeployment/templates/_helpers.tpl:
	//   inferencepool: <deploymentName>-inferencepool-epp
	selector := fmt.Sprintf("inferencepool=%s-inferencepool-epp", deploymentName)

	Eventually(func() (string, error) {
		pods, err := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return "", err
		}
		for _, pod := range pods.Items {
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					return pod.Status.PodIP, nil
				}
			}
		}
		return "", fmt.Errorf("no ready EPP pods found for %s in %s", deploymentName, ns)
	}, utils.InferenceSetReadyTimeout, utils.PollInterval).ShouldNot(BeEmpty(),
		"EPP pod did not become ready in %s", ns)

	pods, err := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	Expect(err).NotTo(HaveOccurred())

	var (
		ip   string
		port int32
	)
	for _, pod := range pods.Items {
		ready := false
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				ready = true
				break
			}
		}
		if !ready {
			continue
		}
		ip = pod.Status.PodIP
		for _, c := range pod.Spec.Containers {
			for _, p := range c.Ports {
				if p.ContainerPort > 0 {
					port = p.ContainerPort
					break
				}
			}
			if port > 0 {
				break
			}
		}
		if ip != "" && port > 0 {
			break
		}
	}
	return ip, port
}

// networkPolicyEnforcementDiagnostics returns a multi-line, human-readable
// snapshot of the state most relevant to "why is the canary not being
// blocked" — i.e. whether the NetworkPolicies actually exist in the
// workload namespace and what the EPP pod that the canary is targeting
// looks like. Helps distinguish "policies missing" from "policies present
// but unenforced by Cilium".
//
// `probeNS` is optional — pass "" to skip the probe-side dump. When
// supplied, the helper also reports the probe namespace's labels and any
// Istio-injection markers on probe pods in it, since either can cause a
// probe to short-circuit NetworkPolicy enforcement (mesh mTLS makes the
// destination see 127.0.0.1; namespace labels can match an unintended
// `allowedIngressNamespaces` selector).
//
// It is intentionally lenient: every clientset call swallows errors and
// degrades to a marker string, because the failure message is best-effort
// and must never panic from inside Gomega's lazy formatter.
func networkPolicyEnforcementDiagnostics(ctx context.Context, clientset *kubernetes.Clientset, ns, targetIP string, probeNS ...string) string {
	var b strings.Builder

	// 1. List the NetworkPolicies in the workload namespace. If empty,
	//    the modelharness Helm chart never rendered them (or they were
	//    already torn down by the time we got here) — that alone fully
	//    explains the canary reaching the EPP pod. Dump each ingress
	//    rule's `from:` peers so a chart regression that accidentally
	//    widens `allowedIngressNamespaces` (e.g. to include `default`)
	//    is visible in the failure log without re-running with kubectl.
	fmt.Fprintf(&b, "NetworkPolicies in %s:\n", ns)
	nps, err := clientset.NetworkingV1().NetworkPolicies(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(&b, "  <list error: %v>\n", err)
	} else if len(nps.Items) == 0 {
		fmt.Fprintf(&b, "  <none found — modelharness chart did not render NetworkPolicies, or they were uninstalled>\n")
	} else {
		for _, np := range nps.Items {
			fmt.Fprintf(&b, "  - %s (policyTypes=%v, podSelector=%s)\n",
				np.Name, np.Spec.PolicyTypes,
				formatPodSelector(np.Spec.PodSelector))
			for i, rule := range np.Spec.Ingress {
				if len(rule.From) == 0 {
					fmt.Fprintf(&b, "      ingress[%d]: from=<all> (allow-all)\n", i)
					continue
				}
				for j, peer := range rule.From {
					fmt.Fprintf(&b, "      ingress[%d].from[%d]: %s\n",
						i, j, formatNetworkPolicyPeer(peer))
				}
			}
		}
	}

	// 2. Locate the EPP pod backing targetIP and dump its labels +
	//    annotations + node. Cilium identity is derived from labels, so a
	//    mismatch (e.g. missing namespace label, unexpected security
	//    label) here would point at why policy decisions go wrong.
	fmt.Fprintf(&b, "EPP pod with IP %s in %s:\n", targetIP, ns)
	pods, err := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(&b, "  <list error: %v>\n", err)
	} else {
		found := false
		for _, p := range pods.Items {
			if p.Status.PodIP != targetIP {
				continue
			}
			found = true
			fmt.Fprintf(&b, "  name=%s node=%s phase=%s\n  labels=%v\n  annotations=%v\n",
				p.Name, p.Spec.NodeName, p.Status.Phase, p.Labels, p.Annotations)
		}
		if !found {
			fmt.Fprintf(&b, "  <no pod with IP %s in %s — EPP pod may have rolled>\n", targetIP, ns)
		}
	}

	// 3. Sanity-check the workload namespace's metadata labels — Cilium
	//    keys identity decisions off `kubernetes.io/metadata.name` and
	//    similar namespace labels, so any drift here would point at the
	//    cause of cross-namespace traffic being identified as same-NS.
	fmt.Fprintf(&b, "Namespace %s labels:\n", ns)
	if nsObj, err := clientset.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); err != nil {
		fmt.Fprintf(&b, "  <get error: %v>\n", err)
	} else {
		fmt.Fprintf(&b, "  %v\n", nsObj.Labels)
	}

	// 4. Probe-side dump. The probe namespace's labels expose any
	//    accidental match against `allowedIngressNamespaces`. The probe
	//    pod's container list + Istio annotations expose a smuggled-in
	//    `istio-proxy` sidecar (which would wrap traffic in mTLS and
	//    make the destination see `127.0.0.1`, trivially matching the
	//    `podSelector: {}` intra-namespace clause and short-circuiting
	//    the namespace deny rule). The probe creator sets
	//    `sidecar.istio.io/inject: "false"` to avoid this, but a
	//    cluster-wide mutating webhook can override the annotation.
	for _, pns := range probeNS {
		if pns == "" {
			continue
		}
		fmt.Fprintf(&b, "Probe namespace %s labels:\n", pns)
		if nsObj, err := clientset.CoreV1().Namespaces().Get(ctx, pns, metav1.GetOptions{}); err != nil {
			fmt.Fprintf(&b, "  <get error: %v>\n", err)
		} else {
			fmt.Fprintf(&b, "  %v\n", nsObj.Labels)
		}
		fmt.Fprintf(&b, "Probe pods in %s (istio sidecar check):\n", pns)
		probePods, err := clientset.CoreV1().Pods(pns).List(ctx, metav1.ListOptions{})
		if err != nil {
			fmt.Fprintf(&b, "  <list error: %v>\n", err)
			continue
		}
		any := false
		for _, p := range probePods.Items {
			if !strings.HasPrefix(p.Name, "netpol-probe-") && !strings.HasPrefix(p.Name, "canary-probe-") {
				continue
			}
			any = true
			hasSidecar := false
			for _, c := range p.Spec.Containers {
				if c.Name == "istio-proxy" {
					hasSidecar = true
					break
				}
			}
			_, sidecarStatus := p.Annotations["sidecar.istio.io/status"]
			injectAnn := p.Annotations["sidecar.istio.io/inject"]
			fmt.Fprintf(&b, "  - %s node=%s containers=%d hasIstioProxy=%v sidecarStatusAnnotation=%v inject=%q\n",
				p.Name, p.Spec.NodeName, len(p.Spec.Containers), hasSidecar, sidecarStatus, injectAnn)
		}
		if !any {
			fmt.Fprintf(&b, "  <no netpol-* probe pods found — probe was already torn down>\n")
		}
	}

	// 5. Cilium dataplane state. The AKS e2e cluster runs Azure CNI
	//    overlay with `--network-dataplane cilium --network-policy cilium`,
	//    so K8s NetworkPolicies are enforced by cilium-agent on each
	//    node. When the K8s policies are present on the API server but
	//    canary traffic is still admitted, the candidates are:
	//      a) cilium-agent on the EPP's node isn't Ready (or missing),
	//         so the policy never got compiled into the local eBPF map;
	//      b) the EPP pod's CiliumEndpoint exists but its
	//         status.policy.ingress.enforcing is false — common when an
	//         endpoint was created before its identity was allocated;
	//      c) the K8s NetworkPolicies were never translated into Cilium's
	//         internal representation (would show as 0 CiliumNetworkPolicy
	//         resources AND no inline status on CiliumEndpoint).
	//    Dump the minimum that distinguishes (a)/(b)/(c). All calls are
	//    best-effort — on non-Cilium clusters the CRDs / pods are absent
	//    and we just skip the section.
	collectCiliumDiagnostics(ctx, clientset, ns, targetIP, &b)

	return b.String()
}

// collectCiliumDiagnostics is a best-effort dump of Cilium dataplane state
// relevant to NetworkPolicy enforcement. It writes into `b` and never
// errors out — missing resources are reported as a single line so the
// helper is safe to call on non-Cilium clusters.
func collectCiliumDiagnostics(ctx context.Context, clientset *kubernetes.Clientset, ns, targetIP string, b *strings.Builder) {
	fmt.Fprintf(b, "Cilium dataplane state:\n")

	// (a) cilium-agent pods. AKS installs them in `kube-system` as a
	// DaemonSet; on Cilium-the-OSS distributions they live in
	// `cilium`. Try kube-system first (AKS), then fall back to a
	// label-selector across all namespaces.
	agentPods, err := clientset.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{
		LabelSelector: "k8s-app=cilium",
	})
	if err != nil || len(agentPods.Items) == 0 {
		agentPods, err = clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
			LabelSelector: "k8s-app=cilium",
		})
	}
	if err != nil {
		fmt.Fprintf(b, "  cilium-agent pods: <list error: %v>\n", err)
	} else if len(agentPods.Items) == 0 {
		fmt.Fprintf(b, "  cilium-agent pods: <none found — cluster may not be Cilium-backed>\n")
	} else {
		// Find the EPP pod's node so we can flag the relevant agent.
		var eppNode string
		if pods, perr := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{}); perr == nil {
			for _, p := range pods.Items {
				if p.Status.PodIP == targetIP {
					eppNode = p.Spec.NodeName
					break
				}
			}
		}
		fmt.Fprintf(b, "  cilium-agent pods (EPP is on node %q):\n", eppNode)
		for _, p := range agentPods.Items {
			ready := "false"
			for _, cs := range p.Status.ContainerStatuses {
				if cs.Name == "cilium-agent" && cs.Ready {
					ready = "true"
					break
				}
			}
			marker := ""
			if eppNode != "" && p.Spec.NodeName == eppNode {
				marker = "  <-- EPP node"
			}
			fmt.Fprintf(b, "    - %s/%s node=%s phase=%s ready=%s%s\n",
				p.Namespace, p.Name, p.Spec.NodeName, p.Status.Phase, ready, marker)
		}
	}

	// (b) CiliumEndpoint for the EPP pod. The CRD is namespaced and
	// named after the pod. We use the dynamic client so we don't
	// have to import Cilium's API types.
	dyn, derr := utils.GetDynamicClient()
	if derr != nil {
		fmt.Fprintf(b, "  CiliumEndpoint: <dynamic client error: %v>\n", derr)
		return
	}
	cepGVR := schema.GroupVersionResource{
		Group: "cilium.io", Version: "v2", Resource: "ciliumendpoints",
	}
	ceps, err := dyn.Resource(cepGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(b, "  CiliumEndpoints in %s: <list error: %v>\n", ns, err)
	} else {
		fmt.Fprintf(b, "  CiliumEndpoints in %s (looking for IP %s):\n", ns, targetIP)
		matched := false
		for _, cep := range ceps.Items {
			// status.networking.addressing[*].ipv4 == targetIP
			addrs, _, _ := unstructuredNestedSlice(cep.Object, "status", "networking", "addressing")
			ipMatch := false
			for _, a := range addrs {
				if m, ok := a.(map[string]any); ok {
					if v, _ := m["ipv4"].(string); v == targetIP {
						ipMatch = true
						break
					}
				}
			}
			if !ipMatch {
				continue
			}
			matched = true
			identity, _, _ := unstructuredNestedInt64(cep.Object, "status", "identity", "id")
			ingressEnf, _, _ := unstructuredNestedBool(cep.Object, "status", "policy", "ingress", "enforcing")
			egressEnf, _, _ := unstructuredNestedBool(cep.Object, "status", "policy", "egress", "enforcing")
			state, _, _ := unstructuredNestedString(cep.Object, "status", "state")
			fmt.Fprintf(b, "    - name=%s identity=%d state=%s ingress.enforcing=%v egress.enforcing=%v\n",
				cep.GetName(), identity, state, ingressEnf, egressEnf)
		}
		if !matched {
			fmt.Fprintf(b, "    <no CiliumEndpoint with IP %s — endpoint not yet programmed by cilium-agent>\n", targetIP)
		}
	}

	// (c) CiliumNetworkPolicy / CiliumClusterwideNetworkPolicy count.
	// K8s NetworkPolicies do NOT round-trip into CNPs; instead Cilium
	// represents them internally and exposes them via `cilium policy
	// get`. So an empty CNP list is NORMAL — what we actually care
	// about is whether the CRDs are installed at all (proves cilium
	// CRD registration ran), which is informational.
	cnpGVR := schema.GroupVersionResource{
		Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies",
	}
	if cnps, err := dyn.Resource(cnpGVR).Namespace("").List(ctx, metav1.ListOptions{}); err != nil {
		fmt.Fprintf(b, "  CiliumNetworkPolicy CRD: <list error: %v — CRD may not be installed>\n", err)
	} else {
		fmt.Fprintf(b, "  CiliumNetworkPolicy resources cluster-wide: %d (K8s NetworkPolicies do not round-trip into CNPs; this is informational)\n", len(cnps.Items))
	}
}

// waitForCiliumEndpointEnforcing polls the CiliumEndpoint backing the pod
// at `targetIP` in `ns` and fails the current Ginkgo spec if its
// status.policy.ingress.enforcing does not flip to true within the
// timeout.
//
// Background: AKS clusters running with `--network-dataplane cilium
// --network-policy cilium` exhibit a race where a pod created in the
// narrow window between a NetworkPolicy apply and cilium-agent's policy
// compile lands with enforcing=false on its CiliumEndpoint and stays
// that way indefinitely. The K8s NetworkPolicies look correct, cilium-
// agent is healthy, but traffic to the affected endpoint is silently
// admitted because the eBPF map for that identity has no rules
// programmed. Symptom: the canary probe loop sees EXIT=0 forever.
//
// The fix is to wait for the CiliumEndpoint to advertise enforcing=true
// before starting probes. If it never flips (the race triggered and is
// stuck), this helper fails the BeforeAll with a Cilium-specific
// message — turning a 5-minute generic timeout into a 60-second
// actionable one.
//
// Best-effort: on non-Cilium clusters or when the CRD is missing, the
// helper logs the situation and returns without failing, so the suite
// remains portable to kind / OSS Kubernetes clusters.
func waitForCiliumEndpointEnforcing(ctx context.Context, _ *kubernetes.Clientset, ns, targetIP string) {
	const (
		timeout      = 60 * time.Second
		pollInterval = 2 * time.Second
	)

	dyn, derr := utils.GetDynamicClient()
	if derr != nil {
		GinkgoWriter.Printf("waitForCiliumEndpointEnforcing: dynamic client unavailable (%v) — skipping Cilium enforcement gate for %s/%s\n", derr, ns, targetIP)
		return
	}
	cepGVR := schema.GroupVersionResource{
		Group: "cilium.io", Version: "v2", Resource: "ciliumendpoints",
	}

	// Probe once to see if the CRD is registered at all. If not, this is
	// not a Cilium cluster — silently skip so kind / OSS K8s users can
	// still run the suite.
	if _, err := dyn.Resource(cepGVR).Namespace(ns).List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		GinkgoWriter.Printf("waitForCiliumEndpointEnforcing: CiliumEndpoint CRD not available (%v) — skipping enforcement gate for %s/%s\n", err, ns, targetIP)
		return
	}

	var (
		lastState      string
		lastIngressEnf bool
		lastIdentity   int64
		lastName       string
		foundEndpoint  bool
	)

	Eventually(func() bool {
		ceps, err := dyn.Resource(cepGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false
		}
		for _, cep := range ceps.Items {
			addrs, _, _ := unstructuredNestedSlice(cep.Object, "status", "networking", "addressing")
			match := false
			for _, a := range addrs {
				if m, ok := a.(map[string]any); ok {
					if v, _ := m["ipv4"].(string); v == targetIP {
						match = true
						break
					}
				}
			}
			if !match {
				continue
			}
			foundEndpoint = true
			lastName = cep.GetName()
			lastState, _, _ = unstructuredNestedString(cep.Object, "status", "state")
			lastIdentity, _, _ = unstructuredNestedInt64(cep.Object, "status", "identity", "id")
			lastIngressEnf, _, _ = unstructuredNestedBool(cep.Object, "status", "policy", "ingress", "enforcing")
			return lastIngressEnf
		}
		return false
	}, timeout, pollInterval).Should(BeTrue(),
		func() string {
			if !foundEndpoint {
				return fmt.Sprintf(
					"no CiliumEndpoint with IP %s appeared in %s within %s — "+
						"cilium-agent has not programmed the EPP endpoint. "+
						"Check cilium-agent pod health on the EPP's node.",
					targetIP, ns, timeout)
			}
			return fmt.Sprintf(
				"EPP CiliumEndpoint %s/%s (identity=%d state=%s) stuck with "+
					"ingress.enforcing=%v after %s — known AKS Cilium overlay "+
					"race when a pod comes up before its NetworkPolicy is "+
					"compiled into the local eBPF map. Cilium-agent on the "+
					"EPP node may need a restart, or the policy must be "+
					"re-applied after the pod is ready. Skipping the canary "+
					"probe loop because it would otherwise spin for 5 minutes "+
					"on this race.",
				ns, lastName, lastIdentity, lastState, lastIngressEnf, timeout)
		})
}

// unstructuredNestedString / Int64 / Bool / Slice are tiny wrappers around
// the apimachinery helpers. We re-export here to keep the diag function
// readable without importing meta/v1/unstructured in every call site.
func unstructuredNestedString(obj map[string]any, fields ...string) (string, bool, error) {
	return unstructuredGet[string](obj, fields...)
}
func unstructuredNestedInt64(obj map[string]any, fields ...string) (int64, bool, error) {
	return unstructuredGet[int64](obj, fields...)
}
func unstructuredNestedBool(obj map[string]any, fields ...string) (bool, bool, error) {
	return unstructuredGet[bool](obj, fields...)
}
func unstructuredNestedSlice(obj map[string]any, fields ...string) ([]any, bool, error) {
	return unstructuredGet[[]any](obj, fields...)
}
func unstructuredGet[T any](obj map[string]any, fields ...string) (T, bool, error) {
	var zero T
	cur := any(obj)
	for _, f := range fields {
		m, ok := cur.(map[string]any)
		if !ok {
			return zero, false, nil
		}
		cur, ok = m[f]
		if !ok {
			return zero, false, nil
		}
	}
	v, ok := cur.(T)
	return v, ok, nil
}

// formatNetworkPolicyPeer renders a single `from:` entry compactly for
// diagnostic output. Covers the three peer kinds in NetworkPolicyPeer:
// podSelector (namespace-scoped to the policy), namespaceSelector (any
// namespace whose labels match), and ipBlock (CIDR). Empty selectors
// render as "<all>" because that is what `- podSelector: {}` and
// `- namespaceSelector: {}` actually mean in policy semantics.
func formatNetworkPolicyPeer(peer networkingv1.NetworkPolicyPeer) string {
	parts := []string{}
	if peer.PodSelector != nil {
		parts = append(parts, "podSelector="+formatPodSelector(*peer.PodSelector))
	}
	if peer.NamespaceSelector != nil {
		parts = append(parts, "namespaceSelector="+formatPodSelector(*peer.NamespaceSelector))
	}
	if peer.IPBlock != nil {
		parts = append(parts, fmt.Sprintf("ipBlock=%s except=%v", peer.IPBlock.CIDR, peer.IPBlock.Except))
	}
	if len(parts) == 0 {
		return "<empty peer>"
	}
	return strings.Join(parts, " ")
}

// formatPodSelector renders a LabelSelector compactly for diagnostic
// output. Returns "<all>" for the match-everything selector and falls
// back to the matchLabels / matchExpressions string forms for non-empty
// selectors.
func formatPodSelector(sel metav1.LabelSelector) string {
	if len(sel.MatchLabels) == 0 && len(sel.MatchExpressions) == 0 {
		return "<all>"
	}
	parts := []string{}
	for k, v := range sel.MatchLabels {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	for _, e := range sel.MatchExpressions {
		parts = append(parts, fmt.Sprintf("%s %s %v", e.Key, e.Operator, e.Values))
	}
	return strings.Join(parts, ",")
}

// _ ensures the networkingv1 import is referenced even if a future
// refactor removes the only usage. The package is used implicitly
// through clientset's typed NetworkingV1 client, which would otherwise
// import-elide.
var _ = networkingv1.SchemeGroupVersion

// expectNetworkPoliciesPresent asserts that the modelharness Helm release
// in `ns` has rendered both `default-deny-ingress` and
// `allow-inference-traffic` NetworkPolicies. Failing here turns a silent
// chart regression into an immediate, clearly-attributable BeforeAll
// failure rather than a 5-minute canary timeout that — by the time CI's
// post-failure dump runs — has been masked by AfterAll teardown.
func expectNetworkPoliciesPresent(ctx context.Context, clientset *kubernetes.Clientset, ns string) {
	required := map[string]bool{
		"default-deny-ingress":    false,
		"allow-inference-traffic": false,
	}
	nps, err := clientset.NetworkingV1().NetworkPolicies(ns).List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred(), "list NetworkPolicies in %s", ns)
	for _, np := range nps.Items {
		if _, ok := required[np.Name]; ok {
			required[np.Name] = true
		}
	}
	for name, found := range required {
		Expect(found).To(BeTrue(),
			"expected NetworkPolicy %q in %s but the modelharness chart did not render it; "+
				"see chart values .networkPolicy.enabled wiring in test/e2e/cases.go and "+
				"charts/modelharness/templates/networkpolicies.yaml", name, ns)
	}
}
