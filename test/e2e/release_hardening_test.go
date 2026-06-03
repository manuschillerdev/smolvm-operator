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

var _ = Describe("release hardening", Ordered, Label("multinode", "failure"), func() {
	BeforeAll(func() {
		if os.Getenv("RUN_E2E") != "true" {
			Skip("set RUN_E2E=true to run deployed hardening e2e tests")
		}
		installManifest := os.Getenv("E2E_INSTALL_MANIFEST")
		if installManifest == "" {
			Skip("E2E_INSTALL_MANIFEST is required")
		}
		createRuntimeSecret()
		kubectl("apply", "-f", installManifest)
		rollout("deployment/operator-controller-manager")
		rollout("daemonset/operator-smolvm-runtime")
		Eventually(func() int {
			return len(nonEmptyLines(kubectlOut("get", "smolvmnodes", "--no-headers")))
		}, 3*time.Minute, 5*time.Second).Should(BeNumerically(">=", 2))
	})

	AfterAll(func() {
		if os.Getenv("RUN_E2E") != "true" {
			return
		}
		_, _ = utils.Run(exec.Command("kubectl", "delete", "smolvm", "--all", "--ignore-not-found=true", "--wait=false"))
	})

	It("schedules unpinned, pinned, selector-constrained, and port-conflicting VMs", func() {
		nodes := smolVMNodeNames()
		Expect(len(nodes)).To(BeNumerically(">=", 2))
		kubectl("label", "smolvmnode", nodes[1], "e2e-role=selected", "--overwrite")

		applySmolVM("hardening-unpinned", "", "", 0)
		waitPath("smolvm/hardening-unpinned", "{.status.nodeName}", notEmpty)
		waitPath("smolvm/hardening-unpinned", "{.status.machineName}", notEmpty)

		applySmolVM("hardening-pinned", "nodeName: "+nodes[1], "", 0)
		waitPath("smolvm/hardening-pinned", "{.status.nodeName}", equals(nodes[1]))

		applySmolVM("hardening-selector", "", "e2e-role: selected", 0)
		waitPath("smolvm/hardening-selector", "{.status.nodeName}", equals(nodes[1]))

		applySmolVM("hardening-port-owner", "nodeName: "+nodes[0], "", 18080)
		waitPath("smolvm/hardening-port-owner", "{.status.nodeName}", equals(nodes[0]))
		applySmolVM("hardening-port-conflict", "nodeName: "+nodes[0], "", 18080)
		waitConditionReason("smolvm/hardening-port-conflict", "Scheduled", "NoEligibleNodes")
	})

	It("does not migrate an already-bound VM when its runtime is unavailable", func() {
		node := strings.TrimSpace(kubectlOut("get", "smolvm/hardening-unpinned", "-o", "jsonpath={.status.nodeName}"))
		Expect(node).NotTo(BeEmpty())
		markRuntimeUnavailable(node)
		kubectl("annotate", "smolvm/hardening-unpinned", "e2e-reconcile=runtime-down", "--overwrite")
		waitPath("smolvm/hardening-unpinned", "{.status.nodeName}", equals(node))
		waitConditionReason("smolvm/hardening-unpinned", "Reconciled", "RuntimeUnavailable")
	})

	It("blocks deletion while the owning runtime is unavailable and honors force delete", func() {
		applySmolVM("hardening-delete-blocked", "", "", 0)
		waitPath("smolvm/hardening-delete-blocked", "{.status.nodeName}", notEmpty)
		node := strings.TrimSpace(kubectlOut("get", "smolvm/hardening-delete-blocked", "-o", "jsonpath={.status.nodeName}"))
		markRuntimeUnavailable(node)
		kubectl("delete", "smolvm/hardening-delete-blocked", "--wait=false")
		waitConditionReason("smolvm/hardening-delete-blocked", "DeletionBlocked", "RuntimeUnavailable")
		kubectl("annotate", "smolvm/hardening-delete-blocked", "vm.smolvm.dev/force-delete-local-state=true", "--overwrite")
		Eventually(func() string {
			return kubectlOutAllowFail("get", "smolvm/hardening-delete-blocked", "--ignore-not-found")
		}, time.Minute, 3*time.Second).Should(BeEmpty())
	})
})

