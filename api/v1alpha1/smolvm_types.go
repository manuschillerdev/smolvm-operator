/*
Copyright 2026.

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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	SmolVMFinalizer = "smolvms.vm.smolvm.dev/finalizer"

	ConditionReady        = "Ready"
	ConditionRuntimeReady = "RuntimeReady"
	ConditionGuestReady   = "GuestReady"
	ConditionReconciled   = "Reconciled"
)

// SmolVMSpec defines the desired state of a smolvm machine.
// +kubebuilder:validation:XValidation:rule="(has(self.image) && size(self.image) > 0) != (has(self.from) && size(self.from) > 0)",message="exactly one of spec.image or spec.from is required"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.nodeName) || (has(self.nodeName) && self.nodeName == oldSelf.nodeName)",message="spec.nodeName is immutable after binding"
type SmolVMSpec struct {
	// Running declares whether the machine should be running.
	//+kubebuilder:default=true
	Running bool `json:"running,omitempty"`

	// NodeName pins the machine to a Kubernetes node. Machines are node-local and
	// must not move implicitly after creation.
	//+optional
	NodeName string `json:"nodeName,omitempty"`

	// Image is an OCI image to run inside the smolvm machine.
	//+kubebuilder:validation:MinLength=1
	//+optional
	Image string `json:"image,omitempty"`

	// From is a path to a .smolmachine sidecar artifact available to the node runtime.
	// It is mutually exclusive with Image.
	//+kubebuilder:validation:MinLength=1
	//+optional
	From string `json:"from,omitempty"`

	// Resources configures VM CPU and memory.
	//+optional
	Resources SmolVMResources `json:"resources,omitempty"`

	// Storage configures persistent local machine disks.
	//+optional
	Storage SmolVMStorage `json:"storage,omitempty"`

	// Network configures outbound networking and host port mappings.
	//+optional
	Network SmolVMNetwork `json:"network,omitempty"`
}

// SmolVMResources configures VM compute resources.
type SmolVMResources struct {
	// CPUs is the requested vCPU count.
	//+kubebuilder:validation:Minimum=1
	//+optional
	CPUs int32 `json:"cpus,omitempty"`

	// MemoryMiB is the requested memory ceiling in MiB.
	//+kubebuilder:validation:Minimum=64
	//+optional
	MemoryMiB int32 `json:"memoryMiB,omitempty"`
}

// SmolVMStorage configures persistent local smolvm disks.
type SmolVMStorage struct {
	// StorageGiB is the storage disk size. It can only grow.
	//+kubebuilder:validation:Minimum=1
	//+optional
	StorageGiB int64 `json:"storageGiB,omitempty"`

	// OverlayGiB is the overlay disk size. It can only grow.
	//+kubebuilder:validation:Minimum=1
	//+optional
	OverlayGiB int64 `json:"overlayGiB,omitempty"`
}

// SmolVMNetwork configures the currently supported smolvm networking model.
type SmolVMNetwork struct {
	// Enabled enables outbound TCP/UDP networking.
	//+optional
	Enabled bool `json:"enabled,omitempty"`

	// AllowedCIDRs restricts egress to the listed CIDR ranges when supported by the runtime.
	//+kubebuilder:validation:items:Format=cidr
	//+optional
	AllowedCIDRs []string `json:"allowedCIDRs,omitempty"`

	// Ports maps host ports to guest ports on the selected node.
	//+listType=map
	//+listMapKey=hostPort
	//+optional
	Ports []SmolVMPort `json:"ports,omitempty"`
}

// SmolVMPort maps a node host port to a guest port.
type SmolVMPort struct {
	// HostPort is the TCP port opened on the node.
	//+kubebuilder:validation:Minimum=1
	//+kubebuilder:validation:Maximum=65535
	HostPort int32 `json:"hostPort"`

	// GuestPort is the TCP port inside the VM.
	//+kubebuilder:validation:Minimum=1
	//+kubebuilder:validation:Maximum=65535
	GuestPort int32 `json:"guestPort"`
}

// SmolVMStatus defines the observed state of a smolvm machine.
type SmolVMStatus struct {
	// Phase is a compact machine phase such as Pending, Created, Running, Stopped, Failed, or Unknown.
	//+optional
	Phase string `json:"phase,omitempty"`

	// NodeName is the node currently owning this machine's local state.
	//+optional
	NodeName string `json:"nodeName,omitempty"`

	// MachineName is the stable smolvm runtime name owned by this CR.
	//+optional
	MachineName string `json:"machineName,omitempty"`

	// RuntimePID is the local process ID reported by the smolvm runtime when available.
	//+optional
	RuntimePID *int32 `json:"runtimePID,omitempty"`

	// Network is the runtime-observed network enablement.
	//+optional
	Network bool `json:"network,omitempty"`

	// Ports are the runtime-observed host-to-guest port mappings.
	//+optional
	Ports []SmolVMPort `json:"ports,omitempty"`

	// StorageGiB is the runtime-observed storage disk size.
	//+optional
	StorageGiB *int64 `json:"storageGiB,omitempty"`

	// OverlayGiB is the runtime-observed overlay disk size.
	//+optional
	OverlayGiB *int64 `json:"overlayGiB,omitempty"`

	// ObservedGeneration is the metadata generation last reconciled by the controller.
	//+optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions describe reconciliation and readiness.
	//+optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.status.nodeName`
//+kubebuilder:printcolumn:name="Machine",type=string,JSONPath=`.status.machineName`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SmolVM is the Schema for the smolvms API.
type SmolVM struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SmolVMSpec   `json:"spec,omitempty"`
	Status SmolVMStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// SmolVMList contains a list of SmolVM.
type SmolVMList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SmolVM `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SmolVM{}, &SmolVMList{})
}
