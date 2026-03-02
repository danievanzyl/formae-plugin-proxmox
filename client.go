package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client is a lightweight Proxmox VE API client.
type Client struct {
	baseURL    string
	apiToken   string
	httpClient *http.Client
	startID    int
}

// apiResponse is the standard Proxmox API response envelope.
type apiResponse struct {
	Data   json.RawMessage   `json:"data"`
	Errors map[string]string `json:"errors,omitempty"`
}

// NewClient creates a new Proxmox API client.
func NewClient(baseURL, apiToken string, insecure bool) *Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: insecure, //nolint:gosec // user-configured for self-signed certs
		},
	}
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		apiToken: apiToken,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
	}
}

// Get performs a GET request and returns the unwrapped data field.
func (c *Client) Get(ctx context.Context, path string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL(path), nil)
	if err != nil {
		return nil, err
	}
	return c.do(req)
}

// Post performs a POST request with form-encoded params, returns the data field.
func (c *Client) Post(ctx context.Context, path string, params map[string]string) (json.RawMessage, error) {
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	var body io.Reader
	if len(params) > 0 {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL(path), body)
	if err != nil {
		return nil, err
	}
	if len(params) > 0 {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return c.do(req)
}

// Put performs a PUT request with form-encoded params.
func (c *Client) Put(ctx context.Context, path string, params map[string]string) (json.RawMessage, error) {
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	var body io.Reader
	if len(params) > 0 {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.apiURL(path), body)
	if err != nil {
		return nil, err
	}
	if len(params) > 0 {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return c.do(req)
}

// Delete performs a DELETE request with params as query string, returns the data field.
func (c *Client) Delete(ctx context.Context, path string, params map[string]string) (json.RawMessage, error) {
	u := c.apiURL(path)
	if len(params) > 0 {
		q := url.Values{}
		for k, v := range params {
			q.Set(k, v)
		}
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return nil, err
	}
	return c.do(req)
}

func (c *Client) apiURL(path string) string {
	return c.baseURL + "/api2/json" + path
}

func (c *Client) do(req *http.Request) (json.RawMessage, error) {
	req.Header.Set("Authorization", "PVEAPIToken="+c.apiToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp apiResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	if len(apiResp.Errors) > 0 {
		return nil, fmt.Errorf("API errors: %v", apiResp.Errors)
	}

	return apiResp.Data, nil
}

// getNextID returns the next available VMID, respecting the configured startID.
func (c *Client) getNextID(ctx context.Context) (int, error) {
	if c.startID <= 0 {
		// No minimum — use Proxmox default
		data, err := c.Get(ctx, "/cluster/nextid")
		if err != nil {
			return 0, err
		}
		var idStr string
		if err := json.Unmarshal(data, &idStr); err != nil {
			return 0, fmt.Errorf("parsing nextid: %w", err)
		}
		return strconv.Atoi(idStr)
	}

	// Get all existing VMIDs from the cluster
	data, err := c.Get(ctx, "/cluster/resources?type=vm")
	if err != nil {
		return 0, fmt.Errorf("listing cluster resources: %w", err)
	}

	var resources []struct {
		VMID int `json:"vmid"`
	}
	if err := json.Unmarshal(data, &resources); err != nil {
		return 0, fmt.Errorf("parsing cluster resources: %w", err)
	}

	used := make(map[int]bool, len(resources))
	for _, r := range resources {
		used[r.VMID] = true
	}

	for id := c.startID; id < 1000000000; id++ {
		if !used[id] {
			return id, nil
		}
	}

	return 0, fmt.Errorf("no free VMID >= %d", c.startID)
}
