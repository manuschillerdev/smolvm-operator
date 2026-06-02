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
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

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

//+kubebuilder:rbac:groups=vm.smolvm.dev,resources=smolvms,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=vm.smolvm.dev,resources=smolvms/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=vm.smolvm.dev,resources=smolvms/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *SmolVMReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var vm vmv1alpha1.SmolVM
	if err := r.Get(ctx, req.NamespacedName, &vm); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	runtimeNode := os.Getenv("SMOLVM_NODE_NAME")
	desiredNode := vm.Spec.NodeName
	ownerNode := vm.Status.NodeName
	if ownerNode == "" {
		ownerNode = desiredNode
	}

	machineName := vm.Status.MachineName
	if machineName == "" {
		machineName = stableMachineName(&vm)
	}

	if runtimeNode != "" && ownerNode == "" {
		return r.updateStatus(ctx, &vm, statusInput{
			Phase:       "Pending",
			NodeName:    "",
			MachineName: machineName,
			Ready:       metav1.ConditionFalse,
			ReadyReason: "AwaitingNodeAssignment",
			ReadyMsg:    "spec.nodeName is required in node-local mode",
			Recon:       metav1.ConditionFalse,
			ReconReason: "AwaitingNodeAssignment",
			ReconMsg:    "set spec.nodeName to a node running the smolvm controller",
		})
	}

	if ownerNode != "" && runtimeNode != "" && ownerNode != runtimeNode {
		return ctrl.Result{}, nil
	}

	api := r.runtimeClient()

	if !vm.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &vm, api, machineName, ownerNode)
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

func (r *SmolVMReconciler) reconcileDelete(ctx context.Context, vm *vmv1alpha1.SmolVM, api SmolVMRuntime, machineName, ownerNode string) (ctrl.Result, error) {
	if machineName != "" {
		r.event(vm, "Normal", "Deleting", fmt.Sprintf("deleting smolvm machine %s", machineName))
		if err := api.DeleteMachine(ctx, machineName); err != nil && !smolvmapi.IsNotFound(err) {
			return r.runtimeUnavailable(ctx, vm, ownerNode, machineName, err)
		}
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
	Phase       string
	NodeName    string
	MachineName string
	Machine     *smolvmapi.MachineInfo
	Ready       metav1.ConditionStatus
	ReadyReason string
	ReadyMsg    string
	Recon       metav1.ConditionStatus
	ReconReason string
	ReconMsg    string
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
	setCondition(&latest.Status.Conditions, vmv1alpha1.ConditionReady, in.Ready, in.ReadyReason, in.ReadyMsg, latest.Generation)
	setCondition(&latest.Status.Conditions, vmv1alpha1.ConditionRuntimeReady, in.Ready, in.ReadyReason, in.ReadyMsg, latest.Generation)
	setCondition(&latest.Status.Conditions, vmv1alpha1.ConditionGuestReady, metav1.ConditionUnknown, "GuestReadinessUnavailable", "smolvm guest readiness is not reported by the runtime API", latest.Generation)
	setCondition(&latest.Status.Conditions, vmv1alpha1.ConditionReconciled, in.Recon, in.ReconReason, in.ReconMsg, latest.Generation)
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

// SetupWithManager sets up the controller with the Manager.
func (r *SmolVMReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("smolvm-controller")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&vmv1alpha1.SmolVM{}).
		Complete(r)
}
