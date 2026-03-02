package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
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
		if vm.Template == 1 {
			continue // skip templates, they have their own resource type
		}
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

	// Clone mode: cloneFrom is set
	if props.CloneFrom != nil {
		cloneSource := resolveString(props.CloneFrom)
		if cloneSource != "" {
			return p.cloneVM(ctx, client, &props, cloneSource)
		}
	}

	node := resolveString(props.Node)
	if node == "" {
		return createFailure(resource.OperationErrorCodeInvalidRequest, "node is required"), nil
	}

	// Check if a VM with the same name already exists on this node.
	if props.Name != "" {
		if existing := p.findVMByName(ctx, client, node, props.Name); existing != nil {
			propsJSON, _ := json.Marshal(existing)
			return &resource.CreateResult{
				ProgressResult: &resource.ProgressResult{
					Operation:          resource.OperationCreate,
					OperationStatus:    resource.OperationStatusSuccess,
					NativeID:           existing.ID,
					ResourceProperties: json.RawMessage(propsJSON),
				},
			}, nil
		}
	}

	vmid := props.VMID
	if vmid == 0 {
		nextID, err := client.getNextID(ctx)
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

	if props.Disk != nil {
		params["scsi0"] = buildVMDiskSpec(props.Disk)
	}

	if props.Network != nil {
		params["net0"] = buildVMNetSpec(props.Network)
	}

	if props.Agent != nil && *props.Agent {
		params["agent"] = "1"
	}

	// Auto-add EFI disk for UEFI boot
	if props.Bios == "ovmf" && props.Disk != nil {
		storage := resolveString(props.Disk.Storage)
		if storage != "" {
			params["efidisk0"] = storage + ":1,efitype=4m,pre-enrolled-keys=1"
		}
	}

	if props.CloudInit != nil {
		// Attach cloud-init drive on ide2
		ciStorage := "local-lvm"
		if props.Disk != nil {
			ciStorage = resolveString(props.Disk.Storage)
		}
		params["ide2"] = ciStorage + ":cloudinit"
		params["boot"] = "order=scsi0"
		applyCloudInitParams(params, props.CloudInit)
	}

	nativeID := compositeID(node, vmid)

	data, err := client.Post(ctx, fmt.Sprintf("/nodes/%s/qemu", node), params)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			// If VMID was auto-assigned, retry once with a new ID (race with concurrent creates)
			if props.VMID == 0 {
				retryID, retryErr := client.getNextID(ctx)
				if retryErr == nil {
					vmid = retryID
					params["vmid"] = strconv.Itoa(vmid)
					nativeID = compositeID(node, vmid)
					data, err = client.Post(ctx, fmt.Sprintf("/nodes/%s/qemu", node), params)
				}
			}
			if err != nil {
				return createFailure(resource.OperationErrorCodeAlreadyExists, err.Error()), nil
			}
		}
		if err != nil {
			return createFailure(resource.OperationErrorCodeInternalFailure, err.Error()), nil
		}
	}

	var upid string
	if err := json.Unmarshal(data, &upid); err != nil {
		return createFailure(resource.OperationErrorCodeInternalFailure, fmt.Sprintf("parsing UPID: %v", err)), nil
	}

	requestID := upid
	if props.Start == nil || *props.Start {
		requestID = "vm:create:" + upid
	}

	return &resource.CreateResult{
		ProgressResult: &resource.ProgressResult{
			Operation:       resource.OperationCreate,
			OperationStatus: resource.OperationStatusInProgress,
			RequestID:       requestID,
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

	configData, err := client.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid))
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "500") {
			return &resource.ReadResult{ResourceType: req.ResourceType, ErrorCode: resource.OperationErrorCodeNotFound}, nil
		}
		return &resource.ReadResult{ResourceType: req.ResourceType, ErrorCode: resource.OperationErrorCodeNetworkFailure}, nil
	}

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

	params := map[string]string{
		"memory":  strconv.Itoa(desired.Memory),
		"cores":   strconv.Itoa(desired.Cores),
		"sockets": strconv.Itoa(desired.Sockets),
	}

	if desired.Name != "" {
		params["name"] = desired.Name
	}
	params["description"] = desired.Description
	if desired.Onboot != nil {
		if *desired.Onboot {
			params["onboot"] = "1"
		} else {
			params["onboot"] = "0"
		}
	}

	if desired.Agent != nil {
		if *desired.Agent {
			params["agent"] = "1"
		} else {
			params["agent"] = "0"
		}
	}

	if desired.CloudInit != nil {
		applyCloudInitParams(params, desired.CloudInit)
	}

	_, err = client.Put(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid), params)
	if err != nil {
		return updateFailure(resource.OperationErrorCodeInternalFailure, err.Error()), nil
	}

	// Disk resize (grow only) — requires VM to be stopped
	if desired.Disk != nil && desired.Disk.Size > 0 {
		if getVMStatus(ctx, client, node, vmid) == "running" {
			// Stop VM before resize, will restart after
			data, err := client.Post(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/status/stop", node, vmid), nil)
			if err == nil {
				var stopUpid string
				if json.Unmarshal(data, &stopUpid) == nil {
					return &resource.UpdateResult{
						ProgressResult: &resource.ProgressResult{
							Operation:       resource.OperationUpdate,
							OperationStatus: resource.OperationStatusInProgress,
							RequestID:       fmt.Sprintf("vmup:stop:%d:%s", desired.Disk.Size, stopUpid),
							NativeID:        req.NativeID,
							StatusMessage:   "stopping VM for disk resize",
						},
					}, nil
				}
			}
			// Stop failed — try resize anyway
		}
		_, _ = client.Put(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/resize", node, vmid), map[string]string{
			"disk": "scsi0",
			"size": fmt.Sprintf("%dG", desired.Disk.Size),
		})
	}

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
		return &resource.DeleteResult{
			ProgressResult: &resource.ProgressResult{
				Operation:       resource.OperationDelete,
				OperationStatus: resource.OperationStatusSuccess,
				NativeID:        req.NativeID,
			},
		}, nil
	}

	// Stop VM first (best effort)
	_, _ = client.Post(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/status/stop", node, vmid), nil)

	data, err := client.Delete(ctx, fmt.Sprintf("/nodes/%s/qemu/%d", node, vmid), map[string]string{
		"purge":                      "1",
		"destroy-unreferenced-disks": "1",
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

// --- Clone ---

func (p *Plugin) cloneVM(ctx context.Context, client *Client, props *VMProperties, cloneSource string) (*resource.CreateResult, error) {
	// Parse clone source "node/vmid"
	sourceNode, sourceVMID, err := parseCompositeID(cloneSource)
	if err != nil {
		return createFailure(resource.OperationErrorCodeInvalidRequest, fmt.Sprintf("invalid cloneFrom: %v", err)), nil
	}

	node := resolveString(props.Node)
	if node == "" {
		node = sourceNode
	}

	// Check if a VM with the same name already exists on this node.
	if props.Name != "" {
		if existing := p.findVMByName(ctx, client, node, props.Name); existing != nil {
			propsJSON, _ := json.Marshal(existing)
			return &resource.CreateResult{
				ProgressResult: &resource.ProgressResult{
					Operation:          resource.OperationCreate,
					OperationStatus:    resource.OperationStatusSuccess,
					NativeID:           existing.ID,
					ResourceProperties: json.RawMessage(propsJSON),
				},
			}, nil
		}
	}

	vmid := props.VMID
	if vmid == 0 {
		nextID, err := client.getNextID(ctx)
		if err != nil {
			return createFailure(resource.OperationErrorCodeInternalFailure, fmt.Sprintf("getting next vmid: %v", err)), nil
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

	fullClone := true
	if props.FullClone != nil {
		fullClone = *props.FullClone
	}
	if fullClone {
		params["full"] = "1"
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
		if strings.Contains(err.Error(), "already exists") && props.VMID == 0 {
			retryID, retryErr := client.getNextID(ctx)
			if retryErr == nil {
				vmid = retryID
				params["newid"] = strconv.Itoa(vmid)
				nativeID = compositeID(node, vmid)
				data, err = client.Post(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/clone", sourceNode, sourceVMID), params)
			}
		}
		if err != nil {
			return createFailure(resource.OperationErrorCodeInternalFailure, err.Error()), nil
		}
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
		CloudInit: props.CloudInit,
		Start:     props.Start == nil || *props.Start,
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

// --- Helpers ---

// findVMByName scans non-template VMs on a node for a matching name.
func (p *Plugin) findVMByName(ctx context.Context, client *Client, node, name string) *VMProperties {
	data, err := client.Get(ctx, fmt.Sprintf("/nodes/%s/qemu", node))
	if err != nil {
		return nil
	}
	var vms []proxmoxVMListEntry
	if err := json.Unmarshal(data, &vms); err != nil {
		return nil
	}
	for _, vm := range vms {
		if vm.Template == 1 || vm.Name != name {
			continue
		}
		configData, err := client.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vm.VMID))
		if err != nil {
			continue
		}
		statusData, _ := client.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/status/current", node, vm.VMID))
		props, err := parseVMConfig(node, vm.VMID, configData, statusData)
		if err != nil {
			continue
		}
		return props
	}
	return nil
}

// --- Create→Start Status ---

func (p *Plugin) pollVMStart(ctx context.Context, client *Client, req *resource.StatusRequest) (*resource.StatusResult, error) {
	// requestID format: "vm:<step>:<upid>"
	parts := strings.SplitN(req.RequestID, ":", 3)
	if len(parts) < 3 {
		return statusFailure(resource.OperationErrorCodeInternalFailure, "invalid vm requestID"), nil
	}
	step := parts[1] // "create" or "start"
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
		errMsg := taskStatus.ErrorMessage()
		// "already running" during start is not an error
		if !(step == "start" && strings.Contains(errMsg, "already running")) {
			return statusFailure(resource.OperationErrorCodeInternalFailure, errMsg), nil
		}
	} else if step == "create" {
		node, vmid, err := parseCompositeID(req.NativeID)
		if err != nil {
			return statusFailure(resource.OperationErrorCodeInternalFailure, err.Error()), nil
		}
		// Only start if not already running (idempotent on re-poll)
		if getVMStatus(ctx, client, node, vmid) != "running" {
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
							StatusMessage:   "starting VM",
						},
					}, nil
				}
			}
		}
		// Already running or start failed — fall through to success
	}

	// step == "start" done, or start failed gracefully
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

// --- Update Stop→Resize→Start Status ---

func (p *Plugin) pollVMUpdate(ctx context.Context, client *Client, req *resource.StatusRequest) (*resource.StatusResult, error) {
	// requestID format: "vmup:<step>:<diskSize>:<upid>"
	parts := strings.SplitN(req.RequestID, ":", 4)
	if len(parts) < 4 {
		return statusFailure(resource.OperationErrorCodeInternalFailure, "invalid vmup requestID"), nil
	}
	step := parts[1] // "stop" or "start"
	diskSize, _ := strconv.Atoi(parts[2])
	upid := parts[3]

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
				StatusMessage:   fmt.Sprintf("vm update %s: task running", step),
			},
		}, nil
	}
	// Ignore stop task failure — VM might already be stopped
	if !taskStatus.IsSuccess() && step != "stop" {
		errMsg := taskStatus.ErrorMessage()
		if !strings.Contains(errMsg, "already running") {
			return statusFailure(resource.OperationErrorCodeInternalFailure, errMsg), nil
		}
	}

	node, vmid, err := parseCompositeID(req.NativeID)
	if err != nil {
		return statusFailure(resource.OperationErrorCodeInternalFailure, err.Error()), nil
	}

	if step == "stop" {
		// VM stopped — resize disk
		if diskSize > 0 {
			_, _ = client.Put(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/resize", node, vmid), map[string]string{
				"disk": "scsi0",
				"size": fmt.Sprintf("%dG", diskSize),
			})
		}
		// Start VM back up
		data, err := client.Post(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/status/start", node, vmid), nil)
		if err == nil {
			var startUpid string
			if json.Unmarshal(data, &startUpid) == nil {
				return &resource.StatusResult{
					ProgressResult: &resource.ProgressResult{
						Operation:       resource.OperationCheckStatus,
						OperationStatus: resource.OperationStatusInProgress,
						RequestID:       fmt.Sprintf("vmup:start:%d:%s", diskSize, startUpid),
						NativeID:        req.NativeID,
						StatusMessage:   "starting VM after resize",
					},
				}, nil
			}
		}
		// Start failed — fall through to success
	}

	// step == "start" done or failed — read back and return success
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

