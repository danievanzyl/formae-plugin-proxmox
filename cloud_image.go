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
		contentData, err := client.Get(ctx, fmt.Sprintf("/nodes/%s/storage/%s/content?content=import", node, s.Storage))
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
			propsJSON, err := json.Marshal(props)
			if err != nil {
				return &resource.ReadResult{ResourceType: req.ResourceType, ErrorCode: resource.OperationErrorCodeInternalFailure}, nil
			}
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
