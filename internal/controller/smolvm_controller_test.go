package controller

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	vmv1alpha1 "github.com/manuschillerdev/smolvm-operator/api/v1alpha1"
	smolvmapi "github.com/manuschillerdev/smolvm-operator/internal/smolvm"
)

func TestValidateSpecRejectsInvalidNetworkInputs(t *testing.T) {
	vm := &vmv1alpha1.SmolVM{
		Spec: vmv1alpha1.SmolVMSpec{
			Image: "alpine:latest",
			Network: vmv1alpha1.SmolVMNetwork{
				AllowedCIDRs: []string{"not-a-cidr"},
			},
		},
	}
	if err := validateSpec(vm); err == nil {
		t.Fatal("expected invalid CIDR to be rejected")
	}

	vm.Spec.Network.AllowedCIDRs = nil
	vm.Spec.Network.Ports = []vmv1alpha1.SmolVMPort{
		{HostPort: 8080, GuestPort: 80},
		{HostPort: 8080, GuestPort: 8080},
	}
	if err := validateSpec(vm); err == nil {
		t.Fatal("expected duplicate hostPort to be rejected")
	}
}

func TestValidateSpecRejectsNodeMoveAfterBinding(t *testing.T) {
	vm := &vmv1alpha1.SmolVM{
		Spec:   vmv1alpha1.SmolVMSpec{NodeName: "node-b", Image: "alpine:latest"},
		Status: vmv1alpha1.SmolVMStatus{NodeName: "node-a"},
	}
	if err := validateSpec(vm); err == nil {
		t.Fatal("expected nodeName mutation to be rejected")
	}
}

func TestValidateImmutableRuntimeFieldsRejectsUnsupportedDrift(t *testing.T) {
	vm := &vmv1alpha1.SmolVM{
		Spec: vmv1alpha1.SmolVMSpec{
			Image:     "alpine:latest",
			Resources: vmv1alpha1.SmolVMResources{CPUs: 2, MemoryMiB: 256},
			Network:   vmv1alpha1.SmolVMNetwork{Ports: []vmv1alpha1.SmolVMPort{{HostPort: 8080, GuestPort: 80}}},
		},
	}
	machine := &smolvmapi.MachineInfo{
		CPUs:      1,
		MemoryMiB: 256,
		Ports:     []smolvmapi.PortSpec{{Host: 8080, Guest: 80}},
	}
	if err := validateImmutableRuntimeFields(vm, machine); err == nil {
		t.Fatal("expected CPU drift to be rejected")
	}

	machine.CPUs = 2
	machine.Ports = []smolvmapi.PortSpec{{Host: 9090, Guest: 80}}
	if err := validateImmutableRuntimeFields(vm, machine); err == nil {
		t.Fatal("expected port drift to be rejected")
	}
}

func TestBuildCreateRequestMapsSpec(t *testing.T) {
	vm := &vmv1alpha1.SmolVM{
		ObjectMeta: metav1.ObjectMeta{Name: "alpine", Namespace: "default"},
		Spec: vmv1alpha1.SmolVMSpec{
			Image:     "alpine:latest",
			Resources: vmv1alpha1.SmolVMResources{CPUs: 2, MemoryMiB: 256},
			Storage:   vmv1alpha1.SmolVMStorage{StorageGiB: 4, OverlayGiB: 2},
			Network: vmv1alpha1.SmolVMNetwork{
				Enabled:      true,
				AllowedCIDRs: []string{"10.0.0.0/8"},
				Ports:        []vmv1alpha1.SmolVMPort{{HostPort: 8080, GuestPort: 80}},
			},
		},
	}

	req := buildCreateRequest(vm, "machine-name")
	if req.Name != "machine-name" || req.Image != "alpine:latest" || req.CPUs != 2 || req.MemoryMiB != 256 {
		t.Fatalf("unexpected create request: %#v", req)
	}
	if req.StorageGB == nil || *req.StorageGB != 4 || req.OverlayGB == nil || *req.OverlayGB != 2 {
		t.Fatalf("storage was not mapped: %#v", req)
	}
	if len(req.Ports) != 1 || req.Ports[0].Host != 8080 || req.Ports[0].Guest != 80 {
		t.Fatalf("ports were not mapped: %#v", req.Ports)
	}
}

