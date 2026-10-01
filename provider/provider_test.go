package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

func fakeConfig(serverURL string) *pluginv1.WatchSyncProviderConfig {
	return &pluginv1.WatchSyncProviderConfig{
		Values: map[string]string{configBaseURL: serverURL},
	}
}

func TestExchangeAPIKeySucceeds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "a-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(scrobRatingsResponse{})
	}))
	defer server.Close()

	s := NewServer(server.Client())
	resp, err := s.ExchangeAPIKey(context.Background(), &pluginv1.WatchSyncExchangeAPIKeyRequest{
		CapabilityId:   capabilityID,
		ProviderConfig: fakeConfig(server.URL),
		ApiKey:         "a-key",
	})
	if err != nil {
		t.Fatalf("ExchangeAPIKey() error: %v", err)
	}
	if resp.GetFault() != nil {
		t.Fatalf("ExchangeAPIKey() fault: %v", resp.GetFault())
	}
	if resp.GetCredentials().GetAccessToken() != "a-key" {
		t.Fatalf("AccessToken = %q, want a-key", resp.GetCredentials().GetAccessToken())
	}
	if resp.GetCredentials().GetSecretAttributes()[configBaseURL] != server.URL {
		t.Fatalf("SecretAttributes[%q] = %q, want %q", configBaseURL, resp.GetCredentials().GetSecretAttributes()[configBaseURL], server.URL)
	}
}

func TestExchangeAPIKeyRejectsInvalidKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	s := NewServer(server.Client())
	resp, err := s.ExchangeAPIKey(context.Background(), &pluginv1.WatchSyncExchangeAPIKeyRequest{
		CapabilityId:   capabilityID,
		ProviderConfig: fakeConfig(server.URL),
		ApiKey:         "bad-key",
	})
	if err != nil {
		t.Fatalf("ExchangeAPIKey() error: %v", err)
	}
	if resp.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL {
		t.Fatalf("Fault = %v, want INVALID_CREDENTIAL", resp.GetFault())
	}
}

func TestRequestsGoThroughTheAPIProxyPrefix(t *testing.T) {
	// Regression test: a standard Scrob deployment only publishes the
	// frontend's port. The frontend serves its own pages at bare paths like
	// /ratings, which is not one of its routes, so an unauthenticated
	// request there 302s to /login, and only forwards to the backend under
	// /api/proxy/*.
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(scrobRatingsResponse{})
	}))
	defer server.Close()

	s := NewServer(server.Client())
	_, err := s.ExchangeAPIKey(context.Background(), &pluginv1.WatchSyncExchangeAPIKeyRequest{
		CapabilityId:   capabilityID,
		ProviderConfig: fakeConfig(server.URL),
		ApiKey:         "a-key",
	})
	if err != nil {
		t.Fatalf("ExchangeAPIKey() error: %v", err)
	}
	if gotPath != "/api/proxy/ratings" {
		t.Fatalf("request path = %q, want /api/proxy/ratings", gotPath)
	}
}

func TestClientDefaultsBareHostPortToHTTP(t *testing.T) {
	// Regression test: a self-hosted bare "ip:port" entry must not become
	// https:// and silently fail a TLS handshake against a plain-HTTP server.
	client, err := newAPIClient("192.168.1.50:7330", "key", nil)
	if err != nil {
		t.Fatalf("newAPIClient() error: %v", err)
	}
	if got := client.baseURL.String(); got != "http://192.168.1.50:7330" {
		t.Fatalf("baseURL = %q, want http://192.168.1.50:7330", got)
	}
}

func TestClientKeepsExplicitHTTPS(t *testing.T) {
	client, err := newAPIClient("https://scrob.example.com", "key", nil)
	if err != nil {
		t.Fatalf("newAPIClient() error: %v", err)
	}
	if got := client.baseURL.String(); got != "https://scrob.example.com" {
		t.Fatalf("baseURL = %q, want https://scrob.example.com", got)
	}
}

func TestCheckCapabilityRejectsUnknown(t *testing.T) {
	if fault := checkCapability("not-scrob"); fault == nil {
		t.Fatalf("checkCapability() = nil, want a fault for an unknown capability")
	}
	if fault := checkCapability(capabilityID); fault != nil {
		t.Fatalf("checkCapability() = %v, want nil", fault)
	}
}
