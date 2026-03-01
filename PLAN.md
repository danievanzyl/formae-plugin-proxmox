# Formae Proxmox Plugin Implementation Plan

## Overview

Implement a comprehensive Formae plugin for Proxmox VE that manages core compute resources (QEMU VMs and LXC containers) as child resources of discoverable Node parents, using an internal HTTP client with API token auth and async task polling via Proxmox UPIDs.

## Current State Analysis

- Scaffolded Formae plugin template at `v0.1.0`
- All CRUD methods return `ErrNotImplemented`
- Pkl schema has placeholder `ExampleResource` (needs complete replacement)
- Go module: `github.com/danievanyl/formae-plugin-proxmox`, Go 1.25
- Formae SDK v0.1.14, conformance tests v0.1.21

### Key Discoveries:
- `proxmox.go:21` — `Plugin` struct is empty, needs client field
- `schema/pkl/proxmox.pkl:11` — module name is `example`, needs to be `proxmox`
- `formae-plugin.pkl:19` — description is generic `"Proxmox plugin"`, needs update
- `testdata/*.pkl` — reference `example.ExampleResource`, needs complete rewrite
- `examples/basic/main.pkl` — references `example.*`, needs rewrite

## Desired End State

A working Formae plugin that can:
1. Discover Proxmox nodes and list them as `PROXMOX::Node::Node` resources
2. Create, read, update, delete, and discover QEMU VMs as `PROXMOX::Compute::VirtualMachine`
3. Create, read, update, delete, and discover LXC containers as `PROXMOX::Compute::Container`
4. Handle async operations via Proxmox UPID task polling
5. Pass conformance tests against a live Proxmox instance

### Verification:
- `make build` compiles cleanly
- `make lint` passes
- `make install` installs to `~/.pel/formae/plugins/proxmox/v0.1.0/`
- `make conformance-test` passes against a Proxmox instance (with `PROXMOX_API_TOKEN` and target config set)

## What We're NOT Doing

- Storage management (`PROXMOX::Storage::*`)
- Network/bridge management (`PROXMOX::Network::*`)
- Firewall rules
- Backup/restore operations
- Resource pools
- User/permission management
- SDN/HA/Ceph
- Cloud-init configuration
- Advanced VM options (NUMA, CPU pinning, PCI passthrough, etc.)
- Snapshot management
- VM migration
- Template conversion

## Implementation Approach

Build bottom-up: HTTP client → config → types → parent resource (Node) → child resources (VM, Container). Each resource type gets its own Go file. Async operations (Create/Delete) return `InProgress` with Proxmox UPIDs; the `Status()` method polls the task endpoint.

---

## Phase 1: Foundation

### Overview
Build the HTTP client, config parsing, shared types, and Pkl schema for all resource types. This is the foundation everything else depends on.

### Changes Required:

#### 1. HTTP Client
**File**: `client.go` (new)
**Changes**: Internal Proxmox API client with token auth, JSON envelope handling, TLS skip support

```go
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a lightweight Proxmox VE API client.
type Client struct {
	baseURL    string
	apiToken   string
	httpClient *http.Client
}

// apiResponse is the standard Proxmox API response envelope.
type apiResponse struct {
	Data   json.RawMessage        `json:"data"`
	Errors map[string]string      `json:"errors,omitempty"`
}

// NewClient creates a new Proxmox API client.
func NewClient(baseURL, apiToken string, insecure bool) *Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: insecure,
		},
	}
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		apiToken: apiToken,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
	}
}

// Get performs a GET request and returns the unwrapped data field.
func (c *Client) Get(ctx context.Context, path string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(path), nil)
	if err != nil {
		return nil, err
	}
	return c.do(req)
}

// Post performs a POST request with form-encoded params, returns the data field.
func (c *Client) Post(ctx context.Context, path string, params map[string]string) (json.RawMessage, error) {
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(path), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.do(req)
}

// Put performs a PUT request with form-encoded params.
func (c *Client) Put(ctx context.Context, path string, params map[string]string) (json.RawMessage, error) {
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.url(path), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.do(req)
}

// Delete performs a DELETE request, returns the data field.
func (c *Client) Delete(ctx context.Context, path string, params map[string]string) (json.RawMessage, error) {
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	var body io.Reader
	if len(params) > 0 {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.url(path), body)
	if err != nil {
		return nil, err
	}
	if len(params) > 0 {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return c.do(req)
}

func (c *Client) url(path string) string {
	return c.baseURL + "/api2/json" + path
}

func (c *Client) do(req *http.Request) (json.RawMessage, error) {
	req.Header.Set("Authorization", "PVEAPIToken="+c.apiToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var apiResp apiResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	if len(apiResp.Errors) > 0 {
		return nil, fmt.Errorf("API errors: %v", apiResp.Errors)
	}

	return apiResp.Data, nil
}
```

#### 2. Task Polling
**File**: `task.go` (new)
**Changes**: UPID parsing and task status checking for async operations

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// TaskStatus represents the status of a Proxmox async task.
type TaskStatus struct {
	Status     string `json:"status"`     // "running" or "stopped"
	ExitStatus string `json:"exitstatus"` // "OK" or "ERROR: ..." (only when stopped)
	Node       string `json:"node"`
	ID         string `json:"id"`
	Type       string `json:"type"`
	UPID       string `json:"upid"`
}

// IsRunning returns true if the task is still in progress.
func (t *TaskStatus) IsRunning() bool {
	return t.Status == "running"
}

// IsSuccess returns true if the task completed successfully.
func (t *TaskStatus) IsSuccess() bool {
	return t.Status == "stopped" && t.ExitStatus == "OK"
}

// ErrorMessage returns the error message if the task failed.
func (t *TaskStatus) ErrorMessage() string {
	if t.Status == "stopped" && t.ExitStatus != "OK" {
		return t.ExitStatus
	}
	return ""
}

// nodeFromUPID extracts the node name from a UPID string.
// UPID format: UPID:<node>:<pid>:<pstart>:<starttime>:<type>:<id>:<user>:
func nodeFromUPID(upid string) (string, error) {
	parts := strings.Split(upid, ":")
	if len(parts) < 3 || parts[0] != "UPID" {
		return "", fmt.Errorf("invalid UPID: %s", upid)
	}
	return parts[1], nil
}

// GetTaskStatus checks the status of an async task by UPID.
func (c *Client) GetTaskStatus(ctx context.Context, upid string) (*TaskStatus, error) {
	node, err := nodeFromUPID(upid)
	if err != nil {
		return nil, err
	}

	data, err := c.Get(ctx, fmt.Sprintf("/nodes/%s/tasks/%s/status", node, upid))
	if err != nil {
		return nil, fmt.Errorf("getting task status: %w", err)
	}

	var status TaskStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return nil, fmt.Errorf("parsing task status: %w", err)
	}
	return &status, nil
}
```

#### 3. Config Parsing
**File**: `config.go` (new)
**Changes**: Target config and env var parsing, lazy client initialization

```go
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// TargetConfig holds the Proxmox connection configuration from Pkl Config class.
type TargetConfig struct {
	URL      string `json:"Url"`
	Insecure bool   `json:"Insecure"`
}

func parseTargetConfig(data json.RawMessage) (*TargetConfig, error) {
	var cfg TargetConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("invalid target config: %w", err)
	}
	if cfg.URL == "" {
		return nil, fmt.Errorf("target config missing 'Url'")
	}
	return &cfg, nil
}

// clientCache provides thread-safe lazy initialization of the Proxmox client.
type clientCache struct {
	mu     sync.Mutex
	client *Client
}

func (cc *clientCache) get(targetConfig json.RawMessage) (*Client, error) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.client != nil {
		return cc.client, nil
	}

	cfg, err := parseTargetConfig(targetConfig)
	if err != nil {
		return nil, err
	}

	apiToken := os.Getenv("PROXMOX_API_TOKEN")
	if apiToken == "" {
		return nil, fmt.Errorf("PROXMOX_API_TOKEN environment variable not set")
	}

	cc.client = NewClient(cfg.URL, apiToken, cfg.Insecure)
	return cc.client, nil
}
```

#### 4. Shared Types
**File**: `types.go` (new)
**Changes**: Go structs for API request/response mapping

```go
package main

import "fmt"

// --- NativeID helpers ---

// compositeID builds a NativeID from node and vmid: "node/vmid"
func compositeID(node string, vmid int) string {
	return fmt.Sprintf("%s/%d", node, vmid)
}

// parseCompositeID splits a NativeID "node/vmid" into parts.
func parseCompositeID(nativeID string) (node string, vmid int, err error) {
	_, err = fmt.Sscanf(nativeID, "%[^/]/%d", &node, &vmid)
	if err != nil {
		return "", 0, fmt.Errorf("invalid native ID %q: expected node/vmid", nativeID)
	}
	return node, vmid, nil
}

// --- VM types ---

// VMProperties is the formae-facing properties struct for VirtualMachine.
type VMProperties struct {
	ID          string              `json:"Id"`
	Node        string              `json:"node"`
	VMID        int                 `json:"vmid"`
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	Memory      int                 `json:"memory"`
	Cores       int                 `json:"cores"`
	Sockets     int                 `json:"sockets"`
	OSType      string              `json:"ostype"`
	ScsiHW      string              `json:"scsihw"`
	Bios        string              `json:"bios,omitempty"`
	Machine     string              `json:"machine,omitempty"`
	Onboot      *bool               `json:"onboot,omitempty"`
	Disk        *DiskProperties     `json:"disk"`
	Network     *NetworkProperties  `json:"network"`
	Status      string              `json:"status,omitempty"`
}