var _ = Describe("SmolVM Controller", func() {
	It("creates a missing runtime machine and records status", func() {
		ctx := context.Background()
		name := types.NamespacedName{Name: "runtime-create", Namespace: "default"}
		resource := testSmolVM(name, "")
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())

		runtime := &fakeRuntime{}
		reconciler := testReconciler(runtime)

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).NotTo(HaveOccurred())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).NotTo(HaveOccurred())

		Expect(runtime.created).To(Equal(1))
		latest := &vmv1alpha1.SmolVM{}
		Expect(k8sClient.Get(ctx, name, latest)).To(Succeed())
		Expect(latest.Status.Phase).To(Equal("Stopped"))
		Expect(latest.Status.MachineName).NotTo(BeEmpty())
		Expect(latest.Status.Conditions).NotTo(BeEmpty())
	})

	It("adopts an existing runtime machine after restart", func() {
		ctx := context.Background()
		name := types.NamespacedName{Name: "runtime-adopt", Namespace: "default"}
		machineName := "k8s-default-runtime-adopt-test"
		Expect(createSmolVMWithStatus(ctx, name, "node-a", machineName)).To(Succeed())

		runtime := &fakeRuntime{machine: &smolvmapi.MachineInfo{Name: machineName, State: "running", CPUs: 1, MemoryMiB: 128}}
		_, err := testReconciler(runtime).Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).NotTo(HaveOccurred())

		Expect(runtime.created).To(Equal(0))
		latest := &vmv1alpha1.SmolVM{}
		Expect(k8sClient.Get(ctx, name, latest)).To(Succeed())
		Expect(latest.Status.Phase).To(Equal("Running"))
		Expect(conditionReason(latest, vmv1alpha1.ConditionReconciled)).To(Equal("Reconciled"))
	})

	It("marks unsupported runtime drift without mutating the machine", func() {
		ctx := context.Background()
		name := types.NamespacedName{Name: "runtime-drift", Namespace: "default"}
		machineName := "k8s-default-runtime-drift-test"
		Expect(createSmolVMWithStatus(ctx, name, "node-a", machineName)).To(Succeed())

		runtime := &fakeRuntime{machine: &smolvmapi.MachineInfo{Name: machineName, State: "running", CPUs: 2, MemoryMiB: 128}}
		_, err := testReconciler(runtime).Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).NotTo(HaveOccurred())

		latest := &vmv1alpha1.SmolVM{}
		Expect(k8sClient.Get(ctx, name, latest)).To(Succeed())
		Expect(conditionReason(latest, vmv1alpha1.ConditionReconciled)).To(Equal("UnsupportedUpdate"))
		Expect(runtime.started).To(Equal(0))
	})

	It("keeps the finalizer when runtime deletion fails", func() {
		ctx := context.Background()
		name := types.NamespacedName{Name: "runtime-delete-fail", Namespace: "default"}
		machineName := "k8s-default-runtime-delete-fail-test"
		Expect(createSmolVMWithStatus(ctx, name, "node-a", machineName)).To(Succeed())
		withNodeName("node-a", func() {
			runtime := &fakeRuntime{deleteErr: fmt.Errorf("runtime unavailable")}
			Expect(k8sClient.Delete(ctx, &vmv1alpha1.SmolVM{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace}})).To(Succeed())

			_, err := testReconciler(runtime).Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			Expect(runtime.deleted).To(Equal(1))

			latest := &vmv1alpha1.SmolVM{}
			Expect(k8sClient.Get(ctx, name, latest)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(latest, vmv1alpha1.SmolVMFinalizer)).To(BeTrue())
			Expect(conditionReason(latest, vmv1alpha1.ConditionReconciled)).To(Equal("RuntimeUnavailable"))
		})
	})

	It("deletes on the status owner node rather than a changed spec node", func() {
		ctx := context.Background()
		name := types.NamespacedName{Name: "runtime-delete-owner", Namespace: "default"}
		machineName := "k8s-default-runtime-delete-owner-test"
		vm := testSmolVM(name, "node-b")
		controllerutil.AddFinalizer(vm, vmv1alpha1.SmolVMFinalizer)
		Expect(k8sClient.Create(ctx, vm)).To(Succeed())
		latest := &vmv1alpha1.SmolVM{}
		Expect(k8sClient.Get(ctx, name, latest)).To(Succeed())
		latest.Status.NodeName = "node-a"
		latest.Status.MachineName = machineName
		Expect(k8sClient.Status().Update(ctx, latest)).To(Succeed())

		withNodeName("node-a", func() {
			runtime := &fakeRuntime{}
			Expect(k8sClient.Delete(ctx, &vmv1alpha1.SmolVM{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace}})).To(Succeed())
			_, err := testReconciler(runtime).Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			Expect(runtime.deleted).To(Equal(1))
		})
	})

	It("starts and stops machines to match desired running state", func() {
		ctx := context.Background()
		startName := types.NamespacedName{Name: "runtime-start", Namespace: "default"}
		Expect(createSmolVMWithStatus(ctx, startName, "node-a", "runtime-start-machine")).To(Succeed())
		startRuntime := &fakeRuntime{machine: &smolvmapi.MachineInfo{Name: "runtime-start-machine", State: "stopped", CPUs: 1, MemoryMiB: 128}}
		_, err := testReconciler(startRuntime).Reconcile(ctx, reconcile.Request{NamespacedName: startName})
		Expect(err).NotTo(HaveOccurred())
		Expect(startRuntime.started).To(Equal(1))

		stopName := types.NamespacedName{Name: "runtime-stop", Namespace: "default"}
		vm := testSmolVM(stopName, "node-a")
		vm.Spec.Running = false
		Expect(createSmolVMWithSpecStatus(ctx, vm, "node-a", "runtime-stop-machine")).To(Succeed())
		stopRuntime := &fakeRuntime{machine: &smolvmapi.MachineInfo{Name: "runtime-stop-machine", State: "running", CPUs: 1, MemoryMiB: 128}}
		_, err = testReconciler(stopRuntime).Reconcile(ctx, reconcile.Request{NamespacedName: stopName})
		Expect(err).NotTo(HaveOccurred())
		Expect(stopRuntime.stopped).To(Equal(1))
	})

	It("removes finalizers after successful or already-missing runtime deletion", func() {
		ctx := context.Background()
		for _, tc := range []struct {
			name    string
			machine *smolvmapi.MachineInfo
		}{
			{name: "runtime-delete-success", machine: &smolvmapi.MachineInfo{Name: "runtime-delete-success-machine", State: "stopped", CPUs: 1, MemoryMiB: 128}},
			{name: "runtime-delete-notfound"},
		} {
			name := types.NamespacedName{Name: tc.name, Namespace: "default"}
			machineName := tc.name + "-machine"
			Expect(createSmolVMWithStatus(ctx, name, "node-a", machineName)).To(Succeed())
			if tc.machine != nil {
				tc.machine.Name = machineName
			}
			runtime := &fakeRuntime{machine: tc.machine}
			Expect(k8sClient.Delete(ctx, &vmv1alpha1.SmolVM{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace}})).To(Succeed())
			_, err := testReconciler(runtime).Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			latest := &vmv1alpha1.SmolVM{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, name, latest)
				return apierrors.IsNotFound(err)
			}).Should(BeTrue())
		}
	})

	It("handles storage resize lifecycle", func() {
		ctx := context.Background()
		expandName := types.NamespacedName{Name: "runtime-storage-expand", Namespace: "default"}
		expandVM := testSmolVM(expandName, "node-a")
		expandVM.Spec.Storage.StorageGiB = 4
		Expect(createSmolVMWithSpecStatus(ctx, expandVM, "node-a", "runtime-storage-expand-machine")).To(Succeed())
		oldSize := int64(2)
		expandRuntime := &fakeRuntime{machine: &smolvmapi.MachineInfo{Name: "runtime-storage-expand-machine", State: "stopped", CPUs: 1, MemoryMiB: 128, StorageGB: &oldSize}}
		_, err := testReconciler(expandRuntime).Reconcile(ctx, reconcile.Request{NamespacedName: expandName})
		Expect(err).NotTo(HaveOccurred())
		Expect(expandRuntime.resized).To(Equal(1))

		shrinkName := types.NamespacedName{Name: "runtime-storage-shrink", Namespace: "default"}
		shrinkVM := testSmolVM(shrinkName, "node-a")
		shrinkVM.Spec.Storage.StorageGiB = 1
		Expect(createSmolVMWithSpecStatus(ctx, shrinkVM, "node-a", "runtime-storage-shrink-machine")).To(Succeed())
		largeSize := int64(2)
		shrinkRuntime := &fakeRuntime{machine: &smolvmapi.MachineInfo{Name: "runtime-storage-shrink-machine", State: "stopped", CPUs: 1, MemoryMiB: 128, StorageGB: &largeSize}}
		_, err = testReconciler(shrinkRuntime).Reconcile(ctx, reconcile.Request{NamespacedName: shrinkName})
		Expect(err).NotTo(HaveOccurred())
		Expect(shrinkRuntime.resized).To(Equal(0))
		latest := &vmv1alpha1.SmolVM{}
		Expect(k8sClient.Get(ctx, shrinkName, latest)).To(Succeed())
		Expect(conditionReason(latest, vmv1alpha1.ConditionReconciled)).To(Equal("InvalidStorageResize"))

		runningName := types.NamespacedName{Name: "runtime-storage-running", Namespace: "default"}
		runningVM := testSmolVM(runningName, "node-a")
		runningVM.Spec.Storage.StorageGiB = 4
		Expect(createSmolVMWithSpecStatus(ctx, runningVM, "node-a", "runtime-storage-running-machine")).To(Succeed())
		runningRuntime := &fakeRuntime{machine: &smolvmapi.MachineInfo{Name: "runtime-storage-running-machine", State: "running", CPUs: 1, MemoryMiB: 128, StorageGB: &oldSize}}
		_, err = testReconciler(runningRuntime).Reconcile(ctx, reconcile.Request{NamespacedName: runningName})
		Expect(err).NotTo(HaveOccurred())
		Expect(runningRuntime.resized).To(Equal(0))
		Expect(k8sClient.Get(ctx, runningName, latest)).To(Succeed())
		Expect(conditionReason(latest, vmv1alpha1.ConditionReconciled)).To(Equal("ResizeRequiresStoppedMachine"))
	})

	It("marks runtime failures for create, start, stop, and resize", func() {
		ctx := context.Background()
		cases := []struct {
			name   string
			rt     *fakeRuntime
			vm     *vmv1alpha1.SmolVM
			assert func(*fakeRuntime)
		}{
			{name: "runtime-create-error", rt: &fakeRuntime{createErr: fmt.Errorf("create failed")}, vm: testSmolVM(types.NamespacedName{Name: "runtime-create-error", Namespace: "default"}, "node-a"), assert: func(rt *fakeRuntime) { Expect(rt.created).To(Equal(1)) }},
			{name: "runtime-start-error", rt: &fakeRuntime{machine: &smolvmapi.MachineInfo{Name: "runtime-start-error-machine", State: "stopped", CPUs: 1, MemoryMiB: 128}, startErr: fmt.Errorf("start failed")}, vm: testSmolVM(types.NamespacedName{Name: "runtime-start-error", Namespace: "default"}, "node-a"), assert: func(rt *fakeRuntime) { Expect(rt.started).To(Equal(1)) }},
			{name: "runtime-stop-error", rt: &fakeRuntime{machine: &smolvmapi.MachineInfo{Name: "runtime-stop-error-machine", State: "running", CPUs: 1, MemoryMiB: 128}, stopErr: fmt.Errorf("stop failed")}, vm: stoppedSmolVM(types.NamespacedName{Name: "runtime-stop-error", Namespace: "default"}, "node-a"), assert: func(rt *fakeRuntime) { Expect(rt.stopped).To(Equal(1)) }},
			{name: "runtime-resize-error", rt: &fakeRuntime{machine: &smolvmapi.MachineInfo{Name: "runtime-resize-error-machine", State: "stopped", CPUs: 1, MemoryMiB: 128, StorageGB: int64Ptr(1)}, resizeErr: fmt.Errorf("resize failed")}, vm: storageSmolVM(types.NamespacedName{Name: "runtime-resize-error", Namespace: "default"}, "node-a", 2), assert: func(rt *fakeRuntime) { Expect(rt.resized).To(Equal(1)) }},
		}
		for _, tc := range cases {
			name := types.NamespacedName{Name: tc.name, Namespace: "default"}
			if tc.name == "runtime-create-error" {
				Expect(k8sClient.Create(ctx, tc.vm)).To(Succeed())
				_, err := testReconciler(tc.rt).Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
				_, err = testReconciler(tc.rt).Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(createSmolVMWithSpecStatus(ctx, tc.vm, "node-a", tc.name+"-machine")).To(Succeed())
				_, err := testReconciler(tc.rt).Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
			}
			tc.assert(tc.rt)
			latest := &vmv1alpha1.SmolVM{}
			Expect(k8sClient.Get(ctx, name, latest)).To(Succeed())
			Expect(conditionReason(latest, vmv1alpha1.ConditionReconciled)).To(Equal("RuntimeUnavailable"))
		}
	})

	It("records runtime fields and readiness conditions", func() {
		ctx := context.Background()
		name := types.NamespacedName{Name: "runtime-status-fields", Namespace: "default"}
		machineName := "runtime-status-fields-machine"
		Expect(createSmolVMWithStatus(ctx, name, "node-a", machineName)).To(Succeed())
		pid := int32(1234)
		storage := int64(4)
		runtime := &fakeRuntime{machine: &smolvmapi.MachineInfo{Name: machineName, State: "running", CPUs: 1, MemoryMiB: 128, PID: &pid, Network: true, Ports: []smolvmapi.PortSpec{{Host: 8080, Guest: 80}}, StorageGB: &storage}}
		_, err := testReconciler(runtime).Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).NotTo(HaveOccurred())
		latest := &vmv1alpha1.SmolVM{}
		Expect(k8sClient.Get(ctx, name, latest)).To(Succeed())
		Expect(*latest.Status.RuntimePID).To(Equal(pid))
		Expect(latest.Status.Network).To(BeTrue())
		Expect(latest.Status.Ports).To(Equal([]vmv1alpha1.SmolVMPort{{HostPort: 8080, GuestPort: 80}}))
		Expect(*latest.Status.StorageGiB).To(Equal(storage))
		Expect(conditionReason(latest, vmv1alpha1.ConditionRuntimeReady)).To(Equal("Observed"))
		Expect(conditionReason(latest, vmv1alpha1.ConditionGuestReady)).To(Equal("GuestReadinessUnavailable"))
	})

	It("does not write status when observed state is unchanged", func() {
		ctx := context.Background()
		name := types.NamespacedName{Name: "runtime-status-unchanged", Namespace: "default"}
		machineName := "runtime-status-unchanged-machine"
		Expect(createSmolVMWithStatus(ctx, name, "node-a", machineName)).To(Succeed())
		runtime := &fakeRuntime{machine: &smolvmapi.MachineInfo{Name: machineName, State: "running", CPUs: 1, MemoryMiB: 128}}
		reconciler := testReconciler(runtime)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).NotTo(HaveOccurred())
		latest := &vmv1alpha1.SmolVM{}
		Expect(k8sClient.Get(ctx, name, latest)).To(Succeed())
		resourceVersion := latest.ResourceVersion
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, name, latest)).To(Succeed())
		Expect(latest.ResourceVersion).To(Equal(resourceVersion))
	})

	It("no-ops on other nodes and reports awaiting node assignment", func() {
		ctx := context.Background()
		mismatchName := types.NamespacedName{Name: "runtime-node-mismatch", Namespace: "default"}
		Expect(createSmolVMWithStatus(ctx, mismatchName, "node-a", "runtime-node-mismatch-machine")).To(Succeed())
		withNodeName("node-b", func() {
			runtime := &fakeRuntime{}
			_, err := testReconciler(runtime).Reconcile(ctx, reconcile.Request{NamespacedName: mismatchName})
			Expect(err).NotTo(HaveOccurred())
			Expect(runtime.created + runtime.started + runtime.deleted).To(Equal(0))
		})

		pendingName := types.NamespacedName{Name: "runtime-awaiting-node", Namespace: "default"}
		Expect(k8sClient.Create(ctx, testSmolVM(pendingName, ""))).To(Succeed())
		withNodeName("node-a", func() {
			_, err := testReconciler(&fakeRuntime{}).Reconcile(ctx, reconcile.Request{NamespacedName: pendingName})
			Expect(err).NotTo(HaveOccurred())
		})
		latest := &vmv1alpha1.SmolVM{}
		Expect(k8sClient.Get(ctx, pendingName, latest)).To(Succeed())
		Expect(conditionReason(latest, vmv1alpha1.ConditionReconciled)).To(Equal("AwaitingNodeAssignment"))
	})

	It("rejects invalid specs through CRD admission", func() {
		ctx := context.Background()
		both := testSmolVM(types.NamespacedName{Name: "admission-image-from", Namespace: "default"}, "node-a")
		both.Spec.From = "/tmp/machine.smolmachine"
		Expect(k8sClient.Create(ctx, both)).NotTo(Succeed())

		duplicatePorts := testSmolVM(types.NamespacedName{Name: "admission-duplicate-ports", Namespace: "default"}, "node-a")
		duplicatePorts.Spec.Network.Ports = []vmv1alpha1.SmolVMPort{{HostPort: 8080, GuestPort: 80}, {HostPort: 8080, GuestPort: 81}}
		Expect(k8sClient.Create(ctx, duplicatePorts)).NotTo(Succeed())

		invalidCIDR := testSmolVM(types.NamespacedName{Name: "admission-invalid-cidr", Namespace: "default"}, "node-a")
		invalidCIDR.Spec.Network.AllowedCIDRs = []string{"not-a-cidr"}
		Expect(k8sClient.Create(ctx, invalidCIDR)).NotTo(Succeed())
	})

	It("emits Kubernetes events for lifecycle and failure transitions", func() {
		ctx := context.Background()
		createName := types.NamespacedName{Name: "event-create", Namespace: "default"}
		Expect(k8sClient.Create(ctx, testSmolVM(createName, "node-a"))).To(Succeed())
		createRecorder := record.NewFakeRecorder(4)
		createRuntime := &fakeRuntime{}
		_, err := testReconcilerWithRecorder(createRuntime, createRecorder).Reconcile(ctx, reconcile.Request{NamespacedName: createName})
		Expect(err).NotTo(HaveOccurred())
		_, err = testReconcilerWithRecorder(createRuntime, createRecorder).Reconcile(ctx, reconcile.Request{NamespacedName: createName})
		Expect(err).NotTo(HaveOccurred())
		Eventually(createRecorder.Events).Should(Receive(ContainSubstring("Creating")))

		failureName := types.NamespacedName{Name: "event-runtime-failure", Namespace: "default"}
		Expect(createSmolVMWithStatus(ctx, failureName, "node-a", "event-runtime-failure-machine")).To(Succeed())
		failureRecorder := record.NewFakeRecorder(4)
		failureRuntime := &fakeRuntime{getErr: fmt.Errorf("runtime unavailable")}
		_, err = testReconcilerWithRecorder(failureRuntime, failureRecorder).Reconcile(ctx, reconcile.Request{NamespacedName: failureName})
		Expect(err).NotTo(HaveOccurred())
		Eventually(failureRecorder.Events).Should(Receive(ContainSubstring("RuntimeUnavailable")))
	})
})

