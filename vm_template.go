package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/platform-engineering-labs/formae/pkg/plugin/resource"
)

// --- List ---

func (p *Plugin) listVMTemplates(ctx context.Context, client *Client, req *resource.ListRequest) (*resource.ListResult, error) {
	nodes := []string{req.AdditionalProperties["node"]}
	if nodes[0] == "" {
		nodes = allNodeNames(ctx, client)
	}

	var ids []string
	for _, node := range nodes {
		data, err := client.Get(ctx, fmt.Sprintf("/nodes/%s/qemu", node))
		if err != nil {
			continue
		}
		var vms []proxmoxVMListEntry
		if err := json.Unmarshal(data, &vms); err != nil {
			continue
		}
		for _, vm := range vms {
			if vm.Template != 1 {
				continue
			}
			ids = append(ids, compositeID(node, vm.VMID))
		}
	}
	return &resource.ListResult{NativeIDs: ids}, nil
}

// --- Read ---

func (p *Plugin) readVMTemplate(ctx context.Context, client *Client, req *resource.ReadRequest) (*resource.ReadResult, error) {
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

	props, err := parseVMTemplateConfig(node, vmid, configData, statusData)
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

// --- Create (multi-step) ---

func (p *Plugin) createVMTemplate(ctx context.Context, client *Client, req *resource.CreateRequest) (*resource.CreateResult, error) {
	var props VMTemplateProperties
	if err := json.Unmarshal(req.Properties, &props); err != nil {
		return createFailure(resource.OperationErrorCodeInvalidRequest, fmt.Sprintf("invalid properties: %v", err)), nil
	}

	node := resolveString(props.Node)
	if node == "" {
		return createFailure(resource.OperationErrorCodeInvalidRequest, "node is required"), nil
	}

	cloudImageVolid := resolveString(props.CloudImage)
	if cloudImageVolid == "" {
		return createFailure(resource.OperationErrorCodeInvalidRequest, "cloudImage is required"), nil
	}

	if props.Disk == nil {
		return createFailure(resource.OperationErrorCodeInvalidRequest, "disk is required"), nil
	}
	diskStorage := resolveString(props.Disk.Storage)
	if diskStorage == "" {
		return createFailure(resource.OperationErrorCodeInvalidRequest, "disk.storage is required"), nil
	}

	// Check if a template with the same name already exists on this node.
	if props.Name != "" {
		if existing := p.findVMTemplateByName(ctx, client, node, props.Name); existing != nil {
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

	// Step 1: Create VM shell (no disk yet — disk will be imported in step 2 via Status)
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
	if props.CPU != "" {
		params["cpu"] = props.CPU
	}
	if props.Onboot != nil && *props.Onboot {
		params["onboot"] = "1"
	}
	if props.Network != nil {
		params["net0"] = buildVMNetSpec(props.Network)
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

	// Encode step config for the Status handler
	stepCfg := vmTemplateStepConfig{
		CloudImageVolid: cloudImageVolid,
		DiskStorage:     diskStorage,
		DiskSize:        props.Disk.Size,
		DiskCache:       props.Disk.Cache,
	}
	if props.Disk.Discard != nil && *props.Disk.Discard {
		stepCfg.DiskDiscard = true
	}
	if props.Agent != nil && *props.Agent {
		stepCfg.Agent = true
	}
	if props.CloudInit != nil {
		stepCfg.CloudInit = props.CloudInit
	}

	cfgJSON, _ := json.Marshal(stepCfg)
	cfgB64 := base64.StdEncoding.EncodeToString(cfgJSON)

	requestID := fmt.Sprintf("vmtpl:create:%s:%s", cfgB64, upid)

	return &resource.CreateResult{
		ProgressResult: &resource.ProgressResult{
			Operation:       resource.OperationCreate,
			OperationStatus: resource.OperationStatusInProgress,
			RequestID:       requestID,
			NativeID:        nativeID,
		},
	}, nil
}

// --- Update ---

func (p *Plugin) updateVMTemplate(ctx context.Context, client *Client, req *resource.UpdateRequest) (*resource.UpdateResult, error) {
	node, vmid, err := parseCompositeID(req.NativeID)
	if err != nil {
		return updateFailure(resource.OperationErrorCodeNotFound, err.Error()), nil
	}

	var desired VMTemplateProperties
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
			params["agent"] = "enabled=1"
		} else {
			params["agent"] = "enabled=0"
		}
	}
	if desired.CloudInit != nil {
		applyCloudInitParams(params, desired.CloudInit)
	}

	_, err = client.Put(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid), params)
	if err != nil {
		return updateFailure(resource.OperationErrorCodeInternalFailure, err.Error()), nil
	}

	readResult, _ := p.readVMTemplate(ctx, client, &resource.ReadRequest{
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

func (p *Plugin) deleteVMTemplate(ctx context.Context, client *Client, req *resource.DeleteRequest) (*resource.DeleteResult, error) {
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

	// Stop first (best effort — templates are usually stopped)
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

// --- Multi-step Status ---

func (p *Plugin) pollVMTemplateTask(ctx context.Context, client *Client, req *resource.StatusRequest) (*resource.StatusResult, error) {
	// requestID format: "vmtpl:<step>:<base64-config>:<upid>"
	parts := strings.SplitN(req.RequestID, ":", 4)
	if len(parts) < 4 {
		return statusFailure(resource.OperationErrorCodeInternalFailure, "invalid vmtpl requestID"), nil
	}
	step := parts[1]
	configB64 := parts[2]
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
				StatusMessage:   fmt.Sprintf("vmtpl %s: task running", step),
			},
		}, nil
	}

	if !taskStatus.IsSuccess() {
		return statusFailure(resource.OperationErrorCodeInternalFailure, taskStatus.ErrorMessage()), nil
	}

	// Decode step config (shared by create/import steps)
	var stepCfg vmTemplateStepConfig
	if configB64 != "" {
		cfgJSON, err := base64.StdEncoding.DecodeString(configB64)
		if err == nil {
			json.Unmarshal(cfgJSON, &stepCfg)
		}
	}

	node, vmid, parseErr := parseCompositeID(req.NativeID)
	if parseErr != nil {
		return statusFailure(resource.OperationErrorCodeInternalFailure, parseErr.Error()), nil
	}

	switch step {
	case "create", "import":
		// Read current VM state to decide what to do next.
		existingConfig, err := client.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid))
		if err != nil {
			return statusFailure(resource.OperationErrorCodeNetworkFailure, fmt.Sprintf("reading config: %v", err)), nil
		}
		var cfgMap map[string]interface{}
		if err := json.Unmarshal(existingConfig, &cfgMap); err != nil {
			return statusFailure(resource.OperationErrorCodeInternalFailure, fmt.Sprintf("parsing config: %v", err)), nil
		}

		_, hasScsi0 := cfgMap["scsi0"]
		lockVal, hasLock := cfgMap["lock"]

		// If VM is locked, something is running (import, resize, etc.) — just wait.
		if hasLock && lockVal != nil && lockVal != "" {
			return &resource.StatusResult{
				ProgressResult: &resource.ProgressResult{
					Operation:       resource.OperationCheckStatus,
					OperationStatus: resource.OperationStatusInProgress,
					RequestID:       req.RequestID,
					NativeID:        req.NativeID,
					StatusMessage:   fmt.Sprintf("VM locked: %v", lockVal),
				},
			}, nil
		}

		if hasScsi0 {
			// Disk imported. Apply ide2 + cloud-init params if not already done.
			if _, hasIDE2 := cfgMap["ide2"]; !hasIDE2 {
				ciParams := map[string]string{
					"ide2": stepCfg.DiskStorage + ":cloudinit",
				}
				if stepCfg.CloudInit != nil {
					applyCloudInitParams(ciParams, stepCfg.CloudInit)
				}
				// Use short timeout — the PUT may block while creating the cloudinit
				// LV. The lock-check loop will wait for completion on next poll.
				ciCtx, ciCancel := context.WithTimeout(ctx, 5*time.Second)
				defer ciCancel()
				_, _ = client.Put(ciCtx, fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid), ciParams)
				// Return in-progress — the PUT may lock the VM while creating
				// the cloudinit drive. Next poll will wait for lock to clear
				// via the hasLock check above before proceeding.
				return &resource.StatusResult{
					ProgressResult: &resource.ProgressResult{
						Operation:       resource.OperationCheckStatus,
						OperationStatus: resource.OperationStatusInProgress,
						RequestID:       req.RequestID,
						NativeID:        req.NativeID,
						StatusMessage:   "applying cloud-init configuration",
					},
				}, nil
			}
			// ide2 present and no lock — proceed to resize + template conversion.
			return p.convertToTemplate(ctx, client, req, node, vmid, &stepCfg)
		}

		// "import" step means we already submitted the POST — just wait for lock/scsi0.
		if step == "import" {
			return &resource.StatusResult{
				ProgressResult: &resource.ProgressResult{
					Operation:       resource.OperationCheckStatus,
					OperationStatus: resource.OperationStatusInProgress,
					RequestID:       req.RequestID,
					NativeID:        req.NativeID,
					StatusMessage:   "waiting for disk import to complete",
				},
			}, nil
		}

		// "create" step: fire the disk import once, then switch to "import" step.
		importCtx, importCancel := context.WithTimeout(ctx, 5*time.Second)
		defer importCancel()

		configParams := map[string]string{
			"boot": "order=scsi0",
		}
		diskSpec := fmt.Sprintf("%s:0,import-from=%s", stepCfg.DiskStorage, stepCfg.CloudImageVolid)
		if stepCfg.DiskCache != "" {
			diskSpec += ",cache=" + stepCfg.DiskCache
		}
		if stepCfg.DiskDiscard {
			diskSpec += ",discard=on"
		}
		configParams["scsi0"] = diskSpec
		// ide2 (cloudinit drive) is added separately after import completes
		// to avoid LV collision killing the entire task and dropping CI params.
		configParams["serial0"] = "socket"
		configParams["vga"] = "serial0"
		if stepCfg.Agent {
			configParams["agent"] = "enabled=1"
		}
		// Cloud-init params are applied separately after import completes,
		// together with ide2 — Proxmox's async POST auto-allocates a cloudinit
		// drive when CI params are present, causing LV collision on ide2 creation.

		// Auto-add EFI disk for UEFI boot
		if bios, ok := cfgMap["bios"].(string); ok && bios == "ovmf" {
			configParams["efidisk0"] = stepCfg.DiskStorage + ":1,efitype=4m,pre-enrolled-keys=1"
		}

		_, _ = client.Post(importCtx, fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid), configParams)

		// Switch to "import" step so subsequent polls don't re-POST.
		importRequestID := "vmtpl:import:" + configB64 + ":" + upid
		return &resource.StatusResult{
			ProgressResult: &resource.ProgressResult{
				Operation:       resource.OperationCheckStatus,
				OperationStatus: resource.OperationStatusInProgress,
				RequestID:       importRequestID,
				NativeID:        req.NativeID,
				StatusMessage:   "importing disk from cloud image",
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

	return statusFailure(resource.OperationErrorCodeInternalFailure, fmt.Sprintf("unknown vmtpl step: %s", step)), nil
}

// --- Clone Status ---

func (p *Plugin) pollCloneTask(ctx context.Context, client *Client, req *resource.StatusRequest) (*resource.StatusResult, error) {
	// requestID format: "clone:<base64-config>:<upid>"
	parts := strings.SplitN(req.RequestID, ":", 3)
	if len(parts) < 3 {
		return statusFailure(resource.OperationErrorCodeInternalFailure, "invalid clone requestID"), nil
	}
	configB64 := parts[1]
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
				StatusMessage:   "cloning in progress",
			},
		}, nil
	}

	if !taskStatus.IsSuccess() {
		return statusFailure(resource.OperationErrorCodeInternalFailure, taskStatus.ErrorMessage()), nil
	}

	// Clone done. Apply post-clone overrides.
	var stepCfg cloneStepConfig
	cfgJSON, err := base64.StdEncoding.DecodeString(configB64)
	if err != nil {
		return statusFailure(resource.OperationErrorCodeInternalFailure, fmt.Sprintf("decoding clone config: %v", err)), nil
	}
	if err := json.Unmarshal(cfgJSON, &stepCfg); err != nil {
		return statusFailure(resource.OperationErrorCodeInternalFailure, fmt.Sprintf("parsing clone config: %v", err)), nil
	}

	node, vmid, _ := parseCompositeID(req.NativeID)

	// Read current config for lock check and disk size comparison.
	existingConfig, err := client.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid))
	var cfgMap map[string]interface{}
	if err == nil {
		json.Unmarshal(existingConfig, &cfgMap)
	}

	// Wait if VM is locked (previous config/resize still running).
	if cfgMap != nil {
		lockVal, hasLock := cfgMap["lock"]
		if hasLock && lockVal != nil && lockVal != "" {
			return &resource.StatusResult{
				ProgressResult: &resource.ProgressResult{
					Operation:       resource.OperationCheckStatus,
					OperationStatus: resource.OperationStatusInProgress,
					RequestID:       req.RequestID,
					NativeID:        req.NativeID,
					StatusMessage:   fmt.Sprintf("post-clone: VM locked (%v)", lockVal),
				},
			}, nil
		}
	}

	// Apply sizing + cloud-init overrides (idempotent — PUT overwrites)
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
			configParams["agent"] = "enabled=1"
		} else {
			configParams["agent"] = "enabled=0"
		}
	}
	if stepCfg.CloudInit != nil {
		applyCloudInitParams(configParams, stepCfg.CloudInit)
	}

	if len(configParams) > 0 {
		_, _ = client.Put(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid), configParams)
	}

	// Disk resize if requested — check current size to avoid duplicate resize.
	if stepCfg.DiskSize > 0 {
		needResize := true
		if cfgMap != nil {
			if scsi0, ok := cfgMap["scsi0"].(string); ok {
				currentDisk := parseVMDiskFromConfig(scsi0)
				if currentDisk.Size >= stepCfg.DiskSize {
					needResize = false
				}
			}
		}
		if needResize {
			_, _ = client.Put(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/resize", node, vmid), map[string]string{
				"disk": "scsi0",
				"size": fmt.Sprintf("%dG", stepCfg.DiskSize),
			})
			// Return in-progress — next poll will start VM after resize completes.
			return &resource.StatusResult{
				ProgressResult: &resource.ProgressResult{
					Operation:       resource.OperationCheckStatus,
					OperationStatus: resource.OperationStatusInProgress,
					RequestID:       req.RequestID,
					NativeID:        req.NativeID,
					StatusMessage:   "resizing disk after clone",
				},
			}, nil
		}
	}

	// Start VM if requested and not already running
	if stepCfg.Start && getVMStatus(ctx, client, node, vmid) != "running" {
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
		// Start failed — fall through to success with stopped VM
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

// convertToTemplate resizes disk (if needed) and converts VM to template.
// Checks current state before each action to avoid duplicate operations.
func (p *Plugin) convertToTemplate(ctx context.Context, client *Client, req *resource.StatusRequest, node string, vmid int, stepCfg *vmTemplateStepConfig) (*resource.StatusResult, error) {
	// Check current status — is it already a template?
	statusData, err := client.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/status/current", node, vmid))
	if err == nil {
		var statusMap map[string]interface{}
		if json.Unmarshal(statusData, &statusMap) == nil {
			if tpl, ok := statusMap["template"].(float64); ok && tpl == 1 {
	
				return p.vmTemplateSuccess(ctx, req)
			}
		}
	}

	// Resize disk if requested (best effort, ignore errors)
	if stepCfg.DiskSize > 0 {

		_, _ = client.Put(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/resize", node, vmid), map[string]string{
			"disk": "scsi0",
			"size": fmt.Sprintf("%dG", stepCfg.DiskSize),
		})
	}

	// Convert to template

	data, err := client.Post(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/template", node, vmid), nil)
	if err != nil {
		if strings.Contains(err.Error(), "already a template") || strings.Contains(err.Error(), "template to a template") {

			return p.vmTemplateSuccess(ctx, req)
		}
		if isVMLockError(err) {
			return &resource.StatusResult{
				ProgressResult: &resource.ProgressResult{
					Operation:       resource.OperationCheckStatus,
					OperationStatus: resource.OperationStatusInProgress,
					RequestID:       req.RequestID,
					NativeID:        req.NativeID,
					StatusMessage:   "waiting for VM lock before template conversion",
				},
			}, nil
		}
		return statusFailure(resource.OperationErrorCodeInternalFailure, fmt.Sprintf("converting to template: %v", err)), nil
	}

	var templateUpid string
	if err := json.Unmarshal(data, &templateUpid); err != nil {
		return p.vmTemplateSuccess(ctx, req)
	}

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
}

// vmTemplateSuccess reads back the template properties and returns success.
func (p *Plugin) vmTemplateSuccess(ctx context.Context, req *resource.StatusRequest) (*resource.StatusResult, error) {
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

// --- Helpers ---

// findVMTemplateByName scans templates on a node for a matching name.
func (p *Plugin) findVMTemplateByName(ctx context.Context, client *Client, node, name string) *VMTemplateProperties {
	data, err := client.Get(ctx, fmt.Sprintf("/nodes/%s/qemu", node))
	if err != nil {
		return nil
	}
	var vms []proxmoxVMListEntry
	if err := json.Unmarshal(data, &vms); err != nil {
		return nil
	}
	for _, vm := range vms {
		if vm.Template != 1 || vm.Name != name {
			continue
		}
		configData, err := client.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vm.VMID))
		if err != nil {
			continue
		}
		statusData, _ := client.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/status/current", node, vm.VMID))
		props, err := parseVMTemplateConfig(node, vm.VMID, configData, statusData)
		if err != nil {
			continue
		}
		return props
	}
	return nil
}

