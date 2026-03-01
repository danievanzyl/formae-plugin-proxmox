package main

import (
	"context"
	"encoding/json"

	"github.com/platform-engineering-labs/formae/pkg/plugin/resource"
)

func (p *Plugin) listStorage(ctx context.Context, client *Client, req *resource.ListRequest) (*resource.ListResult, error) {
	data, err := client.Get(ctx, "/storage")
	if err != nil {
		return &resource.ListResult{NativeIDs: []string{}}, nil
	}

	var storages []proxmoxStorageListEntry
	if err := json.Unmarshal(data, &storages); err != nil {
		return &resource.ListResult{NativeIDs: []string{}}, nil
	}

	ids := make([]string, 0, len(storages))
	for _, s := range storages {
		ids = append(ids, s.Storage)
	}
	return &resource.ListResult{NativeIDs: ids}, nil
}

func (p *Plugin) readStorage(ctx context.Context, client *Client, req *resource.ReadRequest) (*resource.ReadResult, error) {
	data, err := client.Get(ctx, "/storage")
	if err != nil {
		return &resource.ReadResult{
			ResourceType: req.ResourceType,
			ErrorCode:    resource.OperationErrorCodeNetworkFailure,
		}, nil
	}

	var storages []proxmoxStorageListEntry
	if err := json.Unmarshal(data, &storages); err != nil {
		return &resource.ReadResult{
			ResourceType: req.ResourceType,
			ErrorCode:    resource.OperationErrorCodeInternalFailure,
		}, nil
	}

	for _, s := range storages {
		if s.Storage == req.NativeID {
			shared := s.Shared == 1
			enabled := s.Disable == 0
			props := StorageProperties{
				Storage: s.Storage,
				Type:    s.Type,
				Content: s.Content,
				Shared:  &shared,
				Enabled: &enabled,
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