func testSmolVM(name types.NamespacedName, nodeName string) *vmv1alpha1.SmolVM {
	return &vmv1alpha1.SmolVM{
		ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace},
		Spec: vmv1alpha1.SmolVMSpec{
			Running:  true,
			NodeName: nodeName,
			Image:    "alpine:latest",
			Resources: vmv1alpha1.SmolVMResources{
				CPUs:      1,
				MemoryMiB: 128,
			},
		},
	}
}

func stoppedSmolVM(name types.NamespacedName, nodeName string) *vmv1alpha1.SmolVM {
	vm := testSmolVM(name, nodeName)
	vm.Spec.Running = false
	return vm
}

func storageSmolVM(name types.NamespacedName, nodeName string, storageGiB int64) *vmv1alpha1.SmolVM {
	vm := testSmolVM(name, nodeName)
	vm.Spec.Storage.StorageGiB = storageGiB
	return vm
}

func int64Ptr(value int64) *int64 {
	return &value
}

func testReconciler(runtime *fakeRuntime) *SmolVMReconciler {
	return testReconcilerWithRecorder(runtime, nil)
}

func testReconcilerWithRecorder(runtime *fakeRuntime, recorder record.EventRecorder) *SmolVMReconciler {
	return &SmolVMReconciler{
		Client:   k8sClient,
		Scheme:   k8sClient.Scheme(),
		Recorder: recorder,
		RuntimeFactory: func() SmolVMRuntime {
			return runtime
		},
	}
}