// DiskProperties maps to VirtualMachineDisk sub-resource.
type DiskProperties struct {
	Storage string `json:"storage"`
	Size    int    `json:"size"`
	Cache   string `json:"cache,omitempty"`
	Discard *bool  `json:"discard,omitempty"`
}

// NetworkProperties maps to VirtualMachineNetwork sub-resource.
type NetworkProperties struct {
	Model    string `json:"model"`
	Bridge   string `json:"bridge"`
	Firewall *bool  `json:"firewall,omitempty"`
	Tag      *int   `json:"tag,omitempty"`
}

// --- Container types ---

// ContainerProperties is the formae-facing properties struct for Container.
type ContainerProperties struct {
	ID           string                    `json:"Id"`
	Node         string                    `json:"node"`
	VMID         int                       `json:"vmid"`
	Hostname     string                    `json:"hostname"`
	Description  string                    `json:"description,omitempty"`
	OSTemplate   string                    `json:"ostemplate"`
	Memory       int                       `json:"memory"`
	Swap         int                       `json:"swap"`
	Cores        int                       `json:"cores"`
	Unprivileged *bool                     `json:"unprivileged,omitempty"`
	Onboot       *bool                     `json:"onboot,omitempty"`
	Rootfs       *ContainerRootfsProperties `json:"rootfs"`
	Network      *ContainerNetProperties    `json:"network"`
	Status       string                    `json:"status,omitempty"`
}

// ContainerRootfsProperties maps to ContainerRootfs sub-resource.
type ContainerRootfsProperties struct {
	Storage string `json:"storage"`
	Size    int    `json:"size"`
}

// ContainerNetProperties maps to ContainerNetwork sub-resource.
type ContainerNetProperties struct {
	Name     string `json:"name"`
	Bridge   string `json:"bridge"`
	IP       string `json:"ip"`
	Gateway  string `json:"gw,omitempty"`
	Firewall *bool  `json:"firewall,omitempty"`
	Tag      *int   `json:"tag,omitempty"`
}

// --- Node types ---

// NodeProperties is the formae-facing properties struct for Node.
type NodeProperties struct {
	Node    string `json:"node"`
	Status  string `json:"status"`
	MaxCPU  int    `json:"maxcpu"`
	MaxMem  int64  `json:"maxmem"`
	MaxDisk int64  `json:"maxdisk"`
}

// proxmoxNodeListEntry represents a node in the GET /nodes response.
type proxmoxNodeListEntry struct {
	Node    string  `json:"node"`
	Status  string  `json:"status"`
	MaxCPU  int     `json:"maxcpu"`
	MaxMem  int64   `json:"maxmem"`
	MaxDisk int64   `json:"maxdisk"`
	CPU     float64 `json:"cpu"`
	Mem     int64   `json:"mem"`
	Disk    int64   `json:"disk"`
	Uptime  int64   `json:"uptime"`
}

