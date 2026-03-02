# Starting VMID Configuration

## Overview

Add `startId` to target Config so auto-assigned VMIDs for VMs, templates, and containers start from a user-defined minimum instead of Proxmox's default (100).

## Current State

- VMIDs auto-assigned via `GET /cluster/nextid` — returns lowest free ID >= 100
- `getNextID` is a standalone function in `vm.go:692` taking `(ctx, client)`
- Called in 8 places: initial + retry in createVM, cloneVM, createVMTemplate, createContainer
- No VMID config in `Config` class or `TargetConfig` struct
- Proxmox `nextid` API's `vmid` param only validates a specific ID, not "next >= N"
- Each resource already has optional `vmid: Int?` for explicit override

## Desired End State

- Users can set `startId = 200` in their target Config
- All auto-assigned VMIDs will be >= 200
- Explicit `vmid` on individual resources still takes precedence
- Without `startId`, behavior unchanged (Proxmox default)

## What We're NOT Doing

- Per-resource-type ID ranges (e.g., VMs from 200, CTs from 500)
- ID pool/reservation system
- Validation that startId doesn't conflict with existing resources

## Implementation Approach

1. Add `startId` to Pkl Config and Go TargetConfig
2. Store startId on the Client struct (same package, direct access)
3. Convert `getNextID` to a Client method that uses `/cluster/resources?type=vm` to find the next free ID >= startId when configured
4. Update all 8 call sites

---

## Phase 1: Schema + Types + Client

### Overview
Add startId field to Pkl schema, Go config, and Client. Convert getNextID to Client method with startId-aware logic.

### Changes Required:

#### 1. Pkl Schema
**File**: `schema/pkl/proxmox.pkl` — `Config` class

Add after `insecure`:
```pkl
    /// Starting VM ID for auto-assignment (100-999999999). If omitted, uses Proxmox default.
    startId: Int?
```

#### 2. Go TargetConfig
**File**: `config.go` — `TargetConfig` struct

Add field:
```go
type TargetConfig struct {
    URL      string `json:"url"`
    Insecure bool   `json:"insecure"`
    StartID  int    `json:"startId"`
}
```

#### 3. Client struct
**File**: `client.go` — `Client` struct

Add field:
```go
type Client struct {
    baseURL    string
    apiToken   string
    httpClient *http.Client
    startID    int
}
```

#### 4. Wire startID into Client
**File**: `config.go` — `clientCache.get`

After creating the client, set startID:
```go
cc.client = NewClient(cfg.URL, apiToken, cfg.Insecure)
cc.client.startID = cfg.StartID
```

#### 5. Convert getNextID to Client method
**File**: `vm.go`

Remove the standalone `getNextID` function and replace with a Client method on `client.go`:

**File**: `client.go` — new method

```go
// getNextID returns the next available VMID, respecting the configured startID.
func (c *Client) getNextID(ctx context.Context) (int, error) {
    if c.startID <= 0 {
        // No minimum — use Proxmox default
        data, err := c.Get(ctx, "/cluster/nextid")
        if err != nil {
            return 0, err
        }
        var idStr string
        if err := json.Unmarshal(data, &idStr); err != nil {
            return 0, fmt.Errorf("parsing nextid: %w", err)
        }
        return strconv.Atoi(idStr)
    }

    // Get all existing VMIDs from the cluster
    data, err := c.Get(ctx, "/cluster/resources?type=vm")
    if err != nil {
        return 0, fmt.Errorf("listing cluster resources: %w", err)
    }

    var resources []struct {
        VMID int `json:"vmid"`
    }
    if err := json.Unmarshal(data, &resources); err != nil {
        return 0, fmt.Errorf("parsing cluster resources: %w", err)
    }

    used := make(map[int]bool, len(resources))
    for _, r := range resources {
        used[r.VMID] = true
    }

    for id := c.startID; id < 1000000000; id++ {
        if !used[id] {
            return id, nil
        }
    }

    return 0, fmt.Errorf("no free VMID >= %d", c.startID)
}
```

Requires adding `"encoding/json"`, `"fmt"`, and `"strconv"` to `client.go` imports.

#### 6. Update all call sites
Change all 8 occurrences of `getNextID(ctx, client)` to `client.getNextID(ctx)`:

**File**: `vm.go` — `createVM` (line ~81, ~149)
**File**: `vm.go` — `cloneVM` (line ~399, ~437)
**File**: `vm_template.go` — `createVMTemplate` (line ~123, ~164)
**File**: `container.go` — `createContainer` (line ~68, ~112)

#### 7. Update example
**File**: `examples/basic/main.pkl`

Add `startId` to the target config:
```pkl
config = new proxmox.Config {
    url = "https://192.168.1.20:8006"
    insecure = true
    startId = 200
}
```

### Success Criteria:

#### Automated Verification:
- [x] `go build ./...` compiles cleanly
- [x] `make install` succeeds

#### Manual Verification:
- [ ] With `startId = 200`: new VM gets VMID >= 200
- [ ] With `startId = 200` and existing VMID 200: new VM gets 201
- [ ] Without `startId`: behavior unchanged (Proxmox default from 100)
- [ ] Explicit `vmid: 150` on a resource still works (ignores startId)

---

## Unresolved Questions

None.
