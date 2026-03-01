package main

import (
	"fmt"
	"strings"
)

// --- NativeID helpers ---

// compositeID builds a NativeID from node and vmid: "node/vmid"
func compositeID(node string, vmid int) string {
	return fmt.Sprintf("%s/%d", node, vmid)
}

// parseCompositeID splits a NativeID "node/vmid" into parts.
func parseCompositeID(nativeID string) (node string, vmid int, err error) {
	_, err = fmt.Sscanf(nativeID, "%[^/]/%d", &node, &vmid)
	if err != nil {
		return "", 0, fmt.Errorf("invalid native ID %q: expected node/vmid", nativeID)
	}
	return node, vmid, nil
}

// --- VM types ---

// VMProperties is the formae-facing properties struct for VirtualMachine.
type VMProperties struct {
	ID          string             `json:"id"`
	Node        interface{}        `json:"node"`
	VMID        int                `json:"vmid"`
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	Memory      int                `json:"memory"`
	Cores       int                `json:"cores"`
	Sockets     int                `json:"sockets"`
	OSType      string             `json:"ostype"`
	ScsiHW      string             `json:"scsihw"`
	Bios        string             `json:"bios,omitempty"`
	Machine     string             `json:"machine,omitempty"`
	Onboot      *bool              `json:"onboot,omitempty"`
	Disk        *DiskProperties    `json:"disk"`
	Network     *NetworkProperties `json:"network"`
	Status      string             `json:"status,omitempty"`
}

// DiskProperties maps to VirtualMachineDisk sub-resource.
type DiskProperties struct {
	Storage interface{} `json:"storage"`
	Size    int    `json:"size"`
	Cache   string `json:"cache,omitempty"`
	Discard *bool  `json:"discard,omitempty"`
}

// NetworkProperties maps to VirtualMachineNetwork sub-resource.
type NetworkProperties struct {
	Model    string `json:"model"`
	Bridge   string `json:"bridge"`
	Firewall *bool  `json:"firewall,omitempty"`
	Tag      *int   `json:"tag,omitempty"`
}

// --- Container types ---

// ContainerProperties is the formae-facing properties struct for Container.
type ContainerProperties struct {
	ID           string                     `json:"id"`
	Node         interface{}                `json:"node"`
	VMID         int                        `json:"vmid"`
	Hostname     string                     `json:"hostname"`
	Description  string                     `json:"description,omitempty"`
	OSTemplate   interface{}                 `json:"ostemplate"`
	Memory       int                        `json:"memory"`
	Swap         int                        `json:"swap"`
	Cores        int                        `json:"cores"`
	Unprivileged *bool                      `json:"unprivileged,omitempty"`
	Onboot       *bool                      `json:"onboot,omitempty"`
	Rootfs       *ContainerRootfsProperties `json:"rootfs"`
	Network      *ContainerNetProperties    `json:"network"`
	Password     string                     `json:"password,omitempty"`
	Status       string                     `json:"status,omitempty"`
}

// ContainerRootfsProperties maps to ContainerRootfs sub-resource.
type ContainerRootfsProperties struct {
	Storage interface{} `json:"storage"`
	Size    int    `json:"size"`
}

// ContainerNetProperties maps to ContainerNetwork sub-resource.
type ContainerNetProperties struct {
	Name     string `json:"name"`
	Bridge   string `json:"bridge"`
	IP       string `json:"ip"`
	Gateway  string `json:"gw,omitempty"`
	Firewall *bool  `json:"firewall,omitempty"`
	Tag      *int   `json:"tag,omitempty"`
}

// --- Node types ---

// NodeProperties is the formae-facing properties struct for Node.
type NodeProperties struct {
	Node    string `json:"node"`
	Status  string `json:"status"`
	MaxCPU  int    `json:"maxcpu"`
	MaxMem  int64  `json:"maxmem"`
	MaxDisk int64  `json:"maxdisk"`
}

// proxmoxNodeListEntry represents a node in the GET /nodes response.
type proxmoxNodeListEntry struct {
	Node    string  `json:"node"`
	Status  string  `json:"status"`
	MaxCPU  int     `json:"maxcpu"`
	MaxMem  int64   `json:"maxmem"`
	MaxDisk int64   `json:"maxdisk"`
	CPU     float64 `json:"cpu"`
	Mem     int64   `json:"mem"`
	Disk    int64   `json:"disk"`
	Uptime  int64   `json:"uptime"`
}

// --- Storage types ---

// StorageProperties is the formae-facing properties struct for Storage.
type StorageProperties struct {
	Storage string `json:"storage"`
	Type    string `json:"storageType"`
	Content string `json:"content,omitempty"`
	Shared  *bool  `json:"shared,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
}

// proxmoxStorageListEntry represents a storage in the GET /storage response.
type proxmoxStorageListEntry struct {
	Storage string `json:"storage"`
	Type    string `json:"type"`
	Content string `json:"content"`
	Shared  int    `json:"shared"`
	Disable int    `json:"disable"`
	Nodes   string `json:"nodes,omitempty"`
}

// --- Template types ---

// TemplateProperties is the formae-facing properties struct for Template.
type TemplateProperties struct {
	ID       string      `json:"id"`
	Node     interface{} `json:"node"`
	Storage  interface{} `json:"storage"`
	Template string      `json:"template,omitempty"`
	Volid    string      `json:"volid"`
	Size     int64       `json:"size,omitempty"`
}

// proxmoxStorageContentEntry represents an item in the GET /nodes/{node}/storage/{storage}/content response.
type proxmoxStorageContentEntry struct {
	Volid   string `json:"volid"`
	Content string `json:"content"`
	Format  string `json:"format"`
	Size    int64  `json:"size"`
}

// templateNativeID builds a NativeID from node and volid: "node/volid"
func templateNativeID(node, volid string) string {
	return node + "/" + volid
}

// parseTemplateNativeID splits a NativeID "node/storage:vztmpl/filename" into parts.
func parseTemplateNativeID(nativeID string) (node, volid, storage string, err error) {
	idx := strings.Index(nativeID, "/")
	if idx < 0 {
		return "", "", "", fmt.Errorf("invalid template ID %q", nativeID)
	}
	node = nativeID[:idx]
	volid = nativeID[idx+1:]
	colonIdx := strings.Index(volid, ":")
	if colonIdx < 0 {
		return "", "", "", fmt.Errorf("invalid volid in template ID %q", nativeID)
	}
	storage = volid[:colonIdx]
	return node, volid, storage, nil
}

// proxmoxVMListEntry represents a VM in the GET /nodes/{node}/qemu response.
type proxmoxVMListEntry struct {
	VMID   int    `json:"vmid"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// proxmoxCTListEntry represents a container in the GET /nodes/{node}/lxc response.
type proxmoxCTListEntry struct {
	VMID   int    `json:"vmid"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// resolveString extracts a string from a value that may be a string or a
// Resolvable reference ({"$ref": "...", "$value": "..."}).
func resolveString(v interface{}) string {
	switch val := v.(type) {
	case string:
		return val
	case map[string]interface{}:
		if s, ok := val["$value"].(string); ok {
			return s
		}
	}
	return ""
}