// proxmoxVMListEntry represents a VM in the GET /nodes/{node}/qemu response.
type proxmoxVMListEntry struct {
	VMID   int    `json:"vmid"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// proxmoxCTListEntry represents a container in the GET /nodes/{node}/lxc response.
type proxmoxCTListEntry struct {
	VMID   int    `json:"vmid"`
	Name   string `json:"name"`
	Status string `json:"status"`
}
```

#### 5. Plugin Routing
**File**: `proxmox.go` (rewrite)
**Changes**: Add client cache, route CRUD by resource type to per-resource handlers

```go
package main

import (
	"context"
	"fmt"

	"github.com/platform-engineering-labs/formae/pkg/plugin"
	"github.com/platform-engineering-labs/formae/pkg/plugin/resource"
)

const (
	ResourceTypeNode      = "PROXMOX::Node::Node"
	ResourceTypeVM        = "PROXMOX::Compute::VirtualMachine"
	ResourceTypeContainer = "PROXMOX::Compute::Container"
)

// Plugin implements the Formae ResourcePlugin interface for Proxmox VE.
type Plugin struct {
	clients clientCache
}

var _ plugin.ResourcePlugin = &Plugin{}

func (p *Plugin) RateLimit() plugin.RateLimitConfig {
	return plugin.RateLimitConfig{
		Scope:                            plugin.RateLimitScopeNamespace,
		MaxRequestsPerSecondForNamespace: 10,
	}
}

func (p *Plugin) DiscoveryFilters() []plugin.MatchFilter {
	return nil
}

func (p *Plugin) LabelConfig() plugin.LabelConfig {
	return plugin.LabelConfig{
		DefaultQuery: "$.node",
		ResourceOverrides: map[string]string{
			ResourceTypeVM:        "$.name",
			ResourceTypeContainer: "$.hostname",
		},
	}
}

func (p *Plugin) Create(ctx context.Context, req *resource.CreateRequest) (*resource.CreateResult, error) {
	client, err := p.clients.get(req.TargetConfig)
	if err != nil {
		return createFailure(resource.OperationErrorCodeInvalidCredentials, err.Error()), nil
	}
	switch req.ResourceType {
	case ResourceTypeNode:
		return createFailure(resource.OperationErrorCodeInvalidRequest, "nodes are read-only and cannot be created"), nil
	case ResourceTypeVM:
		return p.createVM(ctx, client, req)
	case ResourceTypeContainer:
		return p.createContainer(ctx, client, req)
	default:
		return createFailure(resource.OperationErrorCodeInvalidRequest, fmt.Sprintf("unknown resource type: %s", req.ResourceType)), nil
	}
}

func (p *Plugin) Read(ctx context.Context, req *resource.ReadRequest) (*resource.ReadResult, error) {
	client, err := p.clients.get(req.TargetConfig)
	if err != nil {
		return &resource.ReadResult{ResourceType: req.ResourceType, ErrorCode: resource.OperationErrorCodeInvalidCredentials}, nil
	}
	switch req.ResourceType {
	case ResourceTypeNode:
		return p.readNode(ctx, client, req)
	case ResourceTypeVM:
		return p.readVM(ctx, client, req)
	case ResourceTypeContainer:
		return p.readContainer(ctx, client, req)
	default:
		return &resource.ReadResult{ResourceType: req.ResourceType, ErrorCode: resource.OperationErrorCodeInvalidRequest}, nil
	}
}

func (p *Plugin) Update(ctx context.Context, req *resource.UpdateRequest) (*resource.UpdateResult, error) {
	client, err := p.clients.get(req.TargetConfig)
	if err != nil {
		return updateFailure(resource.OperationErrorCodeInvalidCredentials, err.Error()), nil
	}
	switch req.ResourceType {
	case ResourceTypeNode:
		return updateFailure(resource.OperationErrorCodeInvalidRequest, "nodes are read-only and cannot be updated"), nil
	case ResourceTypeVM:
		return p.updateVM(ctx, client, req)
	case ResourceTypeContainer:
		return p.updateContainer(ctx, client, req)
	default:
		return updateFailure(resource.OperationErrorCodeInvalidRequest, fmt.Sprintf("unknown resource type: %s", req.ResourceType)), nil
	}
}

func (p *Plugin) Delete(ctx context.Context, req *resource.DeleteRequest) (*resource.DeleteResult, error) {
	client, err := p.clients.get(req.TargetConfig)
	if err != nil {
		return deleteFailure(resource.OperationErrorCodeInvalidCredentials, err.Error()), nil
	}
	switch req.ResourceType {
	case ResourceTypeNode:
		return deleteFailure(resource.OperationErrorCodeInvalidRequest, "nodes are read-only and cannot be deleted"), nil
	case ResourceTypeVM:
		return p.deleteVM(ctx, client, req)
	case ResourceTypeContainer:
		return p.deleteContainer(ctx, client, req)
	default:
		return deleteFailure(resource.OperationErrorCodeInvalidRequest, fmt.Sprintf("unknown resource type: %s", req.ResourceType)), nil
	}
}

func (p *Plugin) Status(ctx context.Context, req *resource.StatusRequest) (*resource.StatusResult, error) {
	client, err := p.clients.get(req.TargetConfig)
	if err != nil {
		return statusFailure(resource.OperationErrorCodeInvalidCredentials, err.Error()), nil
	}
	// Status polling is generic — all resource types use UPID-based task polling
	return p.pollTask(ctx, client, req)
}

func (p *Plugin) List(ctx context.Context, req *resource.ListRequest) (*resource.ListResult, error) {
	client, err := p.clients.get(req.TargetConfig)
	if err != nil {
		return &resource.ListResult{NativeIDs: []string{}}, nil
	}
	switch req.ResourceType {
	case ResourceTypeNode:
		return p.listNodes(ctx, client, req)
	case ResourceTypeVM:
		return p.listVMs(ctx, client, req)
	case ResourceTypeContainer:
		return p.listContainers(ctx, client, req)
	default:
		return &resource.ListResult{NativeIDs: []string{}}, nil
	}
}

// --- Result helpers ---

func createFailure(code resource.OperationErrorCode, msg string) *resource.CreateResult {
	return &resource.CreateResult{
		ProgressResult: &resource.ProgressResult{
			Operation:       resource.OperationCreate,
			OperationStatus: resource.OperationStatusFailure,
			ErrorCode:       code,
			StatusMessage:   msg,
		},
	}
}

func updateFailure(code resource.OperationErrorCode, msg string) *resource.UpdateResult {
	return &resource.UpdateResult{
		ProgressResult: &resource.ProgressResult{
			Operation:       resource.OperationUpdate,
			OperationStatus: resource.OperationStatusFailure,
			ErrorCode:       code,
			StatusMessage:   msg,
		},
	}
}

func deleteFailure(code resource.OperationErrorCode, msg string) *resource.DeleteResult {
	return &resource.DeleteResult{
		ProgressResult: &resource.ProgressResult{
			Operation:       resource.OperationDelete,
			OperationStatus: resource.OperationStatusFailure,
			ErrorCode:       code,
			StatusMessage:   msg,
		},
	}
}

func statusFailure(code resource.OperationErrorCode, msg string) *resource.StatusResult {
	return &resource.StatusResult{
		ProgressResult: &resource.ProgressResult{
			Operation:       resource.OperationCheckStatus,
			OperationStatus: resource.OperationStatusFailure,
			ErrorCode:       code,
			StatusMessage:   msg,
		},
	}
}
```

#### 6. Pkl Schema
**File**: `schema/pkl/proxmox.pkl` (rewrite)
**Changes**: Replace example schema with real Proxmox resource types

```pkl
/// Formae plugin schema for Proxmox VE.
/// Manages nodes, QEMU virtual machines, and LXC containers.
module proxmox

import "@formae/formae.pkl"

// =============================================================================
// Target Configuration
// =============================================================================

/// Proxmox VE connection configuration.
/// Secrets (API token) are read from PROXMOX_API_TOKEN env var.
open class Config {
    hidden fixed type: String = "PROXMOX"

    /// Proxmox VE API URL (e.g. "https://proxmox.example.com:8006")
    url: String

    /// Skip TLS certificate verification (for self-signed certs)
    insecure: Boolean = false

    fixed Type: String = type
    fixed Url: String = url
    fixed Insecure: Boolean = insecure
}

// =============================================================================
// Node (parent resource, discoverable, read-only)
// =============================================================================

/// A Proxmox VE cluster node.
@formae.ResourceHint {
    type = "PROXMOX::Node::Node"
    identifier = "$.node"
    discoverable = true
}
class Node extends formae.Resource {
    fixed hidden type: String = "PROXMOX::Node::Node"

    /// Node name
    @formae.FieldHint { readOnly = true }
    node: String

    /// Node status (online/offline)
    @formae.FieldHint { readOnly = true }
    status: String?

    /// Total logical CPUs
    @formae.FieldHint { readOnly = true }
    maxcpu: Int?

    /// Total memory in bytes
    @formae.FieldHint { readOnly = true }
    maxmem: Int?

    /// Total disk in bytes
    @formae.FieldHint { readOnly = true }
    maxdisk: Int?
}

// =============================================================================
// Virtual Machine Sub-Resources
// =============================================================================

/// Disk configuration for a QEMU virtual machine.
@formae.SubResourceHint {}
class VirtualMachineDisk extends formae.SubResource {
    /// Storage name (e.g. "local-lvm")
    @formae.FieldHint {}
    storage: String

    /// Disk size in GiB
    @formae.FieldHint {}
    size: Int

    /// Cache mode (writeback, writethrough, none, directsync, unsafe)
    @formae.FieldHint {}
    cache: String?

    /// Enable TRIM/discard passthrough
    @formae.FieldHint {}
    discard: Boolean?
}

/// Network interface configuration for a QEMU virtual machine.
@formae.SubResourceHint {}
class VirtualMachineNetwork extends formae.SubResource {
    /// NIC model (virtio, e1000, etc.)
    @formae.FieldHint {}
    model: String = "virtio"

    /// Bridge to attach to (e.g. "vmbr0")
    @formae.FieldHint {}
    bridge: String = "vmbr0"

    /// Enable firewall on this interface
    @formae.FieldHint {}
    firewall: Boolean?

    /// VLAN tag (1-4094)
    @formae.FieldHint {}
    tag: Int?
}

// =============================================================================
// Virtual Machine (child of Node)
// =============================================================================

/// A QEMU/KVM virtual machine on a Proxmox VE node.
@formae.ResourceHint {
    type = "PROXMOX::Compute::VirtualMachine"
    identifier = "$.Id"
    parent = "PROXMOX::Node::Node"
    listParam = new formae.ListProperty {
        parentProperty = "node"
        listParameter = "node"
    }
}
class VirtualMachine extends formae.Resource {
    fixed hidden type: String = "PROXMOX::Compute::VirtualMachine"

    /// Target node name (resolved from parent Node)
    @formae.FieldHint { createOnly = true }
    node: String|formae.Resolvable

    /// VM ID (100-999999999). Auto-assigned if omitted.
    @formae.FieldHint { createOnly = true }
    vmid: Int?

    /// Display name
    @formae.FieldHint {}
    name: String

    /// Description / notes
    @formae.FieldHint {}
    description: String?

    /// Memory in MiB
    @formae.FieldHint {}
    memory: Int = 2048

    /// CPU cores per socket
    @formae.FieldHint {}
    cores: Int = 1

    /// Number of CPU sockets
    @formae.FieldHint {}
    sockets: Int = 1

    /// Guest OS type (l26, win10, win11, etc.)
    @formae.FieldHint { createOnly = true }
    ostype: String = "l26"

    /// SCSI controller type
    @formae.FieldHint { createOnly = true }
    scsihw: String = "virtio-scsi-pci"

    /// BIOS type (seabios or ovmf)
    @formae.FieldHint { createOnly = true }
    bios: String?

    /// Machine type (q35, pc, etc.)
    @formae.FieldHint { createOnly = true }
    machine: String?

    /// Start VM on host boot
    @formae.FieldHint {}
    onboot: Boolean?

    /// Primary disk configuration
    @formae.FieldHint { createOnly = true }
    disk: VirtualMachineDisk

    /// Primary network interface
    @formae.FieldHint { createOnly = true }
    network: VirtualMachineNetwork

    /// Current VM status (read-only, computed)
    @formae.FieldHint { readOnly = true }
    status: String?
}

// =============================================================================
// Container Sub-Resources
// =============================================================================

/// Root filesystem configuration for an LXC container.
@formae.SubResourceHint {}
class ContainerRootfs extends formae.SubResource {
    /// Storage name (e.g. "local-lvm")
    @formae.FieldHint {}
    storage: String

    /// Root filesystem size in GiB
    @formae.FieldHint {}
    size: Int
}

/// Network interface configuration for an LXC container.
@formae.SubResourceHint {}
class ContainerNetwork extends formae.SubResource {
    /// Interface name inside the container
    @formae.FieldHint {}
    name: String = "eth0"

    /// Bridge to attach to
    @formae.FieldHint {}
    bridge: String = "vmbr0"

    /// IPv4 address (CIDR notation or "dhcp")
    @formae.FieldHint {}
    ip: String = "dhcp"

    /// IPv4 gateway
    @formae.FieldHint {}
    gw: String?

    /// Enable firewall
    @formae.FieldHint {}
    firewall: Boolean?

    /// VLAN tag (1-4094)
    @formae.FieldHint {}
    tag: Int?
}

// =============================================================================
// Container (child of Node)
// =============================================================================

/// An LXC container on a Proxmox VE node.
@formae.ResourceHint {
    type = "PROXMOX::Compute::Container"
    identifier = "$.Id"
    parent = "PROXMOX::Node::Node"
    listParam = new formae.ListProperty {
        parentProperty = "node"
        listParameter = "node"
    }
}
class Container extends formae.Resource {
    fixed hidden type: String = "PROXMOX::Compute::Container"

    /// Target node name (resolved from parent Node)
    @formae.FieldHint { createOnly = true }
    node: String|formae.Resolvable

    /// Container ID (100-999999999). Auto-assigned if omitted.
    @formae.FieldHint { createOnly = true }
    vmid: Int?

    /// Container hostname
    @formae.FieldHint {}
    hostname: String

    /// Description / notes
    @formae.FieldHint {}
    description: String?

    /// OS template (e.g. "local:vztmpl/debian-12-standard_12.0-1_amd64.tar.zst")
    @formae.FieldHint { createOnly = true }
    ostemplate: String

    /// Memory in MiB
    @formae.FieldHint {}
    memory: Int = 512

    /// Swap in MiB
    @formae.FieldHint {}
    swap: Int = 512

    /// CPU cores
    @formae.FieldHint {}
    cores: Int = 1

    /// Run as unprivileged container
    @formae.FieldHint { createOnly = true }
    unprivileged: Boolean = true

    /// Start container on host boot
    @formae.FieldHint {}
    onboot: Boolean?

    /// Root filesystem configuration
    @formae.FieldHint { createOnly = true }
    rootfs: ContainerRootfs

    /// Primary network interface
    @formae.FieldHint { createOnly = true }
    network: ContainerNetwork

    /// Root password (write-only, never returned by Read)
    @formae.FieldHint { writeOnly = true }
    password: String?

    /// Current container status (read-only, computed)
    @formae.FieldHint { readOnly = true }
    status: String?
}
```

#### 7. Update Plugin Manifest
**File**: `formae-plugin.pkl`
**Changes**: Update description

```pkl
name = "proxmox"
version = "0.1.0"
namespace = "PROXMOX"
description = "Proxmox VE plugin for managing nodes, VMs, and containers"
license = "MIT"
minFormaeVersion = "0.80.1"

output {
  renderer = new JsonRenderer {}
}
```

### Success Criteria:

#### Automated Verification:
- [ ] `make build` compiles without errors
- [ ] `make lint` passes (if golangci-lint installed)
- [ ] Pkl schema validates: `make verify-schema`

#### Manual Verification:
- [ ] Review that Pkl schema field hints match expected Proxmox behavior (createOnly, readOnly, writeOnly)
- [ ] Verify Config class outputs correct JSON shape

**Implementation Note**: After completing this phase and all automated verification passes, pause here for manual confirmation before proceeding to Phase 2.

---

## Phase 2: Node Resource

### Overview
Implement List and Read for `PROXMOX::Node::Node`. Nodes are infrastructure — read-only, discoverable parent resources.

### Changes Required:

#### 1. Node Handlers
**File**: `node.go` (new)
**Changes**: List and Read implementations for nodes

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/platform-engineering-labs/formae/pkg/plugin/resource"
)

