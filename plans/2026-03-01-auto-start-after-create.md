# Auto-Start, EFI Disk, SSH Key Fix

## Overview

1. Add `start: Boolean = true` to VirtualMachine and Container — auto-start after creation
2. Auto-add `efidisk0` when `bios=ovmf` for both VMTemplate and VirtualMachine
3. Fix cloud-init `sshkeys` URL encoding bug

## Current State

- VMs/containers are created and left in `stopped` state
- No `start` API calls exist in the codebase
- Standalone VM and Container creation use plain UPIDs (single-step status)
- Clone VMs use `clone:<b64>:<upid>` (multi-step, but no start step)
- VMTemplate has a 3-step state machine (`vmtpl:create/import/template`) as reference pattern
- **Bug**: `sshkeys` in `applyCloudInitParams` (`vm.go:691`) is passed raw — Proxmox requires it to be URL-encoded because it applies an extra decode layer on that field specifically
- **Missing**: No `efidisk0` support — VMs/templates with `bios=ovmf` need an EFI disk for UEFI variable storage

## Desired End State

- `formae apply` creates and starts VMs/containers by default
- User can opt out with `start = false` to leave them stopped
- Status field reads `"running"` after successful creation (when start=true)
- Existing behavior preserved when `start = false`
- Cloud-init SSH keys work correctly (no "invalid urlencoded string" errors)
- VMs/templates with `bios=ovmf` automatically get an EFI disk on the same storage as the main disk

## What We're NOT Doing

