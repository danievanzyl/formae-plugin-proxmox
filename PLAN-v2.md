# Proxmox Plugin v2: Cloud Images, VM Templates, Cloud-Init & Clone

## Overview

Extend the formae-proxmox plugin with cloud image management, QEMU VM templates, cloud-init configuration, and clone-from-template support. This enables a full declarative workflow: download cloud image → create template with cloud-init defaults → clone VMs from template with overrides.

## Current State Analysis

**What exists:**
- Node/Storage: read-only, discoverable
- LXC Container Template: managed (download from aplinfo index)
- VirtualMachine: basic CRUD (memory, cores, sockets, disk, network)
- Container: full CRUD
- HTTP client with 30s timeout, form-encoded POST/PUT/DELETE
- Async task polling via UPID

**What's missing:**
- Zero cloud-init support
- No QEMU VM templates (only LXC templates)
- No clone-from-template
- No cloud image download from vendor URLs
- VM update limited to memory/cores/sockets/name/description/onboot
- VM list doesn't filter out templates
- Client timeout too short for disk imports

### Key Discoveries:
- `proxmoxVMListEntry` (`types.go:185-190`) lacks `Template` field — templates and VMs are co-mingled in list results
- `buildVMDiskSpec` (`vm.go:273-283`) uses `storage:size` format — needs `import-from` variant for cloud images
- `pollTask` (`status.go:12-67`) assumes single-UPID operations — needs multi-step support for VMTemplate create and clone
- Client timeout is 30s (`client.go:40`) — `import-from` disk operations can take minutes
- `parseVMConfig` (`vm.go:297-359`) doesn't read cloud-init fields (ciuser, ipconfig0, etc.)
- Proxmox `download-url` API (PVE 8.2+) uses `content=import` type, stores in `template/import/` dir

## Desired End State

After implementation:
1. Users can declare `CloudImage` resources that download `.img`/`.qcow2` from vendor URLs
2. Users can declare `VMTemplate` resources that create a VM, import a cloud disk, set cloud-init defaults, and convert to a template — all in one resource declaration
3. Users can declare `VirtualMachine` with `cloneFrom` referencing a `VMTemplate`, with cloud-init overrides and custom sizing
4. Users can update cloud-init, disk size, and network on existing VMs

### Verification:
- `go build && go vet` pass
- All new resource types route correctly in `proxmox.go`
- Pkl schema validates: `make verify-schema`
- Example config demonstrates full workflow
- Conformance tests pass against live Proxmox

## What We're NOT Doing

- Custom cloud-init snippets via `cicustom` (requires filesystem access, not API-friendly)
- Multiple disks/NICs (only scsi0 + net0)
- Cross-node cloning
- ISO management (separate from cloud images)
- VM start/stop/reboot lifecycle management
- Snapshot management
- Storage creation/configuration

## New Resource Types Summary

| Resource | Type String | NativeID Format | Parent |
|---|---|---|---|
| CloudImage | `PROXMOX::Image::CloudImage` | `node/storage:import/filename` | Node |
| VMTemplate | `PROXMOX::Compute::VMTemplate` | `node/vmid` | Node |
| CloudInit | (sub-resource) | — | — |

Enhanced: `PROXMOX::Compute::VirtualMachine` — adds `cloneFrom`, `fullClone`, `cloudInit`, `agent` fields.

---

## Phase 1: Infrastructure Updates

### Overview
Increase client timeout, add template flag filtering to VM list, and build the multi-step status handler framework needed by later phases.

### Changes Required:

#### 1. Increase HTTP client timeout
**File**: `client.go`
**Changes**: Increase default timeout from 30s to 300s to accommodate disk imports during VMTemplate creation.

```go
// client.go:38-40 — change timeout
httpClient: &http.Client{
    Transport: transport,
    Timeout:   300 * time.Second,
},
```

#### 2. Add template flag to VM list entry
**File**: `types.go`
**Changes**: Add `Template` field to `proxmoxVMListEntry`.

```go
// types.go — update proxmoxVMListEntry
type proxmoxVMListEntry struct {
    VMID     int    `json:"vmid"`
    Name     string `json:"name"`
    Status   string `json:"status"`
    Template int    `json:"template"` // 0=VM, 1=template
}
```

#### 3. Filter templates from VM list
**File**: `vm.go`
**Changes**: Skip entries where `Template == 1` in `listVMs`.

```go
// vm.go:listVMs — add filter in the loop
for _, vm := range vms {
    if vm.Template == 1 {
        continue // skip templates, they have their own resource type
    }
    ids = append(ids, compositeID(node, vm.VMID))
}
```

#### 4. Multi-step status handler framework
**File**: `status.go`
**Changes**: Modify `pollTask` to detect multi-step requestID prefixes and dispatch to specialized handlers. Plain UPIDs continue to work as before.

```go
func (p *Plugin) pollTask(ctx context.Context, client *Client, req *resource.StatusRequest) (*resource.StatusResult, error) {
    // Multi-step dispatch based on requestID prefix
    if strings.HasPrefix(req.RequestID, "vmtpl:") {
        return p.pollVMTemplateTask(ctx, client, req)
    }
    if strings.HasPrefix(req.RequestID, "clone:") {
        return p.pollCloneTask(ctx, client, req)
    }

    // Original single-UPID polling (unchanged)
    taskStatus, err := client.GetTaskStatus(ctx, req.RequestID)
    // ... rest unchanged ...
}
```

The `pollVMTemplateTask` and `pollCloneTask` functions will be implemented in Phase 4 and Phase 5 respectively.

#### 5. Add new resource type constants
**File**: `proxmox.go`
**Changes**: Add constants for new resource types.

```go
const (
    ResourceTypeNode       = "PROXMOX::Node::Node"
    ResourceTypeStorage    = "PROXMOX::Storage::Storage"
    ResourceTypeTemplate   = "PROXMOX::Container::Template"
    ResourceTypeVM         = "PROXMOX::Compute::VirtualMachine"
    ResourceTypeContainer  = "PROXMOX::Compute::Container"
    ResourceTypeCloudImage = "PROXMOX::Image::CloudImage"     // NEW
    ResourceTypeVMTemplate = "PROXMOX::Compute::VMTemplate"   // NEW
)
```

Add routing stubs in each CRUD method (Create/Read/Update/Delete/List) that return `OperationErrorCodeInvalidRequest` with "not yet implemented". These will be filled in by later phases.

### Success Criteria:

#### Automated Verification:
- [x] `go build ./...` passes
- [x] `go vet ./...` passes
- [x] Existing VM list behavior unchanged for non-template VMs
- [x] New resource type constants compile

#### Manual Verification:
- [ ] VM list no longer returns template VMs (if any exist on test Proxmox)

**Implementation Note**: After completing this phase and all automated verification passes, pause here for manual confirmation before proceeding to Phase 2.

---

## Phase 2: CloudImage Resource

