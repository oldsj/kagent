// Copyright 2026 The kagent Authors
// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import corev1 "k8s.io/api/core/v1"

// RuntimeEnvVar configures one literal runtime environment variable.
type RuntimeEnvVar struct {
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// Value is a literal value, including an empty string.
	// +required
	Value string `json:"value"`

	// Deferred until secret-backed environment injection is supported.
	// CredentialRef *corev1.SecretKeySelector `json:"credentialRef,omitempty"`
}

// RuntimeSnapshotScope selects what a runtime snapshot keeps.
// +kubebuilder:validation:Enum=Data;Full
type RuntimeSnapshotScope string

const (
	// RuntimeSnapshotScopeData keeps the runtime's durable data. Processes
	// start again when the runtime wakes.
	RuntimeSnapshotScopeData RuntimeSnapshotScope = "Data"
	// RuntimeSnapshotScopeFull also keeps the root filesystem and memory, so
	// processes resume where they stopped.
	RuntimeSnapshotScopeFull RuntimeSnapshotScope = "Full"
)

// RuntimeSnapshotPolicy configures storage for Substrate snapshots.
type RuntimeSnapshotPolicy struct {
	// Location is the snapshot storage location used by Substrate.
	// +kubebuilder:validation:Pattern=`^[^[:space:]]+$`
	// +required
	Location string `json:"location"`

	// OnQuiesce selects what a snapshot keeps when an idle runtime suspends.
	// It defaults to Data. Full keeps processes that are running at
	// suspension alive across it, at the cost of larger snapshots and slower
	// suspend and wake. Harnesses that end their process tree after each turn
	// keep no turn-started processes either way. Substrate keeps memory on a best-effort basis: it may capture
	// less under host pressure, and it wakes a runtime without its memory when
	// the snapshot cannot resume on the assigned worker. A checkpoint taken
	// with Full cannot be forked.
	// +optional
	OnQuiesce RuntimeSnapshotScope `json:"onQuiesce,omitempty"`
}

// RuntimeSubstratePolicy contains the Substrate policy shared by all runtime variants.
//
// +kubebuilder:validation:XValidation:rule="self.workerPoolRef.name.size() > 0",message="workerPoolRef name must not be empty"
type RuntimeSubstratePolicy struct {
	// WorkerPoolRef references a WorkerPool in the resource's namespace.
	// +required
	WorkerPoolRef corev1.LocalObjectReference `json:"workerPoolRef"`

	// SnapshotPolicy configures runtime snapshot storage.
	// +required
	SnapshotPolicy RuntimeSnapshotPolicy `json:"snapshotPolicy"`
}
