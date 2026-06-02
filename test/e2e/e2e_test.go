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

		By("waiting for the controller deployment")
		_, err = utils.Run(exec.Command("kubectl", "wait", "deployment", "operator-controller-manager", "-n", namespace, "--for=condition=Available", "--timeout=2m"))
		Expect(err).NotTo(HaveOccurred())

		By("applying a SmolVM resource")
		_, err = utils.Run(exec.Command("kubectl", "apply", "-f", "config/samples/vm_v1alpha1_smolvm.yaml"))
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