func (p *Plugin) listNodes(ctx context.Context, client *Client, req *resource.ListRequest) (*resource.ListResult, error) {
	data, err := client.Get(ctx, "/nodes")
	if err != nil {
		return &resource.ListResult{NativeIDs: []string{}}, nil
	}

	var nodes []proxmoxNodeListEntry
	if err := json.Unmarshal(data, &nodes); err != nil {
		return &resource.ListResult{NativeIDs: []string{}}, nil
	}

	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.Node)
	}
	return &resource.ListResult{NativeIDs: ids}, nil
}

func (p *Plugin) readNode(ctx context.Context, client *Client, req *resource.ReadRequest) (*resource.ReadResult, error) {
	// Get node list and find the one matching our NativeID
	data, err := client.Get(ctx, "/nodes")
	if err != nil {
		return &resource.ReadResult{
			ResourceType: req.ResourceType,
			ErrorCode:    resource.OperationErrorCodeNetworkFailure,
		}, nil
	}

	var nodes []proxmoxNodeListEntry
	if err := json.Unmarshal(data, &nodes); err != nil {
		return &resource.ReadResult{
			ResourceType: req.ResourceType,
			ErrorCode:    resource.OperationErrorCodeInternalFailure,
		}, nil
	}

	for _, n := range nodes {
		if n.Node == req.NativeID {
			props := NodeProperties{
				Node:    n.Node,
				Status:  n.Status,
				MaxCPU:  n.MaxCPU,
				MaxMem:  n.MaxMem,
				MaxDisk: n.MaxDisk,
			}
			propsJSON, err := json.Marshal(props)
			if err != nil {
				return &resource.ReadResult{
					ResourceType: req.ResourceType,
					ErrorCode:    resource.OperationErrorCodeInternalFailure,
				}, nil
			}
			return &resource.ReadResult{
				ResourceType: req.ResourceType,
				Properties:   string(propsJSON),
			}, nil
		}
	}

	return &resource.ReadResult{
		ResourceType: req.ResourceType,
		ErrorCode:    resource.OperationErrorCodeNotFound,
	}, nil
}
```

### Success Criteria:

#### Automated Verification:
- [ ] `make build` compiles without errors
- [ ] `make lint` passes

#### Manual Verification:
- [ ] `make install` and test against a Proxmox instance — nodes appear in discovery
- [ ] Read returns correct node properties (name, status, cpu/mem/disk counts)

**Implementation Note**: Pause for manual confirmation before Phase 3.

---

## Phase 3: VM Resource

### Overview
Implement full CRUD + async Status + List for `PROXMOX::Compute::VirtualMachine`. This is the core resource type. Create and Delete are async (return UPID), Update is synchronous.

### Changes Required:

#### 1. VM Handlers
**File**: `vm.go` (new)
**Changes**: Full CRUD implementation for QEMU VMs

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/platform-engineering-labs/formae/pkg/plugin/resource"
)

// --- List ---

func (p *Plugin) listVMs(ctx context.Context, client *Client, req *resource.ListRequest) (*resource.ListResult, error) {
	node, ok := req.AdditionalProperties["node"]
	if !ok || node == "" {
		return &resource.ListResult{NativeIDs: []string{}}, nil
	}

	data, err := client.Get(ctx, fmt.Sprintf("/nodes/%s/qemu", node))
	if err != nil {
		return &resource.ListResult{NativeIDs: []string{}}, nil
	}

	var vms []proxmoxVMListEntry
	if err := json.Unmarshal(data, &vms); err != nil {
		return &resource.ListResult{NativeIDs: []string{}}, nil
	}

	ids := make([]string, 0, len(vms))
	for _, vm := range vms {
		ids = append(ids, compositeID(node, vm.VMID))
	}
	return &resource.ListResult{NativeIDs: ids}, nil
}

// --- Create ---

func (p *Plugin) createVM(ctx context.Context, client *Client, req *resource.CreateRequest) (*resource.CreateResult, error) {
	var props VMProperties
	if err := json.Unmarshal(req.Properties, &props); err != nil {
		return createFailure(resource.OperationErrorCodeInvalidRequest, fmt.Sprintf("invalid properties: %v", err)), nil
	}

	node := resolveString(props.Node)
	if node == "" {
		return createFailure(resource.OperationErrorCodeInvalidRequest, "node is required"), nil
	}

	// Auto-assign VMID if not provided
	vmid := props.VMID
	if vmid == 0 {
		nextID, err := getNextID(ctx, client)
		if err != nil {
			return createFailure(resource.OperationErrorCodeInternalFailure, fmt.Sprintf("getting next vmid: %v", err)), nil
		}
		vmid = nextID
	}

	params := map[string]string{
		"vmid":    strconv.Itoa(vmid),
		"name":    props.Name,
		"memory":  strconv.Itoa(props.Memory),
		"cores":   strconv.Itoa(props.Cores),
		"sockets": strconv.Itoa(props.Sockets),
		"ostype":  props.OSType,
		"scsihw":  props.ScsiHW,
	}

	if props.Description != "" {
		params["description"] = props.Description
	}
	if props.Bios != "" {
		params["bios"] = props.Bios
	}
	if props.Machine != "" {
		params["machine"] = props.Machine
	}
	if props.Onboot != nil && *props.Onboot {
		params["onboot"] = "1"
	}

	// Build disk spec: scsi0
	if props.Disk != nil {
		params["scsi0"] = buildVMDiskSpec(props.Disk)
	}

	// Build network spec: net0
	if props.Network != nil {
		params["net0"] = buildVMNetSpec(props.Network)
	}

	nativeID := compositeID(node, vmid)

	data, err := client.Post(ctx, fmt.Sprintf("/nodes/%s/qemu", node), params)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			return createFailure(resource.OperationErrorCodeAlreadyExists, err.Error()), nil
		}
		return createFailure(resource.OperationErrorCodeInternalFailure, err.Error()), nil
	}

	// Extract UPID from response
	var upid string
	if err := json.Unmarshal(data, &upid); err != nil {
		return createFailure(resource.OperationErrorCodeInternalFailure, fmt.Sprintf("parsing UPID: %v", err)), nil
	}

	return &resource.CreateResult{
		ProgressResult: &resource.ProgressResult{
			Operation:       resource.OperationCreate,
			OperationStatus: resource.OperationStatusInProgress,
			RequestID:       upid,
			NativeID:        nativeID,
		},
	}, nil
}

// --- Read ---

func (p *Plugin) readVM(ctx context.Context, client *Client, req *resource.ReadRequest) (*resource.ReadResult, error) {
	node, vmid, err := parseCompositeID(req.NativeID)
	if err != nil {
		return &resource.ReadResult{ResourceType: req.ResourceType, ErrorCode: resource.OperationErrorCodeNotFound}, nil
	}

	// Get VM config
	configData, err := client.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid))
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "500") {
			return &resource.ReadResult{ResourceType: req.ResourceType, ErrorCode: resource.OperationErrorCodeNotFound}, nil
		}
		return &resource.ReadResult{ResourceType: req.ResourceType, ErrorCode: resource.OperationErrorCodeNetworkFailure}, nil
	}

	// Get VM status
	statusData, err := client.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/status/current", node, vmid))
	if err != nil {
		return &resource.ReadResult{ResourceType: req.ResourceType, ErrorCode: resource.OperationErrorCodeNetworkFailure}, nil
	}

	props, err := parseVMConfig(node, vmid, configData, statusData)
	if err != nil {
		return &resource.ReadResult{ResourceType: req.ResourceType, ErrorCode: resource.OperationErrorCodeInternalFailure}, nil
	}

	propsJSON, err := json.Marshal(props)
	if err != nil {
		return &resource.ReadResult{ResourceType: req.ResourceType, ErrorCode: resource.OperationErrorCodeInternalFailure}, nil
	}

	return &resource.ReadResult{
		ResourceType: req.ResourceType,
		Properties:   string(propsJSON),
	}, nil
}

// --- Update ---

func (p *Plugin) updateVM(ctx context.Context, client *Client, req *resource.UpdateRequest) (*resource.UpdateResult, error) {
	node, vmid, err := parseCompositeID(req.NativeID)
	if err != nil {
		return updateFailure(resource.OperationErrorCodeNotFound, err.Error()), nil
	}

	var desired VMProperties
	if err := json.Unmarshal(req.DesiredProperties, &desired); err != nil {
		return updateFailure(resource.OperationErrorCodeInvalidRequest, fmt.Sprintf("invalid desired properties: %v", err)), nil
	}

	params := map[string]string{}

	// Only update mutable fields
	if desired.Name != "" {
		params["name"] = desired.Name
	}
	params["description"] = desired.Description
	params["memory"] = strconv.Itoa(desired.Memory)
	params["cores"] = strconv.Itoa(desired.Cores)
	params["sockets"] = strconv.Itoa(desired.Sockets)
	if desired.Onboot != nil {
		if *desired.Onboot {
			params["onboot"] = "1"
		} else {
			params["onboot"] = "0"
		}
	}

	_, err = client.Put(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid), params)
	if err != nil {
		return updateFailure(resource.OperationErrorCodeInternalFailure, err.Error()), nil
	}

	// Read back current state
	readResult, _ := p.readVM(ctx, client, &resource.ReadRequest{
		NativeID:     req.NativeID,
		ResourceType: req.ResourceType,
		TargetConfig: req.TargetConfig,
	})

	var resourceProps json.RawMessage
	if readResult != nil && readResult.Properties != "" {
		resourceProps = json.RawMessage(readResult.Properties)
	}

	return &resource.UpdateResult{
		ProgressResult: &resource.ProgressResult{
			Operation:          resource.OperationUpdate,
			OperationStatus:    resource.OperationStatusSuccess,
			NativeID:           req.NativeID,
			ResourceProperties: resourceProps,
		},
	}, nil
}

// --- Delete ---

func (p *Plugin) deleteVM(ctx context.Context, client *Client, req *resource.DeleteRequest) (*resource.DeleteResult, error) {
	node, vmid, err := parseCompositeID(req.NativeID)
	if err != nil {
		// If we can't parse the ID, treat as already deleted
		return &resource.DeleteResult{
			ProgressResult: &resource.ProgressResult{
				Operation:       resource.OperationDelete,
				OperationStatus: resource.OperationStatusSuccess,
				NativeID:        req.NativeID,
			},
		}, nil
	}

	// Stop VM first if running (best effort)
	_ = stopVM(ctx, client, node, vmid)

	data, err := client.Delete(ctx, fmt.Sprintf("/nodes/%s/qemu/%d", node, vmid), map[string]string{
		"purge":                       "1",
		"destroy-unreferenced-disks":  "1",
	})
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "not exist") {
			return &resource.DeleteResult{
				ProgressResult: &resource.ProgressResult{
					Operation:       resource.OperationDelete,
					OperationStatus: resource.OperationStatusSuccess,
					NativeID:        req.NativeID,
				},
			}, nil
		}
		return deleteFailure(resource.OperationErrorCodeInternalFailure, err.Error()), nil
	}

	var upid string
	if err := json.Unmarshal(data, &upid); err != nil {
		return deleteFailure(resource.OperationErrorCodeInternalFailure, fmt.Sprintf("parsing UPID: %v", err)), nil
	}

	return &resource.DeleteResult{
		ProgressResult: &resource.ProgressResult{
			Operation:       resource.OperationDelete,
			OperationStatus: resource.OperationStatusInProgress,
			RequestID:       upid,
			NativeID:        req.NativeID,
		},
	}, nil
}

// --- Helpers ---

func getNextID(ctx context.Context, client *Client) (int, error) {
	data, err := client.Get(ctx, "/cluster/nextid")
	if err != nil {
		return 0, err
	}
	var idStr string
	if err := json.Unmarshal(data, &idStr); err != nil {
		return 0, fmt.Errorf("parsing nextid: %w", err)
	}
	return strconv.Atoi(idStr)
}

func stopVM(ctx context.Context, client *Client, node string, vmid int) error {
	_, err := client.Post(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/status/stop", node, vmid), nil)
	return err
}

func buildVMDiskSpec(d *DiskProperties) string {
	spec := fmt.Sprintf("%s:%d", d.Storage, d.Size)
	if d.Cache != "" {
		spec += ",cache=" + d.Cache
	}
	if d.Discard != nil && *d.Discard {
		spec += ",discard=on"
	}
	return spec
}

func buildVMNetSpec(n *NetworkProperties) string {
	spec := n.Model + ",bridge=" + n.Bridge
	if n.Firewall != nil && *n.Firewall {
		spec += ",firewall=1"
	}
	if n.Tag != nil {
		spec += ",tag=" + strconv.Itoa(*n.Tag)
	}
	return spec
}

// resolveString extracts a string from a value that may be a string or a
// Resolvable reference ({"$ref": "...", "$value": "..."}).
func resolveString(v interface{}) string {
	// When coming from JSON unmarshal into interface{}, it's a string
	switch val := v.(type) {
	case string:
		return val
	default:
		return ""
	}
}

// parseVMConfig converts Proxmox API config + status responses to VMProperties.
func parseVMConfig(node string, vmid int, configData, statusData json.RawMessage) (*VMProperties, error) {
	var config map[string]interface{}
	if err := json.Unmarshal(configData, &config); err != nil {
		return nil, err
	}

	var statusMap map[string]interface{}
	if err := json.Unmarshal(statusData, &statusMap); err != nil {
		return nil, err
	}

	props := &VMProperties{
		ID:   compositeID(node, vmid),
		Node: node,
		VMID: vmid,
	}

	if v, ok := config["name"].(string); ok {
		props.Name = v
	}
	if v, ok := config["description"].(string); ok {
		props.Description = v
	}
	if v, ok := config["memory"].(float64); ok {
		props.Memory = int(v)
	}
	if v, ok := config["cores"].(float64); ok {
		props.Cores = int(v)
	}
	if v, ok := config["sockets"].(float64); ok {
		props.Sockets = int(v)
	}
	if v, ok := config["ostype"].(string); ok {
		props.OSType = v
	}
	if v, ok := config["scsihw"].(string); ok {
		props.ScsiHW = v
	}
	if v, ok := config["bios"].(string); ok {
		props.Bios = v
	}
	if v, ok := config["machine"].(string); ok {
		props.Machine = v
	}
	if v, ok := config["onboot"].(float64); ok {
		b := v == 1
		props.Onboot = &b
	}

	// Parse disk from scsi0
	if scsi0, ok := config["scsi0"].(string); ok {
		props.Disk = parseVMDiskFromConfig(scsi0)
	}

	// Parse network from net0
	if net0, ok := config["net0"].(string); ok {
		props.Network = parseVMNetFromConfig(net0)
	}

	// Status from status endpoint
	if v, ok := statusMap["status"].(string); ok {
		props.Status = v
	}

	return props, nil
}

// parseVMDiskFromConfig parses "local-lvm:vm-100-disk-0,size=32G,cache=writeback"
func parseVMDiskFromConfig(spec string) *DiskProperties {
	d := &DiskProperties{}
	parts := strings.Split(spec, ",")
	if len(parts) == 0 {
		return d
	}
	// First part: storage:volume-name or storage:size
	volParts := strings.SplitN(parts[0], ":", 2)
	if len(volParts) >= 1 {
		d.Storage = volParts[0]
	}
	for _, part := range parts[1:] {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "size":
			// Parse "32G" -> 32
			sizeStr := strings.TrimSuffix(kv[1], "G")
			if s, err := strconv.Atoi(sizeStr); err == nil {
				d.Size = s
			}
		case "cache":
			d.Cache = kv[1]
		case "discard":
			b := kv[1] == "on"
			d.Discard = &b
		}
	}
	return d
}

// parseVMNetFromConfig parses "virtio=XX:XX:XX:XX:XX:XX,bridge=vmbr0,firewall=1"
func parseVMNetFromConfig(spec string) *NetworkProperties {
	n := &NetworkProperties{Model: "virtio", Bridge: "vmbr0"}
	parts := strings.Split(spec, ",")
	for _, part := range parts {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "bridge":
			n.Bridge = kv[1]
		case "firewall":
			b := kv[1] == "1"
			n.Firewall = &b
		case "tag":
			if t, err := strconv.Atoi(kv[1]); err == nil {
				n.Tag = &t
			}
		case "model":
			n.Model = kv[1]
		default:
			// First part may be "virtio=MACADDR" — extract model
			if strings.Contains(kv[1], ":") && len(kv[1]) == 17 {
				n.Model = kv[0]
			}
		}
	}
	return n
}
```

