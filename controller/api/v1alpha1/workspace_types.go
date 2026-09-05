package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WorkspacePhase is the workspace state machine's current value. status.phase is
// the sole source of truth (design.md #State Management); no separate database
// tracks workspace state.
type WorkspacePhase string

const (
	WorkspacePhaseProvisioning WorkspacePhase = "Provisioning"
	WorkspacePhaseReady        WorkspacePhase = "Ready"
	WorkspacePhaseSuspended    WorkspacePhase = "Suspended"
	WorkspacePhaseFailed       WorkspacePhase = "Failed"
	WorkspacePhaseTerminating  WorkspacePhase = "Terminating"
)

// DesiredPhase is the subset of WorkspacePhase a caller may request via
// spec.desiredPhase (crd-workspace.yaml).
type DesiredPhase string

const (
	DesiredPhaseReady     DesiredPhase = "Ready"
	DesiredPhaseSuspended DesiredPhase = "Suspended"
)

// WorkspaceSpec mirrors apps/devplatform/templates/crd-workspace.yaml spec schema.
type WorkspaceSpec struct {
	// Repository is the target git repository.
	// +kubebuilder:validation:MinLength=1
	Repository string `json:"repository"`

	// Branch is the target branch.
	// +kubebuilder:validation:MinLength=1
	Branch string `json:"branch"`

	// BaseBranch is the branch to fork from when Branch does not exist on the
	// remote yet (1.5).
	// +optional
	BaseBranch *string `json:"baseBranch,omitempty"`

	// TemplateRef names the WorkspaceTemplate to provision from.
	// +kubebuilder:validation:MinLength=1
	TemplateRef string `json:"templateRef"`

	// DesiredPhase is the caller-requested target state. Suspend is requested by
	// switching this to Suspended (13.1-13.4).
	// +kubebuilder:validation:Enum=Ready;Suspended
	// +kubebuilder:default=Ready
	// +optional
	DesiredPhase DesiredPhase `json:"desiredPhase,omitempty"`
}

// WorkspaceURLs holds the externally reachable endpoints exposed once a
// workspace reaches Ready.
type WorkspaceURLs struct {
	// Preview is the application preview URL.
	// +optional
	Preview string `json:"preview,omitempty"`
	// Session is the execution session connection URL (1.7).
	// +optional
	Session string `json:"session,omitempty"`
	// Report is the test report URL.
	// +optional
	Report string `json:"report,omitempty"`
}

// EvacuationSnapshot records the most recent off-node backup of the working
// directory.
type EvacuationSnapshot struct {
	WorkspaceId     string       `json:"workspaceId,omitempty"`
	Branch          string       `json:"branch,omitempty"`
	HeadCommit      string       `json:"headCommit,omitempty"`
	BundleKey       string       `json:"bundleKey,omitempty"`
	DirtyArchiveKey string       `json:"dirtyArchiveKey,omitempty"`
	CapturedAt      *metav1.Time `json:"capturedAt,omitempty"`
	// +kubebuilder:validation:Minimum=0
	SizeBytes int64 `json:"sizeBytes,omitempty"`
}

// BlackboardEntry is this branch's entry in the Agent Blackboard.
type BlackboardEntry struct {
	Branch           string       `json:"branch,omitempty"`
	Summary          string       `json:"summary,omitempty"`
	ChangedFiles     []string     `json:"changedFiles,omitempty"`
	PublicInterfaces []string     `json:"publicInterfaces,omitempty"`
	Active           bool         `json:"active,omitempty"`
	UpdatedAt        *metav1.Time `json:"updatedAt,omitempty"`
}

// WorkspaceStatus mirrors apps/devplatform/templates/crd-workspace.yaml status
// schema. It is written exclusively by the Workspace Controller (single-writer
// principle).
type WorkspaceStatus struct {
	// +kubebuilder:validation:Enum=Provisioning;Ready;Suspended;Failed;Terminating
	// +kubebuilder:default=Provisioning
	// +optional
	Phase WorkspacePhase `json:"phase,omitempty"`

	// WorkspaceId is the workspace identifier (1.7), also used as the DNS-safe
	// base name for owned resources.
	// +optional
	WorkspaceId string `json:"workspaceId,omitempty"`

	// +optional
	Urls WorkspaceURLs `json:"urls,omitempty"`

	// SessionId is the execution session identifier (1.7).
	// +optional
	SessionId string `json:"sessionId,omitempty"`

	// LastActivityAt is the idle-detection reference time (4.8 / 13.1).
	// +optional
	LastActivityAt *metav1.Time `json:"lastActivityAt,omitempty"`

	// +optional
	LastEvacuation *EvacuationSnapshot `json:"lastEvacuation,omitempty"`

	// +optional
	Blackboard *BlackboardEntry `json:"blackboard,omitempty"`

	// Conditions record per-dependent-resource state and failure reasons (1.8).
	// +optional
	// +kubebuilder:default={}
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=workspaces,scope=Namespaced,shortName=ws
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Branch",type=string,JSONPath=`.spec.branch`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Workspace is the per-branch isolated development workspace request and its
// reconciled state. Instances are created at runtime by callers (e.g. Hermes
// Agent) and are not GitOps-managed (17.4).
type Workspace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkspaceSpec   `json:"spec"`
	Status WorkspaceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WorkspaceList contains a list of Workspace.
type WorkspaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Workspace `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Workspace{}, &WorkspaceList{})
}