### Overview
New resource for downloading cloud images (Ubuntu, Debian, etc.) from vendor URLs to Proxmox storage via the `download-url` API (PVE 8.2+).

### Changes Required:

#### 1. Pkl schema — CloudImage class
**File**: `schema/pkl/proxmox.pkl`
**Changes**: Add CloudImage resource class after the Storage section.

```pkl
// =============================================================================
// Cloud Image (child of Node, managed)
// =============================================================================

/// A cloud disk image downloaded from a vendor URL to Proxmox storage.
/// Used as the base disk for VMTemplate resources.
@formae.ResourceHint {
    type = "PROXMOX::Image::CloudImage"
    identifier = "$.id"
    discoverable = true
    parent = "PROXMOX::Node::Node"
    listParam = new formae.ListProperty {
        parentProperty = "node"
        listParameter = "node"
    }
}
class CloudImage extends formae.Resource {
    fixed hidden type: String = "PROXMOX::Image::CloudImage"

    /// Target node name
    @formae.FieldHint { createOnly = true }
    node: String|formae.Resolvable

    /// Storage to download to (must have "import" content type enabled)
    @formae.FieldHint { createOnly = true }
    storage: String|formae.Resolvable

    /// Remote URL to download from (e.g. "https://cloud-images.ubuntu.com/...")
    @formae.FieldHint { createOnly = true }
    url: String

    /// Filename to save as on storage (e.g. "ubuntu-24.04-cloudimg-amd64.img")
    @formae.FieldHint { createOnly = true }
    filename: String

    /// Optional SHA256 checksum for verification
    @formae.FieldHint { createOnly = true }
    checksum: String?

    /// Checksum algorithm (sha256, sha512, md5). Defaults to sha256 if checksum is set.
    @formae.FieldHint { createOnly = true }
    checksumAlgorithm: String?

    /// Full volume ID after download (e.g. "local:import/ubuntu-24.04-cloudimg-amd64.img")
    @formae.FieldHint {}
    volid: String?

    /// File size in bytes (populated after download)
    @formae.FieldHint {}
    size: Int?
}
```

#### 2. Go types — CloudImageProperties
**File**: `types.go`
**Changes**: Add CloudImage types and NativeID helpers.

```go
// --- Cloud Image types ---

// CloudImageProperties is the formae-facing properties struct for CloudImage.
type CloudImageProperties struct {
    ID                string      `json:"id"`
    Node              interface{} `json:"node"`
    Storage           interface{} `json:"storage"`
    URL               string      `json:"url"`
    Filename          string      `json:"filename"`
    Checksum          string      `json:"checksum,omitempty"`
    ChecksumAlgorithm string      `json:"checksumAlgorithm,omitempty"`
    Volid             string      `json:"volid,omitempty"`
    Size              int64       `json:"size,omitempty"`
}

// cloudImageNativeID builds a NativeID: "node/storage:import/filename"
func cloudImageNativeID(node, storage, filename string) string {
    return node + "/" + storage + ":import/" + filename
}

// parseCloudImageNativeID splits "node/storage:import/filename" into parts.
func parseCloudImageNativeID(nativeID string) (node, volid, storage string, err error) {
    idx := strings.Index(nativeID, "/")
    if idx < 0 {
        return "", "", "", fmt.Errorf("invalid cloud image ID %q", nativeID)
    }
    node = nativeID[:idx]
    volid = nativeID[idx+1:]
    colonIdx := strings.Index(volid, ":")
    if colonIdx < 0 {
        return "", "", "", fmt.Errorf("invalid volid in cloud image ID %q", nativeID)
    }
    storage = volid[:colonIdx]
    return node, volid, storage, nil
}
```

#### 3. Go handler — cloud_image.go
**File**: `cloud_image.go` (NEW)
**Changes**: Full CRUD implementation.

```go
package main

import (
    "context"
    "encoding/json"
    "fmt"
    "net/url"
    "strings"

    "github.com/platform-engineering-labs/formae/pkg/plugin/resource"
)

// --- List ---

func (p *Plugin) listCloudImages(ctx context.Context, client *Client, req *resource.ListRequest) (*resource.ListResult, error) {
    node, ok := req.AdditionalProperties["node"]
    if !ok || node == "" {
        return &resource.ListResult{NativeIDs: []string{}}, nil
    }

    // Get all storages
    storageData, err := client.Get(ctx, "/storage")
    if err != nil {
        return &resource.ListResult{NativeIDs: []string{}}, nil
    }

    var storages []proxmoxStorageListEntry
    if err := json.Unmarshal(storageData, &storages); err != nil {
        return &resource.ListResult{NativeIDs: []string{}}, nil
    }

    var ids []string
    for _, s := range storages {
        // Try to list import content — not all storages support it
        contentData, err := client.Get(ctx, fmt.Sprintf("/nodes/%s/storage/%s/content?content=import", node, s.Storage))
        if err != nil {
            continue
        }
        var entries []proxmoxStorageContentEntry
        if err := json.Unmarshal(contentData, &entries); err != nil {
            continue
        }
        for _, e := range entries {
            ids = append(ids, templateNativeID(node, e.Volid)) // reuse "node/volid" format
        }
    }

    if ids == nil {
        ids = []string{}
    }
    return &resource.ListResult{NativeIDs: ids}, nil
}

// --- Read ---

func (p *Plugin) readCloudImage(ctx context.Context, client *Client, req *resource.ReadRequest) (*resource.ReadResult, error) {
    node, volid, storage, err := parseCloudImageNativeID(req.NativeID)
    if err != nil {
        return &resource.ReadResult{ResourceType: req.ResourceType, ErrorCode: resource.OperationErrorCodeNotFound}, nil
    }

    contentData, err := client.Get(ctx, fmt.Sprintf("/nodes/%s/storage/%s/content?content=import", node, storage))
    if err != nil {
        return &resource.ReadResult{ResourceType: req.ResourceType, ErrorCode: resource.OperationErrorCodeNetworkFailure}, nil
    }

    var entries []proxmoxStorageContentEntry
    if err := json.Unmarshal(contentData, &entries); err != nil {
        return &resource.ReadResult{ResourceType: req.ResourceType, ErrorCode: resource.OperationErrorCodeInternalFailure}, nil
    }

    for _, e := range entries {
        if e.Volid == volid {
            // Extract filename from volid: "local:import/filename" → "filename"
            filename := volid
            if idx := strings.Index(volid, "import/"); idx >= 0 {
                filename = volid[idx+len("import/"):]
            }

            props := CloudImageProperties{
                ID:       req.NativeID,
                Node:     node,
                Storage:  storage,
                Filename: filename,
                Volid:    volid,
                Size:     e.Size,
            }
            propsJSON, _ := json.Marshal(props)
            return &resource.ReadResult{
                ResourceType: req.ResourceType,
                Properties:   string(propsJSON),
            }, nil
        }
    }

    return &resource.ReadResult{ResourceType: req.ResourceType, ErrorCode: resource.OperationErrorCodeNotFound}, nil
}

// --- Create ---

func (p *Plugin) createCloudImage(ctx context.Context, client *Client, req *resource.CreateRequest) (*resource.CreateResult, error) {
    var props CloudImageProperties
    if err := json.Unmarshal(req.Properties, &props); err != nil {
        return createFailure(resource.OperationErrorCodeInvalidRequest, fmt.Sprintf("invalid properties: %v", err)), nil
    }

    node := resolveString(props.Node)
    if node == "" {
        return createFailure(resource.OperationErrorCodeInvalidRequest, "node is required"), nil
    }

    storage := resolveString(props.Storage)
    if storage == "" {
        return createFailure(resource.OperationErrorCodeInvalidRequest, "storage is required"), nil
    }

    if props.URL == "" {
        return createFailure(resource.OperationErrorCodeInvalidRequest, "url is required"), nil
    }
    if props.Filename == "" {
        return createFailure(resource.OperationErrorCodeInvalidRequest, "filename is required"), nil
    }

    params := map[string]string{
        "url":      props.URL,
        "filename": props.Filename,
        "content":  "import",
    }
    if props.Checksum != "" {
        params["checksum"] = props.Checksum
        alg := props.ChecksumAlgorithm
        if alg == "" {
            alg = "sha256"
        }
        params["checksum-algorithm"] = alg
    }

    data, err := client.Post(ctx, fmt.Sprintf("/nodes/%s/storage/%s/download-url", node, storage), params)
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

    nativeID := cloudImageNativeID(node, storage, props.Filename)

    return &resource.CreateResult{
        ProgressResult: &resource.ProgressResult{
            Operation:       resource.OperationCreate,
            OperationStatus: resource.OperationStatusInProgress,
            RequestID:       upid,
            NativeID:        nativeID,
        },
    }, nil
}

// --- Delete ---

func (p *Plugin) deleteCloudImage(ctx context.Context, client *Client, req *resource.DeleteRequest) (*resource.DeleteResult, error) {
    node, volid, storage, err := parseCloudImageNativeID(req.NativeID)
    if err != nil {
        return &resource.DeleteResult{
            ProgressResult: &resource.ProgressResult{
                Operation:       resource.OperationDelete,
                OperationStatus: resource.OperationStatusSuccess,
                NativeID:        req.NativeID,
            },
        }, nil
    }

    // Extract volume path: "local:import/filename" → "import/filename"
    colonIdx := strings.Index(volid, ":")
    volumePath := volid[colonIdx+1:]

    data, err := client.Delete(ctx, fmt.Sprintf("/nodes/%s/storage/%s/content/%s", node, storage, url.PathEscape(volumePath)), nil)
    if err != nil {
        if strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "no such") {
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
        // Synchronous delete
        return &resource.DeleteResult{
            ProgressResult: &resource.ProgressResult{
                Operation:       resource.OperationDelete,
                OperationStatus: resource.OperationStatusSuccess,
                NativeID:        req.NativeID,
            },
        }, nil
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
```

