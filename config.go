package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// TargetConfig holds the Proxmox connection configuration from Pkl Config class.
type TargetConfig struct {
	URL      string `json:"url"`
	Insecure bool   `json:"insecure"`
	StartID  int    `json:"startId"`
}

func parseTargetConfig(data json.RawMessage) (*TargetConfig, error) {
	var cfg TargetConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("invalid target config: %w", err)
	}
	if cfg.URL == "" {
		return nil, fmt.Errorf("target config missing 'url'")
	}
	return &cfg, nil
}

// clientCache provides thread-safe lazy initialization of the Proxmox client.
type clientCache struct {
	mu     sync.Mutex
	client *Client
}

func (cc *clientCache) get(targetConfig json.RawMessage) (*Client, error) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.client != nil {
		return cc.client, nil
	}

	cfg, err := parseTargetConfig(targetConfig)
	if err != nil {
		return nil, err
	}

	apiToken := os.Getenv("PROXMOX_API_TOKEN")
	if apiToken == "" {
		return nil, fmt.Errorf("PROXMOX_API_TOKEN environment variable not set")
	}

	cc.client = NewClient(cfg.URL, apiToken, cfg.Insecure)
	cc.client.startID = cfg.StartID
	return cc.client, nil
}
