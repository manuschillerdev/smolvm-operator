package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	vmv1alpha1 "github.com/manuschillerdev/smolvm-operator/api/v1alpha1"
	smolvmapi "github.com/manuschillerdev/smolvm-operator/internal/smolvm"
)

const protocolVersion = "v1alpha1"

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(vmv1alpha1.AddToScheme(scheme))
}

//+kubebuilder:rbac:groups=vm.smolvm.dev,resources=smolvmnodes,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups=vm.smolvm.dev,resources=smolvmnodes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get

func main() {
	var bindAddr string
	var socketPath string
	var certFile string
	var keyFile string
	var smolvmServe string
	var reportInterval time.Duration
	var orphanCleanupPolicy string
	flag.StringVar(&bindAddr, "bind-address", ":9443", "runtime API bind address")
	flag.StringVar(&socketPath, "smolvm-api-socket", "/var/run/smolvm/smolvm.sock", "local smolvm serve Unix socket")
	flag.StringVar(&certFile, "tls-cert-file", "", "TLS serving certificate")
	flag.StringVar(&keyFile, "tls-private-key-file", "", "TLS private key")
	flag.StringVar(&smolvmServe, "smolvm-serve-command", "smolvm serve start --listen unix:///var/run/smolvm/smolvm.sock", "command used to start smolvm serve; empty disables supervision")
	flag.DurationVar(&reportInterval, "report-interval", 20*time.Second, "SmolVMNode status report interval")
	flag.StringVar(&orphanCleanupPolicy, "orphan-cleanup-policy", "none", "local orphan cleanup policy: none or delete")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	ctx := ctrl.SetupSignalHandler()

	if strings.TrimSpace(smolvmServe) != "" {
		go supervise(ctx, smolvmServe)
	}

	cfg := ctrl.GetConfigOrDie()
	k8s, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fatalErr(err)
	}

	agent := &agent{
		client:     k8s,
		smolvm:     smolvmapi.NewClient("http://unix", socketPath),
		nodeName:   getenv("NODE_NAME", ""),
		podName:    getenv("POD_NAME", ""),
		podNS:      getenv("POD_NAMESPACE", "default"),
		podIP:      getenv("POD_IP", ""),
		version:    getenv("SMOLVM_RUNTIME_VERSION", "unknown"),
		token:      getenv("SMOLVM_RUNTIME_TOKEN", ""),
		listenPort: portFromAddr(bindAddr),
	}
	if agent.nodeName == "" {
		fatalErr(fmt.Errorf("NODE_NAME is required"))
	}

	go agent.reportLoop(ctx, reportInterval)
	if orphanCleanupPolicy == "delete" {
		go agent.orphanCleanupLoop(ctx, 5*time.Minute)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", agent.healthz)
	mux.HandleFunc("/api/v1/identity", agent.withAuth(agent.identity))
	mux.HandleFunc("/api/v1/capabilities", agent.withAuth(agent.capabilities))
	mux.HandleFunc("/api/v1/machines", agent.withAuth(agent.machines))
	mux.HandleFunc("/api/v1/machines/", agent.withAuth(agent.machine))

	server := &http.Server{Addr: bindAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	if certFile != "" && keyFile != "" {
		fatalErr(server.ListenAndServeTLS(certFile, keyFile))
	}
	server.TLSConfig = selfSignedTLSConfig(agent.nodeName)
	listener, err := tls.Listen("tcp", bindAddr, server.TLSConfig)
	if err != nil {
		fatalErr(err)
	}
	fatalErr(server.Serve(listener))
}

type agent struct {
	client     client.Client
	smolvm     *smolvmapi.Client
	nodeName   string
	nodeUID    string
	podName    string
	podNS      string
	podIP      string
	version    string
	token      string
	listenPort int32
}

func (a *agent) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.token != "" && r.Header.Get("Authorization") != "Bearer "+a.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (a *agent) healthz(w http.ResponseWriter, r *http.Request) {
	health := a.health()
	if !health.OK {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	writeJSON(w, health)
}

func (a *agent) health() smolvmapi.Health {
	socketReady := pathExists("/var/run/smolvm/smolvm.sock")
	stateReady := dirWritable("/var/lib/smolvm")
	kvmAvailable := isCharDevice("/dev/kvm")
	ok := socketReady && stateReady && kvmAvailable
	message := "runtime healthy"
	if !ok {
		var missing []string
		if !socketReady {
			missing = append(missing, "smolvm socket unavailable")
		}
		if !stateReady {
			missing = append(missing, "state directory unavailable")
		}
		if !kvmAvailable {
			missing = append(missing, "KVM unavailable")
		}
		message = strings.Join(missing, "; ")
	}
	return smolvmapi.Health{OK: ok, KVMAvailable: kvmAvailable, SocketReady: socketReady, StateReady: stateReady, Message: message}
}

func (a *agent) identity(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, smolvmapi.Identity{NodeName: a.nodeName, NodeUID: a.nodeUID})
}

func (a *agent) capabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.capabilityReport())
}