#### 4. Wire into proxmox.go
**File**: `proxmox.go`
**Changes**: Replace CloudImage stubs with real calls in Create/Read/Update/Delete/List switch statements.

- Create: `return p.createCloudImage(ctx, client, req)`
- Read: `return p.readCloudImage(ctx, client, req)`
- Update: `return updateFailure(resource.OperationErrorCodeInvalidRequest, "cloud images are immutable")`
- Delete: `return p.deleteCloudImage(ctx, client, req)`
- List: `return p.listCloudImages(ctx, client, req)`
- LabelConfig: add `ResourceTypeCloudImage: "$.volid"`

### Success Criteria:

#### Automated Verification:
- [x] `go build ./...` passes
- [x] `go vet ./...` passes
- [x] `make verify-schema` passes (Pkl schema valid)

#### Manual Verification:
- [ ] Download Ubuntu cloud image: declare CloudImage with `url = "https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img"`, verify it appears in Proxmox storage
- [ ] List shows downloaded images
- [ ] Delete removes image from storage

**Implementation Note**: Pause for manual verification before Phase 3.

---

## Phase 3: CloudInit Sub-Resource + VM Enhancement

### Overview
Add CloudInit as a Pkl sub-resource and enhance VirtualMachine to support cloud-init configuration, agent flag, and disk resize on update.

### Changes Required:

#### 1. Pkl schema — CloudInit sub-resource
**File**: `schema/pkl/proxmox.pkl`
**Changes**: Add CloudInit sub-resource before the VirtualMachine section.

```pkl
// =============================================================================
// Cloud-Init Configuration (sub-resource)
// =============================================================================

/// Cloud-init configuration for QEMU virtual machines.
@formae.SubResourceHint {}
class CloudInit extends formae.SubResource {
    /// Default user name
    @formae.FieldHint {}
    ciuser: String?

    /// Default user password (write-only, not returned by Read)
    @formae.FieldHint { writeOnly = true }
    cipassword: String?

    /// SSH public keys (one per line, OpenSSH format)
    @formae.FieldHint { writeOnly = true }
    sshkeys: String?

    /// IP configuration for first interface (e.g. "ip=dhcp" or "ip=x.x.x.x/24,gw=y.y.y.y")
    @formae.FieldHint {}
    ipconfig0: String?

    /// DNS server(s), space-separated
    @formae.FieldHint {}
    nameserver: String?

    /// DNS search domain(s), space-separated
    @formae.FieldHint {}
    searchdomain: String?

    /// Cloud-init type (nocloud, configdrive2)
    @formae.FieldHint {}
    citype: String?

    /// Auto-upgrade packages on first boot
    @formae.FieldHint {}
    ciupgrade: Boolean?
}
```

#### 2. Pkl schema — Enhance VirtualMachine
**File**: `schema/pkl/proxmox.pkl`
**Changes**: Add `cloudInit`, `agent`, `cloneFrom`, `fullClone` fields to VirtualMachine class.

```pkl
class VirtualMachine extends formae.Resource {
    // ... all existing fields unchanged ...

    /// Enable QEMU guest agent
    @formae.FieldHint {}
    agent: Boolean?

    /// Cloud-init configuration
    @formae.FieldHint {}
    cloudInit: CloudInit?

    /// Clone from a VMTemplate (resolves to "node/vmid" NativeID)
    @formae.FieldHint { createOnly = true }
    cloneFrom: String|formae.Resolvable?

    /// Use full clone (true) or linked clone (false). Default: true.
    @formae.FieldHint { createOnly = true }
    fullClone: Boolean = true
}
```

Note: `disk` and `network` FieldHints change from `createOnly = true` to just `{}` to support updates.

```pkl
    /// Primary disk configuration
    @formae.FieldHint {}
    disk: VirtualMachineDisk?   // optional when cloning (inherits from template)

    /// Primary network interface
    @formae.FieldHint {}
    network: VirtualMachineNetwork?  // optional when cloning
```

