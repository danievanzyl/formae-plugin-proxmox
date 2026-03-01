package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// TaskStatus represents the status of a Proxmox async task.
type TaskStatus struct {
	Status     string `json:"status"`     // "running" or "stopped"
	ExitStatus string `json:"exitstatus"` // "OK" or "ERROR: ..." (only when stopped)
	Node       string `json:"node"`
	ID         string `json:"id"`
	Type       string `json:"type"`
	UPID       string `json:"upid"`
}

// IsRunning returns true if the task is still in progress.
func (t *TaskStatus) IsRunning() bool {
	return t.Status == "running"
}

// IsSuccess returns true if the task completed successfully.
func (t *TaskStatus) IsSuccess() bool {
	return t.Status == "stopped" && t.ExitStatus == "OK"
}

// ErrorMessage returns the error message if the task failed.
func (t *TaskStatus) ErrorMessage() string {
	if t.Status == "stopped" && t.ExitStatus != "OK" {
		return t.ExitStatus
	}
	return ""
}

// nodeFromUPID extracts the node name from a UPID string.
// UPID format: UPID:<node>:<pid>:<pstart>:<starttime>:<type>:<id>:<user>:
func nodeFromUPID(upid string) (string, error) {
	parts := strings.Split(upid, ":")
	if len(parts) < 3 || parts[0] != "UPID" {
		return "", fmt.Errorf("invalid UPID: %s", upid)
	}
	return parts[1], nil
}

// GetTaskStatus checks the status of an async task by UPID.
func (c *Client) GetTaskStatus(ctx context.Context, upid string) (*TaskStatus, error) {
	node, err := nodeFromUPID(upid)
	if err != nil {
		return nil, err
	}

	data, err := c.Get(ctx, fmt.Sprintf("/nodes/%s/tasks/%s/status", node, upid))
	if err != nil {
		return nil, fmt.Errorf("getting task status: %w", err)
	}

	var status TaskStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return nil, fmt.Errorf("parsing task status: %w", err)
	}
	return &status, nil
}
