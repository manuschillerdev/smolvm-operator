package e2e

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/manuschillerdev/smolvm-operator/test/utils"
)

const namespace = "operator-system"

var _ = Describe("controller", Ordered, func() {
	const projectImage = "example.com/smolvm-operator:e2e"

	AfterAll(func() {
		_, _ = utils.Run(exec.Command("kubectl", "patch", "smolvm", "smolvm-sample", "--type=merge", "-p", `{"metadata":{"finalizers":[]}}`))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "smolvm", "smolvm-sample", "--ignore-not-found=true", "--wait=false"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "-k", "config/default", "--ignore-not-found=true", "--wait=false"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "-k", "config/crd", "--ignore-not-found=true", "--wait=false"))
	})

	It("deploys on kind and reports RuntimeUnavailable without smolvm", func() {
		By("building the manager image")
		_, err := utils.Run(exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", projectImage)))
		Expect(err).NotTo(HaveOccurred())

		By("loading the manager image into kind")
		Expect(utils.LoadImageToKindClusterWithName(projectImage)).To(Succeed())

		By("installing the CRD")
		_, err = utils.Run(exec.Command("make", "install"))
		Expect(err).NotTo(HaveOccurred())

		By("deploying the controller")
		_, err = utils.Run(exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", projectImage)))
		Expect(err).NotTo(HaveOccurred())

		By("waiting for the controller daemonset")
		_, err = utils.Run(exec.Command("kubectl", "rollout", "status", "daemonset/operator-controller-manager", "-n", namespace, "--timeout=2m"))
		Expect(err).NotTo(HaveOccurred())

		By("applying a node-pinned SmolVM resource")
		_, err = utils.Run(exec.Command("bash", "-c", `
node=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
cat <<EOF | kubectl apply -f -
apiVersion: vm.smolvm.dev/v1alpha1
kind: SmolVM
metadata:
  name: smolvm-sample
spec:
  nodeName: ${node}
  running: true
  image: alpine:latest
  resources:
    cpus: 1
    memoryMiB: 128
EOF
`))
		Expect(err).NotTo(HaveOccurred())

		By("observing runtime-unavailable status")
		Eventually(func() string {
			out, err := utils.Run(exec.Command("kubectl", "get", "smolvm", "smolvm-sample", "-o", "jsonpath={.status.conditions[?(@.type=='Ready')].reason}"))
			if err != nil {
				return err.Error()
			}
			return strings.TrimSpace(string(out))
		}, 2*time.Minute, 2*time.Second).Should(Equal("RuntimeUnavailable"))
	})
})
