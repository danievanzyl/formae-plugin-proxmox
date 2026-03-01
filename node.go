package main

import (
	"context"
	"encoding/json"

	"github.com/platform-engineering-labs/formae/pkg/plugin/resource"
)

func (p *Plugin) listNodes(ctx context.Context, client *Client, req *resource.ListRequest) (*resource.ListResult, error) {
	data, err := client.Get(ctx, "/nodes")
	if err != nil {
		return &resource.ListResult{NativeIDs: []string{}}, nil
	}

	var nodes []proxmoxNodeListEntry
	if err := json.Unmarshal(data, &nodes); err != nil {
		return &resource.ListResult{NativeIDs: []string{}}, nil
	}

	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.Node)
	}
	return &resource.ListResult{NativeIDs: ids}, nil
}

func (p *Plugin) readNode(ctx context.Context, client *Client, req *resource.ReadRequest) (*resource.ReadResult, error) {
	data, err := client.Get(ctx, "/nodes")
	if err != nil {
		return &resource.ReadResult{
			ResourceType: req.ResourceType,
			ErrorCode:    resource.OperationErrorCodeNetworkFailure,
		}, nil
	}

	var nodes []proxmoxNodeListEntry
	if err := json.Unmarshal(data, &nodes); err != nil {
		return &resource.ReadResult{
			ResourceType: req.ResourceType,
			ErrorCode:    resource.OperationErrorCodeInternalFailure,
		}, nil
	}

	for _, n := range nodes {
		if n.Node == req.NativeID {
			props := NodeProperties{
				Node:    n.Node,
				Status:  n.Status,
				MaxCPU:  n.MaxCPU,
				MaxMem:  n.MaxMem,
				MaxDisk: n.MaxDisk,
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
