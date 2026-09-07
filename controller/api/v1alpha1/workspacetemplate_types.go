package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ResourceList is a cpu/memory requests-or-limits pair. Values are validated as
// plain strings (not resource.Quantity) to mirror the CRD schema's regex
// patterns exactly (crd-workspacetemplate.yaml); parsing into resource.Quantity
// happens where the value is consumed (pod template construction).
type ResourceList struct {
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?(m)?$`
	CPU string `json:"cpu"`
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|Pi|Ei|k|M|G|T|P|E)?$`
	Memory string `json:"memory"`
}

// WorkspaceResources holds requests/limits for a workspace Pod.
type WorkspaceResources struct {
	Requests ResourceList `json:"requests"`
	Limits   ResourceList `json:"limits"`
}

// WorkspaceStorage configures the working-directory PVC. local-path is
// non-expandable, so Size is the effective ceiling for the workspace's
// lifetime (C-7).
type WorkspaceStorage struct {
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|Pi|Ei|k|M|G|T|P|E)?$`
	// +kubebuilder:default="20Gi"
	// +optional
	Size string `json:"size,omitempty"`

	// NodeDiskBudget bounds the total working-directory disk usage this
	// template's NodeName may carry across all workspaces pinned to it
	// (15.11). Real disk usage isn't observable from the Kubernetes API alone
	// (no metrics-server dependency here), so the admission check approximates
	// it deterministically as the sum of Size declared by every non-Failed,
	// non-duplicate workspace already targeting that node, plus the new
	// request. Empty means no budget is enforced.
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|Pi|Ei|k|M|G|T|P|E)?$`
	// +kubebuilder:default="100Gi"
	// +optional
	NodeDiskBudget string `json:"nodeDiskBudget,omitempty"`
}

// WorkspaceDatabaseRef references the branch-dedicated CNPG instance.
type WorkspaceDatabaseRef struct {
	// +kubebuilder:validation:MinLength=1
	ClusterRef string `json:"clusterRef"`
}

// WorkspaceAuthRef references the Secret holding Claude Code's long-lived
// credential (5.1).
type WorkspaceAuthRef struct {
	// +kubebuilder:validation:MinLength=1
	SecretRef string `json:"secretRef"`
}

// WorkspaceEvacuation configures the off-node backup destination.
type WorkspaceEvacuation struct {
	// +kubebuilder:validation:MinLength=1
	Bucket string `json:"bucket"`

	// Endpoint is the destination's S3-compatible API endpoint (e.g.
	// http://host:port), not fixed to any particular implementation.
	// +kubebuilder:validation:MinLength=1
	Endpoint string `json:"endpoint"`

	// Region is the bucket's region.
	// +kubebuilder:validation:MinLength=1
	Region string `json:"region"`

	// SecretRef names the Secret holding the destination's S3 credentials
	// (keys access-key and secret-key). Only the workspace Pod reads them:
	// the control plane asks for an evacuation but never performs one.
	// +kubebuilder:validation:MinLength=1
	SecretRef string `json:"secretRef"`
}

// WorkspaceTemplateSpec mirrors crd-workspacetemplate.yaml. It is the operator-
// authored, GitOps-managed provisioning blueprint (design.md #Logical Data
// Model).
type WorkspaceTemplateSpec struct {
	// Image is the base image, referenced by immutable version identifier
	// (digest), never a mutable tag (15.2).
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	Resources WorkspaceResources `json:"resources"`

	// +kubebuilder:default={size: "20Gi"}
	// +optional
	Storage WorkspaceStorage `json:"storage,omitempty"`

	// NodeName is the placement node. local-path PVCs are node-pinned, so a
	// template using one has to set this explicitly; it cannot change after a
	// workspace has been provisioned from this template. Empty imposes no
	// placement constraint and leaves it to the scheduler.
	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// +kubebuilder:default="devplatform-workspace"
	// +optional
	PriorityClassName string `json:"priorityClassName,omitempty"`

	// Model is the default model workspaces provisioned from this template use
	// (7.3). Empty leaves Claude Code's own default in effect.
	// +optional
	Model string `json:"model,omitempty"`

	// Database references the branch-dedicated database cluster. nil means
	// this template provisions workspaces without a branch database at all
	// (Requirement 3.5): resources.go omits the connection env/volume/
	// bootstrap init container and the Workspace Controller never calls its
	// DatabaseAdapter for it.
	// +optional
	Database *WorkspaceDatabaseRef `json:"database,omitempty"`

	Auth WorkspaceAuthRef `json:"auth"`

	Evacuation WorkspaceEvacuation `json:"evacuation"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=workspacetemplates,scope=Namespaced,shortName=wst
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// WorkspaceTemplate is the operator-configured blueprint a Workspace is
// provisioned from.
type WorkspaceTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec WorkspaceTemplateSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// WorkspaceTemplateList contains a list of WorkspaceTemplate.
type WorkspaceTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkspaceTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&WorkspaceTemplate{}, &WorkspaceTemplateList{})
}
