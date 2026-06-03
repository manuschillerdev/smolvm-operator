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

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	vmv1alpha1 "github.com/manuschillerdev/smolvm-operator/api/v1alpha1"
	smolvmapi "github.com/manuschillerdev/smolvm-operator/internal/smolvm"
)

const requeueAfter = 10 * time.Second

// SmolVMReconciler reconciles a SmolVM object.
type SmolVMReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	RuntimeFactory func() SmolVMRuntime
	Recorder       record.EventRecorder
}

type SmolVMRuntime interface {
	Health(context.Context) (*smolvmapi.Health, error)
	GetIdentity(context.Context) (*smolvmapi.Identity, error)
	Capabilities(context.Context) (*smolvmapi.Capabilities, error)
	ListMachines(context.Context) ([]smolvmapi.MachineInfo, error)
	GetMachine(context.Context, string) (*smolvmapi.MachineInfo, error)
	CreateMachine(context.Context, smolvmapi.CreateMachineRequest) (*smolvmapi.MachineInfo, error)
	EnsureMachineRunning(context.Context, string) error
	StopMachine(context.Context, string) error
	DeleteMachine(context.Context, string) error
	ResizeMachine(context.Context, string, smolvmapi.ResizeRequest) error
}

func (r *SmolVMReconciler) runtimeClient() SmolVMRuntime {
	if r.RuntimeFactory != nil {
		return r.RuntimeFactory()
	}
	return smolvmapi.NewClient(os.Getenv("SMOLVM_API_URL"), os.Getenv("SMOLVM_API_SOCKET"))
}

func (r *SmolVMReconciler) runtimeForNode(ctx context.Context, nodeName string) (SmolVMRuntime, error) {
	if r.RuntimeFactory != nil {
		return r.RuntimeFactory(), nil
	}
	var node vmv1alpha1.SmolVMNode
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		return nil, fmt.Errorf("get SmolVMNode %s: %w", nodeName, err)
	}
	if !smolVMNodeEligible(&node) {
		return nil, fmt.Errorf("SmolVMNode %s is not ready or heartbeat is stale", nodeName)
	}
	endpoint := node.Status.Endpoint
	if endpoint.PodIP == "" || endpoint.Port == 0 {
		return nil, fmt.Errorf("SmolVMNode %s has no runtime endpoint", nodeName)
	}
	api := smolvmapi.NewClientWithAuth(fmt.Sprintf("https://%s:%d", endpoint.PodIP, endpoint.Port), "", os.Getenv("SMOLVM_RUNTIME_TOKEN"), os.Getenv("SMOLVM_RUNTIME_INSECURE_SKIP_VERIFY") == "true")
	health, err := api.Health(ctx)
	if err != nil {
		return nil, fmt.Errorf("get runtime health for node %s: %w", nodeName, err)
	}
	if !health.OK {
		return nil, fmt.Errorf("runtime for node %s is unhealthy: %s", nodeName, health.Message)
	}
	identity, err := api.GetIdentity(ctx)
	if err != nil {
		return nil, fmt.Errorf("get runtime identity for node %s: %w", nodeName, err)
	}
	if identity.NodeName != nodeName || (node.Status.NodeUID != "" && identity.NodeUID != "" && identity.NodeUID != node.Status.NodeUID) {
		return nil, fmt.Errorf("runtime endpoint identity mismatch for node %s: got node=%s uid=%s", nodeName, identity.NodeName, identity.NodeUID)
	}
	capabilities, err := api.Capabilities(ctx)
	if err != nil {
		return nil, fmt.Errorf("get runtime capabilities for node %s: %w", nodeName, err)
	}
	if capabilities.ProtocolVersion != "" && capabilities.ProtocolVersion != node.Status.ProtocolVersion {
		return nil, fmt.Errorf("runtime protocol mismatch for node %s: endpoint=%s status=%s", nodeName, capabilities.ProtocolVersion, node.Status.ProtocolVersion)
	}
	return api, nil
}