func (a *agent) machines(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		machines, err := a.smolvm.ListMachines(r.Context())
		writeResult(w, smolvmapi.ListMachinesResponse{Machines: machines}, err)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req smolvmapi.CreateMachineRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	machine, err := a.smolvm.CreateMachine(r.Context(), req)
	writeResult(w, machine, err)
}

func (a *agent) machine(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/machines/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	name := parts[0]
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			machine, err := a.smolvm.GetMachine(r.Context(), name)
			writeResult(w, machine, err)
		case http.MethodDelete:
			writeResult(w, nil, a.smolvm.DeleteMachine(r.Context(), name))
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var err error
	switch parts[1] {
	case "exec", "start":
		err = a.smolvm.EnsureMachineRunning(r.Context(), name)
	case "stop":
		err = a.smolvm.StopMachine(r.Context(), name)
	case "resize":
		var req smolvmapi.ResizeRequest
		if decErr := json.NewDecoder(r.Body).Decode(&req); decErr != nil {
			http.Error(w, decErr.Error(), http.StatusBadRequest)
			return
		}
		err = a.smolvm.ResizeMachine(r.Context(), name, req)
	default:
		http.NotFound(w, r)
		return
	}
	writeResult(w, nil, err)
}

func (a *agent) reportLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		a.report(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *agent) report(ctx context.Context) {
	var k8sNode corev1.Node
	if err := a.client.Get(ctx, client.ObjectKey{Name: a.nodeName}, &k8sNode); err == nil {
		a.nodeUID = string(k8sNode.UID)
	}
	now := metav1.Now()
	node := &vmv1alpha1.SmolVMNode{ObjectMeta: metav1.ObjectMeta{Name: a.nodeName, Labels: k8sNode.Labels}}
	_, _ = ctrl.CreateOrUpdate(ctx, a.client, node, func() error { return nil })
	latest := &vmv1alpha1.SmolVMNode{}
	if err := a.client.Get(ctx, client.ObjectKey{Name: a.nodeName}, latest); err != nil {
		return
	}
	latest.Labels = k8sNode.Labels
	_ = a.client.Update(ctx, latest)
	latest.Status.NodeUID = a.nodeUID
	latest.Status.RuntimeVersion = a.version
	latest.Status.ProtocolVersion = protocolVersion
	latest.Status.HeartbeatTime = &now
	latest.Status.Endpoint = vmv1alpha1.SmolVMNodeEndpoint{PodName: a.podName, PodNamespace: a.podNS, PodIP: a.podIP, Port: a.listenPort}
	capabilities := a.capabilityReport()
	latest.Status.Allocatable = vmv1alpha1.SmolVMNodeAllocatable{CPUs: capabilities.CPUs, MemoryMiB: capabilities.MemoryMiB, StorageGiB: capabilities.StorageGiB}
	health := a.health()
	setCondition(&latest.Status.Conditions, vmv1alpha1.SmolVMNodeConditionReady, conditionStatus(health.OK), "Reported", health.Message)
	setCondition(&latest.Status.Conditions, vmv1alpha1.SmolVMNodeConditionKVMAvailable, conditionStatus(health.KVMAvailable), "Observed", "KVM device availability observed")
	setCondition(&latest.Status.Conditions, vmv1alpha1.SmolVMNodeConditionRuntimeReady, conditionStatus(health.SocketReady), "Reported", health.Message)
	setCondition(&latest.Status.Conditions, vmv1alpha1.SmolVMNodeConditionSchedulable, conditionStatus(kubernetesNodeReady(&k8sNode) && health.OK), "Reported", "node is schedulable for SmolVM")
	_ = a.client.Status().Update(ctx, latest)
}