#### 2. Async Status Polling (generic)
**File**: `status.go` (new)
**Changes**: Generic UPID-based task polling used by all resource types

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/platform-engineering-labs/formae/pkg/plugin/resource"
)

func (p *Plugin) pollTask(ctx context.Context, client *Client, req *resource.StatusRequest) (*resource.StatusResult, error) {
	taskStatus, err := client.GetTaskStatus(ctx, req.RequestID)
	if err != nil {
		return statusFailure(resource.OperationErrorCodeNetworkFailure, fmt.Sprintf("polling task: %v", err)), nil
	}

	if taskStatus.IsRunning() {
		return &resource.StatusResult{
			ProgressResult: &resource.ProgressResult{
				Operation:       resource.OperationCheckStatus,
				OperationStatus: resource.OperationStatusInProgress,
				RequestID:       req.RequestID,
				NativeID:        req.NativeID,
				StatusMessage:   "task running",
			},
		}, nil
	}

	if taskStatus.IsSuccess() {
		// Read back the resource to get current properties
		readResult, _ := p.Read(ctx, &resource.ReadRequest{
			NativeID:     req.NativeID,
			ResourceType: req.ResourceType,
			TargetConfig: req.TargetConfig,
		})

		var resourceProps json.RawMessage
		if readResult != nil && readResult.Properties != "" {
			resourceProps = json.RawMessage(readResult.Properties)
		}

		return &resource.StatusResult{
			ProgressResult: &resource.ProgressResult{
				Operation:          resource.OperationCheckStatus,
				OperationStatus:    resource.OperationStatusSuccess,
				NativeID:           req.NativeID,
				ResourceProperties: resourceProps,
			},
		}, nil
	}

	// Task failed
	errMsg := taskStatus.ErrorMessage()
	errCode := resource.OperationErrorCodeInternalFailure
	if strings.Contains(errMsg, "permission") || strings.Contains(errMsg, "denied") {
		errCode = resource.OperationErrorCodeAccessDenied
	}

	return &resource.StatusResult{
		ProgressResult: &resource.ProgressResult{
			Operation:       resource.OperationCheckStatus,
			OperationStatus: resource.OperationStatusFailure,
			NativeID:        req.NativeID,
			ErrorCode:       errCode,
			StatusMessage:   errMsg,
		},
	}, nil
}
```

### Success Criteria:

#### Automated Verification:
- [ ] `make build` compiles without errors
- [ ] `make lint` passes

#### Manual Verification:
- [ ] Create a VM via formae against a Proxmox instance
- [ ] Read returns correct VM config (name, memory, cores, disk, network)
- [ ] Update mutable fields (name, memory, cores) works
- [ ] Delete stops and removes the VM
- [ ] List discovers existing VMs on a node
- [ ] Async task polling works for Create and Delete

**Implementation Note**: Pause for manual confirmation before Phase 4.

---

## Phase 4: Container Resource

### Overview
Implement full CRUD + async Status + List for `PROXMOX::Compute::Container`. Follows the same patterns as VM with LXC-specific parameter formatting.

### Changes Required:

#### 1. Container Handlers
**File**: `container.go` (new)
**Changes**: Full CRUD for LXC containers

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/platform-engineering-labs/formae/pkg/plugin/resource"
)

// --- List ---

func (p *Plugin) listContainers(ctx context.Context, client *Client, req *resource.ListRequest) (*resource.ListResult, error) {
	node, ok := req.AdditionalProperties["node"]
	if !ok || node == "" {
		return &resource.ListResult{NativeIDs: []string{}}, nil
	}

	data, err := client.Get(ctx, fmt.Sprintf("/nodes/%s/lxc", node))
	if err != nil {
		return &resource.ListResult{NativeIDs: []string{}}, nil
	}

	var cts []proxmoxCTListEntry
	if err := json.Unmarshal(data, &cts); err != nil {
		return &resource.ListResult{NativeIDs: []string{}}, nil
	}

	ids := make([]string, 0, len(cts))
	for _, ct := range cts {
		ids = append(ids, compositeID(node, ct.VMID))
	}
	return &resource.ListResult{NativeIDs: ids}, nil
}

// --- Create ---

func (p *Plugin) createContainer(ctx context.Context, client *Client, req *resource.CreateRequest) (*resource.CreateResult, error) {
	var props ContainerProperties
	if err := json.Unmarshal(req.Properties, &props); err != nil {
		return createFailure(resource.OperationErrorCodeInvalidRequest, fmt.Sprintf("invalid properties: %v", err)), nil
	}

	node := resolveString(props.Node)
	if node == "" {
		return createFailure(resource.OperationErrorCodeInvalidRequest, "node is required"), nil
	}

	vmid := props.VMID
	if vmid == 0 {
		nextID, err := getNextID(ctx, client)
		if err != nil {
			return createFailure(resource.OperationErrorCodeInternalFailure, fmt.Sprintf("getting next vmid: %v", err)), nil
		}
		vmid = nextID
	}

	params := map[string]string{
		"vmid":       strconv.Itoa(vmid),
		"hostname":   props.Hostname,
		"ostemplate": props.OSTemplate,
		"memory":     strconv.Itoa(props.Memory),
		"swap":       strconv.Itoa(props.Swap),
		"cores":      strconv.Itoa(props.Cores),
	}

	if props.Description != "" {
		params["description"] = props.Description
	}
	if props.Unprivileged != nil && *props.Unprivileged {
		params["unprivileged"] = "1"
	}
	if props.Onboot != nil && *props.Onboot {
		params["onboot"] = "1"
	}
	if props.Password != "" {
		params["password"] = props.Password
	}

	// Build rootfs spec
	if props.Rootfs != nil {
		params["rootfs"] = fmt.Sprintf("%s:%d", props.Rootfs.Storage, props.Rootfs.Size)
	}

	// Build network spec
	if props.Network != nil {
		params["net0"] = buildContainerNetSpec(props.Network)
	}

	nativeID := compositeID(node, vmid)

	data, err := client.Post(ctx, fmt.Sprintf("/nodes/%s/lxc", node), params)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			return createFailure(resource.OperationErrorCodeAlreadyExists, err.Error()), nil
		}
		return createFailure(resource.OperationErrorCodeInternalFailure, err.Error()), nil
	}

	var upid string
	if err := json.Unmarshal(data, &upid); err != nil {
		return createFailure(resource.OperationErrorCodeInternalFailure, fmt.Sprintf("parsing UPID: %v", err)), nil
	}

	return &resource.CreateResult{
		ProgressResult: &resource.ProgressResult{
			Operation:       resource.OperationCreate,
			OperationStatus: resource.OperationStatusInProgress,
			RequestID:       upid,
			NativeID:        nativeID,
		},
	}, nil
}

// --- Read ---

func (p *Plugin) readContainer(ctx context.Context, client *Client, req *resource.ReadRequest) (*resource.ReadResult, error) {
	node, vmid, err := parseCompositeID(req.NativeID)
	if err != nil {
		return &resource.ReadResult{ResourceType: req.ResourceType, ErrorCode: resource.OperationErrorCodeNotFound}, nil
	}

	configData, err := client.Get(ctx, fmt.Sprintf("/nodes/%s/lxc/%d/config", node, vmid))
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "500") {
			return &resource.ReadResult{ResourceType: req.ResourceType, ErrorCode: resource.OperationErrorCodeNotFound}, nil
		}
		return &resource.ReadResult{ResourceType: req.ResourceType, ErrorCode: resource.OperationErrorCodeNetworkFailure}, nil
	}

	statusData, err := client.Get(ctx, fmt.Sprintf("/nodes/%s/lxc/%d/status/current", node, vmid))
	if err != nil {
		return &resource.ReadResult{ResourceType: req.ResourceType, ErrorCode: resource.OperationErrorCodeNetworkFailure}, nil
	}

	props, err := parseContainerConfig(node, vmid, configData, statusData)
	if err != nil {
		return &resource.ReadResult{ResourceType: req.ResourceType, ErrorCode: resource.OperationErrorCodeInternalFailure}, nil
	}

	propsJSON, err := json.Marshal(props)
	if err != nil {
		return &resource.ReadResult{ResourceType: req.ResourceType, ErrorCode: resource.OperationErrorCodeInternalFailure}, nil
	}

	return &resource.ReadResult{
		ResourceType: req.ResourceType,
		Properties:   string(propsJSON),
	}, nil
}

// --- Update ---

func (p *Plugin) updateContainer(ctx context.Context, client *Client, req *resource.UpdateRequest) (*resource.UpdateResult, error) {
	node, vmid, err := parseCompositeID(req.NativeID)
	if err != nil {
		return updateFailure(resource.OperationErrorCodeNotFound, err.Error()), nil
	}

	var desired ContainerProperties
	if err := json.Unmarshal(req.DesiredProperties, &desired); err != nil {
		return updateFailure(resource.OperationErrorCodeInvalidRequest, fmt.Sprintf("invalid desired properties: %v", err)), nil
	}

	params := map[string]string{}

	if desired.Hostname != "" {
		params["hostname"] = desired.Hostname
	}
	params["description"] = desired.Description
	params["memory"] = strconv.Itoa(desired.Memory)
	params["swap"] = strconv.Itoa(desired.Swap)
	params["cores"] = strconv.Itoa(desired.Cores)
	if desired.Onboot != nil {
		if *desired.Onboot {
			params["onboot"] = "1"
		} else {
			params["onboot"] = "0"
		}
	}

	_, err = client.Put(ctx, fmt.Sprintf("/nodes/%s/lxc/%d/config", node, vmid), params)
	if err != nil {
		return updateFailure(resource.OperationErrorCodeInternalFailure, err.Error()), nil
	}

	readResult, _ := p.readContainer(ctx, client, &resource.ReadRequest{
		NativeID:     req.NativeID,
		ResourceType: req.ResourceType,
		TargetConfig: req.TargetConfig,
	})

	var resourceProps json.RawMessage
	if readResult != nil && readResult.Properties != "" {
		resourceProps = json.RawMessage(readResult.Properties)
	}

	return &resource.UpdateResult{
		ProgressResult: &resource.ProgressResult{
			Operation:          resource.OperationUpdate,
			OperationStatus:    resource.OperationStatusSuccess,
			NativeID:           req.NativeID,
			ResourceProperties: resourceProps,
		},
	}, nil
}

// --- Delete ---

func (p *Plugin) deleteContainer(ctx context.Context, client *Client, req *resource.DeleteRequest) (*resource.DeleteResult, error) {
	node, vmid, err := parseCompositeID(req.NativeID)
	if err != nil {
		return &resource.DeleteResult{
			ProgressResult: &resource.ProgressResult{
				Operation:       resource.OperationDelete,
				OperationStatus: resource.OperationStatusSuccess,
				NativeID:        req.NativeID,
			},
		}, nil
	}

	// Stop container first (best effort)
	_, _ = client.Post(ctx, fmt.Sprintf("/nodes/%s/lxc/%d/status/stop", node, vmid), nil)

	data, err := client.Delete(ctx, fmt.Sprintf("/nodes/%s/lxc/%d", node, vmid), map[string]string{
		"purge": "1",
		"force": "1",
	})
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "not exist") {
			return &resource.DeleteResult{
				ProgressResult: &resource.ProgressResult{
					Operation:       resource.OperationDelete,
					OperationStatus: resource.OperationStatusSuccess,
					NativeID:        req.NativeID,
				},
			}, nil
		}
		return deleteFailure(resource.OperationErrorCodeInternalFailure, err.Error()), nil
	}

	var upid string
	if err := json.Unmarshal(data, &upid); err != nil {
		return deleteFailure(resource.OperationErrorCodeInternalFailure, fmt.Sprintf("parsing UPID: %v", err)), nil
	}

	return &resource.DeleteResult{
		ProgressResult: &resource.ProgressResult{
			Operation:       resource.OperationDelete,
			OperationStatus: resource.OperationStatusInProgress,
			RequestID:       upid,
			NativeID:        req.NativeID,
		},
	}, nil
}

// --- Helpers ---

func buildContainerNetSpec(n *ContainerNetProperties) string {
	spec := "name=" + n.Name + ",bridge=" + n.Bridge + ",ip=" + n.IP
	if n.Gateway != "" {
		spec += ",gw=" + n.Gateway
	}
	if n.Firewall != nil && *n.Firewall {
		spec += ",firewall=1"
	}
	if n.Tag != nil {
		spec += ",tag=" + strconv.Itoa(*n.Tag)
	}
	return spec
}

// parseContainerConfig converts Proxmox API config + status to ContainerProperties.
func parseContainerConfig(node string, vmid int, configData, statusData json.RawMessage) (*ContainerProperties, error) {
	var config map[string]interface{}
	if err := json.Unmarshal(configData, &config); err != nil {
		return nil, err
	}

	var statusMap map[string]interface{}
	if err := json.Unmarshal(statusData, &statusMap); err != nil {
		return nil, err
	}

	props := &ContainerProperties{
		ID:   compositeID(node, vmid),
		Node: node,
		VMID: vmid,
	}

	if v, ok := config["hostname"].(string); ok {
		props.Hostname = v
	}
	if v, ok := config["description"].(string); ok {
		props.Description = v
	}
	if v, ok := config["memory"].(float64); ok {
		props.Memory = int(v)
	}
	if v, ok := config["swap"].(float64); ok {
		props.Swap = int(v)
	}
	if v, ok := config["cores"].(float64); ok {
		props.Cores = int(v)
	}
	if v, ok := config["unprivileged"].(float64); ok {
		b := v == 1
		props.Unprivileged = &b
	}
	if v, ok := config["onboot"].(float64); ok {
		b := v == 1
		props.Onboot = &b
	}

	// Parse rootfs: "local-lvm:vm-200-disk-0,size=8G"
	if rootfs, ok := config["rootfs"].(string); ok {
		props.Rootfs = parseContainerRootfs(rootfs)
	}

	// Parse net0: "name=eth0,bridge=vmbr0,ip=dhcp,type=veth"
	if net0, ok := config["net0"].(string); ok {
		props.Network = parseContainerNet(net0)
	}

	if v, ok := statusMap["status"].(string); ok {
		props.Status = v
	}

	return props, nil
}

func parseContainerRootfs(spec string) *ContainerRootfsProperties {
	r := &ContainerRootfsProperties{}
	parts := strings.Split(spec, ",")
	if len(parts) == 0 {
		return r
	}
	volParts := strings.SplitN(parts[0], ":", 2)
	if len(volParts) >= 1 {
		r.Storage = volParts[0]
	}
	for _, part := range parts[1:] {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) == 2 && kv[0] == "size" {
			sizeStr := strings.TrimSuffix(kv[1], "G")
			if s, err := strconv.Atoi(sizeStr); err == nil {
				r.Size = s
			}
		}
	}
	return r
}

func parseContainerNet(spec string) *ContainerNetProperties {
	n := &ContainerNetProperties{Name: "eth0", Bridge: "vmbr0", IP: "dhcp"}
	parts := strings.Split(spec, ",")
	for _, part := range parts {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "name":
			n.Name = kv[1]
		case "bridge":
			n.Bridge = kv[1]
		case "ip":
			n.IP = kv[1]
		case "gw":
			n.Gateway = kv[1]
		case "firewall":
			b := kv[1] == "1"
			n.Firewall = &b
		case "tag":
			if t, err := strconv.Atoi(kv[1]); err == nil {
				n.Tag = &t
			}
		}
	}
	return n
}
```