#### 3. Go types — CloudInitProperties
**File**: `types.go`
**Changes**: Add CloudInit struct and update VMProperties.

```go
// CloudInitProperties maps to CloudInit sub-resource.
type CloudInitProperties struct {
    CIUser       string `json:"ciuser,omitempty"`
    CIPassword   string `json:"cipassword,omitempty"`
    SSHKeys      string `json:"sshkeys,omitempty"`
    IPConfig0    string `json:"ipconfig0,omitempty"`
    Nameserver   string `json:"nameserver,omitempty"`
    Searchdomain string `json:"searchdomain,omitempty"`
    CIType       string `json:"citype,omitempty"`
    CIUpgrade    *bool  `json:"ciupgrade,omitempty"`
}
```

Update `VMProperties`:
```go
type VMProperties struct {
    // ... existing fields ...
    Agent     *bool                `json:"agent,omitempty"`
    CloudInit *CloudInitProperties `json:"cloudInit,omitempty"`
    CloneFrom interface{}          `json:"cloneFrom,omitempty"`
    FullClone *bool                `json:"fullClone,omitempty"`
}
```

#### 4. Enhance createVM — cloud-init params
**File**: `vm.go`
**Changes**: In `createVM`, after building base params, add cloud-init and agent params.

```go
// In createVM, after network params:

if props.Agent != nil && *props.Agent {
    params["agent"] = "1"
}

// Cloud-init: attach cloudinit drive + set params
if props.CloudInit != nil {
    ci := props.CloudInit
    // Determine cloud-init storage (use disk storage or default "local-lvm")
    ciStorage := "local-lvm"
    if props.Disk != nil {
        ciStorage = resolveString(props.Disk.Storage)
    }
    params["ide2"] = ciStorage + ":cloudinit"
    params["boot"] = "order=scsi0"

    if ci.CIUser != "" {
        params["ciuser"] = ci.CIUser
    }
    if ci.CIPassword != "" {
        params["cipassword"] = ci.CIPassword
    }
    if ci.SSHKeys != "" {
        params["sshkeys"] = ci.SSHKeys
    }
    if ci.IPConfig0 != "" {
        params["ipconfig0"] = ci.IPConfig0
    }
    if ci.Nameserver != "" {
        params["nameserver"] = ci.Nameserver
    }
    if ci.Searchdomain != "" {
        params["searchdomain"] = ci.Searchdomain
    }
    if ci.CIType != "" {
        params["citype"] = ci.CIType
    }
    if ci.CIUpgrade != nil {
        if *ci.CIUpgrade {
            params["ciupgrade"] = "1"
        } else {
            params["ciupgrade"] = "0"
        }
    }
}
```

Note: `sshkeys` must be URL-encoded when sent via form-urlencoded. The client already handles this via `url.Values.Set()` which encodes values.

#### 5. Enhance readVM/parseVMConfig — read cloud-init fields
**File**: `vm.go`
**Changes**: In `parseVMConfig`, extract cloud-init fields from config.

```go
// In parseVMConfig, after existing field extraction:

// Agent
if v, ok := config["agent"].(string); ok {
    b := strings.HasPrefix(v, "1")
    props.Agent = &b
} else if v, ok := config["agent"].(float64); ok {
    b := v == 1
    props.Agent = &b
}

// Cloud-init fields
ci := &CloudInitProperties{}
hasCI := false
if v, ok := config["ciuser"].(string); ok && v != "" {
    ci.CIUser = v
    hasCI = true
}
if v, ok := config["ipconfig0"].(string); ok && v != "" {
    ci.IPConfig0 = v
    hasCI = true
}
if v, ok := config["nameserver"].(string); ok && v != "" {
    ci.Nameserver = v
    hasCI = true
}
if v, ok := config["searchdomain"].(string); ok && v != "" {
    ci.Searchdomain = v
    hasCI = true
}
if v, ok := config["citype"].(string); ok && v != "" {
    ci.CIType = v
    hasCI = true
}
if v, ok := config["ciupgrade"].(float64); ok {
    b := v == 1
    ci.CIUpgrade = &b
    hasCI = true
}
if hasCI {
    props.CloudInit = ci
}
// Note: cipassword and sshkeys are write-only, never returned by Proxmox API
```

#### 6. Enhance updateVM — cloud-init + disk resize
**File**: `vm.go`
**Changes**: Expand `updateVM` to support cloud-init param updates, agent, and disk resize.

```go
// In updateVM, after existing params:

if desired.Agent != nil {
    if *desired.Agent {
        params["agent"] = "1"
    } else {
        params["agent"] = "0"
    }
}

// Cloud-init updates
if desired.CloudInit != nil {
    ci := desired.CloudInit
    if ci.CIUser != "" {
        params["ciuser"] = ci.CIUser
    }
    if ci.CIPassword != "" {
        params["cipassword"] = ci.CIPassword
    }
    if ci.SSHKeys != "" {
        params["sshkeys"] = ci.SSHKeys
    }
    if ci.IPConfig0 != "" {
        params["ipconfig0"] = ci.IPConfig0
    }
    if ci.Nameserver != "" {
        params["nameserver"] = ci.Nameserver
    }
    if ci.Searchdomain != "" {
        params["searchdomain"] = ci.Searchdomain
    }
    if ci.CIType != "" {
        params["citype"] = ci.CIType
    }
    if ci.CIUpgrade != nil {
        if *ci.CIUpgrade {
            params["ciupgrade"] = "1"
        } else {
            params["ciupgrade"] = "0"
        }
    }
}

// After PUT config, handle disk resize if desired size > current size
if desired.Disk != nil && desired.Disk.Size > 0 {
    // Read current config to compare disk size
    readResult, _ := p.readVM(ctx, client, &resource.ReadRequest{
        NativeID:     req.NativeID,
        ResourceType: req.ResourceType,
        TargetConfig: req.TargetConfig,
    })
    if readResult != nil && readResult.Properties != "" {
        var currentProps VMProperties
        json.Unmarshal([]byte(readResult.Properties), &currentProps)
        if currentProps.Disk != nil && desired.Disk.Size > currentProps.Disk.Size {
            _, _ = client.Put(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/resize", node, vmid), map[string]string{
                "disk": "scsi0",
                "size": fmt.Sprintf("%dG", desired.Disk.Size),
            })
        }
    }
}
```

### Success Criteria:

#### Automated Verification:
- [x] `go build ./...` passes
- [x] `go vet ./...` passes
- [x] `make verify-schema` passes

#### Manual Verification:
- [ ] Create VM with cloud-init (ciuser, sshkeys, ipconfig0) — verify cloud-init config shows in Proxmox UI
- [ ] Read VM returns cloud-init fields
- [ ] Update VM cloud-init params (change ipconfig0) — verify change in Proxmox
- [ ] Update VM disk size (resize) — verify disk grew

