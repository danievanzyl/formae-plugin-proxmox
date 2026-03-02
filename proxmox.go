package main

import (
	"context"
	"fmt"

	"github.com/platform-engineering-labs/formae/pkg/plugin"
	"github.com/platform-engineering-labs/formae/pkg/plugin/resource"
)

const (
	ResourceTypeNode       = "PROXMOX::Node::Node"
	ResourceTypeStorage    = "PROXMOX::Storage::Storage"
	ResourceTypeTemplate   = "PROXMOX::Container::Template"
	ResourceTypeVM         = "PROXMOX::Compute::VirtualMachine"
	ResourceTypeContainer  = "PROXMOX::Compute::Container"
	ResourceTypeCloudImage = "PROXMOX::Image::CloudImage"
	ResourceTypeVMTemplate = "PROXMOX::Compute::VMTemplate"
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
			ResourceTypeStorage:    "$.storage",
			ResourceTypeTemplate:   "$.volid",
			ResourceTypeVM:         "$.name",
			ResourceTypeContainer:  "$.hostname",
			ResourceTypeCloudImage: "$.volid",
			ResourceTypeVMTemplate: "$.name",
		},
	}
}

func (p *Plugin) Create(ctx context.Context, req *resource.CreateRequest) (result *resource.CreateResult, retErr error) {
	defer func() {
		if r := recover(); r != nil {
			result = createFailure(resource.OperationErrorCodeInternalFailure, fmt.Sprintf("panic: %v", r))
			retErr = nil
		}
	}()
	client, err := p.clients.get(req.TargetConfig)
	if err != nil {
		return createFailure(resource.OperationErrorCodeInvalidCredentials, err.Error()), nil
	}
	switch req.ResourceType {
	case ResourceTypeNode:
		return createFailure(resource.OperationErrorCodeInvalidRequest, "nodes are read-only and cannot be created"), nil
	case ResourceTypeStorage:
		return createFailure(resource.OperationErrorCodeInvalidRequest, "storage is read-only and cannot be created"), nil
	case ResourceTypeTemplate:
		return p.createTemplate(ctx, client, req)
	case ResourceTypeVM:
		return p.createVM(ctx, client, req)
	case ResourceTypeContainer:
		return p.createContainer(ctx, client, req)
	case ResourceTypeCloudImage:
		return p.createCloudImage(ctx, client, req)
	case ResourceTypeVMTemplate:
		return p.createVMTemplate(ctx, client, req)
	default:
		return createFailure(resource.OperationErrorCodeInvalidRequest, fmt.Sprintf("unknown resource type: %s", req.ResourceType)), nil
	}
}

func (p *Plugin) Read(ctx context.Context, req *resource.ReadRequest) (result *resource.ReadResult, retErr error) {
	defer func() {
		if r := recover(); r != nil {
			result = &resource.ReadResult{ResourceType: req.ResourceType, ErrorCode: resource.OperationErrorCodeInternalFailure}
			retErr = nil
		}
	}()
	client, err := p.clients.get(req.TargetConfig)
	if err != nil {
		return &resource.ReadResult{ResourceType: req.ResourceType, ErrorCode: resource.OperationErrorCodeInvalidCredentials}, nil
	}
	switch req.ResourceType {
	case ResourceTypeNode:
		return p.readNode(ctx, client, req)
	case ResourceTypeStorage:
		return p.readStorage(ctx, client, req)
	case ResourceTypeTemplate:
		return p.readTemplate(ctx, client, req)
	case ResourceTypeVM:
		return p.readVM(ctx, client, req)
	case ResourceTypeContainer:
		return p.readContainer(ctx, client, req)
	case ResourceTypeCloudImage:
		return p.readCloudImage(ctx, client, req)
	case ResourceTypeVMTemplate:
		return p.readVMTemplate(ctx, client, req)
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
	case ResourceTypeStorage:
		return updateFailure(resource.OperationErrorCodeInvalidRequest, "storage is read-only and cannot be updated"), nil
	case ResourceTypeTemplate:
		return updateFailure(resource.OperationErrorCodeInvalidRequest, "templates are immutable and cannot be updated"), nil
	case ResourceTypeVM:
		return p.updateVM(ctx, client, req)
	case ResourceTypeContainer:
		return p.updateContainer(ctx, client, req)
	case ResourceTypeCloudImage:
		return updateFailure(resource.OperationErrorCodeInvalidRequest, "cloud images are immutable"), nil
	case ResourceTypeVMTemplate:
		return p.updateVMTemplate(ctx, client, req)
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
	case ResourceTypeStorage:
		return deleteFailure(resource.OperationErrorCodeInvalidRequest, "storage is read-only and cannot be deleted"), nil
	case ResourceTypeTemplate:
		return p.deleteTemplate(ctx, client, req)
	case ResourceTypeVM:
		return p.deleteVM(ctx, client, req)
	case ResourceTypeContainer:
		return p.deleteContainer(ctx, client, req)
	case ResourceTypeCloudImage:
		return p.deleteCloudImage(ctx, client, req)
	case ResourceTypeVMTemplate:
		return p.deleteVMTemplate(ctx, client, req)
	default:
		return deleteFailure(resource.OperationErrorCodeInvalidRequest, fmt.Sprintf("unknown resource type: %s", req.ResourceType)), nil
	}
}

func (p *Plugin) Status(ctx context.Context, req *resource.StatusRequest) (result *resource.StatusResult, retErr error) {
	defer func() {
		if r := recover(); r != nil {
			result = statusFailure(resource.OperationErrorCodeInternalFailure, fmt.Sprintf("panic: %v", r))
			retErr = nil
		}
	}()
	client, err := p.clients.get(req.TargetConfig)
	if err != nil {
		return statusFailure(resource.OperationErrorCodeInvalidCredentials, err.Error()), nil
	}
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
	case ResourceTypeStorage:
		return p.listStorage(ctx, client, req)
	case ResourceTypeTemplate:
		return p.listTemplates(ctx, client, req)
	case ResourceTypeVM:
		return p.listVMs(ctx, client, req)
	case ResourceTypeContainer:
		return p.listContainers(ctx, client, req)
	case ResourceTypeCloudImage:
		return p.listCloudImages(ctx, client, req)
	case ResourceTypeVMTemplate:
		return p.listVMTemplates(ctx, client, req)
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
