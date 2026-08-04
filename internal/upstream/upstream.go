package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"codeagentrouter/internal/model"
	"codeagentrouter/internal/store"
)

// Client forwards OpenAI-protocol requests to upstream providers.
type Client struct {
	store        *store.Store
	jsonClient   *http.Client
	streamClient *http.Client
	modelsClient *http.Client
}

func New(st *store.Store) *Client {
	return &Client{
		store:        st,
		jsonClient:   &http.Client{Timeout: 5 * time.Minute},
		streamClient: &http.Client{},
		modelsClient: &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *Client) Do(ctx context.Context, key *model.UpstreamKey, method, path string, body []byte, stream bool) (*http.Response, error) {
	apiKey, err := c.store.DecryptAPIKey(key)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, joinURL(key.BaseURL, path), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	client := c.jsonClient
	if stream {
		client = c.streamClient
	}
	return client.Do(req)
}

// Models fetches and parses the upstream /v1/models list.
func (c *Client) Models(ctx context.Context, key *model.UpstreamKey) ([]map[string]any, error) {
	apiKey, err := c.store.DecryptAPIKey(key)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, joinURL(key.BaseURL, "/v1/models"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := c.modelsClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("upstream models returned %d: %s", resp.StatusCode, string(body))
	}
	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.Data, nil
}

func joinURL(base, path string) string {
	return strings.TrimRight(base, "/") + path
}
