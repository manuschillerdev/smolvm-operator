package smolvm

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to a smolvm serve API over HTTP or a Unix socket.
type Client struct {
	baseURL string
	http    *http.Client
	token   string
}

// NewClient creates a smolvm API client. If socketPath is non-empty, baseURL is
// used only for URL construction and requests are transported over the socket.
func NewClient(baseURL, socketPath string) *Client {
	return NewClientWithAuth(baseURL, socketPath, "", false)
}

// NewClientWithAuth creates a smolvm API client with optional bearer-token auth.
func NewClientWithAuth(baseURL, socketPath, token string, insecureSkipVerify bool) *Client {
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8080"
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if insecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	if socketPath != "" {
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		}
		baseURL = "http://unix"
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http: &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		},
	}
}

// MachineInfo mirrors the smolvm API's machine response fields used by the operator.
type MachineInfo struct {
	Name      string     `json:"name"`
	State     string     `json:"state"`
	CPUs      int32      `json:"cpus"`
	MemoryMiB int32      `json:"memoryMb"`
	PID       *int32     `json:"pid,omitempty"`
	Ports     []PortSpec `json:"ports,omitempty"`
	Network   bool       `json:"network"`
	StorageGB *int64     `json:"storageGb,omitempty"`
	OverlayGB *int64     `json:"overlayGb,omitempty"`
}

type ListMachinesResponse struct {
	Machines []MachineInfo `json:"machines"`
}

// Identity reports the runtime API node identity.
type Identity struct {
	NodeName string `json:"nodeName"`
	NodeUID  string `json:"nodeUID,omitempty"`
}

// Health reports runtime health.
type Health struct {
	OK           bool   `json:"ok"`
	KVMAvailable bool   `json:"kvmAvailable"`
	SocketReady  bool   `json:"socketReady"`
	StateReady   bool   `json:"stateReady"`
	Message      string `json:"message,omitempty"`
}

// Capabilities reports runtime node resources and features.
type Capabilities struct {
	RuntimeVersion  string `json:"runtimeVersion,omitempty"`
	ProtocolVersion string `json:"protocolVersion,omitempty"`
	CPUs            int32  `json:"cpus,omitempty"`
	MemoryMiB       int64  `json:"memoryMiB,omitempty"`
	StorageGiB      int64  `json:"storageGiB,omitempty"`
	KVMAvailable    bool   `json:"kvmAvailable"`
}

type CreateMachineRequest struct {
	Name         string     `json:"name,omitempty"`
	CPUs         int32      `json:"cpus,omitempty"`
	MemoryMiB    int32      `json:"memoryMb,omitempty"`
	Ports        []PortSpec `json:"ports,omitempty"`
	Network      bool       `json:"network,omitempty"`
	StorageGB    *int64     `json:"storageGb,omitempty"`
	OverlayGB    *int64     `json:"overlayGb,omitempty"`
	AllowedCIDRs []string   `json:"allowedCidrs,omitempty"`
	Image        string     `json:"image,omitempty"`
	From         string     `json:"from,omitempty"`
}

type PortSpec struct {
	Host  int32 `json:"host"`
	Guest int32 `json:"guest"`
}

type ResizeRequest struct {
	StorageGB *int64 `json:"storageGb,omitempty"`
	OverlayGB *int64 `json:"overlayGb,omitempty"`
}

type ExecRequest struct {
	Command []string `json:"command"`
}

type ExecResponse struct {
	ExitCode int32  `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

func (c *Client) Health(ctx context.Context) (*Health, error) {
	var health Health
	if err := c.request(ctx, http.MethodGet, "/healthz", nil, &health); err != nil {
		return nil, err
	}
	return &health, nil
}

func (c *Client) GetIdentity(ctx context.Context) (*Identity, error) {
	var identity Identity
	if err := c.request(ctx, http.MethodGet, "/api/v1/identity", nil, &identity); err != nil {
		return nil, err
	}
	return &identity, nil
}

func (c *Client) Capabilities(ctx context.Context) (*Capabilities, error) {
	var capabilities Capabilities
	if err := c.request(ctx, http.MethodGet, "/api/v1/capabilities", nil, &capabilities); err != nil {
		return nil, err
	}
	return &capabilities, nil
}

func (c *Client) ListMachines(ctx context.Context) ([]MachineInfo, error) {
	var result ListMachinesResponse
	if err := c.request(ctx, http.MethodGet, "/api/v1/machines", nil, &result); err != nil {
		return nil, err
	}
	return result.Machines, nil
}

func (c *Client) GetMachine(ctx context.Context, name string) (*MachineInfo, error) {
	var machine MachineInfo
	if err := c.request(ctx, http.MethodGet, "/api/v1/machines/"+url.PathEscape(name), nil, &machine); err != nil {
		return nil, err
	}
	return &machine, nil
}

func (c *Client) CreateMachine(ctx context.Context, req CreateMachineRequest) (*MachineInfo, error) {
	var machine MachineInfo
	if err := c.request(ctx, http.MethodPost, "/api/v1/machines", req, &machine); err != nil {
		return nil, err
	}
	return &machine, nil
}

func (c *Client) StartMachine(ctx context.Context, name string) error {
	return c.request(ctx, http.MethodPost, "/api/v1/machines/"+url.PathEscape(name)+"/start", nil, nil)
}

// EnsureMachineRunning starts the machine by using the exec endpoint with a
// no-op command. smolvm's start endpoint has had stale runtime-marker issues
// after API stop on some macOS builds, while exec goes through the same
// ensure-running path used by normal workloads and leaves the VM running.
func (c *Client) EnsureMachineRunning(ctx context.Context, name string) error {
	var result ExecResponse
	if err := c.request(ctx, http.MethodPost, "/api/v1/machines/"+url.PathEscape(name)+"/exec", ExecRequest{Command: []string{"true"}}, &result); err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("ensure running command exited %d: %s", result.ExitCode, result.Stderr)
	}
	return nil
}

func (c *Client) StopMachine(ctx context.Context, name string) error {
	return c.request(ctx, http.MethodPost, "/api/v1/machines/"+url.PathEscape(name)+"/stop", nil, nil)
}

func (c *Client) DeleteMachine(ctx context.Context, name string) error {
	return c.request(ctx, http.MethodDelete, "/api/v1/machines/"+url.PathEscape(name), nil, nil)
}

func (c *Client) ResizeMachine(ctx context.Context, name string, req ResizeRequest) error {
	return c.request(ctx, http.MethodPost, "/api/v1/machines/"+url.PathEscape(name)+"/resize", req, nil)
}

func (c *Client) request(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return APIError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode smolvm response: %w", err)
	}
	return nil
}

type APIError struct {
	StatusCode int
	Body       string
}

func (e APIError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("smolvm API returned HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("smolvm API returned HTTP %d: %s", e.StatusCode, e.Body)
}

func IsNotFound(err error) bool {
	apiErr, ok := err.(APIError)
	return ok && apiErr.StatusCode == http.StatusNotFound
}