**Implementation Note**: Pause for manual verification before Phase 4.

---

## Phase 4: VMTemplate Resource

### Overview
New resource for QEMU VM templates. Create is a multi-step async operation: create VM shell → import cloud disk + configure cloud-init + attach cloudinit drive → convert to template. Uses encoded requestID to track progress across Status() polls.

### Changes Required:

#### 1. Pkl schema — VMTemplate class
**File**: `schema/pkl/proxmox.pkl`
**Changes**: Add VMTemplate resource and VMTemplateDisk sub-resource.

```pkl
// =============================================================================
// VM Template Disk (sub-resource)
// =============================================================================

/// Disk configuration for a VM template. The disk is imported from a CloudImage.
@formae.SubResourceHint {}
class VMTemplateDisk extends formae.SubResource {
    /// Storage for the imported disk (e.g. "local-lvm")
    @formae.FieldHint {}
    storage: String|formae.Resolvable

    /// Resize disk to this size in GiB after import (optional, must be >= image size)
    @formae.FieldHint {}
    size: Int?

    /// Cache mode
    @formae.FieldHint {}
    cache: String?

    /// Enable TRIM/discard
    @formae.FieldHint {}
    discard: Boolean?
}

// =============================================================================
// VM Template (child of Node, managed)
// =============================================================================

/// A QEMU VM template created from a cloud image with cloud-init defaults.
/// Multi-step creation: create VM → import disk → configure cloud-init → convert to template.
@formae.ResourceHint {
    type = "PROXMOX::Compute::VMTemplate"
    identifier = "$.id"
    discoverable = true
    parent = "PROXMOX::Node::Node"
    listParam = new formae.ListProperty {
        parentProperty = "node"
        listParameter = "node"
    }
}
class VMTemplate extends formae.Resource {
    fixed hidden type: String = "PROXMOX::Compute::VMTemplate"

    /// Target node name
    @formae.FieldHint { createOnly = true }
    node: String|formae.Resolvable

    /// VM ID (100-999999999). Auto-assigned if omitted.
    @formae.FieldHint { createOnly = true }
    vmid: Int?

    /// Template display name
    @formae.FieldHint {}
    name: String

    /// Description / notes
    @formae.FieldHint {}
    description: String?

    /// Cloud image to import as disk (resolves to CloudImage volid)
    @formae.FieldHint { createOnly = true }
    cloudImage: String|formae.Resolvable

    /// Memory in MiB (default for VMs cloned from this template)
    @formae.FieldHint {}
    memory: Int = 2048

    /// CPU cores per socket
    @formae.FieldHint {}
    cores: Int = 1

    /// Number of CPU sockets
    @formae.FieldHint {}
    sockets: Int = 1

    /// Guest OS type
    @formae.FieldHint { createOnly = true }
    ostype: String = "l26"

    /// SCSI controller type
    @formae.FieldHint { createOnly = true }
    scsihw: String = "virtio-scsi-pci"

    /// BIOS type
    @formae.FieldHint { createOnly = true }
    bios: String?

    /// Machine type
    @formae.FieldHint { createOnly = true }
    machine: String?

    /// Enable QEMU guest agent
    @formae.FieldHint {}
    agent: Boolean?

    /// Start on host boot
    @formae.FieldHint {}
    onboot: Boolean?

    /// Disk configuration (imported from cloud image)
    @formae.FieldHint { createOnly = true }
    disk: VMTemplateDisk

    /// Network interface
    @formae.FieldHint { createOnly = true }
    network: VirtualMachineNetwork

    /// Default cloud-init configuration baked into the template
    @formae.FieldHint {}
    cloudInit: CloudInit?

    /// Current status (computed)
    @formae.FieldHint {}
    status: String?
}
```

#### 2. Go types — VMTemplateProperties
**File**: `types.go`
**Changes**: Add VMTemplate types.

```go
// --- VM Template types ---

// VMTemplateProperties is the formae-facing properties struct for VMTemplate.
type VMTemplateProperties struct {
    ID          string               `json:"id"`
    Node        interface{}          `json:"node"`
    VMID        int                  `json:"vmid"`
    Name        string               `json:"name"`
    Description string               `json:"description,omitempty"`
    CloudImage  interface{}          `json:"cloudImage"`
    Memory      int                  `json:"memory"`
    Cores       int                  `json:"cores"`
    Sockets     int                  `json:"sockets"`
    OSType      string               `json:"ostype"`
    ScsiHW      string               `json:"scsihw"`
    Bios        string               `json:"bios,omitempty"`
    Machine     string               `json:"machine,omitempty"`
    Agent       *bool                `json:"agent,omitempty"`
    Onboot      *bool                `json:"onboot,omitempty"`
    Disk        *VMTemplateDiskProps  `json:"disk"`
    Network     *NetworkProperties   `json:"network"`
    CloudInit   *CloudInitProperties `json:"cloudInit,omitempty"`
    Status      string               `json:"status,omitempty"`
}

// VMTemplateDiskProps maps to VMTemplateDisk sub-resource.
type VMTemplateDiskProps struct {
    Storage interface{} `json:"storage"`
    Size    int         `json:"size,omitempty"`
    Cache   string      `json:"cache,omitempty"`
    Discard *bool       `json:"discard,omitempty"`
}

// vmTemplateStepConfig holds data needed between multi-step operations.
// Serialized to JSON and base64-encoded in the requestID.
type vmTemplateStepConfig struct {
    CloudImageVolid string               `json:"civ"`
    DiskStorage     string               `json:"ds"`
    DiskSize        int                  `json:"dsz,omitempty"`
    DiskCache       string               `json:"dc,omitempty"`
    DiskDiscard     bool                 `json:"dd,omitempty"`
    Agent           bool                 `json:"ag,omitempty"`
    CloudInit       *CloudInitProperties `json:"ci,omitempty"`
}
```

#### 3. Go handler — vm_template.go
**File**: `vm_template.go` (NEW)
**Changes**: Full implementation with multi-step create.

**List**: GET /nodes/{node}/qemu, filter where `Template == 1`.

**Read**: GET /nodes/{node}/qemu/{vmid}/config + status/current, verify `template == 1` in config.

**Create** (multi-step):
1. Validate props, resolve node/cloudImage/storage
2. Get next VMID if not specified
3. Build VM create params (name, memory, cores, sockets, ostype, scsihw, bios, machine, net0)
4. POST /nodes/{node}/qemu → UPID
5. Encode step config (cloud image volid, disk storage/size, cloud-init) as base64 JSON
6. Return InProgress with requestID = `vmtpl:create:<base64-config>:<upid>`

**Delete**: Same as regular VM delete (stop + DELETE with purge).

**Update**: Limited to name, description, memory, cores, sockets, onboot, agent, cloud-init params via PUT /config. Returns synchronous success. Note: template VMs can have their config updated but cannot be started.

**pollVMTemplateTask** (in `status.go` or `vm_template.go`):