//+kubebuilder:rbac:groups=vm.smolvm.dev,resources=smolvms,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=vm.smolvm.dev,resources=smolvms/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=vm.smolvm.dev,resources=smolvms/finalizers,verbs=update
//+kubebuilder:rbac:groups=vm.smolvm.dev,resources=smolvmnodes,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *SmolVMReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var vm vmv1alpha1.SmolVM
	if err := r.Get(ctx, req.NamespacedName, &vm); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	ownerNode := vm.Status.NodeName

	machineName := vm.Status.MachineName
	if machineName == "" {
		machineName = stableMachineName(&vm)
	}

	if !vm.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &vm, machineName, ownerNode)
	}

	if !controllerutil.ContainsFinalizer(&vm, vmv1alpha1.SmolVMFinalizer) {
		controllerutil.AddFinalizer(&vm, vmv1alpha1.SmolVMFinalizer)
		return ctrl.Result{}, r.Update(ctx, &vm)
	}

	if err := validateSpec(&vm); err != nil {
		logger.Info("invalid SmolVM spec", "error", err.Error())
		r.event(&vm, "Warning", "InvalidSpec", err.Error())
		return r.updateStatus(ctx, &vm, statusInput{
			Phase:       "Failed",
			NodeName:    ownerNode,
			MachineName: machineName,
			Ready:       metav1.ConditionFalse,
			ReadyReason: "InvalidSpec",
			ReadyMsg:    err.Error(),
			Recon:       metav1.ConditionFalse,
			ReconReason: "InvalidSpec",
			ReconMsg:    err.Error(),
		})
	}

	if ownerNode == "" {
		nodeName, msg, err := r.schedule(ctx, &vm)
		if err != nil {
			return ctrl.Result{}, err
		}
		if nodeName == "" {
			return r.updateStatus(ctx, &vm, statusInput{
				Phase:           "Pending",
				NodeName:        "",
				MachineName:     machineName,
				Scheduled:       metav1.ConditionFalse,
				ScheduledReason: "NoEligibleNodes",
				ScheduledMsg:    msg,
				Ready:           metav1.ConditionFalse,
				ReadyReason:     "AwaitingNodeAssignment",
				ReadyMsg:        msg,
				Recon:           metav1.ConditionFalse,
				ReconReason:     "AwaitingNodeAssignment",
				ReconMsg:        msg,
			})
		}
		_, statusErr := r.updateStatus(ctx, &vm, statusInput{
			Phase:           "Scheduled",
			NodeName:        nodeName,
			MachineName:     machineName,
			Scheduled:       metav1.ConditionTrue,
			ScheduledReason: "Bound",
			ScheduledMsg:    fmt.Sprintf("assigned to node %s", nodeName),
			Ready:           metav1.ConditionFalse,
			ReadyReason:     "Scheduled",
			ReadyMsg:        "waiting for node runtime reconciliation",
			Recon:           metav1.ConditionFalse,
			ReconReason:     "RuntimePending",
			ReconMsg:        "waiting for node runtime reconciliation",
		})
		if statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	api, err := r.runtimeForNode(ctx, ownerNode)
	if err != nil {
		return r.runtimeUnavailable(ctx, &vm, ownerNode, machineName, err)
	}

	machine, err := api.GetMachine(ctx, machineName)
	if smolvmapi.IsNotFound(err) {
		createReq := buildCreateRequest(&vm, machineName)
		logger.Info("creating smolvm machine", "machine", machineName)
		r.event(&vm, "Normal", "Creating", fmt.Sprintf("creating smolvm machine %s", machineName))
		machine, err = api.CreateMachine(ctx, createReq)
		if err != nil {
			return r.runtimeUnavailable(ctx, &vm, ownerNode, machineName, err)
		}
		_, statusErr := r.updateStatus(ctx, &vm, statusInput{
			Phase:       phaseFromMachine(machine),
			NodeName:    ownerNode,
			MachineName: machineName,
			Machine:     machine,
			Ready:       metav1.ConditionFalse,
			ReadyReason: "Created",
			ReadyMsg:    "smolvm machine has been created; waiting before start",
			Recon:       metav1.ConditionFalse,
			ReconReason: "StartPending",
			ReconMsg:    "waiting for smolvm create operation to release runtime locks",
		})
		if statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	if err != nil {
		return r.runtimeUnavailable(ctx, &vm, ownerNode, machineName, err)
	}

	if immutableErr := validateImmutableRuntimeFields(&vm, machine); immutableErr != nil {
		r.event(&vm, "Warning", "UnsupportedUpdate", immutableErr.Error())
		return r.updateStatus(ctx, &vm, statusInput{
			Phase:       phaseFromMachine(machine),
			NodeName:    ownerNode,
			MachineName: machineName,
			Machine:     machine,
			Ready:       readyFromMachine(machine),
			ReadyReason: "Observed",
			ReadyMsg:    fmt.Sprintf("smolvm machine is %s", machine.State),
			Recon:       metav1.ConditionFalse,
			ReconReason: "UnsupportedUpdate",
			ReconMsg:    immutableErr.Error(),
		})
	}

	if invalidResize := validateStorageDoesNotShrink(&vm, machine); invalidResize != nil {
		return r.updateStatus(ctx, &vm, statusInput{
			Phase:       phaseFromMachine(machine),
			NodeName:    ownerNode,
			MachineName: machineName,
			Machine:     machine,
			Ready:       readyFromMachine(machine),
			ReadyReason: "Observed",
			ReadyMsg:    fmt.Sprintf("smolvm machine is %s", machine.State),
			Recon:       metav1.ConditionFalse,
			ReconReason: "InvalidStorageResize",
			ReconMsg:    invalidResize.Error(),
		})
	}

	if needsStorageExpansion(&vm, machine) {
		if machine.State == "running" {
			return r.updateStatus(ctx, &vm, statusInput{
				Phase:       phaseFromMachine(machine),
				NodeName:    ownerNode,
				MachineName: machineName,
				Machine:     machine,
				Ready:       readyFromMachine(machine),
				ReadyReason: "ResizePending",
				ReadyMsg:    "storage expansion requires the machine to be stopped",
				Recon:       metav1.ConditionFalse,
				ReconReason: "ResizeRequiresStoppedMachine",
				ReconMsg:    "set spec.running=false before expanding storage",
			})
		}
		resize := buildResizeRequest(&vm)
		logger.Info("resizing smolvm machine", "machine", machineName)
		r.event(&vm, "Normal", "Resizing", fmt.Sprintf("resizing smolvm machine %s", machineName))
		if err := api.ResizeMachine(ctx, machineName, resize); err != nil {
			return r.runtimeUnavailable(ctx, &vm, ownerNode, machineName, err)
		}
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	if vm.Spec.Running && machine.State != "running" {
		logger.Info("starting smolvm machine", "machine", machineName, "state", machine.State)
		r.event(&vm, "Normal", "Starting", fmt.Sprintf("starting smolvm machine %s", machineName))
		if err := api.EnsureMachineRunning(ctx, machineName); err != nil {
			return r.runtimeUnavailable(ctx, &vm, ownerNode, machineName, err)
		}
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	if !vm.Spec.Running && machine.State == "running" {
		logger.Info("stopping smolvm machine", "machine", machineName)
		r.event(&vm, "Normal", "Stopping", fmt.Sprintf("stopping smolvm machine %s", machineName))
		if err := api.StopMachine(ctx, machineName); err != nil {
			return r.runtimeUnavailable(ctx, &vm, ownerNode, machineName, err)
		}
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	result, err := r.updateStatus(ctx, &vm, statusInput{
		Phase:       phaseFromMachine(machine),
		NodeName:    ownerNode,
		MachineName: machineName,
		Machine:     machine,
		Ready:       readyFromMachine(machine),
		ReadyReason: "Observed",
		ReadyMsg:    fmt.Sprintf("smolvm machine is %s", machine.State),
		Recon:       metav1.ConditionTrue,
		ReconReason: "Reconciled",
		ReconMsg:    "desired state matches smolvm runtime state",
	})
	if err != nil {
		return result, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *SmolVMReconciler) reconcileDelete(ctx context.Context, vm *vmv1alpha1.SmolVM, machineName, ownerNode string) (ctrl.Result, error) {
	if ownerNode == "" || machineName == "" {
		controllerutil.RemoveFinalizer(vm, vmv1alpha1.SmolVMFinalizer)
		return ctrl.Result{}, r.Update(ctx, vm)
	}

	api, err := r.runtimeForNode(ctx, ownerNode)
	if err != nil {
		if vm.Annotations[vmv1alpha1.ForceDeleteLocalStateAnnotation] == "true" {
			r.event(vm, "Warning", "ForceDeleteLocalState", fmt.Sprintf("removing finalizer while runtime for node %s is unavailable; local VM state may remain", ownerNode))
			controllerutil.RemoveFinalizer(vm, vmv1alpha1.SmolVMFinalizer)
			return ctrl.Result{}, r.Update(ctx, vm)
		}
		r.event(vm, "Warning", "DeletionBlocked", err.Error())
		_, statusErr := r.updateStatus(ctx, vm, statusInput{
			Phase:                 "Deleting",
			NodeName:              ownerNode,
			MachineName:           machineName,
			Scheduled:             metav1.ConditionTrue,
			ScheduledReason:       "Bound",
			ScheduledMsg:          fmt.Sprintf("assigned to node %s", ownerNode),
			Ready:                 metav1.ConditionFalse,
			ReadyReason:           "RuntimeUnavailable",
			ReadyMsg:              err.Error(),
			Recon:                 metav1.ConditionFalse,
			ReconReason:           "DeletionBlocked",
			ReconMsg:              err.Error(),
			DeletionBlocked:       metav1.ConditionTrue,
			DeletionBlockedReason: "RuntimeUnavailable",
			DeletionBlockedMsg:    fmt.Sprintf("owning node %s is unavailable; local VM state may still exist", ownerNode),
		})
		if statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	r.event(vm, "Normal", "Deleting", fmt.Sprintf("deleting smolvm machine %s", machineName))
	if err := api.DeleteMachine(ctx, machineName); err != nil && !smolvmapi.IsNotFound(err) {
		return r.runtimeUnavailable(ctx, vm, ownerNode, machineName, err)
	}
	controllerutil.RemoveFinalizer(vm, vmv1alpha1.SmolVMFinalizer)
	return ctrl.Result{}, r.Update(ctx, vm)
}

func (r *SmolVMReconciler) runtimeUnavailable(ctx context.Context, vm *vmv1alpha1.SmolVM, nodeName, machineName string, err error) (ctrl.Result, error) {
	r.event(vm, "Warning", "RuntimeUnavailable", err.Error())
	_, statusErr := r.updateStatus(ctx, vm, statusInput{
		Phase:       "Unknown",
		NodeName:    nodeName,
		MachineName: machineName,
		Ready:       metav1.ConditionFalse,
		ReadyReason: "RuntimeUnavailable",
		ReadyMsg:    err.Error(),
		Recon:       metav1.ConditionFalse,
		ReconReason: "RuntimeUnavailable",
		ReconMsg:    err.Error(),
	})
	if statusErr != nil {
		return ctrl.Result{}, statusErr
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

type statusInput struct {
	Phase                 string
	NodeName              string
	MachineName           string
	Machine               *smolvmapi.MachineInfo
	Scheduled             metav1.ConditionStatus
	ScheduledReason       string
	ScheduledMsg          string
	Ready                 metav1.ConditionStatus
	ReadyReason           string
	ReadyMsg              string
	Recon                 metav1.ConditionStatus
	ReconReason           string
	ReconMsg              string
	DeletionBlocked       metav1.ConditionStatus
	DeletionBlockedReason string
	DeletionBlockedMsg    string
}

func (r *SmolVMReconciler) updateStatus(ctx context.Context, vm *vmv1alpha1.SmolVM, in statusInput) (ctrl.Result, error) {
	latest := &vmv1alpha1.SmolVM{}
	if err := r.Get(ctx, types.NamespacedName{Name: vm.Name, Namespace: vm.Namespace}, latest); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	previous := latest.Status.DeepCopy()
	latest.Status.Phase = in.Phase
	latest.Status.NodeName = in.NodeName
	latest.Status.MachineName = in.MachineName
	if in.Machine != nil {
		latest.Status.RuntimePID = in.Machine.PID
		latest.Status.Network = in.Machine.Network
		latest.Status.Ports = portsFromRuntime(in.Machine.Ports)
		latest.Status.StorageGiB = in.Machine.StorageGB
		latest.Status.OverlayGiB = in.Machine.OverlayGB
	}
	latest.Status.ObservedGeneration = latest.Generation
	if in.ScheduledReason == "" && in.NodeName != "" {
		in.Scheduled = metav1.ConditionTrue
		in.ScheduledReason = "Bound"
		in.ScheduledMsg = fmt.Sprintf("assigned to node %s", in.NodeName)
	}
	if in.ScheduledReason != "" {
		setCondition(&latest.Status.Conditions, vmv1alpha1.ConditionScheduled, in.Scheduled, in.ScheduledReason, in.ScheduledMsg, latest.Generation)
	}
	setCondition(&latest.Status.Conditions, vmv1alpha1.ConditionReady, in.Ready, in.ReadyReason, in.ReadyMsg, latest.Generation)
	setCondition(&latest.Status.Conditions, vmv1alpha1.ConditionRuntimeReady, in.Ready, in.ReadyReason, in.ReadyMsg, latest.Generation)
	setCondition(&latest.Status.Conditions, vmv1alpha1.ConditionGuestReady, metav1.ConditionUnknown, "GuestReadinessUnavailable", "smolvm guest readiness is not reported by the runtime API", latest.Generation)
	setCondition(&latest.Status.Conditions, vmv1alpha1.ConditionReconciled, in.Recon, in.ReconReason, in.ReconMsg, latest.Generation)
	if in.DeletionBlockedReason != "" {
		setCondition(&latest.Status.Conditions, vmv1alpha1.ConditionDeletionBlocked, in.DeletionBlocked, in.DeletionBlockedReason, in.DeletionBlockedMsg, latest.Generation)
	}
	if apiequality.Semantic.DeepEqual(previous, &latest.Status) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, r.Status().Update(ctx, latest)
}

func setCondition(conditions *[]metav1.Condition, typ string, status metav1.ConditionStatus, reason, msg string, generation int64) {
	for i := range *conditions {
		if (*conditions)[i].Type == typ {
			if (*conditions)[i].Status != status {
				(*conditions)[i].LastTransitionTime = metav1.Now()
			}
			(*conditions)[i].Status = status
			(*conditions)[i].Reason = reason
			(*conditions)[i].Message = msg
			(*conditions)[i].ObservedGeneration = generation
			return
		}
	}
	*conditions = append(*conditions, metav1.Condition{
		Type:               typ,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: generation,
		LastTransitionTime: metav1.Now(),
	})
}

func (r *SmolVMReconciler) schedule(ctx context.Context, vm *vmv1alpha1.SmolVM) (string, string, error) {
	bound, err := r.boundVMs(ctx, vm)
	if err != nil {
		return "", "", err
	}
	if vm.Spec.NodeName != "" {
		var node vmv1alpha1.SmolVMNode
		if err := r.Get(ctx, types.NamespacedName{Name: vm.Spec.NodeName}, &node); err != nil {
			if apierrors.IsNotFound(err) {
				return "", fmt.Sprintf("pinned node %s has no SmolVMNode runtime status", vm.Spec.NodeName), nil
			}
			return "", "", err
		}
		if reason := r.nodeRejectReason(ctx, &node, vm, bound); reason != "" {
			return "", fmt.Sprintf("pinned node %s is not eligible: %s", vm.Spec.NodeName, reason), nil
		}
		return node.Name, "", nil
	}

	var nodes vmv1alpha1.SmolVMNodeList
	if err := r.List(ctx, &nodes); err != nil {
		return "", "", err
	}
	var reasons []string
	for _, node := range nodes.Items {
		if reason := r.nodeRejectReason(ctx, &node, vm, bound); reason != "" {
			reasons = append(reasons, fmt.Sprintf("%s: %s", node.Name, reason))
			continue
		}
		return node.Name, "", nil
	}
	if len(reasons) == 0 {
		return "", "no SmolVMNode objects exist", nil
	}
	return "", "no ready SmolVMNode matches this VM: " + strings.Join(reasons, "; "), nil
}

func (r *SmolVMReconciler) nodeRejectReason(ctx context.Context, node *vmv1alpha1.SmolVMNode, vm *vmv1alpha1.SmolVM, bound []vmv1alpha1.SmolVM) string {
	if !smolVMNodeEligible(node) {
		return "runtime not ready, heartbeat stale, or endpoint missing"
	}
	if !matchesNodeSelector(node, vm.Spec.NodeSelector) {
		return "nodeSelector does not match"
	}
	var k8sNode corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: node.Name}, &k8sNode); err != nil {
		return fmt.Sprintf("Kubernetes Node unavailable: %v", err)
	}
	if k8sNode.Spec.Unschedulable {
		return "Kubernetes Node is unschedulable"
	}
	if !kubernetesNodeReady(&k8sNode) {
		return "Kubernetes Node is not Ready"
	}
	if reason := resourceFitReason(node, vm, bound); reason != "" {
		return reason
	}
	if reason := hostPortConflictReason(node.Name, vm, bound); reason != "" {
		return reason
	}
	return ""
}

func (r *SmolVMReconciler) boundVMs(ctx context.Context, pending *vmv1alpha1.SmolVM) ([]vmv1alpha1.SmolVM, error) {
	var list vmv1alpha1.SmolVMList
	if err := r.List(ctx, &list); err != nil {
		return nil, err
	}
	out := make([]vmv1alpha1.SmolVM, 0, len(list.Items))
	for _, item := range list.Items {
		if item.UID == pending.UID || item.Status.NodeName == "" || !item.DeletionTimestamp.IsZero() {
			continue
		}
		out = append(out, item)
	}
	return out, nil
}

func kubernetesNodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func resourceFitReason(node *vmv1alpha1.SmolVMNode, vm *vmv1alpha1.SmolVM, bound []vmv1alpha1.SmolVM) string {
	usedCPU, usedMemory, usedStorage := int32(0), int64(0), int64(0)
	for _, existing := range bound {
		if existing.Status.NodeName != node.Name {
			continue
		}
		usedCPU += existing.Spec.Resources.CPUs
		usedMemory += int64(existing.Spec.Resources.MemoryMiB)
		usedStorage += existing.Spec.Storage.StorageGiB + existing.Spec.Storage.OverlayGiB
	}
	if node.Status.Allocatable.CPUs > 0 && usedCPU+vm.Spec.Resources.CPUs > node.Status.Allocatable.CPUs {
		return fmt.Sprintf("insufficient cpu: requested %d used %d allocatable %d", vm.Spec.Resources.CPUs, usedCPU, node.Status.Allocatable.CPUs)
	}
	if node.Status.Allocatable.MemoryMiB > 0 && usedMemory+int64(vm.Spec.Resources.MemoryMiB) > node.Status.Allocatable.MemoryMiB {
		return fmt.Sprintf("insufficient memory: requested %dMiB used %dMiB allocatable %dMiB", vm.Spec.Resources.MemoryMiB, usedMemory, node.Status.Allocatable.MemoryMiB)
	}
	requestedStorage := vm.Spec.Storage.StorageGiB + vm.Spec.Storage.OverlayGiB
	if node.Status.Allocatable.StorageGiB > 0 && usedStorage+requestedStorage > node.Status.Allocatable.StorageGiB {
		return fmt.Sprintf("insufficient storage: requested %dGiB used %dGiB allocatable %dGiB", requestedStorage, usedStorage, node.Status.Allocatable.StorageGiB)
	}
	return ""
}

func hostPortConflictReason(nodeName string, vm *vmv1alpha1.SmolVM, bound []vmv1alpha1.SmolVM) string {
	requested := map[int32]struct{}{}
	for _, port := range vm.Spec.Network.Ports {
		requested[port.HostPort] = struct{}{}
	}
	if len(requested) == 0 {
		return ""
	}
	for _, existing := range bound {
		if existing.Status.NodeName != nodeName {
			continue
		}
		for _, port := range existing.Spec.Network.Ports {
			if _, ok := requested[port.HostPort]; ok {
				return fmt.Sprintf("hostPort %d conflicts with %s/%s", port.HostPort, existing.Namespace, existing.Name)
			}
		}
	}
	return ""
}

func smolVMNodeEligible(node *vmv1alpha1.SmolVMNode) bool {
	if !hasCondition(node.Status.Conditions, vmv1alpha1.SmolVMNodeConditionReady, metav1.ConditionTrue) ||
		!hasCondition(node.Status.Conditions, vmv1alpha1.SmolVMNodeConditionKVMAvailable, metav1.ConditionTrue) ||
		!hasCondition(node.Status.Conditions, vmv1alpha1.SmolVMNodeConditionRuntimeReady, metav1.ConditionTrue) ||
		!hasCondition(node.Status.Conditions, vmv1alpha1.SmolVMNodeConditionSchedulable, metav1.ConditionTrue) {
		return false
	}
	if node.Status.ProtocolVersion != "" && node.Status.ProtocolVersion != "v1alpha1" {
		return false
	}
	if node.Status.HeartbeatTime == nil || time.Since(node.Status.HeartbeatTime.Time) > 2*time.Minute {
		return false
	}
	return node.Status.Endpoint.PodIP != "" && node.Status.Endpoint.Port > 0
}

func hasCondition(conditions []metav1.Condition, typ string, status metav1.ConditionStatus) bool {
	for _, condition := range conditions {
		if condition.Type == typ && condition.Status == status {
			return true
		}
	}
	return false
}

func matchesNodeSelector(node *vmv1alpha1.SmolVMNode, selector map[string]string) bool {
	for key, value := range selector {
		if node.Labels[key] != value {
			return false
		}
	}
	return true
}

func validateSpec(vm *vmv1alpha1.SmolVM) error {
	if vm.Status.NodeName != "" && vm.Spec.NodeName != "" && vm.Spec.NodeName != vm.Status.NodeName {
		return fmt.Errorf("spec.nodeName is immutable after binding; machine is owned by node %q", vm.Status.NodeName)
	}
	if vm.Spec.Image != "" && vm.Spec.From != "" {
		return fmt.Errorf("spec.image and spec.from are mutually exclusive")
	}
	if vm.Spec.Image == "" && vm.Spec.From == "" {
		return fmt.Errorf("one of spec.image or spec.from is required")
	}
	seenPorts := map[int32]struct{}{}
	for _, port := range vm.Spec.Network.Ports {
		if _, ok := seenPorts[port.HostPort]; ok {
			return fmt.Errorf("duplicate hostPort %d", port.HostPort)
		}
		seenPorts[port.HostPort] = struct{}{}
	}
	for _, cidr := range vm.Spec.Network.AllowedCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("invalid allowedCIDR %q: %w", cidr, err)
		}
	}
	return nil
}

func buildCreateRequest(vm *vmv1alpha1.SmolVM, machineName string) smolvmapi.CreateMachineRequest {
	req := smolvmapi.CreateMachineRequest{
		Name:         machineName,
		CPUs:         vm.Spec.Resources.CPUs,
		MemoryMiB:    vm.Spec.Resources.MemoryMiB,
		Network:      vm.Spec.Network.Enabled,
		AllowedCIDRs: vm.Spec.Network.AllowedCIDRs,
		Image:        vm.Spec.Image,
		From:         vm.Spec.From,
	}
	if vm.Spec.Storage.StorageGiB > 0 {
		req.StorageGB = &vm.Spec.Storage.StorageGiB
	}
	if vm.Spec.Storage.OverlayGiB > 0 {
		req.OverlayGB = &vm.Spec.Storage.OverlayGiB
	}
	for _, p := range vm.Spec.Network.Ports {
		req.Ports = append(req.Ports, smolvmapi.PortSpec{Host: p.HostPort, Guest: p.GuestPort})
	}
	return req
}

func buildResizeRequest(vm *vmv1alpha1.SmolVM) smolvmapi.ResizeRequest {
	var req smolvmapi.ResizeRequest
	if vm.Spec.Storage.StorageGiB > 0 {
		req.StorageGB = &vm.Spec.Storage.StorageGiB
	}
	if vm.Spec.Storage.OverlayGiB > 0 {
		req.OverlayGB = &vm.Spec.Storage.OverlayGiB
	}
	return req
}

func validateImmutableRuntimeFields(vm *vmv1alpha1.SmolVM, machine *smolvmapi.MachineInfo) error {
	if vm.Spec.Resources.CPUs > 0 && machine.CPUs > 0 && vm.Spec.Resources.CPUs != machine.CPUs {
		return fmt.Errorf("cpus is immutable after creation; runtime has %d and spec requests %d", machine.CPUs, vm.Spec.Resources.CPUs)
	}
	if vm.Spec.Resources.MemoryMiB > 0 && machine.MemoryMiB > 0 && vm.Spec.Resources.MemoryMiB != machine.MemoryMiB {
		return fmt.Errorf("memoryMiB is immutable after creation; runtime has %d and spec requests %d", machine.MemoryMiB, vm.Spec.Resources.MemoryMiB)
	}
	if !samePorts(vm.Spec.Network.Ports, machine.Ports) {
		return fmt.Errorf("network port mappings are immutable after creation")
	}
	return nil
}

func samePorts(spec []vmv1alpha1.SmolVMPort, runtime []smolvmapi.PortSpec) bool {
	if len(spec) != len(runtime) {
		return false
	}
	specPorts := make([]string, 0, len(spec))
	for _, p := range spec {
		specPorts = append(specPorts, fmt.Sprintf("%d:%d", p.HostPort, p.GuestPort))
	}
	runtimePorts := make([]string, 0, len(runtime))
	for _, p := range runtime {
		runtimePorts = append(runtimePorts, fmt.Sprintf("%d:%d", p.Host, p.Guest))
	}
	sort.Strings(specPorts)
	sort.Strings(runtimePorts)
	for i := range specPorts {
		if specPorts[i] != runtimePorts[i] {
			return false
		}
	}
	return true
}

func portsFromRuntime(ports []smolvmapi.PortSpec) []vmv1alpha1.SmolVMPort {
	if len(ports) == 0 {
		return nil
	}
	out := make([]vmv1alpha1.SmolVMPort, 0, len(ports))
	for _, port := range ports {
		out = append(out, vmv1alpha1.SmolVMPort{HostPort: port.Host, GuestPort: port.Guest})
	}
	return out
}

func validateStorageDoesNotShrink(vm *vmv1alpha1.SmolVM, machine *smolvmapi.MachineInfo) error {
	if vm.Spec.Storage.StorageGiB > 0 && machine.StorageGB != nil && vm.Spec.Storage.StorageGiB < *machine.StorageGB {
		return fmt.Errorf("storageGiB cannot shrink from %d to %d", *machine.StorageGB, vm.Spec.Storage.StorageGiB)
	}
	if vm.Spec.Storage.OverlayGiB > 0 && machine.OverlayGB != nil && vm.Spec.Storage.OverlayGiB < *machine.OverlayGB {
		return fmt.Errorf("overlayGiB cannot shrink from %d to %d", *machine.OverlayGB, vm.Spec.Storage.OverlayGiB)
	}
	return nil
}

func needsStorageExpansion(vm *vmv1alpha1.SmolVM, machine *smolvmapi.MachineInfo) bool {
	return vm.Spec.Storage.StorageGiB > 0 && machine.StorageGB != nil && vm.Spec.Storage.StorageGiB > *machine.StorageGB ||
		vm.Spec.Storage.OverlayGiB > 0 && machine.OverlayGB != nil && vm.Spec.Storage.OverlayGiB > *machine.OverlayGB
}

func phaseFromMachine(machine *smolvmapi.MachineInfo) string {
	switch machine.State {
	case "running":
		return "Running"
	case "stopped":
		return "Stopped"
	case "created":
		return "Created"
	default:
		return "Unknown"
	}
}

func readyFromMachine(machine *smolvmapi.MachineInfo) metav1.ConditionStatus {
	if machine.State == "running" {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func stableMachineName(vm *vmv1alpha1.SmolVM) string {
	sum := sha256.Sum256([]byte(string(vm.UID)))
	uid := hex.EncodeToString(sum[:])[:10]
	return fmt.Sprintf("k8s-%s-%s-%s", vm.Namespace, vm.Name, uid)
}

func (r *SmolVMReconciler) event(vm *vmv1alpha1.SmolVM, eventType, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Event(vm, eventType, reason, message)
	}
}

func (r *SmolVMReconciler) requestsForNodeChange(ctx context.Context, nodeName string) []reconcile.Request {
	var vms vmv1alpha1.SmolVMList
	if err := r.List(ctx, &vms); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(vms.Items))
	for _, vm := range vms.Items {
		if vm.Status.NodeName == nodeName || (vm.Status.NodeName == "" && (vm.Spec.NodeName == "" || vm.Spec.NodeName == nodeName)) {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: vm.Name, Namespace: vm.Namespace}})
		}
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *SmolVMReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("smolvm-controller")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&vmv1alpha1.SmolVM{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
			return r.requestsForNodeChange(ctx, obj.GetName())
		})).
		Watches(&vmv1alpha1.SmolVMNode{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
			return r.requestsForNodeChange(ctx, obj.GetName())
		})).
		Complete(r)
}
