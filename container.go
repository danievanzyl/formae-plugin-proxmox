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

	// Check if a container with the same hostname already exists on this node.
	if props.Hostname != "" {
		if existing := p.findContainerByHostname(ctx, client, node, props.Hostname); existing != nil {
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
		"vmid":       strconv.Itoa(vmid),
		"hostname":   props.Hostname,
		"ostemplate": resolveString(props.OSTemplate),
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

	if props.Rootfs != nil {
		params["rootfs"] = fmt.Sprintf("%s:%d", resolveString(props.Rootfs.Storage), props.Rootfs.Size)
	}

	if props.Network != nil {
		params["net0"] = buildContainerNetSpec(props.Network)
	}

	nativeID := compositeID(node, vmid)

	data, err := client.Post(ctx, fmt.Sprintf("/nodes/%s/lxc", node), params)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			// If VMID was auto-assigned, retry once with a new ID (race with concurrent creates)
			if props.VMID == 0 {
				retryID, retryErr := client.getNextID(ctx)
				if retryErr == nil {
					vmid = retryID
					params["vmid"] = strconv.Itoa(vmid)
					nativeID = compositeID(node, vmid)
					data, err = client.Post(ctx, fmt.Sprintf("/nodes/%s/lxc", node), params)
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
		requestID = "ct:create:" + upid
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

	params := map[string]string{
		"memory": strconv.Itoa(desired.Memory),
		"swap":   strconv.Itoa(desired.Swap),
		"cores":  strconv.Itoa(desired.Cores),
	}

	if desired.Hostname != "" {
		params["hostname"] = desired.Hostname
	}
	params["description"] = desired.Description
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

// --- Create→Start Status ---

func (p *Plugin) pollCTStart(ctx context.Context, client *Client, req *resource.StatusRequest) (*resource.StatusResult, error) {
	// requestID format: "ct:<step>:<upid>"
	parts := strings.SplitN(req.RequestID, ":", 3)
	if len(parts) < 3 {
		return statusFailure(resource.OperationErrorCodeInternalFailure, "invalid ct requestID"), nil
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
				StatusMessage:   fmt.Sprintf("ct %s: task running", step),
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
		if getCTStatus(ctx, client, node, vmid) != "running" {
			data, err := client.Post(ctx, fmt.Sprintf("/nodes/%s/lxc/%d/status/start", node, vmid), nil)
			if err == nil {
				var startUpid string
				if json.Unmarshal(data, &startUpid) == nil {
					return &resource.StatusResult{
						ProgressResult: &resource.ProgressResult{
							Operation:       resource.OperationCheckStatus,
							OperationStatus: resource.OperationStatusInProgress,
							RequestID:       "ct:start:" + startUpid,
							NativeID:        req.NativeID,
							StatusMessage:   "starting container",
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

// --- Helpers ---

// getCTStatus returns the current status of a container ("running", "stopped", etc.).
func getCTStatus(ctx context.Context, client *Client, node string, vmid int) string {
	data, err := client.Get(ctx, fmt.Sprintf("/nodes/%s/lxc/%d/status/current", node, vmid))
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
	if v, ok := toInt(config["memory"]); ok {
		props.Memory = v
	}
	if v, ok := toInt(config["swap"]); ok {
		props.Swap = v
	}
	if v, ok := toInt(config["cores"]); ok {
		props.Cores = v
	}
	if v, ok := toInt(config["unprivileged"]); ok {
		b := v == 1
		props.Unprivileged = &b
	}
	if v, ok := toInt(config["onboot"]); ok {
		b := v == 1
		props.Onboot = &b
	}

	if rootfs, ok := config["rootfs"].(string); ok {
		props.Rootfs = parseContainerRootfs(rootfs)
	}

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

// findContainerByHostname scans existing containers on a node for a matching hostname.
func (p *Plugin) findContainerByHostname(ctx context.Context, client *Client, node, hostname string) *ContainerProperties {
	data, err := client.Get(ctx, fmt.Sprintf("/nodes/%s/lxc", node))
	if err != nil {
		return nil
	}
	var cts []proxmoxCTListEntry
	if err := json.Unmarshal(data, &cts); err != nil {
		return nil
	}
	for _, ct := range cts {
		configData, err := client.Get(ctx, fmt.Sprintf("/nodes/%s/lxc/%d/config", node, ct.VMID))
		if err != nil {
			continue
		}
		var config map[string]interface{}
		if err := json.Unmarshal(configData, &config); err != nil {
			continue
		}
		if h, ok := config["hostname"].(string); ok && h == hostname {
			statusData, _ := client.Get(ctx, fmt.Sprintf("/nodes/%s/lxc/%d/status/current", node, ct.VMID))
			props, err := parseContainerConfig(node, ct.VMID, configData, statusData)
			if err != nil {
				continue
			}
			return props
		}
	}
	return nil
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