```go
func (p *Plugin) pollVMTemplateTask(ctx context.Context, client *Client, req *resource.StatusRequest) (*resource.StatusResult, error) {
    // Parse requestID: "vmtpl:<step>:<base64-config>:<upid>"
    parts := strings.SplitN(req.RequestID, ":", 4)
    if len(parts) < 4 {
        return statusFailure(resource.OperationErrorCodeInternalFailure, "invalid vmtpl requestID"), nil
    }
    step := parts[1]
    configB64 := parts[2]
    upid := parts[3]

    // Poll current UPID
    taskStatus, err := client.GetTaskStatus(ctx, upid)
    if err != nil {
        return statusFailure(resource.OperationErrorCodeNetworkFailure, err.Error()), nil
    }

    if taskStatus.IsRunning() {
        return &resource.StatusResult{
            ProgressResult: &resource.ProgressResult{
                Operation:       resource.OperationCheckStatus,
                OperationStatus: resource.OperationStatusInProgress,
                RequestID:       req.RequestID,
                NativeID:        req.NativeID,
                StatusMessage:   fmt.Sprintf("vmtpl %s: task running", step),
            },
        }, nil
    }

    if !taskStatus.IsSuccess() {
        return statusFailure(resource.OperationErrorCodeInternalFailure, taskStatus.ErrorMessage()), nil
    }

    switch step {
    case "create":
        // VM shell created. Now apply config: import disk + cloud-init drive + cloud-init params.
        var stepCfg vmTemplateStepConfig
        cfgJSON, _ := base64.StdEncoding.DecodeString(configB64)
        json.Unmarshal(cfgJSON, &stepCfg)

        node, vmid, _ := parseCompositeID(req.NativeID)

        // Build config params
        configParams := map[string]string{
            "boot": "order=scsi0",
        }

        // Import disk from cloud image
        diskSpec := fmt.Sprintf("%s:0,import-from=%s", stepCfg.DiskStorage, stepCfg.CloudImageVolid)
        if stepCfg.DiskCache != "" {
            diskSpec += ",cache=" + stepCfg.DiskCache
        }
        if stepCfg.DiskDiscard {
            diskSpec += ",discard=on"
        }
        configParams["scsi0"] = diskSpec

        // Attach cloud-init drive
        configParams["ide2"] = stepCfg.DiskStorage + ":cloudinit"

        // Serial console (needed for cloud-init)
        configParams["serial0"] = "socket"
        configParams["vga"] = "serial0"

        // Agent
        if stepCfg.Agent {
            configParams["agent"] = "1"
        }

        // Cloud-init defaults
        if stepCfg.CloudInit != nil {
            applyCloudInitParams(configParams, stepCfg.CloudInit)
        }

        // Apply config (synchronous, may take time for disk import)
        _, err := client.Put(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid), configParams)
        if err != nil {
            return statusFailure(resource.OperationErrorCodeInternalFailure, fmt.Sprintf("applying config: %v", err)), nil
        }

        // Resize disk if requested
        if stepCfg.DiskSize > 0 {
            _, _ = client.Put(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/resize", node, vmid), map[string]string{
                "disk": "scsi0",
                "size": fmt.Sprintf("%dG", stepCfg.DiskSize),
            })
        }

        // Convert to template (async)
        data, err := client.Post(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/template", node, vmid), nil)
        if err != nil {
            return statusFailure(resource.OperationErrorCodeInternalFailure, fmt.Sprintf("converting to template: %v", err)), nil
        }
        var templateUpid string
        json.Unmarshal(data, &templateUpid)

        // Return InProgress for template conversion step
        newRequestID := fmt.Sprintf("vmtpl:template::%s", templateUpid)
        return &resource.StatusResult{
            ProgressResult: &resource.ProgressResult{
                Operation:       resource.OperationCheckStatus,
                OperationStatus: resource.OperationStatusInProgress,
                RequestID:       newRequestID,
                NativeID:        req.NativeID,
                StatusMessage:   "converting to template",
            },
        }, nil

    case "template":
        // Template conversion done. Read back properties.
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

    return statusFailure(resource.OperationErrorCodeInternalFailure, "unknown vmtpl step"), nil
}
```

**Helper — applyCloudInitParams**: Shared function used by both VMTemplate and VM.

```go
// applyCloudInitParams adds cloud-init params to a config map.
func applyCloudInitParams(params map[string]string, ci *CloudInitProperties) {
    if ci.CIUser != "" {
        params["ciuser"] = ci.CIUser
    }
    if ci.CIPassword != "" {
        params["cipassword"] = ci.CIPassword
    }
    if ci.SSHKeys != "" {
        params["sshkeys"] = ci.SSHKeys
    }
    if ci.IPConfig0 != "" {
        params["ipconfig0"] = ci.IPConfig0
    }
    if ci.Nameserver != "" {
        params["nameserver"] = ci.Nameserver
    }
    if ci.Searchdomain != "" {
        params["searchdomain"] = ci.Searchdomain
    }
    if ci.CIType != "" {
        params["citype"] = ci.CIType
    }
    if ci.CIUpgrade != nil {
        if *ci.CIUpgrade {
            params["ciupgrade"] = "1"
        } else {
            params["ciupgrade"] = "0"
        }
    }
}
```

Refactor Phase 3's inline cloud-init code in `createVM` and `updateVM` to use this shared helper.

#### 4. Wire into proxmox.go
**File**: `proxmox.go`
**Changes**: Replace VMTemplate stubs with real calls.

- Create: `return p.createVMTemplate(ctx, client, req)`
- Read: `return p.readVMTemplate(ctx, client, req)`
- Update: `return p.updateVMTemplate(ctx, client, req)`
- Delete: `return p.deleteVMTemplate(ctx, client, req)`
- List: `return p.listVMTemplates(ctx, client, req)`
- LabelConfig: add `ResourceTypeVMTemplate: "$.name"`

### Success Criteria:

#### Automated Verification:
- [x] `go build ./...` passes
- [x] `go vet ./...` passes
- [x] `make verify-schema` passes

#### Manual Verification:
- [ ] Create VMTemplate pointing to downloaded Ubuntu CloudImage
  - Verify: VM appears in Proxmox with template flag
  - Verify: Disk imported from cloud image
  - Verify: Cloud-init drive (ide2) attached
  - Verify: Default cloud-init params set (ciuser, ipconfig0)
- [ ] List VMTemplates returns only template VMs
- [ ] Read VMTemplate returns correct properties
- [ ] Delete VMTemplate removes the template VM
- [ ] Multi-step status polling works (visible in formae logs)

**Implementation Note**: Pause for manual verification before Phase 5.

---

## Phase 5: VM Clone + Enhanced Update

### Overview
Enhance VirtualMachine Create to support cloning from a VMTemplate. When `cloneFrom` is set, use the clone API instead of raw creation. Apply sizing overrides, cloud-init overrides, and disk resize after clone.

### Changes Required:

#### 1. Modify createVM — detect clone mode
**File**: `vm.go`
**Changes**: At the top of `createVM`, check for `cloneFrom`. If set, delegate to `cloneVM`.

```go
func (p *Plugin) createVM(ctx context.Context, client *Client, req *resource.CreateRequest) (*resource.CreateResult, error) {
    var props VMProperties
    if err := json.Unmarshal(req.Properties, &props); err != nil {
        return createFailure(resource.OperationErrorCodeInvalidRequest, fmt.Sprintf("invalid properties: %v", err)), nil
    }

    // Clone mode: cloneFrom is set
    if props.CloneFrom != nil {
        cloneSource := resolveString(props.CloneFrom)
        if cloneSource != "" {
            return p.cloneVM(ctx, client, req, &props, cloneSource)
        }
    }

    // Original create path (unchanged) ...
}
```

#### 2. Implement cloneVM
**File**: `vm.go`
**Changes**: New function for clone-based creation.

```go
func (p *Plugin) cloneVM(ctx context.Context, client *Client, req *resource.CreateRequest, props *VMProperties, cloneSource string) (*resource.CreateResult, error) {
    // Parse clone source "node/vmid"
    sourceNode, sourceVMID, err := parseCompositeID(cloneSource)
    if err != nil {
        return createFailure(resource.OperationErrorCodeInvalidRequest, fmt.Sprintf("invalid cloneFrom: %v", err)), nil
    }

    node := resolveString(props.Node)
    if node == "" {
        node = sourceNode
    }

    vmid := props.VMID
    if vmid == 0 {
        nextID, err := getNextID(ctx, client)
        if err != nil {
            return createFailure(resource.OperationErrorCodeInternalFailure, err.Error()), nil
        }
        vmid = nextID
    }

    params := map[string]string{
        "newid": strconv.Itoa(vmid),
    }
    if props.Name != "" {
        params["name"] = props.Name
    }
    if props.Description != "" {
        params["description"] = props.Description
    }

    // Full clone by default
    fullClone := true
    if props.FullClone != nil {
        fullClone = *props.FullClone
    }
    if fullClone {
        params["full"] = "1"
        // Target storage for full clone
        if props.Disk != nil {
            storage := resolveString(props.Disk.Storage)
            if storage != "" {
                params["storage"] = storage
            }
        }
    } else {
        params["full"] = "0"
    }

    nativeID := compositeID(node, vmid)

    data, err := client.Post(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/clone", sourceNode, sourceVMID), params)
    if err != nil {
        return createFailure(resource.OperationErrorCodeInternalFailure, err.Error()), nil
    }

    var upid string
    if err := json.Unmarshal(data, &upid); err != nil {
        return createFailure(resource.OperationErrorCodeInternalFailure, fmt.Sprintf("parsing UPID: %v", err)), nil
    }

    // Encode post-clone config in requestID for multi-step status
    stepCfg := cloneStepConfig{
        Memory:    props.Memory,
        Cores:     props.Cores,
        Sockets:   props.Sockets,
        Onboot:    props.Onboot,
        Agent:     props.Agent,
        DiskSize:  0,
        CloudInit: props.CloudInit,
    }
    if props.Disk != nil && props.Disk.Size > 0 {
        stepCfg.DiskSize = props.Disk.Size
    }
    cfgJSON, _ := json.Marshal(stepCfg)
    cfgB64 := base64.StdEncoding.EncodeToString(cfgJSON)

    requestID := fmt.Sprintf("clone:%s:%s", cfgB64, upid)

    return &resource.CreateResult{
        ProgressResult: &resource.ProgressResult{
            Operation:       resource.OperationCreate,
            OperationStatus: resource.OperationStatusInProgress,
            RequestID:       requestID,
            NativeID:        nativeID,
        },
    }, nil
}
```

#### 3. Go types — cloneStepConfig
**File**: `types.go`
**Changes**: Add clone step config struct.

```go
// cloneStepConfig holds post-clone config data encoded in requestID.
type cloneStepConfig struct {
    Memory    int                  `json:"mem,omitempty"`
    Cores     int                  `json:"cor,omitempty"`
    Sockets   int                  `json:"soc,omitempty"`
    Onboot    *bool                `json:"ob,omitempty"`
    Agent     *bool                `json:"ag,omitempty"`
    DiskSize  int                  `json:"dsz,omitempty"`
    CloudInit *CloudInitProperties `json:"ci,omitempty"`
}
```

#### 4. Implement pollCloneTask
**File**: `status.go` (or `vm.go`)
**Changes**: Multi-step status handler for clone operations.

```go
func (p *Plugin) pollCloneTask(ctx context.Context, client *Client, req *resource.StatusRequest) (*resource.StatusResult, error) {
    // Parse requestID: "clone:<base64-config>:<upid>"
    parts := strings.SplitN(req.RequestID, ":", 3)
    if len(parts) < 3 {
        return statusFailure(resource.OperationErrorCodeInternalFailure, "invalid clone requestID"), nil
    }
    configB64 := parts[1]
    upid := parts[2]

    taskStatus, err := client.GetTaskStatus(ctx, upid)
    if err != nil {
        return statusFailure(resource.OperationErrorCodeNetworkFailure, err.Error()), nil
    }

    if taskStatus.IsRunning() {
        return &resource.StatusResult{
            ProgressResult: &resource.ProgressResult{
                Operation:       resource.OperationCheckStatus,
                OperationStatus: resource.OperationStatusInProgress,
                RequestID:       req.RequestID,
                NativeID:        req.NativeID,
                StatusMessage:   "cloning in progress",
            },
        }, nil
    }

    if !taskStatus.IsSuccess() {
        return statusFailure(resource.OperationErrorCodeInternalFailure, taskStatus.ErrorMessage()), nil
    }

    // Clone done. Apply post-clone overrides.
    var stepCfg cloneStepConfig
    cfgJSON, _ := base64.StdEncoding.DecodeString(configB64)
    json.Unmarshal(cfgJSON, &stepCfg)

    node, vmid, _ := parseCompositeID(req.NativeID)

    // Apply sizing + cloud-init overrides
    configParams := map[string]string{}
    if stepCfg.Memory > 0 {
        configParams["memory"] = strconv.Itoa(stepCfg.Memory)
    }
    if stepCfg.Cores > 0 {
        configParams["cores"] = strconv.Itoa(stepCfg.Cores)
    }
    if stepCfg.Sockets > 0 {
        configParams["sockets"] = strconv.Itoa(stepCfg.Sockets)
    }
    if stepCfg.Onboot != nil {
        if *stepCfg.Onboot {
            configParams["onboot"] = "1"
        } else {
            configParams["onboot"] = "0"
        }
    }
    if stepCfg.Agent != nil {
        if *stepCfg.Agent {
            configParams["agent"] = "1"
        } else {
            configParams["agent"] = "0"
        }
    }
    if stepCfg.CloudInit != nil {
        applyCloudInitParams(configParams, stepCfg.CloudInit)
    }

    if len(configParams) > 0 {
        _, _ = client.Put(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid), configParams)
    }

    // Disk resize if requested
    if stepCfg.DiskSize > 0 {
        _, _ = client.Put(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/resize", node, vmid), map[string]string{
            "disk": "scsi0",
            "size": fmt.Sprintf("%dG", stepCfg.DiskSize),
        })
    }

    // Read back final state
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
```