### Success Criteria:

#### Automated Verification:
- [ ] `make build` compiles without errors
- [ ] `make lint` passes

#### Manual Verification:
- [ ] Create a container via formae against a Proxmox instance
- [ ] Read returns correct container config (hostname, memory, cores, rootfs, network)
- [ ] Update mutable fields (hostname, memory, swap, cores) works
- [ ] Delete stops and removes the container
- [ ] List discovers existing containers on a node

**Implementation Note**: Pause for manual confirmation before Phase 5.

---

## Phase 5: Tests & Polish

### Overview
Update test data PKL files, examples, and clean-environment script for the new resource types. Ensure conformance tests can run against a live Proxmox instance.

### Changes Required:

#### 1. Test Data — VM Create
**File**: `testdata/resource.pkl` (rewrite)

```pkl
amends "@formae/forma.pkl"
import "@formae/formae.pkl"
import "@proxmox/proxmox.pkl"

local testRunID = read("env:FORMAE_TEST_RUN_ID")
local stackName = "plugin-sdk-test-stack"

forma {
  new formae.Stack {
    label = stackName
    description = "Plugin SDK conformance test stack"
  }

  new formae.Target {
    label = "proxmox-target"
    namespace = "PROXMOX"
    config = new proxmox.Config {
      url = read?("env:PROXMOX_URL") ?? "https://localhost:8006"
      insecure = true
    }
  }

  new proxmox.VirtualMachine {
    label = "plugin-sdk-test-vm"
    node = read?("env:PROXMOX_NODE") ?? "pve"
    name = "formae-plugin-sdk-test-\(testRunID)"
    description = "Test VM for plugin SDK conformance tests"
    memory = 512
    cores = 1
    sockets = 1
    ostype = "l26"
    scsihw = "virtio-scsi-pci"
    disk = new proxmox.VirtualMachineDisk {
      storage = read?("env:PROXMOX_STORAGE") ?? "local-lvm"
      size = 4
    }
    network = new proxmox.VirtualMachineNetwork {
      model = "virtio"
      bridge = "vmbr0"
    }
  }
}
```