// isVMLockError returns true if err indicates the VM is locked (596, "lock", "locked", "timeout" on lock file).
func isVMLockError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "596") ||
		strings.Contains(msg, "lock") ||
		strings.Contains(msg, "locked")
}

// parseVMTemplateConfig converts Proxmox API config + status responses to VMTemplateProperties.
func parseVMTemplateConfig(node string, vmid int, configData, statusData json.RawMessage) (*VMTemplateProperties, error) {
	var config map[string]interface{}
	if err := json.Unmarshal(configData, &config); err != nil {
		return nil, err
	}

	var statusMap map[string]interface{}
	if err := json.Unmarshal(statusData, &statusMap); err != nil {
		return nil, err
	}

	props := &VMTemplateProperties{
		ID:   compositeID(node, vmid),
		VMID: vmid,
	}

	if v, ok := config["name"].(string); ok {
		props.Name = v
	}
	if v, ok := config["description"].(string); ok {
		props.Description = v
	}
	if v, ok := toInt(config["memory"]); ok {
		props.Memory = v
	}
	if v, ok := toInt(config["cores"]); ok {
		props.Cores = v
	}
	if v, ok := toInt(config["sockets"]); ok {
		props.Sockets = v
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
	if v, ok := config["cpu"].(string); ok {
		props.CPU = v
	}
	if v, ok := toInt(config["onboot"]); ok {
		b := v == 1
		props.Onboot = &b
	}

	// Agent — Proxmox returns "1", "enabled=1", or "enabled=1,fstrim_cloned_disks=0" etc.
	if v, ok := config["agent"].(string); ok {
		b := v == "1" || strings.Contains(v, "enabled=1")
		props.Agent = &b
	} else if v, ok := toInt(config["agent"]); ok {
		b := v == 1
		props.Agent = &b
	}

	// Disk
	if scsi0, ok := config["scsi0"].(string); ok {
		disk := parseVMDiskFromConfig(scsi0)
		props.Disk = &VMTemplateDiskProps{
			Size:    disk.Size,
			Cache:   disk.Cache,
			Discard: disk.Discard,
		}
	}

	// Network
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
	if v, ok := toInt(config["ciupgrade"]); ok {
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