#### 5. Update example config
**File**: `examples/basic/main.pkl`
**Changes**: Add full workflow example demonstrating CloudImage → VMTemplate → cloned VM.

```pkl
// Cloud image download
new proxmox.CloudImage {
    label = "ubuntu-24.04"
    node = new formae.Resolvable { label = "pve"; type = "PROXMOX::Node::Node"; stack = "testing1"; property = "node" }
    storage = "local"
    url = "https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img"
    filename = "noble-server-cloudimg-amd64.img"
    target = myProxmox.res
    stack = myStack.res
}

// VM template from cloud image
new proxmox.VMTemplate {
    label = "ubuntu-template"
    node = new formae.Resolvable { label = "pve"; type = "PROXMOX::Node::Node"; stack = "testing1"; property = "node" }
    name = "ubuntu-24.04-template"
    cloudImage = new formae.Resolvable { label = "ubuntu-24.04"; type = "PROXMOX::Image::CloudImage"; stack = "testing1"; property = "volid" }
    memory = 2048
    cores = 2
    disk = new proxmox.VMTemplateDisk {
        storage = new formae.Resolvable { label = "local-lvm"; type = "PROXMOX::Storage::Storage"; stack = "testing1"; property = "storage" }
        size = 20
    }
    network = new proxmox.VirtualMachineNetwork {
        model = "virtio"
        bridge = "vmbr0"
    }
    cloudInit = new proxmox.CloudInit {
        ciuser = "ubuntu"
        ipconfig0 = "ip=dhcp"
    }
    target = myProxmox.res
    stack = myStack.res
}

// VM cloned from template
new proxmox.VirtualMachine {
    label = "web-server-1"
    node = new formae.Resolvable { label = "pve"; type = "PROXMOX::Node::Node"; stack = "testing1"; property = "node" }
    name = "web-server-1"
    cloneFrom = new formae.Resolvable { label = "ubuntu-template"; type = "PROXMOX::Compute::VMTemplate"; stack = "testing1"; property = "id" }
    fullClone = true
    memory = 4096
    cores = 4
    disk = new proxmox.VirtualMachineDisk {
        storage = new formae.Resolvable { label = "local-lvm"; type = "PROXMOX::Storage::Storage"; stack = "testing1"; property = "storage" }
        size = 50
    }
    network = new proxmox.VirtualMachineNetwork {
        model = "virtio"
        bridge = "vmbr0"
    }
    cloudInit = new proxmox.CloudInit {
        ciuser = "ubuntu"
        sshkeys = "ssh-ed25519 AAAA... user@machine"
        ipconfig0 = "ip=192.168.1.100/24,gw=192.168.1.1"
        nameserver = "8.8.8.8"
    }
    target = myProxmox.res
    stack = myStack.res
}
```

### Success Criteria:

#### Automated Verification:
- [x] `go build ./...` passes
- [x] `go vet ./...` passes
- [x] `make verify-schema` passes

#### Manual Verification:
- [ ] Full workflow: CloudImage → VMTemplate → cloned VM
  - Download Ubuntu cloud image
  - Create VMTemplate from cloud image (verify multi-step: VM created → disk imported → converted to template)
  - Clone VM from template with 4096 MB RAM, 4 cores, 50G disk
  - Verify cloud-init overrides (custom IP, SSH keys) applied to cloned VM
  - Verify disk resized to 50G on cloned VM
- [ ] Update cloned VM: change memory, change cloud-init ipconfig0
- [ ] Delete cloned VM, delete template, delete cloud image (in reverse order)
- [ ] Linked clone: set `fullClone = false`, verify linked clone works

**Implementation Note**: After all phases complete, run full conformance test suite: `make conformance-test`

---

## Testing Strategy

### Unit Tests:
- `parseCloudImageNativeID` / `cloudImageNativeID` round-trip
- `applyCloudInitParams` builds correct param map
- `parseVMConfig` extracts cloud-init fields
- Multi-step requestID encode/decode for vmtpl and clone
- VM list filtering (template flag)

### Integration Tests (conformance):
- CloudImage: create → read → list → delete
- VMTemplate: create (multi-step) → read → list → update → delete
- VM with clone: create (clone) → read → update (cloud-init, resize) → delete
- VM without clone: create with cloud-init → read → update → delete

### Manual Testing Steps:
1. Download Ubuntu 24.04 cloud image to local storage
2. Create VM template with cloud-init defaults (ciuser=ubuntu, ipconfig0=dhcp)
3. Clone VM from template with custom IP and SSH key
4. Verify VM boots with cloud-init applied (SSH in with key)
5. Update VM memory from 4096 to 8192, verify change
6. Resize disk from 50G to 100G, verify in Proxmox
7. Delete everything in reverse order

## Performance Considerations

- Client timeout increased to 300s for disk imports (cloud images can be 1-2GB)
- Multi-step operations avoid blocking Create() for extended periods — only the synchronous config PUT in Status() may take time
- UPID polling is handled by formae SDK at its own pace — no busy-wait in plugin

## File Summary

| File | Action | Description |
|---|---|---|
| `client.go` | Modify | Timeout 30s → 300s |
| `types.go` | Modify | Add CloudImage, VMTemplate, CloudInit, clone types; template flag on VM list entry |
| `proxmox.go` | Modify | Add resource type constants + routing for CloudImage, VMTemplate |
| `proxmox.pkl` | Modify | Add CloudImage, CloudInit, VMTemplate, VMTemplateDisk classes; enhance VirtualMachine |
| `cloud_image.go` | New | CloudImage CRUD |
| `vm_template.go` | New | VMTemplate CRUD + multi-step create + pollVMTemplateTask |
| `vm.go` | Modify | Add cloud-init to create/read/update, clone support, disk resize, template filtering |
| `status.go` | Modify | Multi-step dispatch (vmtpl:/clone: prefixes) |
| `examples/basic/main.pkl` | Modify | Full workflow example |

## References

- Proxmox API: cloud-init params — https://pve.proxmox.com/wiki/Cloud-Init_Support
- Proxmox API: download-url — `POST /nodes/{node}/storage/{storage}/download-url`
- Proxmox API: clone — `POST /nodes/{node}/qemu/{vmid}/clone`
- Proxmox API: template — `POST /nodes/{node}/qemu/{vmid}/template`
- Proxmox API: resize — `PUT /nodes/{node}/qemu/{vmid}/resize`
- Existing PLAN.md — original 5-phase plan for base plugin
