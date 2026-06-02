package controller

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

func testReconciler(runtime *fakeRuntime) *SmolVMReconciler {
	return &SmolVMReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
		RuntimeFactory: func() SmolVMRuntime {
			return runtime
		},
	}
}

func createSmolVMWithStatus(ctx context.Context, name types.NamespacedName, nodeName, machineName string) error {
	vm := testSmolVM(name, nodeName)
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
	deleteErr error
	created   int
	started   int
	stopped   int
	deleted   int
	resized   int
}

func (f *fakeRuntime) GetMachine(context.Context, string) (*smolvmapi.MachineInfo, error) {
	if f.machine == nil {
		return nil, smolvmapi.APIError{StatusCode: http.StatusNotFound}
	}
	return f.machine, nil
}

func (f *fakeRuntime) CreateMachine(_ context.Context, req smolvmapi.CreateMachineRequest) (*smolvmapi.MachineInfo, error) {
	f.created++
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
	if f.machine != nil {
		f.machine.State = "running"
	}
	return nil
}

func (f *fakeRuntime) StopMachine(context.Context, string) error {
	f.stopped++
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
	return nil
}