#### 2. Test Data — VM Update
**File**: `testdata/resource-update.pkl` (rewrite)

```pkl
amends "@formae/forma.pkl"
import "@formae/formae.pkl"
import "@proxmox/proxmox.pkl"

local testRunID = read("env:FORMAE_TEST_RUN_ID")
local stackName = "plugin-sdk-test-stack"

forma {
  new formae.Stack {
    label = stackName
    description = "Plugin SDK conformance test stack"
  }

  new formae.Target {
    label = "proxmox-target"
    namespace = "PROXMOX"
    config = new proxmox.Config {
      url = read?("env:PROXMOX_URL") ?? "https://localhost:8006"
      insecure = true
    }
  }

  new proxmox.VirtualMachine {
    label = "plugin-sdk-test-vm"
    node = read?("env:PROXMOX_NODE") ?? "pve"
    name = "formae-plugin-sdk-test-\(testRunID)-updated"  // CHANGED
    description = "Test VM - UPDATED"                      // CHANGED
    memory = 1024                                          // CHANGED from 512
    cores = 2                                              // CHANGED from 1
    sockets = 1
    ostype = "l26"                                         // unchanged (createOnly)
    scsihw = "virtio-scsi-pci"                             // unchanged (createOnly)
    disk = new proxmox.VirtualMachineDisk {                // unchanged (createOnly)
      storage = read?("env:PROXMOX_STORAGE") ?? "local-lvm"
      size = 4
    }
    network = new proxmox.VirtualMachineNetwork {          // unchanged (createOnly)
      model = "virtio"
      bridge = "vmbr0"
    }
  }
}
```

