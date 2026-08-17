// DNSRecordClaim extends this operator beyond IPAM into DNS: it lets a team
// declare a DNS record the same way IPSpaceClaim lets them declare an
// address block, reconciled against the same Infoblox Universal DDI control
// plane. Infoblox's own Universal DDI Management already fans a single
// record out across Route 53, Azure DNS, and Google Cloud DNS (see the
// "Multi-cloud DNS management" panel in docs/architecture-aws.svg) — this
// CRD is a thin, declarative front door onto that existing capability, not
// a reimplementation of multi-cloud DNS sync. The operator talks only to
// Infoblox's DDI v1 API; Infoblox's own control plane does the fan-out.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DNSRecordClaimSpec describes the desired DNS record.
type DNSRecordClaimSpec struct {
	// Zone is the DNS zone this record belongs to, e.g. "example.com".
	// +kubebuilder:validation:Required
	Zone string `json:"zone"`

	// Name is the record's hostname within the zone, e.g. "checkout" for
	// checkout.example.com. Use "@" for the zone apex.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// RecordType is the DNS record type.
	// +kubebuilder:validation:Enum=A;CNAME;TXT
	// +kubebuilder:validation:Required
	RecordType string `json:"recordType"`

	// Value is the record's target — an IP for A, a hostname for CNAME,
	// free text for TXT.
	// +kubebuilder:validation:Required
	Value string `json:"value"`

	// TTL in seconds. Defaults to 300 if unset.
	// +kubebuilder:default=300
	TTL int32 `json:"ttl,omitempty"`

	// Tags are propagated to Infoblox as Extensible Attributes, same as
	// IPSpaceClaim, for traceability back to the owning namespace/team.
	// +optional
	Tags map[string]string `json:"tags,omitempty"`

	// ReclaimPolicy mirrors IPSpaceClaim's: "Release" (default) deletes the
	// record in Infoblox when this CRD is deleted; "Retain" leaves it.
	// +kubebuilder:validation:Enum=Release;Retain
	// +kubebuilder:default=Release
	ReclaimPolicy string `json:"reclaimPolicy,omitempty"`
}

// DNSRecordClaimPhase mirrors IPSpaceClaimPhase's state machine.
type DNSRecordClaimPhase string

const (
	DNSPhasePending   DNSRecordClaimPhase = "Pending"
	DNSPhaseBound     DNSRecordClaimPhase = "Bound"
	DNSPhaseDrifted   DNSRecordClaimPhase = "Drifted"
	DNSPhaseReleasing DNSRecordClaimPhase = "Releasing"
	DNSPhaseFailed    DNSRecordClaimPhase = "Failed"
)

// DNSRecordClaimStatus reflects the observed state of the record in Infoblox.
type DNSRecordClaimStatus struct {
	Phase DNSRecordClaimPhase `json:"phase,omitempty"`

	// FQDN is the fully-qualified name actually created, e.g.
	// "checkout.example.com".
	FQDN string `json:"fqdn,omitempty"`

	// InfobloxRef is the DDI v1 resource ref (e.g. "dns/record/<uuid>"),
	// used for drift-check GETs and the release DELETE.
	InfobloxRef string `json:"infobloxRef,omitempty"`

	LastReconciledGeneration int64 `json:"lastReconciledGeneration,omitempty"`

	LastDriftCheckTime *metav1.Time `json:"lastDriftCheckTime,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Zone",type=string,JSONPath=`.spec.zone`
// +kubebuilder:printcolumn:name="FQDN",type=string,JSONPath=`.status.fqdn`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.recordType`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DNSRecordClaim is the schema for requesting and tracking an Infoblox
// Universal DDI-managed DNS record as a native Kubernetes object.
type DNSRecordClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DNSRecordClaimSpec   `json:"spec,omitempty"`
	Status DNSRecordClaimStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DNSRecordClaimList contains a list of DNSRecordClaim.
type DNSRecordClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSRecordClaim `json:"items"`
}
