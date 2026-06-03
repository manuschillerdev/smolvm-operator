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

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	ConditionScheduled       = "Scheduled"
	ConditionDeletionBlocked = "DeletionBlocked"

	SmolVMNodeConditionReady        = "Ready"
	SmolVMNodeConditionKVMAvailable = "KVMAvailable"
	SmolVMNodeConditionRuntimeReady = "RuntimeReady"
	SmolVMNodeConditionSchedulable  = "Schedulable"

	ForceDeleteLocalStateAnnotation = "vm.smolvm.dev/force-delete-local-state"
)

// SmolVMNodeStatus defines node-local runtime capability and endpoint status.
type SmolVMNodeStatus struct {
	// NodeUID is the Kubernetes Node UID observed by the node runtime.
	//+optional
	NodeUID string `json:"nodeUID,omitempty"`

	// RuntimeVersion is the smolvm runtime agent version.
	//+optional
	RuntimeVersion string `json:"runtimeVersion,omitempty"`

	// ProtocolVersion is the runtime API protocol version.
	//+optional
	ProtocolVersion string `json:"protocolVersion,omitempty"`

	// HeartbeatTime is the last time the runtime reported node status.
	//+optional
	HeartbeatTime *metav1.Time `json:"heartbeatTime,omitempty"`

	// Endpoint is the runtime API endpoint for this node.
	//+optional
	Endpoint SmolVMNodeEndpoint `json:"endpoint,omitempty"`

	// Allocatable reports VM resources available to smolvm on this node.
	//+optional
	Allocatable SmolVMNodeAllocatable `json:"allocatable,omitempty"`

	// Conditions advertise runtime readiness and scheduling capability.
	//+optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SmolVMNodeEndpoint identifies the node runtime API endpoint.
type SmolVMNodeEndpoint struct {
	// PodName is the runtime DaemonSet pod name.
	//+optional
	PodName string `json:"podName,omitempty"`

	// PodNamespace is the runtime DaemonSet pod namespace.
	//+optional
	PodNamespace string `json:"podNamespace,omitempty"`

	// PodIP is the runtime pod IP address.
	//+optional
	PodIP string `json:"podIP,omitempty"`

	// Port is the authenticated runtime API port.
	//+optional
	Port int32 `json:"port,omitempty"`
}

// SmolVMNodeAllocatable reports VM resource capacity.
type SmolVMNodeAllocatable struct {
	// CPUs is the available vCPU count.
	//+optional
	CPUs int32 `json:"cpus,omitempty"`

	// MemoryMiB is the available memory in MiB.
	//+optional
	MemoryMiB int64 `json:"memoryMiB,omitempty"`

	// StorageGiB is the available local storage in GiB.
	//+optional
	StorageGiB int64 `json:"storageGiB,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster
//+kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
//+kubebuilder:printcolumn:name="Runtime",type=string,JSONPath=`.status.runtimeVersion`
//+kubebuilder:printcolumn:name="Heartbeat",type=date,JSONPath=`.status.heartbeatTime`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SmolVMNode is the Schema for node-local smolvm runtime capability.
type SmolVMNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Status SmolVMNodeStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// SmolVMNodeList contains a list of SmolVMNode.
type SmolVMNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SmolVMNode `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SmolVMNode{}, &SmolVMNodeList{})
}
