package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/manuschillerdev/smolvm-operator/test/utils"
)

const (
	runtimeControllerImage = "example.com/smolvm-operator:e2e"
	runtimeImage           = "example.com/smolvm-runtime:e2e"
	runtimeSmolVMName      = "runtime-smoke"
)

var _ = Describe("runtime lifecycle", Label("runtime"), Ordered, func() {
	var machineName string

	BeforeAll(func() {
		if os.Getenv("RUNTIME_E2E") != "true" {
			Skip("set RUNTIME_E2E=true to run runtime lifecycle e2e tests")
		}
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			dumpRuntimeDiagnostics()
		}
	})

	AfterAll(func() {
		if os.Getenv("RUNTIME_E2E") != "true" {
			return
		}

		_, _ = utils.Run(exec.Command("kubectl", "delete", "smolvm", runtimeSmolVMName, "--ignore-not-found=true", "--wait=false"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "-k", "config/default", "--ignore-not-found=true", "--wait=false"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "-k", "config/crd", "--ignore-not-found=true", "--wait=false"))
	})

	It("builds and sanity-checks controller and runtime images", func() {
		By("building the controller image")
		_, err := utils.Run(exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", runtimeControllerImage)))
		Expect(err).NotTo(HaveOccurred())

		By("building the runtime image")
		_, err = utils.Run(exec.Command("make", "docker-build-runtime", fmt.Sprintf("RUNTIME_IMG=%s", runtimeImage)))
		Expect(err).NotTo(HaveOccurred())

		By("checking the smolvm binary")
		_, err = utils.Run(exec.Command("docker", "run", "--rm", "--entrypoint", "smolvm", runtimeImage, "--version"))
		Expect(err).NotTo(HaveOccurred())

		By("checking the runtime agent binary")
		_, err = utils.Run(exec.Command("docker", "run", "--rm", "--entrypoint", "/runtime-agent", runtimeImage, "--help"))
		Expect(err).NotTo(HaveOccurred())
	})

	It("loads images into kind", func() {
		Expect(utils.LoadImageToKindClusterWithName(runtimeControllerImage)).To(Succeed())
		Expect(utils.LoadImageToKindClusterWithName(runtimeImage)).To(Succeed())
	})

	It("installs CRDs and deploys the full stack", func() {
		By("installing CRDs")
		_, err := utils.Run(exec.Command("make", "install"))
		Expect(err).NotTo(HaveOccurred())

		By("deploying controller and runtime")
		_, err = utils.Run(exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", runtimeControllerImage), fmt.Sprintf("RUNTIME_IMG=%s", runtimeImage)))
		Expect(err).NotTo(HaveOccurred())
	})

	It("waits for controller Deployment and runtime DaemonSet", func() {
		_, err := utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/operator-controller-manager", "-n", namespace, "--timeout=3m"))
		Expect(err).NotTo(HaveOccurred())

		_, err = utils.Run(exec.Command("kubectl", "rollout", "status", "daemonset/operator-smolvm-runtime", "-n", namespace, "--timeout=5m"))
		Expect(err).NotTo(HaveOccurred())
	})

	It("waits for SmolVMNode Ready and RuntimeReady", func() {
		var nodeName string
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "smolvmnodes", "-o", "yaml"))
			if err == nil {
				fmt.Fprintln(GinkgoWriter, string(out))
			}

			nodeName = readyRuntimeNodeName()
			g.Expect(nodeName).NotTo(BeEmpty())
		}, 3*time.Minute, 2*time.Second).Should(Succeed())

		Expect(kubectlOutput("get", "smolvmnode", nodeName, "-o", "jsonpath={.status.endpoint.podNamespace}")).To(Equal(namespace))
	})

	It("creates an unpinned SmolVM and observes Running", func() {
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(`apiVersion: vm.smolvm.dev/v1alpha1
kind: SmolVM
metadata:
  name: runtime-smoke
spec:
  running: true
  image: alpine:latest
  resources:
    cpus: 1
    memoryMiB: 128
`)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() string {
			out, err := utils.Run(exec.Command("kubectl", "get", "smolvm", runtimeSmolVMName, "-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`))
			if err != nil {
				return err.Error()
			}
			return strings.TrimSpace(string(out))
		}, 10*time.Minute, 10*time.Second).Should(Equal("True"))

		Expect(kubectlOutput("get", "smolvm", runtimeSmolVMName, "-o", `jsonpath={.status.conditions[?(@.type=="Scheduled")].reason}`)).To(Equal("Bound"))
		Expect(kubectlOutput("get", "smolvm", runtimeSmolVMName, "-o", `jsonpath={.status.conditions[?(@.type=="RuntimeReady")].status}`)).To(Equal("True"))
		Expect(kubectlOutput("get", "smolvm", runtimeSmolVMName, "-o", "jsonpath={.status.phase}")).To(Equal("Running"))
	})

	It("patches spec.running=false and observes Stopped", func() {
		_, err := utils.Run(exec.Command("kubectl", "patch", "smolvm", runtimeSmolVMName, "--type=merge", "-p", `{"spec":{"running":false}}`))
		Expect(err).NotTo(HaveOccurred())

		_, err = utils.Run(exec.Command("kubectl", "wait", "smolvm", runtimeSmolVMName, "--for=jsonpath={.status.phase}=Stopped", "--timeout=5m"))
		Expect(err).NotTo(HaveOccurred())
	})

	It("deletes the SmolVM and verifies runtime machine cleanup", func() {
		machineName = kubectlOutput("get", "smolvm", runtimeSmolVMName, "-o", "jsonpath={.status.machineName}")
		Expect(machineName).NotTo(BeEmpty())

		runtimePod := kubectlOutput("-n", namespace, "get", "pod", "-l", "app.kubernetes.io/name=smolvm-runtime", "-o", "jsonpath={.items[0].metadata.name}")
		Expect(runtimePod).NotTo(BeEmpty())

		_, err := utils.Run(exec.Command("kubectl", "delete", "smolvm", runtimeSmolVMName, "--wait=true", "--timeout=5m"))
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() string {
			out, err := utils.Run(exec.Command("kubectl", "-n", namespace, "exec", runtimePod, "--", "smolvm", "machine", "ls"))
			if err != nil {
				return err.Error()
			}
			return string(out)
		}, time.Minute, 2*time.Second).ShouldNot(ContainSubstring(machineName))
	})
})

