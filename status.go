package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/platform-engineering-labs/formae/pkg/plugin/resource"
)

func (p *Plugin) pollTask(ctx context.Context, client *Client, req *resource.StatusRequest) (*resource.StatusResult, error) {
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
