// Package ha is a minimal Home Assistant REST client: it only calls
// services. Entity resolution (friendly name -> entity_id) is deliberately
// not here — Miranda's orchestrator resolves devices and hands this service
// a concrete entity_id.
package ha

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client calls Home Assistant's REST API with a long-lived access token.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New builds a Client against baseURL (e.g. "http://homeassistant.local:8123").
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		// assist_satellite.announce blocks until the satellite finishes
		// speaking, so the timeout must outlast a long announcement.
		http: &http.Client{Timeout: 2 * time.Minute},
	}
}

// CallService invokes domain.service with data as the JSON body.
func (c *Client) CallService(ctx context.Context, domain, service string, data map[string]any) error {
	body, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("ha: marshal service call body: %w", err)
	}

	url := fmt.Sprintf("%s/api/services/%s/%s", c.baseURL, domain, service)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("ha: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ha: call %s.%s: %w", domain, service, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ha: call %s.%s: status %d: %s", domain, service, resp.StatusCode, string(respBody))
	}
	return nil
}