func readyRuntimeNodeName() string {
	out, err := kubectlOutputE("get", "smolvmnodes", "-o", `jsonpath={range .items[*]}{.metadata.name}{"\t"}{.status.conditions[?(@.type=="Ready")].status}{"\t"}{.status.conditions[?(@.type=="RuntimeReady")].status}{"\n"}{end}`)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[1] == "True" && fields[2] == "True" {
			return fields[0]
		}
	}
	return ""
}

func kubectlOutput(args ...string) string {
	out, err := kubectlOutputE(args...)
	Expect(err).NotTo(HaveOccurred())
	return out
}

func kubectlOutputE(args ...string) (string, error) {
	out, err := utils.Run(exec.Command("kubectl", args...))
	return strings.TrimSpace(string(out)), err
}

func dumpRuntimeDiagnostics() {
	commands := []*exec.Cmd{
		exec.Command("kubectl", "-n", namespace, "get", "pods", "-o", "wide"),
		exec.Command("kubectl", "-n", namespace, "logs", "deployment/operator-controller-manager", "--all-containers=true"),
		exec.Command("kubectl", "-n", namespace, "logs", "daemonset/operator-smolvm-runtime", "--all-containers=true"),
		exec.Command("kubectl", "get", "smolvmnodes", "-o", "yaml"),
		exec.Command("kubectl", "get", "smolvm", runtimeSmolVMName, "-o", "yaml"),
	}
	for _, cmd := range commands {
		out, err := utils.Run(cmd)
		fmt.Fprintf(GinkgoWriter, "\n--- %s ---\n%s", strings.Join(cmd.Args, " "), string(out))
		if err != nil {
			fmt.Fprintf(GinkgoWriter, "warning: %v\n", err)
		}
	}
}
