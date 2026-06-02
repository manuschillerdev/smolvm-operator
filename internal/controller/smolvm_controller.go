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
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
}

//+kubebuilder:rbac:groups=vm.smolvm.dev,resources=smolvms,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=vm.smolvm.dev,resources=smolvms/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=vm.smolvm.dev,resources=smolvms/finalizers,verbs=update

func (r *SmolVMReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var vm vmv1alpha1.SmolVM
	if err := r.Get(ctx, req.NamespacedName, &vm); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	runtimeNode := os.Getenv("SMOLVM_NODE_NAME")
	desiredNode := vm.Spec.NodeName
	if desiredNode == "" {
		desiredNode = vm.Status.NodeName
	}

	machineName := vm.Status.MachineName
	if machineName == "" {
		machineName = stableMachineName(&vm)
	}

	if runtimeNode != "" && desiredNode == "" {
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

	if desiredNode != "" && runtimeNode != "" && desiredNode != runtimeNode {
		return ctrl.Result{}, nil
	}

	api := smolvmapi.NewClient(os.Getenv("SMOLVM_API_URL"), os.Getenv("SMOLVM_API_SOCKET"))

	if !vm.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &vm, api, machineName)
	}

	if !controllerutil.ContainsFinalizer(&vm, vmv1alpha1.SmolVMFinalizer) {
		controllerutil.AddFinalizer(&vm, vmv1alpha1.SmolVMFinalizer)
		return ctrl.Result{}, r.Update(ctx, &vm)
	}

	if err := validateSpec(&vm); err != nil {
		logger.Info("invalid SmolVM spec", "error", err.Error())
		return r.updateStatus(ctx, &vm, statusInput{
			Phase:       "Failed",
			NodeName:    desiredNode,
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
		machine, err = api.CreateMachine(ctx, createReq)
		if err != nil {
			return r.runtimeUnavailable(ctx, &vm, desiredNode, machineName, err)
		}
		_, statusErr := r.updateStatus(ctx, &vm, statusInput{
			Phase:       phaseFromMachine(machine),
			NodeName:    desiredNode,
			MachineName: machineName,
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
		return r.runtimeUnavailable(ctx, &vm, desiredNode, machineName, err)
	}

	if invalidResize := validateStorageDoesNotShrink(&vm, machine); invalidResize != nil {
		return r.updateStatus(ctx, &vm, statusInput{
			Phase:       phaseFromMachine(machine),
			NodeName:    desiredNode,
			MachineName: machineName,
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
				NodeName:    desiredNode,
				MachineName: machineName,
				Ready:       readyFromMachine(machine),
				ReadyReason: "ResizePending",
				ReadyMsg:    "storage expansion requires the machine to be stopped",
				Recon:       metav1.ConditionFalse,
				ReconReason: "ResizeRequiresStoppedMachine",
				ReconMsg:    "set spec.running=false before expanding storage",
			})
		}
		resize := buildResizeRequest(&vm)
		if err := api.ResizeMachine(ctx, machineName, resize); err != nil {
			return r.runtimeUnavailable(ctx, &vm, desiredNode, machineName, err)
		}
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	if vm.Spec.Running && machine.State != "running" {
		if err := api.EnsureMachineRunning(ctx, machineName); err != nil {
			return r.runtimeUnavailable(ctx, &vm, desiredNode, machineName, err)
		}
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	if !vm.Spec.Running && machine.State == "running" {
		if err := api.StopMachine(ctx, machineName); err != nil {
			return r.runtimeUnavailable(ctx, &vm, desiredNode, machineName, err)
		}
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	result, err := r.updateStatus(ctx, &vm, statusInput{
		Phase:       phaseFromMachine(machine),
		NodeName:    desiredNode,
		MachineName: machineName,
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

func (r *SmolVMReconciler) reconcileDelete(ctx context.Context, vm *vmv1alpha1.SmolVM, api *smolvmapi.Client, machineName string) (ctrl.Result, error) {
	if machineName != "" {
		if err := api.DeleteMachine(ctx, machineName); err != nil && !smolvmapi.IsNotFound(err) {
			return r.runtimeUnavailable(ctx, vm, vm.Status.NodeName, machineName, err)
		}
	}
	controllerutil.RemoveFinalizer(vm, vmv1alpha1.SmolVMFinalizer)
	return ctrl.Result{}, r.Update(ctx, vm)
}

func (r *SmolVMReconciler) runtimeUnavailable(ctx context.Context, vm *vmv1alpha1.SmolVM, nodeName, machineName string, err error) (ctrl.Result, error) {
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
	latest.Status.Phase = in.Phase
	latest.Status.NodeName = in.NodeName
	latest.Status.MachineName = in.MachineName
	latest.Status.ObservedGeneration = latest.Generation
	setCondition(&latest.Status.Conditions, vmv1alpha1.ConditionReady, in.Ready, in.ReadyReason, in.ReadyMsg, latest.Generation)
	setCondition(&latest.Status.Conditions, vmv1alpha1.ConditionReconciled, in.Recon, in.ReconReason, in.ReconMsg, latest.Generation)
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
	if vm.Spec.Image != "" && vm.Spec.From != "" {
		return fmt.Errorf("spec.image and spec.from are mutually exclusive")
	}
	if vm.Spec.Image == "" && vm.Spec.From == "" {
		return fmt.Errorf("one of spec.image or spec.from is required")
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

// SetupWithManager sets up the controller with the Manager.
func (r *SmolVMReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&vmv1alpha1.SmolVM{}).
		Complete(r)
}