func createSmolVMWithStatus(ctx context.Context, name types.NamespacedName, nodeName, machineName string) error {
	return createSmolVMWithSpecStatus(ctx, testSmolVM(name, nodeName), nodeName, machineName)
}

func createSmolVMWithSpecStatus(ctx context.Context, vm *vmv1alpha1.SmolVM, nodeName, machineName string) error {
	name := types.NamespacedName{Name: vm.Name, Namespace: vm.Namespace}
	controllerutil.AddFinalizer(vm, vmv1alpha1.SmolVMFinalizer)
	if err := k8sClient.Create(ctx, vm); err != nil {
		return err
	}
	latest := &vmv1alpha1.SmolVM{}
	if err := k8sClient.Get(ctx, name, latest); err != nil {
		return err
	}
	latest.Status.NodeName = nodeName
	latest.Status.MachineName = machineName
	return k8sClient.Status().Update(ctx, latest)
}

func withNodeName(nodeName string, fn func()) {
	previous, hadPrevious := os.LookupEnv("SMOLVM_NODE_NAME")
	Expect(os.Setenv("SMOLVM_NODE_NAME", nodeName)).To(Succeed())
	defer func() {
		if hadPrevious {
			Expect(os.Setenv("SMOLVM_NODE_NAME", previous)).To(Succeed())
			return
		}
		Expect(os.Unsetenv("SMOLVM_NODE_NAME")).To(Succeed())
	}()
	fn()
}

