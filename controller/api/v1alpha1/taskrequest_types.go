package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TaskPhase is a queued task's state. The queue itself is the set of
// TaskRequest objects (design.md #Task Queue and Quota Governor), so the API
// server — not the control plane's memory — is what survives a restart.
type TaskPhase string

const (
	TaskPhasePending TaskPhase = "Pending"
	TaskPhaseRunning TaskPhase = "Running"
	// TaskPhaseVerifying and TaskPhaseHumanIntervention still occupy the
	// workspace's session, so they keep consuming a concurrency slot.
	TaskPhaseVerifying         TaskPhase = "Verifying"
	TaskPhaseHumanIntervention TaskPhase = "HumanIntervention"
	TaskPhaseCompleted         TaskPhase = "Completed"
	TaskPhaseFailed            TaskPhase = "Failed"
)

// TaskRequestSpec mirrors apps/devplatform/templates/crd-taskrequest.yaml spec
// schema.
type TaskRequestSpec struct {
	// WorkspaceRef names the Workspace this task runs in. Dispatch waits for
	// that workspace to reach Ready.
	// +kubebuilder:validation:MinLength=1
	WorkspaceRef string `json:"workspaceRef"`
}

// TaskRequestStatus mirrors apps/devplatform/templates/crd-taskrequest.yaml
// status schema. Pending -> Running is written exclusively by the task queue;
// every later transition is written by whoever runs and verifies the task.
type TaskRequestStatus struct {
	// +kubebuilder:validation:Enum=Pending;Running;Verifying;HumanIntervention;Completed;Failed
	// +kubebuilder:default=Pending
	// +optional
	Phase TaskPhase `json:"phase,omitempty"`

	// Conditions carry the Dispatched condition, whose false Reason records
	// why a task is still queued (7.12).
	// +optional
	// +kubebuilder:default={}
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=taskrequests,scope=Namespaced,shortName=tr
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Workspace",type=string,JSONPath=`.spec.workspaceRef`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// TaskRequest is one entry in the Claude Code task queue. Instances are
// created at runtime by callers (e.g. Hermes Agent) and are not GitOps-managed.
type TaskRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TaskRequestSpec   `json:"spec"`
	Status TaskRequestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TaskRequestList contains a list of TaskRequest.
type TaskRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TaskRequest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TaskRequest{}, &TaskRequestList{})
}
