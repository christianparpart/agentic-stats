package wire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrUnauthorized reports that the device token was rejected. Retrying will
// not help; the device must be re-enrolled.
var ErrUnauthorized = errors.New("wire: device token rejected")

// ErrServerBusy reports a transient server-side condition worth retrying.
var ErrServerBusy = errors.New("wire: server temporarily unavailable")

// ingestPath is the ingest endpoint, relative to the configured base URL.
const ingestPath = "/v1/ingest"

// ClientConfig is everything a Client needs.
type ClientConfig struct {
	// BaseURL is the server root, e.g. https://stats.example.com. Required.
	BaseURL string
	// Token authenticates this device. Required.
	Token string
	// Device describes the originating machine. Required.
	Device Device
	// HTTPClient performs requests. Zero selects a client with a sane timeout.
	HTTPClient *http.Client
}

// Client uploads batches to the ingest API.
type Client struct {
	baseURL string
	token   string
	device  Device
	http    *http.Client
}

// NewClient returns a Client for the configured server.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("wire: ClientConfig.BaseURL is required")
	}
	if _, err := url.Parse(cfg.BaseURL); err != nil {
		return nil, fmt.Errorf("wire: invalid BaseURL: %w", err)
	}
	if cfg.Token == "" {
		return nil, errors.New("wire: ClientConfig.Token is required")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute}
	}
	return &Client{
		baseURL: strings.TrimSuffix(cfg.BaseURL, "/"),
		token:   cfg.Token,
		device:  cfg.Device,
		http:    hc,
	}, nil
}

// Ingest uploads records and reports what the server did with them.
func (c *Client) Ingest(ctx context.Context, records []Record) (IngestResult, error) {
	if len(records) == 0 {
		return IngestResult{}, nil
	}

	var body bytes.Buffer
	if err := EncodeBatch(&body, Header{Device: c.device}, records); err != nil {
		return IngestResult{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+ingestPath, &body)
	if err != nil {
		return IngestResult{}, fmt.Errorf("wire: build request: %w", err)
	}
	req.Header.Set("Content-Type", ContentType)
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return IngestResult{}, fmt.Errorf("wire: post batch: %w", err)
	}
	// The response has been read by the time this runs; a close failure cannot
	// change the outcome, only leak a connection, which the transport handles.
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return IngestResult{}, ErrUnauthorized
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return IngestResult{}, fmt.Errorf("%w: status %d", ErrServerBusy, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		// A failure reading the detail must not mask the status code, which is
		// the actionable part; report whatever detail was recovered.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return IngestResult{}, fmt.Errorf("wire: ingest failed: status %d: %s",
			resp.StatusCode, strings.TrimSpace(string(detail)))
	}

	var result IngestResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return IngestResult{}, fmt.Errorf("wire: decode response: %w", err)
	}
	return result, nil
}