#### 3. Test Data — VM Replace
**File**: `testdata/resource-replace.pkl` (rewrite)

```pkl
amends "@formae/forma.pkl"
import "@formae/formae.pkl"
import "@proxmox/proxmox.pkl"

local testRunID = read("env:FORMAE_TEST_RUN_ID")
local stackName = "plugin-sdk-test-stack"

forma {
  new formae.Stack {
    label = stackName
    description = "Plugin SDK conformance test stack"
  }

  new formae.Target {
    label = "proxmox-target"
    namespace = "PROXMOX"
    config = new proxmox.Config {
      url = read?("env:PROXMOX_URL") ?? "https://localhost:8006"
      insecure = true
    }
  }

  new proxmox.VirtualMachine {
    label = "plugin-sdk-test-vm"
    node = read?("env:PROXMOX_NODE") ?? "pve"
    name = "formae-plugin-sdk-test-\(testRunID)"
    description = "Test VM for plugin SDK conformance tests"
    memory = 512
    cores = 1
    sockets = 1
    ostype = "other"                                       // CHANGED - triggers replacement
    scsihw = "virtio-scsi-pci"
    disk = new proxmox.VirtualMachineDisk {
      storage = read?("env:PROXMOX_STORAGE") ?? "local-lvm"
      size = 4
    }
    network = new proxmox.VirtualMachineNetwork {
      model = "virtio"
      bridge = "vmbr0"
    }
  }
}
```

#### 4. Test Data PklProject
**File**: `testdata/PklProject` (update if needed — should already reference `@proxmox`)

No changes needed — already imports `../schema/pkl/PklProject` as `proxmox`.

#### 5. Example
**File**: `examples/basic/main.pkl` (rewrite)

```pkl
amends "@formae/forma.pkl"
import "@formae/formae.pkl"
import "@proxmox/proxmox.pkl"

forma {
  new formae.Stack {
    label = "default"
    description = "Proxmox VE resources"
  }

  new formae.Target {
    label = "my-proxmox"
    namespace = "PROXMOX"
    config = new proxmox.Config {
      url = "https://proxmox.example.com:8006"
      insecure = true
    }
  }

  new proxmox.VirtualMachine {
    label = "web-server"
    node = "pve"
    name = "web-server-01"
    description = "Production web server"
    memory = 4096
    cores = 2
    sockets = 1
    ostype = "l26"
    scsihw = "virtio-scsi-pci"
    onboot = true
    disk = new proxmox.VirtualMachineDisk {
      storage = "local-lvm"
      size = 32
      discard = true
    }
    network = new proxmox.VirtualMachineNetwork {
      model = "virtio"
      bridge = "vmbr0"
      firewall = true
    }
  }

  new proxmox.Container {
    label = "dns-server"
    node = "pve"
    hostname = "dns-01"
    description = "Internal DNS server"
    ostemplate = "local:vztmpl/debian-12-standard_12.0-1_amd64.tar.zst"
    memory = 256
    swap = 256
    cores = 1
    unprivileged = true
    rootfs = new proxmox.ContainerRootfs {
      storage = "local-lvm"
      size = 4
    }
    network = new proxmox.ContainerNetwork {
      name = "eth0"
      bridge = "vmbr0"
      ip = "192.168.1.10/24"
      gw = "192.168.1.1"
    }
  }
}
```

#### 6. Clean Environment Script
**File**: `scripts/ci/clean-environment.sh` (update)

```bash
#!/bin/bash
set -euo pipefail

TEST_PREFIX="${TEST_PREFIX:-formae-plugin-sdk-test-}"
PROXMOX_URL="${PROXMOX_URL:-https://localhost:8006}"
PROXMOX_NODE="${PROXMOX_NODE:-pve}"
PROXMOX_API_TOKEN="${PROXMOX_API_TOKEN:-}"

echo "clean-environment.sh: Cleaning test VMs/containers with prefix '${TEST_PREFIX}'"

if [ -z "$PROXMOX_API_TOKEN" ]; then
    echo "PROXMOX_API_TOKEN not set, skipping cleanup"
    exit 0
fi

AUTH="Authorization: PVEAPIToken=${PROXMOX_API_TOKEN}"
API="${PROXMOX_URL}/api2/json"

# Clean VMs
echo "Listing VMs on node ${PROXMOX_NODE}..."
VMS=$(curl -sk -H "$AUTH" "${API}/nodes/${PROXMOX_NODE}/qemu" 2>/dev/null | python3 -c "
import sys, json
try:
    data = json.load(sys.stdin).get('data', [])
    for vm in data:
        if vm.get('name', '').startswith('${TEST_PREFIX}'):
            print(vm['vmid'])
except: pass
" 2>/dev/null || true)

for VMID in $VMS; do
    echo "  Stopping and deleting VM ${VMID}..."
    curl -sk -X POST -H "$AUTH" "${API}/nodes/${PROXMOX_NODE}/qemu/${VMID}/status/stop" 2>/dev/null || true
    sleep 2
    curl -sk -X DELETE -H "$AUTH" "${API}/nodes/${PROXMOX_NODE}/qemu/${VMID}?purge=1&destroy-unreferenced-disks=1" 2>/dev/null || true
done

# Clean containers
echo "Listing containers on node ${PROXMOX_NODE}..."
CTS=$(curl -sk -H "$AUTH" "${API}/nodes/${PROXMOX_NODE}/lxc" 2>/dev/null | python3 -c "
import sys, json
try:
    data = json.load(sys.stdin).get('data', [])
    for ct in data:
        if ct.get('name', '').startswith('${TEST_PREFIX}'):
            print(ct['vmid'])
except: pass
" 2>/dev/null || true)

for VMID in $CTS; do
    echo "  Stopping and deleting container ${VMID}..."
    curl -sk -X POST -H "$AUTH" "${API}/nodes/${PROXMOX_NODE}/lxc/${VMID}/status/stop" 2>/dev/null || true
    sleep 2
    curl -sk -X DELETE -H "$AUTH" "${API}/nodes/${PROXMOX_NODE}/lxc/${VMID}?purge=1&force=1" 2>/dev/null || true
done

echo "clean-environment.sh: Cleanup complete"
```

### Success Criteria:

#### Automated Verification:
- [ ] `make build` compiles without errors
- [ ] `make lint` passes
- [ ] `make verify-schema` validates Pkl schema
- [ ] `make install` installs correctly

#### Manual Verification:
- [ ] `make conformance-test` passes against a live Proxmox instance with:
  - `PROXMOX_API_TOKEN=user@realm!token=uuid`
  - `PROXMOX_URL=https://proxmox-host:8006`
  - `PROXMOX_NODE=pve`
  - `PROXMOX_STORAGE=local-lvm`
- [ ] Clean-environment script successfully removes test VMs/containers
- [ ] Example PKL renders valid JSON via `pkl eval examples/basic/main.pkl`

---

## Testing Strategy

### Conformance Tests (existing framework):
- `TestPluginConformance` — full Create → Read → Update → Read → Replace → Read → Delete lifecycle
- `TestPluginDiscovery` — List → Read flow for all resource types

### Manual Testing Steps:
1. Install plugin: `make install`
2. Set env vars: `PROXMOX_API_TOKEN`, `PROXMOX_URL`, `PROXMOX_NODE`, `PROXMOX_STORAGE`
3. Apply example: `formae apply --mode reconcile examples/basic/main.pkl`
4. Verify VM created on Proxmox web UI
5. Modify memory in example PKL, re-apply — verify in-place update
6. Remove VM from PKL, re-apply — verify deletion
7. Test discovery: `formae discover --namespace PROXMOX`

### Environment Variables Required:
| Variable | Description | Example |
|----------|-------------|---------|
| `PROXMOX_API_TOKEN` | API token (secret) | `root@pam!formae=uuid-here` |
| `PROXMOX_URL` | Proxmox API base URL | `https://192.168.1.100:8006` |
| `PROXMOX_NODE` | Default node for tests | `pve` |
| `PROXMOX_STORAGE` | Storage for test disks | `local-lvm` |

## Performance Considerations

- Rate limit set to 10 req/sec — Proxmox default limit is generous but we're conservative
- Client is cached per plugin lifetime (lazy init, thread-safe)
- Async task polling offloaded to formae agent (plugin just checks once per Status call)
- TLS skip option for self-signed certs (common in Proxmox deployments)

## References

- [Proxmox VE API Wiki](https://pve.proxmox.com/wiki/Proxmox_VE_API)
- [Proxmox VE API Viewer](https://pve.proxmox.com/pve-docs/api-viewer/)
- [qm.conf(5) — QEMU config reference](https://pve.proxmox.com/pve-docs/qm.conf.5.html)
- [pct.conf(5) — LXC config reference](https://pve.proxmox.com/pve-docs/pct.conf.5.html)
- [Formae Plugin Docs](https://docs.formae.io/en/latest/core-concepts/plugin/)
- [Formae Plugin SDK Reference](https://docs.formae.io/en/latest/plugin-sdk/reference/plugin-interface/)
- [Formae Parent-Child Resources](https://docs.formae.io/en/latest/plugin-sdk/advanced/parent-child-resources/)
- [formae-plugin-sftp (reference implementation)](https://github.com/platform-engineering-labs/formae-plugin-sftp)
