# GPU Mocker

## Overview

```
┌─────────────────────────────────────────────────────────────────────┐
│                        Karpenter creates                           │
│                          NodeClaim                                 │
│                             │                                      │
│                             ▼                                      │
│               ┌─────────────────────────┐                          │
│               │  Phase 1: NodeClaim     │                          │
│               │     Reconciler          │                          │
│               └────────────┬────────────┘                          │
│                            │                                       │
│              ┌─────────────┼─────────────┐                         │
│              ▼             ▼             ▼                          │
│        ┌──────────┐ ┌──────────┐ ┌────────────┐                   │
│        │Fake Node │ │NodeClaim │ │   Lease    │                    │
│        │          │ │  Status  │ │ Heartbeat  │                    │
│        │fake://.. │ │Ready=True│ │ (10s loop) │                    │
│        │gpu labels│ │Registered│ │            │                    │
│        │gpu taint │ │Initialized│ │           │                    │
│        └──────────┘ └──────────┘ └────────────┘                   │
│                            │                                       │
│               KAITO sees GPU node ready                            │
│               → creates inference Pod                              │
│                            │                                       │
│                            ▼                                       │
│               ┌─────────────────────────┐                          │
│               │  Phase 2: ShadowPod     │                          │
│               │     Reconciler          │                          │
│               └────────────┬────────────┘                          │
│                            │                                       │
│              ┌─────────────┼─────────────┐                         │
│              ▼             ▼             ▼                          │
│        ┌──────────┐ ┌──────────┐ ┌────────────┐                   │
│        │Shadow Pod│ │Inference │ │ Annotation │                    │
│        │          │ │Pod Status│ │            │                    │
│        │llm-mocker│ │podIP=real│ │shadow-pod  │                    │
│        │on real   │ │Running   │ │  -ref      │                    │
│        │AKS node  │ │Ready=True│ │            │                    │
│        └──────────┘ └──────────┘ └────────────┘                   │
│                            │                                       │
│               KAITO sees inference pod running                     │
│               → traffic hits llm-mocker via real IP                │
└─────────────────────────────────────────────────────────────────────┘
```

## Phase 1 — Fake the infrastructure (NodeClaimReconciler)

- **Creates a fake Node** for each Karpenter NodeClaim — with `providerID: fake://...`, workspace labels, instance-type labels, `sku=gpu` taint, and `nvidia.com/gpu` in capacity. This makes KAITO think a GPU VM exists.
- **Patches the NodeClaim status** — sets `nodeName`, `providerID`, `Ready=True`, `Registered=True`, `Initialized=True`. This tells KAITO the NodeClaim is fulfilled so it proceeds to create inference pods.
- **Maintains a Lease heartbeat** — creates a Lease in `kube-node-lease` and renews it every 10 seconds in a background goroutine. This prevents the node-lifecycle-controller from marking the fake node as Unknown.

## Phase 2 — Fake the workload (ShadowPodReconciler)

- **Creates a shadow pod** for each inference pod that's Pending on a fake node — the shadow pod runs the `llm-mocker` image on a real AKS node and gets a real CNI IP.
- **Patches the inference pod's status** — copies the shadow pod's IP into the inference pod's `status.podIP`, sets `phase=Running`, `conditions[Ready]=True`, and builds fake `containerStatuses`. This makes KAITO think the inference pod is running.
- **Annotates the inference pod** with `kaito.sh/shadow-pod-ref` pointing to the shadow pod, so future reconciles can correlate them.