var _ = Describe("upgrade hardening", Ordered, Label("upgrade"), func() {
	It("upgrades a previous install artifact to the RC artifact without losing VM identity", func() {
		if os.Getenv("RUN_E2E") != "true" {
			Skip("set RUN_E2E=true to run deployed upgrade e2e tests")
		}
		previous := os.Getenv("PREVIOUS_INSTALL_MANIFEST")
		rc := os.Getenv("RC_INSTALL_MANIFEST")
		if previous == "" || rc == "" {
			Fail("PREVIOUS_INSTALL_MANIFEST and RC_INSTALL_MANIFEST are required")
		}
		createRuntimeSecret()
		kubectl("apply", "-f", previous)
		rollout("deployment/operator-controller-manager")
		rollout("daemonset/operator-smolvm-runtime")
		applySmolVM("hardening-upgrade", "", "", 0)
		node := waitPath("smolvm/hardening-upgrade", "{.status.nodeName}", notEmpty)
		machine := waitPath("smolvm/hardening-upgrade", "{.status.machineName}", notEmpty)

		kubectl("apply", "-f", rc)
		rollout("deployment/operator-controller-manager")
		rollout("daemonset/operator-smolvm-runtime")
		waitPath("smolvm/hardening-upgrade", "{.status.nodeName}", equals(node))
		waitPath("smolvm/hardening-upgrade", "{.status.machineName}", equals(machine))
		kubectl("patch", "smolvm/hardening-upgrade", "--type=merge", "-p", `{"spec":{"running":false}}`)
		waitPath("smolvm/hardening-upgrade", "{.status.phase}", equals("Stopped"))
		kubectl("patch", "smolvm/hardening-upgrade", "--type=merge", "-p", `{"spec":{"running":true}}`)
		waitPath("smolvm/hardening-upgrade", "{.status.phase}", equals("Running"))
		kubectl("delete", "smolvm/hardening-upgrade", "--timeout=90s")
	})
})

func createRuntimeSecret() {
	bash("kubectl create namespace " + namespace + " --dry-run=client -o yaml | kubectl apply -f -")
	bash("kubectl -n " + namespace + " create secret generic smolvm-runtime-auth " +
		"--from-literal=token=test-token --dry-run=client -o yaml | kubectl apply -f -")
}

func rollout(resource string) {
	kubectl("-n", namespace, "rollout", "status", resource, "--timeout=180s")
}

func applySmolVM(name, pin, selector string, hostPort int) {
	manifest := smolVMManifest(name, pin, selector, hostPort)
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
}

func smolVMManifest(name, pin, selector string, hostPort int) string {
	var b strings.Builder
	b.WriteString("apiVersion: vm.smolvm.dev/v1alpha1\nkind: SmolVM\nmetadata:\n  name: ")
	b.WriteString(name)
	b.WriteString("\nspec:\n")
	if pin != "" {
		b.WriteString("  ")
		b.WriteString(pin)
		b.WriteString("\n")
	}
	if selector != "" {
		b.WriteString("  nodeSelector:\n    ")
		b.WriteString(selector)
		b.WriteString("\n")
	}
	b.WriteString("  running: true\n  image: alpine:latest\n  resources:\n    cpus: 1\n    memoryMiB: 128\n")
	if hostPort > 0 {
		b.WriteString(fmt.Sprintf("  network:\n    ports:\n    - hostPort: %d\n      guestPort: 80\n", hostPort))
	}
	return b.String()
}

func smolVMNodeNames() []string {
	return nonEmptyLines(kubectlOut("get", "smolvmnodes", "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}"))
}

func markRuntimeUnavailable(node string) {
	patch := `{"status":{"heartbeatTime":"2000-01-01T00:00:00Z","endpoint":{"podIP":"127.0.0.1","port":1}}}`
	kubectl("patch", "smolvmnode", node, "--subresource=status", "--type=merge", "-p", patch)
}

func waitConditionReason(resource, conditionType, reason string) {
	path := "{.status.conditions[?(@.type==\"" + conditionType + "\")].reason}"
	waitPath(resource, path, equals(reason))
}

type stringPredicate func(string) bool

func notEmpty(s string) bool { return strings.TrimSpace(s) != "" }
func equals(want string) stringPredicate {
	return func(got string) bool { return strings.TrimSpace(got) == want }
}
func waitPath(resource, path string, predicate stringPredicate) string {
	var value string
	Eventually(func() bool {
		value = kubectlOutAllowFail("get", resource, "-o", "jsonpath="+path)
		return predicate(value)
	}, 2*time.Minute, 3*time.Second).Should(BeTrue(), "last value: %q", value)
	return strings.TrimSpace(value)
}

func bash(script string) {
	_, err := utils.Run(exec.Command("bash", "-c", script))
	Expect(err).NotTo(HaveOccurred())
}

func kubectl(args ...string) {
	_, err := utils.Run(exec.Command("kubectl", args...))
	Expect(err).NotTo(HaveOccurred())
}

func kubectlOut(args ...string) string {
	out, err := utils.Run(exec.Command("kubectl", args...))
	Expect(err).NotTo(HaveOccurred())
	return string(out)
}

func kubectlOutAllowFail(args ...string) string {
	out, _ := utils.Run(exec.Command("kubectl", args...))
	return string(out)
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}