- No stop/restart management (operational concern)
- No start on Update (only on Create)
- No VMTemplate start (templates can't be started)
- No waiting for guest agent ready / cloud-init completion
- No explicit EFI disk schema field — auto-inferred from bios setting

## Implementation Approach

Convert single-step flows (plain UPID) to two-step flows (create → start) using requestID prefixes. Reuse the existing multi-step pattern from VMTemplate.

New requestID formats:
| Step | Format | Split |
|---|---|---|
| VM create (needs start) | `vm:create:<createUpid>` | SplitN(":", 3) |
| VM start | `vm:start:<startUpid>` | SplitN(":", 3) |
| CT create (needs start) | `ct:create:<createUpid>` | SplitN(":", 3) |
| CT start | `ct:start:<startUpid>` | SplitN(":", 3) |
| Clone (needs start) | existing `clone:<b64>:<upid>` then → `vm:start:<startUpid>` | — |

When `start = false` (or nil): plain UPID, existing single-step behavior.

---

## Phase 0: Fix SSH Keys URL Encoding

### Overview
Proxmox requires the `sshkeys` parameter to be URL-encoded before form submission. The form encoder in `client.Post` already does standard form encoding, but Proxmox applies an additional URL-decode on the `sshkeys` field specifically — so the value must be double-encoded.

Error: `invalid format - invalid urlencoded string: ssh-ed25519 AAAA... user@machine\n`

### Changes Required:

#### 1. URL-encode sshkeys
**File**: `vm.go` — `applyCloudInitParams`

Change:
```go
if ci.SSHKeys != "" {
    params["sshkeys"] = ci.SSHKeys
}
```

To:
```go
if ci.SSHKeys != "" {
    params["sshkeys"] = url.PathEscape(ci.SSHKeys)
}
```

Note: use `url.PathEscape` (not `url.QueryEscape`) because `QueryEscape` turns spaces into `+` which Proxmox doesn't handle correctly for this field. `PathEscape` uses `%20` for spaces and `%0A` for newlines.

Requires adding `"net/url"` to imports in `vm.go`.

### Success Criteria:

#### Automated Verification:
- [x] `go build ./...` compiles cleanly

#### Manual Verification:
- [ ] VM/template with `sshkeys` in cloud-init creates/updates without "invalid urlencoded string" error

---

## Phase 0.5: Auto EFI Disk for UEFI Boot

### Overview
When `bios = "ovmf"`, Proxmox requires an `efidisk0` for UEFI variable storage. Without it, Proxmox uses a temporary efivars disk and logs warnings. Auto-add `efidisk0` on the same storage as the main disk.

Format: `<storage>:1,efitype=4m,pre-enrolled-keys=1`
- `efitype=4m`: recommended for all new VMs, supports Secure Boot
- `pre-enrolled-keys=1`: pre-load Microsoft + distro Secure Boot keys

### Changes Required:

#### 1. VMTemplate Create — disk import step
**File**: `vm_template.go` — `pollVMTemplateTask`, case `"create"`

When building `configParams` for POST /config, add EFI disk if bios is ovmf:
```go
// Check if VM has ovmf bios
if bios, ok := cfgMap["bios"].(string); ok && bios == "ovmf" {
    configParams["efidisk0"] = stepCfg.DiskStorage + ":1,efitype=4m,pre-enrolled-keys=1"
}
```

This goes alongside the existing `scsi0`, `ide2`, `serial0`, `vga` params in the import step.

**But**: we don't have access to `cfgMap` at the point where we build `configParams` in the "create" step. The bios is set during VM shell creation and is available in the config. We need to read it.

Actually, looking at the flow: in the "create"/"import" case, we already fetch `existingConfig` and unmarshal to `cfgMap`. So we CAN check bios there:

```go
// In the "create" step, after reading cfgMap and before building configParams:
if bios, ok := cfgMap["bios"].(string); ok && bios == "ovmf" {
    configParams["efidisk0"] = stepCfg.DiskStorage + ":1,efitype=4m,pre-enrolled-keys=1"
}
```

#### 2. Standalone VM Create
**File**: `vm.go` — `createVM`

When building `params` for POST /nodes/{node}/qemu, add EFI disk if bios is ovmf:
```go
if props.Bios == "ovmf" && props.Disk != nil {
    storage := resolveString(props.Disk.Storage)
    if storage != "" {
        params["efidisk0"] = storage + ":1,efitype=4m,pre-enrolled-keys=1"
    }
}
```

This goes after the existing disk/network/agent/cloud-init params.

#### 3. Clone VMs
No change needed — clones inherit the EFI disk from the source template/VM.

#### 4. Read back EFI disk info
No schema change needed. The EFI disk is an implementation detail, not a user-facing property. It's auto-managed based on `bios`.

### Success Criteria:

#### Automated Verification:
- [x] `go build ./...` compiles cleanly

#### Manual Verification:
- [ ] VMTemplate with `bios = "ovmf"` creates with efidisk0 visible in Proxmox UI
- [ ] Standalone VM with `bios = "ovmf"` creates with efidisk0
- [ ] VM/template with `bios = "seabios"` (or default) does NOT get efidisk0
- [ ] Cloned VM from OVMF template inherits efidisk0

---

## Phase 1: Schema + Types

### Overview
Add the `start` property to Pkl schema and Go structs.

### Changes Required:

#### 1. Pkl Schema
**File**: `schema/pkl/proxmox.pkl`

Add to `VirtualMachine` class (after `fullClone`):
```pkl
    /// Start VM after creation. Default: true.
    @formae.FieldHint { createOnly = true }
    start: Boolean = true
```

Add to `Container` class (after `password`):
```pkl
    /// Start container after creation. Default: true.
    @formae.FieldHint { createOnly = true }
    start: Boolean = true
```

#### 2. Go Types
**File**: `types.go`

Add to `VMProperties`:
```go
Start *bool `json:"start,omitempty"`
```

Add to `ContainerProperties`:
```go
Start *bool `json:"start,omitempty"`
```

Add to `cloneStepConfig`:
```go
Start bool `json:"st,omitempty"`
```

### Success Criteria:

#### Automated Verification:
- [x] `go build ./...` compiles cleanly

---

## Phase 2: Multi-Step Status Handlers

### Overview
Add `vm:` and `ct:` prefix handlers for the create→start two-step flow.

### Changes Required:

#### 1. Status Dispatch
**File**: `status.go`

Add new prefix checks (after `clone:`, before generic fallthrough):
```go
if strings.HasPrefix(req.RequestID, "vm:") {
    return p.pollVMStart(ctx, client, req)
}
if strings.HasPrefix(req.RequestID, "ct:") {
    return p.pollCTStart(ctx, client, req)
}
```

#### 2. VM Start Handler
**File**: `vm.go` (new function)

```go
func (p *Plugin) pollVMStart(ctx context.Context, client *Client, req *resource.StatusRequest) (*resource.StatusResult, error) {
    parts := strings.SplitN(req.RequestID, ":", 3)
    if len(parts) < 3 {
        return statusFailure(resource.OperationErrorCodeInternalFailure, "invalid vm requestID"), nil
    }
    step := parts[1]  // "create" or "start"
    upid := parts[2]

    taskStatus, err := client.GetTaskStatus(ctx, upid)
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
                StatusMessage:   fmt.Sprintf("vm %s: task running", step),
            },
        }, nil
    }
    if !taskStatus.IsSuccess() {
        return statusFailure(resource.OperationErrorCodeInternalFailure, taskStatus.ErrorMessage()), nil
    }

    switch step {
    case "create":
        // Create done — fire start
        node, vmid, err := parseCompositeID(req.NativeID)
        if err != nil {
            return statusFailure(resource.OperationErrorCodeInternalFailure, err.Error()), nil
        }
        data, err := client.Post(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/status/start", node, vmid), nil)
        if err != nil {
            // Start failed but VM was created — return success anyway
            // (VM exists, just not started)
            break
        }
        var startUpid string
        if err := json.Unmarshal(data, &startUpid); err != nil {
            break // same — return success with stopped VM
        }
        return &resource.StatusResult{
            ProgressResult: &resource.ProgressResult{
                Operation:       resource.OperationCheckStatus,
                OperationStatus: resource.OperationStatusInProgress,
                RequestID:       "vm:start:" + startUpid,
                NativeID:        req.NativeID,
                StatusMessage:   "starting VM",
            },
        }, nil
    }

    // step == "start" done, or start failed gracefully — read back and return success
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

#### 3. Container Start Handler
**File**: `container.go` (new function)

Same pattern as `pollVMStart` but uses `/nodes/%s/lxc/%d/status/start` and `ct:` prefix.

#### 4. Clone Flow Enhancement
**File**: `vm_template.go` — `pollCloneTask`

After overrides are applied and before reading final state, if `stepCfg.Start`:
```go
if stepCfg.Start {
    data, err := client.Post(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/status/start", node, vmid), nil)
    if err == nil {
        var startUpid string
        if json.Unmarshal(data, &startUpid) == nil {
            return &resource.StatusResult{
                ProgressResult: &resource.ProgressResult{
                    Operation:       resource.OperationCheckStatus,
                    OperationStatus: resource.OperationStatusInProgress,
                    RequestID:       "vm:start:" + startUpid,
                    NativeID:        req.NativeID,
                    StatusMessage:   "starting cloned VM",
                },
            }, nil
        }
    }
    // If start fails, fall through to success with stopped VM
}
```

### Success Criteria:

#### Automated Verification:
- [x] `go build ./...` compiles cleanly

---

## Phase 3: Wire Up Create Functions

### Overview
Modify createVM, cloneVM, createContainer to use prefixed requestIDs when start=true.

### Changes Required:

#### 1. Standalone VM Create
**File**: `vm.go` — `createVM`

After getting the UPID, before returning:
```go
requestID := upid // default: plain UPID (no start)
if props.Start == nil || *props.Start {
    requestID = "vm:create:" + upid
}
```

#### 2. Clone VM Create
**File**: `vm.go` — `cloneVM`

Add Start to cloneStepConfig encoding:
```go
stepCfg := cloneStepConfig{
    // ... existing fields ...
    Start: props.Start == nil || *props.Start,
}
```

No requestID change needed — the clone prefix handler reads Start from config.

#### 3. Container Create
**File**: `container.go` — `createContainer`

Same pattern as standalone VM:
```go
requestID := upid
if props.Start == nil || *props.Start {
    requestID = "ct:create:" + upid
}
```

### Success Criteria:

#### Automated Verification:
- [x] `go build ./...` compiles cleanly
- [x] `make install` succeeds

#### Manual Verification:
- [ ] VM with `start = true` (default): created and running after `formae apply`
- [ ] VM with `start = false`: created but stopped
- [ ] Cloned VM: started after clone + overrides
- [ ] Container with default: created and running
- [ ] Container with `start = false`: stays stopped
- [ ] Re-running `formae apply` doesn't duplicate resources (existing dedup still works)

---

## Unresolved Questions

None.
