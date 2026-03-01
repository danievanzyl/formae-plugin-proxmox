package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/platform-engineering-labs/formae/pkg/plugin/resource"
)

func (p *Plugin) listTemplates(ctx context.Context, client *Client, req *resource.ListRequest) (*resource.ListResult, error) {
	node, ok := req.AdditionalProperties["node"]
	if !ok || node == "" {
		return &resource.ListResult{NativeIDs: []string{}}, nil
	}

	// Get all storages, filter those supporting vztmpl content.
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
		if !strings.Contains(s.Content, "vztmpl") {
			continue
		}
		contentData, err := client.Get(ctx, fmt.Sprintf("/nodes/%s/storage/%s/content?content=vztmpl", node, s.Storage))
		if err != nil {
			continue
		}
		var entries []proxmoxStorageContentEntry
		if err := json.Unmarshal(contentData, &entries); err != nil {
			continue
		}
		for _, e := range entries {
			ids = append(ids, templateNativeID(node, e.Volid))
		}
	}

	if ids == nil {
		ids = []string{}
	}
	return &resource.ListResult{NativeIDs: ids}, nil
}

func (p *Plugin) readTemplate(ctx context.Context, client *Client, req *resource.ReadRequest) (*resource.ReadResult, error) {
	node, volid, storage, err := parseTemplateNativeID(req.NativeID)
	if err != nil {
		return &resource.ReadResult{ResourceType: req.ResourceType, ErrorCode: resource.OperationErrorCodeNotFound}, nil
	}

	contentData, err := client.Get(ctx, fmt.Sprintf("/nodes/%s/storage/%s/content?content=vztmpl", node, storage))
	if err != nil {
		return &resource.ReadResult{
			ResourceType: req.ResourceType,
			ErrorCode:    resource.OperationErrorCodeNetworkFailure,
		}, nil
	}

	var entries []proxmoxStorageContentEntry
	if err := json.Unmarshal(contentData, &entries); err != nil {
		return &resource.ReadResult{
			ResourceType: req.ResourceType,
			ErrorCode:    resource.OperationErrorCodeInternalFailure,
		}, nil
	}

	for _, e := range entries {
		if e.Volid == volid {
			// Extract template filename from volid: "local:vztmpl/filename" → "filename"
			templateName := volid
			if idx := strings.Index(volid, "vztmpl/"); idx >= 0 {
				templateName = volid[idx+len("vztmpl/"):]
			}

			props := TemplateProperties{
				ID:       req.NativeID,
				Node:     node,
				Storage:  storage,
				Template: templateName,
				Volid:    volid,
				Size:     e.Size,
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

func (p *Plugin) createTemplate(ctx context.Context, client *Client, req *resource.CreateRequest) (*resource.CreateResult, error) {
	var props TemplateProperties
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

	if props.Template == "" {
		return createFailure(resource.OperationErrorCodeInvalidRequest, "template is required"), nil
	}

	// Download template from Proxmox appliance index.
	data, err := client.Post(ctx, fmt.Sprintf("/nodes/%s/aplinfo", node), map[string]string{
		"storage":  storage,
		"template": props.Template,
	})
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

	volid := storage + ":vztmpl/" + props.Template
	nativeID := templateNativeID(node, volid)

	return &resource.CreateResult{
		ProgressResult: &resource.ProgressResult{
			Operation:       resource.OperationCreate,
			OperationStatus: resource.OperationStatusInProgress,
			RequestID:       upid,
			NativeID:        nativeID,
		},
	}, nil
}

func (p *Plugin) deleteTemplate(ctx context.Context, client *Client, req *resource.DeleteRequest) (*resource.DeleteResult, error) {
	node, volid, storage, err := parseTemplateNativeID(req.NativeID)
	if err != nil {
		return &resource.DeleteResult{
			ProgressResult: &resource.ProgressResult{
				Operation:       resource.OperationDelete,
				OperationStatus: resource.OperationStatusSuccess,
				NativeID:        req.NativeID,
			},
		}, nil
	}

	// Extract volume path from volid: "local:vztmpl/filename" → "vztmpl/filename"
	colonIdx := strings.Index(volid, ":")
	volumePath := volid[colonIdx+1:]

	data, err := client.Delete(ctx, fmt.Sprintf("/nodes/%s/storage/%s/content/%s", node, storage, url.PathEscape(volumePath)), nil)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "not exist") || strings.Contains(err.Error(), "no such") {
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
		// Some deletes complete immediately (no UPID returned).
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
