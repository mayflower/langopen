package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type AgentDeploymentSpec struct {
	Image            string   `json:"image"`
	RollbackImage    string   `json:"rollbackImage,omitempty"`
	Replicas         *int32   `json:"replicas,omitempty"`
	RuntimeClassName string   `json:"runtimeClassName,omitempty"`
	Mode             string   `json:"mode,omitempty"`
	EgressAllowlist  []string `json:"egressAllowlist,omitempty"`
	SandboxTemplate  string   `json:"sandboxTemplate,omitempty"`
	WarmPoolReplicas *int32   `json:"warmPoolReplicas,omitempty"`
}

type AgentDeploymentStatus struct {
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	Ready              bool   `json:"ready"`
	Message            string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type AgentDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentDeploymentSpec   `json:"spec,omitempty"`
	Status AgentDeploymentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AgentDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentDeployment `json:"items"`
}

func (in *AgentDeployment) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(AgentDeployment)
	*out = *in
	out.TypeMeta = in.TypeMeta
	out.ObjectMeta = *in.ObjectMeta.DeepCopy()
	if in.Spec.Replicas != nil {
		replicas := *in.Spec.Replicas
		out.Spec.Replicas = &replicas
	}
	if len(in.Spec.EgressAllowlist) > 0 {
		out.Spec.EgressAllowlist = append([]string(nil), in.Spec.EgressAllowlist...)
	}
	if in.Spec.WarmPoolReplicas != nil {
		replicas := *in.Spec.WarmPoolReplicas
		out.Spec.WarmPoolReplicas = &replicas
	}
	return out
}

func (in *AgentDeploymentList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(AgentDeploymentList)
	*out = *in
	out.TypeMeta = in.TypeMeta
	out.ListMeta = in.ListMeta
	if len(in.Items) > 0 {
		out.Items = make([]AgentDeployment, len(in.Items))
		copy(out.Items, in.Items)
	}
	return out
}