// getVMStatus returns the current status of a VM ("running", "stopped", etc.).
func getVMStatus(ctx context.Context, client *Client, node string, vmid int) string {
	data, err := client.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/status/current", node, vmid))
	if err != nil {
		return ""
	}
	var statusMap map[string]interface{}
	if json.Unmarshal(data, &statusMap) != nil {
		return ""
	}
	status, _ := statusMap["status"].(string)
	return status
}

func buildVMDiskSpec(d *DiskProperties) string {
	storage := resolveString(d.Storage)
	spec := fmt.Sprintf("%s:%d", storage, d.Size)
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

	// Agent
	if v, ok := config["agent"].(string); ok {
		b := strings.HasPrefix(v, "1")
		props.Agent = &b
	} else if v, ok := config["agent"].(float64); ok {
		b := v == 1
		props.Agent = &b
	}

	if scsi0, ok := config["scsi0"].(string); ok {
		props.Disk = parseVMDiskFromConfig(scsi0)
	}

	if net0, ok := config["net0"].(string); ok {
		props.Network = parseVMNetFromConfig(net0)
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
	if v, ok := config["cicustom"].(string); ok && v != "" {
		ci.CICustom = v
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
			// "virtio=MACADDR" pattern — extract model from key
			if len(kv[1]) == 17 && strings.Count(kv[1], ":") == 5 {
				n.Model = kv[0]
			}
		}
	}
	return n
}

// applyCloudInitParams adds cloud-init params to a config map.
func applyCloudInitParams(params map[string]string, ci *CloudInitProperties) {
	if ci.CIUser != "" {
		params["ciuser"] = ci.CIUser
	}
	if ci.CIPassword != "" {
		params["cipassword"] = ci.CIPassword
	}
	if ci.SSHKeys != "" {
		params["sshkeys"] = url.PathEscape(ci.SSHKeys)
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
	if ci.CICustom != "" {
		params["cicustom"] = ci.CICustom
	}
	if ci.CIUpgrade != nil {
		if *ci.CIUpgrade {
			params["ciupgrade"] = "1"
		} else {
			params["ciupgrade"] = "0"
		}
	}
}