func conditionReason(vm *vmv1alpha1.SmolVM, conditionType string) string {
	for _, condition := range vm.Status.Conditions {
		if condition.Type == conditionType {
			return condition.Reason
		}
	}
	return ""
}

type fakeRuntime struct {
	machine   *smolvmapi.MachineInfo
	getErr    error
	createErr error
	startErr  error
	stopErr   error
	deleteErr error
	resizeErr error
	created   int
	started   int
	stopped   int
	deleted   int
	resized   int
}

func (f *fakeRuntime) GetMachine(context.Context, string) (*smolvmapi.MachineInfo, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.machine == nil {
		return nil, smolvmapi.APIError{StatusCode: http.StatusNotFound}
	}
	return f.machine, nil
}

func (f *fakeRuntime) CreateMachine(_ context.Context, req smolvmapi.CreateMachineRequest) (*smolvmapi.MachineInfo, error) {
	f.created++
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.machine = &smolvmapi.MachineInfo{
		Name:      req.Name,
		State:     "stopped",
		CPUs:      req.CPUs,
		MemoryMiB: req.MemoryMiB,
		Ports:     req.Ports,
		Network:   req.Network,
		StorageGB: req.StorageGB,
		OverlayGB: req.OverlayGB,
	}
	return f.machine, nil
}

func (f *fakeRuntime) EnsureMachineRunning(context.Context, string) error {
	f.started++
	if f.startErr != nil {
		return f.startErr
	}
	if f.machine != nil {
		f.machine.State = "running"
	}
	return nil
}

func (f *fakeRuntime) StopMachine(context.Context, string) error {
	f.stopped++
	if f.stopErr != nil {
		return f.stopErr
	}
	if f.machine != nil {
		f.machine.State = "stopped"
	}
	return nil
}

func (f *fakeRuntime) DeleteMachine(context.Context, string) error {
	f.deleted++
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.machine = nil
	return nil
}

func (f *fakeRuntime) ResizeMachine(context.Context, string, smolvmapi.ResizeRequest) error {
	f.resized++
	return f.resizeErr
}
