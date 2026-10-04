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

// RuntimeSnapshotScope selects how much Actor state a snapshot captures.
//
// +kubebuilder:validation:Enum=Data;Full
type RuntimeSnapshotScope string

const (
	// RuntimeSnapshotScopeData captures only the durable /data directory. The
	// Actor restarts from its golden image, so processes do not survive.
	RuntimeSnapshotScopeData RuntimeSnapshotScope = "Data"
	// RuntimeSnapshotScopeFull also captures guest memory and the root
	// filesystem, so running processes survive the snapshot. Snapshots are
	// larger and slower to take and restore.
	RuntimeSnapshotScopeFull RuntimeSnapshotScope = "Full"
)

// RuntimeSnapshotPolicy configures storage and scope for Substrate snapshots.
type RuntimeSnapshotPolicy struct {
	// Location is the snapshot storage location used by Substrate.
	// +kubebuilder:validation:Pattern=`^[^[:space:]]+$`
	// +required
	Location string `json:"location"`

	// OnQuiesce selects the snapshot scope taken when an idle Actor suspends or
	// a Session is explicitly suspended. It defaults to Data. Full keeps
	// processes such as a dev server alive across suspension, at the cost of
	// larger snapshots and slower suspend and wake. Snapshots taken while a
	// task waits for input are always Full.
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
