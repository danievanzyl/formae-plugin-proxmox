package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/platform-engineering-labs/formae/pkg/plugin/resource"
)

func (p *Plugin) pollTask(ctx context.Context, client *Client, req *resource.StatusRequest) (*resource.StatusResult, error) {
	// Multi-step dispatch based on requestID prefix
	if strings.HasPrefix(req.RequestID, "vmtpl:") {
		return p.pollVMTemplateTask(ctx, client, req)
	}
	if strings.HasPrefix(req.RequestID, "clone:") {
		return p.pollCloneTask(ctx, client, req)
	}
	if strings.HasPrefix(req.RequestID, "vmci:") {
		return p.pollVMCreateCI(ctx, client, req)
	}
	if strings.HasPrefix(req.RequestID, "vm:") {
		return p.pollVMStart(ctx, client, req)
	}
	if strings.HasPrefix(req.RequestID, "ct:") {
		return p.pollCTStart(ctx, client, req)
	}
	if strings.HasPrefix(req.RequestID, "vmup:") {
		return p.pollVMUpdate(ctx, client, req)
	}

	taskStatus, err := client.GetTaskStatus(ctx, req.RequestID)
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
				StatusMessage:   "task running",
			},
		}, nil
	}

	if taskStatus.IsSuccess() {
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

	errMsg := taskStatus.ErrorMessage()
	errCode := resource.OperationErrorCodeInternalFailure
	if strings.Contains(errMsg, "permission") || strings.Contains(errMsg, "denied") {
		errCode = resource.OperationErrorCodeAccessDenied
	}

	return &resource.StatusResult{
		ProgressResult: &resource.ProgressResult{
			Operation:       resource.OperationCheckStatus,
			OperationStatus: resource.OperationStatusFailure,
			NativeID:        req.NativeID,
			ErrorCode:       errCode,
			StatusMessage:   errMsg,
		},
	}, nil
}
