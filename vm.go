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

	if props.Disk != nil {
		params["scsi0"] = buildVMDiskSpec(props.Disk)
	}

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

	_, err = client.Put(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid), params)
	if err != nil {
		return updateFailure(resource.OperationErrorCodeInternalFailure, err.Error()), nil
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

	if scsi0, ok := config["scsi0"].(string); ok {
		props.Disk = parseVMDiskFromConfig(scsi0)
	}

	if net0, ok := config["net0"].(string); ok {
		props.Network = parseVMNetFromConfig(net0)
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