func (a *agent) orphanCleanupLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		a.cleanupOrphans(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *agent) cleanupOrphans(ctx context.Context) {
	machines, err := a.smolvm.ListMachines(ctx)
	if err != nil {
		return
	}
	var vms vmv1alpha1.SmolVMList
	if err := a.client.List(ctx, &vms); err != nil {
		return
	}
	owned := map[string]struct{}{}
	for _, vm := range vms.Items {
		if vm.Status.NodeName != a.nodeName || vm.Status.MachineName == "" || !vm.DeletionTimestamp.IsZero() {
			continue
		}
		owned[vm.Status.MachineName] = struct{}{}
	}
	for _, machine := range machines {
		if !strings.HasPrefix(machine.Name, "k8s-") {
			continue
		}
		if _, ok := owned[machine.Name]; ok {
			continue
		}
		_ = a.smolvm.DeleteMachine(ctx, machine.Name)
	}
}

func supervise(ctx context.Context, command string) {
	args := strings.Fields(command)
	if len(args) == 0 {
		return
	}
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		_ = cmd.Run()
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func setCondition(conditions *[]metav1.Condition, typ string, status metav1.ConditionStatus, reason, msg string) {
	for i := range *conditions {
		if (*conditions)[i].Type == typ {
			if (*conditions)[i].Status != status {
				(*conditions)[i].LastTransitionTime = metav1.Now()
			}
			(*conditions)[i].Status = status
			(*conditions)[i].Reason = reason
			(*conditions)[i].Message = msg
			return
		}
	}
	*conditions = append(*conditions, metav1.Condition{Type: typ, Status: status, Reason: reason, Message: msg, LastTransitionTime: metav1.Now()})
}

func selfSignedTLSConfig(nodeName string) *tls.Config {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		fatalErr(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: nodeName},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{nodeName},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		fatalErr(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		fatalErr(err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
}

func (a *agent) capabilityReport() smolvmapi.Capabilities {
	cpus, memoryMiB := int32(0), int64(0)
	var node corev1.Node
	if err := a.client.Get(context.Background(), client.ObjectKey{Name: a.nodeName}, &node); err == nil {
		cpus = int32(node.Status.Allocatable.Cpu().Value())
		memoryMiB = node.Status.Allocatable.Memory().Value() / 1024 / 1024
	}
	return smolvmapi.Capabilities{
		RuntimeVersion:  a.version,
		ProtocolVersion: protocolVersion,
		CPUs:            cpus,
		MemoryMiB:       memoryMiB,
		StorageGiB:      storageGiB("/var/lib/smolvm"),
		KVMAvailable:    isCharDevice("/dev/kvm"),
	}
}

func kubernetesNodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func writeResult(w http.ResponseWriter, out any, err error) {
	if err != nil {
		status := http.StatusInternalServerError
		if smolvmapi.IsNotFound(err) || apierrors.IsNotFound(err) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	if out == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, out)
}

func writeJSON(w http.ResponseWriter, out any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func conditionStatus(ok bool) metav1.ConditionStatus {
	if ok {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func isCharDevice(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func dirWritable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	file, err := os.CreateTemp(path, ".smolvm-write-check-*")
	if err != nil {
		return false
	}
	name := file.Name()
	_ = file.Close()
	_ = os.Remove(name)
	return true
}

func storageGiB(path string) int64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0
	}
	return int64(stat.Bavail) * int64(stat.Bsize) / 1024 / 1024 / 1024
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func portFromAddr(addr string) int32 {
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return 0
	}
	port, _ := strconv.Atoi(addr[idx+1:])
	return int32(port)
}

func fatalErr(err error) {
	if err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}
