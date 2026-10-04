package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/durationpb"
)

const (
	maxResponseBytes = 8 << 20
	// A cold Scrob instance can legitimately take a while to answer a large
	// history page. Stay below the host's two-minute watch-sync RPC deadline
	// so transport failures are still reported as provider faults with room
	// for cleanup.
	defaultRequestTimeout = 110 * time.Second

	// apiProxyPrefix routes every call through Scrob's frontend, the only
	// port a standard deployment publishes (see docker-compose.yaml in the
	// Scrob repo). The backend's own routes live under no prefix at all, but
	// the frontend only forwards to it under /api/proxy/*; anything else
	// falls through to the frontend's own page routing, which has no session
	// cookie to show and redirects to /login. See
	// frontend/src/pages/api/proxy/[...path].ts and frontend/src/middleware.ts
	// in the Scrob repo, which documents this as the sanctioned path for an
	// API-key client with no browser session.
	apiProxyPrefix = "/api/proxy"
)

// apiClient serves one RPC; it is not shared across calls.
type apiClient struct {
	baseURL *url.URL
	apiKey  string
	http    *http.Client
}

func newAPIClient(rawBaseURL, apiKey string, httpClient *http.Client) (*apiClient, error) {
	raw := strings.TrimSpace(rawBaseURL)
	if raw != "" && !strings.Contains(raw, "://") {
		// Scrob is self-hosted: a bare host:port is overwhelmingly a LAN
		// address served over plain HTTP, not TLS, unlike a public SaaS
		// provider that is always HTTPS. Defaulting to https:// here would
		// silently turn a correct "ip:port" entry into a TLS handshake
		// failure against an http-only server.
		raw = "http://" + raw
	}
	baseURL, err := url.Parse(raw)
	if err != nil || baseURL.Host == "" || (baseURL.Scheme != "http" && baseURL.Scheme != "https") {
		return nil, errors.New("Scrob server URL must be an absolute http or https URL")
	}
	if baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, errors.New("Scrob server URL must not include credentials, a query, or a fragment")
	}
	baseURL.Path = strings.TrimRight(baseURL.Path, "/")
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultRequestTimeout}
	}
	return &apiClient{baseURL: baseURL, apiKey: strings.TrimSpace(apiKey), http: httpClient}, nil
}

func (c *apiClient) endpoint(path string, query url.Values) string {
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(c.baseURL.Path, "/") + apiProxyPrefix + "/" + strings.TrimLeft(path, "/")
	endpoint.RawQuery = query.Encode()
	return endpoint.String()
}

func (c *apiClient) get(ctx context.Context, path string, query url.Values, output any) *pluginv1.WatchSyncFault {
	_, fault := c.request(ctx, http.MethodGet, path, query, nil, output)
	return fault
}

func (c *apiClient) post(ctx context.Context, path string, payload, output any) *pluginv1.WatchSyncFault {
	_, fault := c.request(ctx, http.MethodPost, path, nil, payload, output)
	return fault
}

// postQuery is post for a route that takes query parameters, such as the
// webhook endpoint that authenticates with api_key in the query string.
func (c *apiClient) postQuery(ctx context.Context, path string, query url.Values, payload, output any) *pluginv1.WatchSyncFault {
	_, fault := c.request(ctx, http.MethodPost, path, query, payload, output)
	return fault
}

// postStatus is post for callers that must read the HTTP status, such as a
// rating Scrob rejects because it does not track the episode yet.
func (c *apiClient) postStatus(ctx context.Context, path string, payload, output any) (int, *pluginv1.WatchSyncFault) {
	return c.request(ctx, http.MethodPost, path, nil, payload, output)
}

func (c *apiClient) delete(ctx context.Context, path string, query url.Values) (int, *pluginv1.WatchSyncFault) {
	return c.request(ctx, http.MethodDelete, path, query, nil, nil)
}

// request is the shared request path. It returns the HTTP status, zero when
// no response arrived, alongside the fault.
func (c *apiClient) request(ctx context.Context, method, path string, query url.Values, payload, output any) (int, *pluginv1.WatchSyncFault) {
	if c.apiKey == "" {
		return 0, invalidCredentialFault("Scrob API key is missing; reconnect Scrob")
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return 0, permanentFault("Scrob request could not be encoded")
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path, query), body)
	if err != nil {
		return 0, permanentFault("Scrob request could not be created")
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	// The key travels as a header, never in the URL or query string, so
	// unlike providers authenticated by URL parameter there is no credential
	// to redact from a transport error.
	req.Header.Set("X-Api-Key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, temporaryFault("Scrob is temporarily unreachable")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return resp.StatusCode, faultForHTTPResponse(resp)
	}
	if output == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		return resp.StatusCode, nil
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes))
	if err := decoder.Decode(output); err != nil {
		return resp.StatusCode, temporaryFault("Scrob returned an unreadable response")
	}
	return resp.StatusCode, nil
}

func faultForHTTPResponse(resp *http.Response) *pluginv1.WatchSyncFault {
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return invalidCredentialFault("Scrob rejected the API key")
	case http.StatusTooManyRequests:
		return &pluginv1.WatchSyncFault{
			Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED,
			SafeMessage: "Scrob rate limit reached",
			RetryAfter:  durationpb.New(retryAfter(resp.Header.Get("Retry-After"))),
		}
	case http.StatusNotFound:
		// Callers that treat a miss as a no-op (clearing a rating or an
		// unwatch Scrob has nothing to undo) check the status this fault
		// came with directly; everything else surfaces it as a rejection.
		return &pluginv1.WatchSyncFault{
			Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST,
			SafeMessage: "Scrob does not have this item",
		}
	case http.StatusRequestTimeout:
		return temporaryFault("Scrob request timed out")
	case http.StatusBadRequest, http.StatusConflict, http.StatusUnprocessableEntity:
		return &pluginv1.WatchSyncFault{
			Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST,
			SafeMessage: fmt.Sprintf("Scrob rejected the request (HTTP %d)", resp.StatusCode),
		}
	default:
		if resp.StatusCode >= http.StatusInternalServerError {
			return temporaryFault("Scrob is temporarily unavailable")
		}
		return permanentFault(fmt.Sprintf("Scrob request failed (HTTP %d)", resp.StatusCode))
	}
}

func retryAfter(value string) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(0, time.Until(at))
	}
	return 0
}
