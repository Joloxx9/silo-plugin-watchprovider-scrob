// Package provider implements the Scrob watch-sync provider. Scrob
// (https://github.com/ellite/scrob) is self-hosted, so unlike a public SaaS
// provider each connection supplies its own server URL alongside the API
// key, captured through the plugin's connection config schema
// ("scrob_server"/"server_url" in manifest.json) and returned to the host as
// part of WatchSyncCredentials.SecretAttributes, which the host persists for
// every plugin-sourced connection.
package provider

import (
	"context"
	"net/http"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

const (
	capabilityID  = "scrob"
	configBaseURL = "scrob_server.server_url"

	// ApplyEvents and a ListRemoteState page stop starting new work after
	// this long, or sooner when the call's deadline, less
	// syncDeadlineMargin, comes first, so the host receives the finished
	// results before its RPC deadline.
	syncBudget         = 90 * time.Second
	syncDeadlineMargin = 5 * time.Second
)

type Server struct {
	pluginv1.UnimplementedWatchSyncProviderServer
	http *http.Client
	now  func() time.Time
}

func NewServer(httpClient *http.Client) *Server {
	return &Server{http: httpClient, now: time.Now}
}

func (s *Server) ExchangeAPIKey(ctx context.Context, req *pluginv1.WatchSyncExchangeAPIKeyRequest) (*pluginv1.WatchSyncCredentialResponse, error) {
	client, fault := s.client(req.GetCapabilityId(), req.GetProviderConfig(), req.GetApiKey(), "")
	if fault != nil {
		return &pluginv1.WatchSyncCredentialResponse{Fault: fault}, nil
	}
	account, fault := validateAccount(ctx, client)
	if fault != nil {
		return &pluginv1.WatchSyncCredentialResponse{Fault: fault}, nil
	}
	return &pluginv1.WatchSyncCredentialResponse{
		Credentials: credentials(req.GetApiKey(), client.baseURL.String()),
		Account:     account,
	}, nil
}

// RefreshCredentials returns the stored credentials unchanged: Scrob API keys
// do not expire on their own.
func (s *Server) RefreshCredentials(ctx context.Context, req *pluginv1.WatchSyncRefreshCredentialsRequest) (*pluginv1.WatchSyncCredentialResponse, error) {
	client, fault := s.authenticatedClient(req.GetContext())
	if fault != nil {
		return &pluginv1.WatchSyncCredentialResponse{Fault: fault}, nil
	}
	account, fault := validateAccount(ctx, client)
	if fault != nil {
		return &pluginv1.WatchSyncCredentialResponse{Fault: fault}, nil
	}
	return &pluginv1.WatchSyncCredentialResponse{
		Credentials: cloneCredentials(req.GetContext().GetCredentials()),
		Account:     account,
	}, nil
}

func (s *Server) GetAccount(ctx context.Context, req *pluginv1.WatchSyncGetAccountRequest) (*pluginv1.WatchSyncGetAccountResponse, error) {
	client, fault := s.authenticatedClient(req.GetContext())
	if fault != nil {
		return &pluginv1.WatchSyncGetAccountResponse{Fault: fault}, nil
	}
	account, fault := validateAccount(ctx, client)
	return &pluginv1.WatchSyncGetAccountResponse{Account: account, Fault: fault}, nil
}

func (s *Server) ListRemoteState(ctx context.Context, req *pluginv1.WatchSyncListRemoteStateRequest) (*pluginv1.WatchSyncListRemoteStateResponse, error) {
	client, fault := s.authenticatedClient(req.GetContext())
	if fault != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: fault}, nil
	}
	kind, fault := requestedStateKind(req.GetStateKinds())
	if fault != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: fault}, nil
	}
	switch kind {
	case pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHED:
		return s.listWatched(ctx, client, req)
	case pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_PROGRESS:
		return s.listProgress(ctx, client)
	case pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_RATING:
		return s.listRatings(ctx, client)
	default:
		return &pluginv1.WatchSyncListRemoteStateResponse{
			Fault: invalidRequestFault("Scrob does not support the requested state family"),
		}, nil
	}
}

