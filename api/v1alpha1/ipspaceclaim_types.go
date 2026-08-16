// Package v1alpha1 contains API schema definitions for the infoblox.udaykishore.dev
// group, starting with IPSpaceClaim: a Kubernetes-native declarative representation
// of an Infoblox Universal DDI IP space / address block allocation.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IPSpaceClaimSpec describes the desired IP allocation an application or
// platform team wants sourced from Infoblox Universal DDI.
type IPSpaceClaimSpec struct {
	// IPSpaceName is the name of the Infoblox IP Space (e.g. "prod-eks-us-east-1")
	// that this claim should be allocated from. Maps to /api/ddi/v1/ipam/ip_space.
	// +kubebuilder:validation:Required
	IPSpaceName string `json:"ipSpaceName"`

	// CIDRSize is the requested prefix length for the allocated address block,
	// e.g. 28 for a /28. Mutually exclusive with FixedCIDR.
	// +optional
	CIDRSize *int32 `json:"cidrSize,omitempty"`

	// FixedCIDR requests a specific, pre-known CIDR rather than "next available".
	// Useful for migrating existing statically-assigned ranges under operator management.
	// +optional
	FixedCIDR string `json:"fixedCIDR,omitempty"`

	// Tags are propagated to Infoblox as Extensible Attributes for traceability
	// (cluster name, namespace, owning team, cost center).
	// +optional
	Tags map[string]string `json:"tags,omitempty"`

	// ReclaimPolicy controls what happens to the Infoblox-side allocation when
	// this CRD is deleted. "Release" (default) frees it back to Infoblox.
	// "Retain" leaves the allocation intact for manual cleanup.
	// +kubebuilder:validation:Enum=Release;Retain
	// +kubebuilder:default=Release
	ReclaimPolicy string `json:"reclaimPolicy,omitempty"`
}

// IPSpaceClaimPhase represents the reconciliation state machine.
type IPSpaceClaimPhase string

const (
	PhasePending   IPSpaceClaimPhase = "Pending"
	PhaseBound     IPSpaceClaimPhase = "Bound"
	PhaseDrifted   IPSpaceClaimPhase = "Drifted"
	PhaseReleasing IPSpaceClaimPhase = "Releasing"
	PhaseFailed    IPSpaceClaimPhase = "Failed"
)

// IPSpaceClaimStatus reflects the observed state of the allocation in Infoblox.
type IPSpaceClaimStatus struct {
	// Phase is the current lifecycle state of this claim.
	Phase IPSpaceClaimPhase `json:"phase,omitempty"`

	// AllocatedCIDR is the CIDR actually granted by Infoblox.
	AllocatedCIDR string `json:"allocatedCIDR,omitempty"`

	// InfobloxRef is the resource ID returned by the DDI v1 API
	// (e.g. "ipam/address_block/<uuid>"), used for update/delete calls
	// and for detecting out-of-band edits made in the Infoblox portal.
	InfobloxRef string `json:"infobloxRef,omitempty"`

	// LastReconciledGeneration lets the controller skip no-op reconciles.
	LastReconciledGeneration int64 `json:"lastReconciledGeneration,omitempty"`

	// LastDriftCheckTime records the last successful drift-detection poll
	// against the Infoblox API.
	LastDriftCheckTime *metav1.Time `json:"lastDriftCheckTime,omitempty"`

	// Conditions follow the standard Kubernetes condition pattern
	// (Ready, DriftDetected, etc.) for tooling / kubectl wait compatibility.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="IPSpace",type=string,JSONPath=`.spec.ipSpaceName`
// +kubebuilder:printcolumn:name="CIDR",type=string,JSONPath=`.status.allocatedCIDR`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// IPSpaceClaim is the schema for requesting and tracking an Infoblox
// Universal DDI address block allocation as a native Kubernetes object.
type IPSpaceClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IPSpaceClaimSpec   `json:"spec,omitempty"`
	Status IPSpaceClaimStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// IPSpaceClaimList contains a list of IPSpaceClaim.
type IPSpaceClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IPSpaceClaim `json:"items"`
}
