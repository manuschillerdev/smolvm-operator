package controller

import (
	"context"
	"net/http"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	vmv1alpha1 "github.com/manuschillerdev/smolvm-operator/api/v1alpha1"
	smolvmapi "github.com/manuschillerdev/smolvm-operator/internal/smolvm"
)

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
		resource := &vmv1alpha1.SmolVM{
			ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace},
			Spec: vmv1alpha1.SmolVMSpec{
				Running: true,
				Image:   "alpine:latest",
				Resources: vmv1alpha1.SmolVMResources{
					CPUs:      1,
					MemoryMiB: 128,
				},
			},
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())

		runtime := &fakeRuntime{}
		reconciler := &SmolVMReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			RuntimeFactory: func() SmolVMRuntime {
				return runtime
			},
		}

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
})

type fakeRuntime struct {
	created int
}

func (f *fakeRuntime) GetMachine(context.Context, string) (*smolvmapi.MachineInfo, error) {
	return nil, smolvmapi.APIError{StatusCode: http.StatusNotFound}
}

func (f *fakeRuntime) CreateMachine(_ context.Context, req smolvmapi.CreateMachineRequest) (*smolvmapi.MachineInfo, error) {
	f.created++
	return &smolvmapi.MachineInfo{
		Name:      req.Name,
		State:     "stopped",
		CPUs:      req.CPUs,
		MemoryMiB: req.MemoryMiB,
	}, nil
}

func (f *fakeRuntime) EnsureMachineRunning(context.Context, string) error { return nil }
func (f *fakeRuntime) StopMachine(context.Context, string) error          { return nil }
func (f *fakeRuntime) DeleteMachine(context.Context, string) error        { return nil }
func (f *fakeRuntime) ResizeMachine(context.Context, string, smolvmapi.ResizeRequest) error {
	return nil
}