// ApplyEvents applies each run of consecutive events that share an operation
// as one group. Scrob has no bulk write endpoint for watched history or
// ratings, so a group still costs one request per event, but grouping keeps
// the time-box and rate-limit bookkeeping in one place, matching Silo's other
// plugin providers.
func (s *Server) ApplyEvents(ctx context.Context, req *pluginv1.WatchSyncApplyEventsRequest) (*pluginv1.WatchSyncApplyEventsResponse, error) {
	client, fault := s.authenticatedClient(req.GetContext())
	if fault != nil {
		return &pluginv1.WatchSyncApplyEventsResponse{Fault: fault}, nil
	}
	budget := syncTimeBox(ctx)
	ctx, cancel := context.WithTimeout(ctx, syncRequestLimit(ctx))
	defer cancel()
	startedAt := s.clock()

	events := req.GetEvents()
	response := &pluginv1.WatchSyncApplyEventsResponse{
		Results: make([]*pluginv1.WatchSyncApplyResult, 0, len(events)),
	}
	for start := 0; start < len(events); {
		end := start + 1
		for end < len(events) && events[end].GetOperation() == events[start].GetOperation() {
			end++
		}
		group := events[start:end]
		start = end
		if s.clock().Sub(startedAt) >= budget {
			response.Results = append(response.Results, resultsFromFault(group, temporaryFault("Scrob sync time limit reached; the event will be retried"))...)
			continue
		}
		results := newResultSet(group)
		connectionFault := applyGroup(ctx, client, group, results)
		if connectionFault != nil {
			return &pluginv1.WatchSyncApplyEventsResponse{Fault: connectionFault}, nil
		}
		response.Results = append(response.Results, results.list()...)
	}
	return response, nil
}

func applyGroup(ctx context.Context, client *apiClient, events []*pluginv1.WatchSyncEvent, results *resultSet) *pluginv1.WatchSyncFault {
	pending := make([]*pluginv1.WatchSyncEvent, 0, len(events))
	for _, event := range events {
		if strings.TrimSpace(event.GetEventId()) == "" {
			results.reject(event, "Watch event ID is required")
			continue
		}
		pending = append(pending, event)
	}
	if len(pending) == 0 {
		return nil
	}
	switch pending[0].GetOperation() {
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED:
		return markWatched(ctx, client, pending, results)
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_UNWATCHED:
		return markUnwatched(ctx, client, pending, results)
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SET_RATING:
		return writeRatings(ctx, client, pending, results, false)
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_REMOVE_RATING:
		return writeRatings(ctx, client, pending, results, true)
	default:
		for _, event := range pending {
			results.reject(event, "Scrob does not support this watch operation")
		}
		return nil
	}
}

func validateAccount(ctx context.Context, client *apiClient) (*pluginv1.WatchSyncAccount, *pluginv1.WatchSyncFault) {
	// Scrob has no api-key-friendly whoami endpoint (GET /auth/me requires a
	// session JWT), so a ratings read doubles as the connectivity and
	// credential check.
	var response scrobRatingsResponse
	if fault := client.get(ctx, "/ratings", nil, &response); fault != nil {
		return nil, fault
	}
	label := accountLabel(client.baseURL.String())
	return &pluginv1.WatchSyncAccount{
		ExternalSubject: client.baseURL.String(),
		Username:        label,
		DisplayName:     label,
	}, nil
}

func accountLabel(baseURL string) string {
	// Scrob's account identity is scoped to the key, which the plugin must
	// not log or persist outside the credential record; the server's own
	// host is the only stable, display-safe label available.
	for _, prefix := range []string{"https://", "http://"} {
		if strings.HasPrefix(baseURL, prefix) {
			return strings.TrimPrefix(baseURL, prefix)
		}
	}
	return baseURL
}

func (s *Server) authenticatedClient(auth *pluginv1.WatchSyncAuthenticatedContext) (*apiClient, *pluginv1.WatchSyncFault) {
	if fault := checkCapability(auth.GetCapabilityId()); fault != nil {
		return nil, fault
	}
	apiKey := strings.TrimSpace(auth.GetCredentials().GetAccessToken())
	if apiKey == "" {
		return nil, invalidCredentialFault("Scrob API key is missing; reconnect Scrob")
	}
	baseURL := auth.GetCredentials().GetSecretAttributes()[configBaseURL]
	return s.client(auth.GetCapabilityId(), auth.GetProviderConfig(), apiKey, baseURL)
}

func (s *Server) client(requestedCapability string, config *pluginv1.WatchSyncProviderConfig, apiKey, connectionBaseURL string) (*apiClient, *pluginv1.WatchSyncFault) {
	if fault := checkCapability(requestedCapability); fault != nil {
		return nil, fault
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, invalidRequestFault("Scrob API key is required")
	}
	baseURL := strings.TrimSpace(connectionBaseURL)
	if baseURL == "" && config != nil {
		baseURL = config.GetValues()[configBaseURL]
		if baseURL == "" {
			baseURL = config.GetSecretValues()[configBaseURL]
		}
	}
	client, err := newAPIClient(baseURL, apiKey, s.http)
	if err != nil {
		return nil, invalidRequestFault(err.Error())
	}
	return client, nil
}

func credentials(apiKey, baseURL string) *pluginv1.WatchSyncCredentials {
	return &pluginv1.WatchSyncCredentials{
		AccessToken:      strings.TrimSpace(apiKey),
		SecretAttributes: map[string]string{configBaseURL: strings.TrimSpace(baseURL)},
	}
}

func cloneCredentials(value *pluginv1.WatchSyncCredentials) *pluginv1.WatchSyncCredentials {
	if value == nil {
		return nil
	}
	return &pluginv1.WatchSyncCredentials{
		AccessToken:      value.GetAccessToken(),
		RefreshToken:     value.GetRefreshToken(),
		ExpiresAt:        value.GetExpiresAt(),
		TokenType:        value.GetTokenType(),
		Scopes:           append([]string(nil), value.GetScopes()...),
		SecretAttributes: cloneMap(value.GetSecretAttributes()),
	}
}

func checkCapability(requested string) *pluginv1.WatchSyncFault {
	if requested != capabilityID {
		return invalidRequestFault("Unknown Scrob capability")
	}
	return nil
}

func requestedStateKind(kinds []pluginv1.WatchSyncRemoteStateKind) (pluginv1.WatchSyncRemoteStateKind, *pluginv1.WatchSyncFault) {
	if len(kinds) == 0 {
		return pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHED, nil
	}
	if len(kinds) != 1 {
		return 0, invalidRequestFault("Scrob accepts one state family per traversal")
	}
	return kinds[0], nil
}

// resultSet collects one result per event of a group, in event order.
type resultSet struct {
	events  []*pluginv1.WatchSyncEvent
	results map[*pluginv1.WatchSyncEvent]*pluginv1.WatchSyncApplyResult
}

func newResultSet(events []*pluginv1.WatchSyncEvent) *resultSet {
	return &resultSet{events: events, results: make(map[*pluginv1.WatchSyncEvent]*pluginv1.WatchSyncApplyResult, len(events))}
}

func (r *resultSet) set(event *pluginv1.WatchSyncEvent, status pluginv1.WatchSyncApplyStatus) {
	r.results[event] = &pluginv1.WatchSyncApplyResult{EventId: event.GetEventId(), Status: status}
}

func (r *resultSet) fail(event *pluginv1.WatchSyncEvent, fault *pluginv1.WatchSyncFault) {
	r.results[event] = resultFromFault(event.GetEventId(), fault)
}

func (r *resultSet) reject(event *pluginv1.WatchSyncEvent, message string) {
	r.fail(event, invalidRequestFault(message))
}

func (r *resultSet) list() []*pluginv1.WatchSyncApplyResult {
	for _, event := range r.events {
		if _, ok := r.results[event]; !ok {
			r.fail(event, temporaryFault("Scrob did not confirm the event; it will be retried"))
		}
	}
	out := make([]*pluginv1.WatchSyncApplyResult, 0, len(r.events))
	for _, event := range r.events {
		out = append(out, r.results[event])
	}
	return out
}

func syncTimeBox(ctx context.Context) time.Duration {
	budget := syncBudget
	if deadline, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(deadline)-syncDeadlineMargin)
	}
	return budget
}

func syncRequestLimit(ctx context.Context) time.Duration {
	limit := syncBudget + defaultRequestTimeout
	if deadline, ok := ctx.Deadline(); ok {
		limit = min(limit, max(time.Until(deadline)-syncDeadlineMargin, 0))
	}
	return limit
}

func (s *Server) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func resultFromFault(eventID string, fault *pluginv1.WatchSyncFault) *pluginv1.WatchSyncApplyResult {
	status := pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED
	if fault.GetCode() == pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY ||
		fault.GetCode() == pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED {
		status = pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY
	}
	return &pluginv1.WatchSyncApplyResult{EventId: eventID, Status: status, Fault: fault}
}

func resultsFromFault(events []*pluginv1.WatchSyncEvent, fault *pluginv1.WatchSyncFault) []*pluginv1.WatchSyncApplyResult {
	results := make([]*pluginv1.WatchSyncApplyResult, 0, len(events))
	for _, event := range events {
		results = append(results, resultFromFault(event.GetEventId(), fault))
	}
	return results
}

// connectionWide reports whether a fault affects every request of the
// connection, so it ends the call instead of failing one event.
func connectionWide(fault *pluginv1.WatchSyncFault) bool {
	return fault.GetCode() == pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL ||
		fault.GetCode() == pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED
}

func invalidRequestFault(message string) *pluginv1.WatchSyncFault {
	return &pluginv1.WatchSyncFault{Code: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST, SafeMessage: message}
}

func invalidCredentialFault(message string) *pluginv1.WatchSyncFault {
	return &pluginv1.WatchSyncFault{Code: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL, SafeMessage: message}
}

func temporaryFault(message string) *pluginv1.WatchSyncFault {
	return &pluginv1.WatchSyncFault{Code: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY, SafeMessage: message}
}

func permanentFault(message string) *pluginv1.WatchSyncFault {
	return &pluginv1.WatchSyncFault{Code: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT, SafeMessage: message}
}

func cloneMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	out := make(map[string]string, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}
