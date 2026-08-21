package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestVersionStringIncludesBuildMetadata(t *testing.T) {
	oldVersion, oldCommit, oldDate := version, commit, date
	t.Cleanup(func() {
		version, commit, date = oldVersion, oldCommit, oldDate
	})

	version = "v1.2.3"
	commit = "abc1234"
	date = "2026-06-04T00:00:00Z"

	got := versionString()
	for _, want := range []string{"opencode2api", "v1.2.3", "abc1234", "2026-06-04T00:00:00Z"} {
		if !strings.Contains(got, want) {
			t.Fatalf("versionString() = %q, want it to contain %q", got, want)
		}
	}
}

type fakeUpstreamResponse struct {
	status int
	body   string
	header http.Header
}

type fakeRetryTransport struct {
	t               *testing.T
	responses       []fakeUpstreamResponse
	requestedModels []string
	requestedURLs   []string
	requestPayloads []map[string]any
	closeIdleCalls  int
}

func (f *fakeRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(f.responses) == 0 {
		f.t.Fatalf("unexpected request to %s", req.URL.String())
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		f.t.Fatalf("read request body: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		f.t.Fatalf("unmarshal request body: %v", err)
	}
	model, _ := payload["model"].(string)
	f.requestedModels = append(f.requestedModels, model)
	f.requestedURLs = append(f.requestedURLs, req.URL.String())
	f.requestPayloads = append(f.requestPayloads, payload)

	next := f.responses[0]
	f.responses = f.responses[1:]
	header := next.header
	if header == nil {
		header = make(http.Header)
	}
	return &http.Response{
		StatusCode: next.status,
		Header:     header.Clone(),
		Body:       io.NopCloser(strings.NewReader(next.body)),
		Request:    req,
	}, nil
}

func (f *fakeRetryTransport) CloseIdleConnections() {
	f.closeIdleCalls++
}

func installFakeOpenCodeClient(t *testing.T, responses []fakeUpstreamResponse) *fakeRetryTransport {
	t.Helper()

	oldHTTPClient := httpClient
	oldModelsCache := modelsCache
	oldGoModelsCache := goModelsCache
	oldOCClientVer := ocClientVer
	oldOCSessionID := ocSessionID
	oldOCProjectID := ocProjectID
	oldActiveSocks5 := activeSocks5
	oldSocks5Client := socks5Client
	oldSocks5ClientAddr := socks5ClientAddr
	oldSocks5PaidDirect := socks5PaidDirect
	oldSocks5Proxies := socks5Proxies

	transport := &fakeRetryTransport{
		t:         t,
		responses: append([]fakeUpstreamResponse(nil), responses...),
	}
	httpClient = &http.Client{Transport: transport}

	modelMu.Lock()
	modelsCache = []ModelInfo{{ID: "fallback-model-free"}}
	goModelsCache = nil
	modelMu.Unlock()

	socks5Mu.Lock()
	activeSocks5 = ""
	socks5Client = nil
	socks5ClientAddr = ""
	socks5PaidDirect = false
	socks5Proxies = nil
	socks5Mu.Unlock()

	ocInitMu.Lock()
	ocInitDone = true
	ocInitMu.Unlock()
	ocClientVer = "test-version"
	ocSessionID = "ses_test"
	ocProjectID = "project_test"

	t.Cleanup(func() {
		httpClient = oldHTTPClient
		modelMu.Lock()
		modelsCache = oldModelsCache
		goModelsCache = oldGoModelsCache
		modelMu.Unlock()
		socks5Mu.Lock()
		activeSocks5 = oldActiveSocks5
		socks5Client = oldSocks5Client
		socks5ClientAddr = oldSocks5ClientAddr
		socks5PaidDirect = oldSocks5PaidDirect
		socks5Proxies = oldSocks5Proxies
		socks5Mu.Unlock()
		ocInitMu.Lock()
		ocInitDone = false
		ocInitMu.Unlock()
		ocClientVer = oldOCClientVer
		ocSessionID = oldOCSessionID
		ocProjectID = oldOCProjectID
	})

	return transport
}

func TestCallOpenCodeAPIRetriesSameModelWithoutFallback(t *testing.T) {
	tests := []struct {
		name        string
		stream      bool
		responses   []fakeUpstreamResponse
		wantStatus  int
		wantBody    string
		wantModels  []string
		wantCloses  int
		requestBody string
		auth        UpstreamAuth
	}{
		{
			name:   "non-stream retries 401 on same model then succeeds",
			stream: false,
			responses: []fakeUpstreamResponse{
				{status: http.StatusUnauthorized, body: `{"error":"unauthorized"}`},
				{status: http.StatusOK, body: `{"id":"chatcmpl_test","choices":[]}`},
			},
			wantStatus:  http.StatusOK,
			wantBody:    `{"id":"chatcmpl_test","choices":[]}`,
			wantModels:  []string{"primary-model", "primary-model"},
			wantCloses:  1,
			requestBody: `{"model":"primary-model","messages":[]}`,
			auth:        UpstreamAuth{Mode: AuthRoutePublic},
		},
		{
			name:   "stream retries 429 on same model then succeeds",
			stream: true,
			responses: []fakeUpstreamResponse{
				{status: http.StatusTooManyRequests, body: `{"error":"rate_limited"}`},
				{status: http.StatusOK, body: "data: ok\n\n"},
			},
			wantStatus:  http.StatusOK,
			wantBody:    "data: ok\n\n",
			wantModels:  []string{"primary-model", "primary-model"},
			wantCloses:  1,
			requestBody: `{"model":"primary-model","messages":[],"stream":true}`,
			auth:        UpstreamAuth{Mode: AuthRoutePublic},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := installFakeOpenCodeClient(t, tt.responses)
			modelMu.Lock()
			modelsCache = []ModelInfo{{ID: "primary-model"}, {ID: "fallback-model-free"}}
			modelMu.Unlock()

			var (
				body   []byte
				status int
				err    error
			)
			if tt.stream {
				var respBody io.ReadCloser
				respBody, status, _, err = callOpenCodeAPIStream(context.Background(), []byte(tt.requestBody), "primary-model", tt.auth)
				if respBody != nil {
					defer respBody.Close()
				}
				if err == nil {
					body, err = io.ReadAll(respBody)
				}
			} else {
				body, status, _, err = callOpenCodeAPI(context.Background(), []byte(tt.requestBody), "primary-model", tt.auth)
			}
			if err != nil {
				t.Fatalf("upstream call error = %v", err)
			}
			if status != tt.wantStatus {
				t.Fatalf("upstream call status = %d, want %d", status, tt.wantStatus)
			}
			if string(body) != tt.wantBody {
				t.Fatalf("upstream call body = %q, want %q", string(body), tt.wantBody)
			}
			if !reflect.DeepEqual(transport.requestedModels, tt.wantModels) {
				t.Fatalf("requested models = %#v, want %#v", transport.requestedModels, tt.wantModels)
			}
			if transport.closeIdleCalls != tt.wantCloses {
				t.Fatalf("CloseIdleConnections calls = %d, want %d", transport.closeIdleCalls, tt.wantCloses)
			}
		})
	}
}

func TestCallOpenCodeAPIKeyedAuthDoesNotCrossModelFallback(t *testing.T) {
	tests := []struct {
		name   string
		stream bool
	}{
		{name: "non-stream", stream: false},
		{name: "stream", stream: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			creditsBody := `{"type":"error","error":{"type":"CreditsError","message":"Insufficient balance. Manage your billing here: https://opencode.ai/workspace/billing"}}`
			transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
				{status: http.StatusUnauthorized, body: creditsBody},
			})
			modelMu.Lock()
			modelsCache = []ModelInfo{{ID: "claude-sonnet-4-5"}, {ID: "claude-opus-5"}, {ID: "fallback-model-free"}}
			goModelsCache = []ModelInfo{{ID: "go-only-model"}, {ID: "shared-model"}}
			modelMu.Unlock()

			auth := UpstreamAuth{Mode: AuthRouteAuto, Token: "sk-validkey0123456789abcdef"}
			body := []byte(`{"model":"claude-sonnet-4-5","messages":[]}`)
			if tt.stream {
				body = []byte(`{"model":"claude-sonnet-4-5","messages":[],"stream":true}`)
				respBody, status, _, err := callOpenCodeAPIStream(context.Background(), body, "claude-sonnet-4-5", auth)
				if respBody != nil {
					defer respBody.Close()
					got, _ := io.ReadAll(respBody)
					if string(got) != creditsBody {
						t.Fatalf("stream body = %s, want credits error", string(got))
					}
				}
				if err != nil {
					t.Fatalf("callOpenCodeAPIStream() error = %v", err)
				}
				if status != http.StatusUnauthorized {
					t.Fatalf("callOpenCodeAPIStream() status = %d, want %d", status, http.StatusUnauthorized)
				}
			} else {
				got, status, _, err := callOpenCodeAPI(context.Background(), body, "claude-sonnet-4-5", auth)
				if err == nil {
					t.Fatal("callOpenCodeAPI() error = nil, want upstream error")
				}
				if status != http.StatusUnauthorized {
					t.Fatalf("callOpenCodeAPI() status = %d, want %d", status, http.StatusUnauthorized)
				}
				if string(got) != creditsBody {
					t.Fatalf("callOpenCodeAPI() body = %s, want credits error", string(got))
				}
			}

			wantModels := []string{"claude-sonnet-4-5"}
			if !reflect.DeepEqual(transport.requestedModels, wantModels) {
				t.Fatalf("requested models = %#v, want %#v (no cross-model fallback, no credits retry)", transport.requestedModels, wantModels)
			}
			wantURL := "https://opencode.ai/zen/v1/chat/completions"
			if !reflect.DeepEqual(transport.requestedURLs, []string{wantURL}) {
				t.Fatalf("requested URLs = %#v, want single %q", transport.requestedURLs, wantURL)
			}
		})
	}
}

func TestCallOpenCodeAPIGoEndpointKeepsSurfaceWithoutFallback(t *testing.T) {
	tests := []struct {
		name   string
		stream bool
	}{
		{name: "non-stream", stream: false},
		{name: "stream", stream: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
				{status: http.StatusOK, body: `{"id":"chatcmpl_test","choices":[]}`},
			})
			modelMu.Lock()
			modelsCache = []ModelInfo{{ID: "shared-model"}}
			goModelsCache = []ModelInfo{{ID: "go-only-model"}, {ID: "shared-model"}}
			modelMu.Unlock()

			auth := UpstreamAuth{Mode: AuthRouteAuto, Token: "sk-validkey0123456789abcdef"}
			body := []byte(`{"model":"go-only-model","messages":[]}`)
			if tt.stream {
				body = []byte(`{"model":"go-only-model","messages":[],"stream":true}`)
				respBody, status, _, err := callOpenCodeAPIStream(context.Background(), body, "go-only-model", auth)
				if respBody != nil {
					defer respBody.Close()
				}
				if err != nil {
					t.Fatalf("callOpenCodeAPIStream() error = %v", err)
				}
				if status != http.StatusOK {
					t.Fatalf("callOpenCodeAPIStream() status = %d, want %d", status, http.StatusOK)
				}
			} else {
				_, status, _, err := callOpenCodeAPI(context.Background(), body, "go-only-model", auth)
				if err != nil {
					t.Fatalf("callOpenCodeAPI() error = %v", err)
				}
				if status != http.StatusOK {
					t.Fatalf("callOpenCodeAPI() status = %d, want %d", status, http.StatusOK)
				}
			}

			wantURL := "https://opencode.ai/zen/go/v1/chat/completions"
			if !reflect.DeepEqual(transport.requestedURLs, []string{wantURL}) {
				t.Fatalf("requested URLs = %#v, want %q", transport.requestedURLs, wantURL)
			}
			if !reflect.DeepEqual(transport.requestedModels, []string{"go-only-model"}) {
				t.Fatalf("requested models = %#v, want [go-only-model]", transport.requestedModels)
			}
		})
	}
}

func TestIsNonRetryableUpstreamError(t *testing.T) {
	credits := []byte(`{"type":"error","error":{"type":"CreditsError","message":"Insufficient balance"}}`)
	if !isNonRetryableUpstreamError(http.StatusUnauthorized, credits) {
		t.Fatal("CreditsError should be non-retryable")
	}
	plain := []byte(`{"error":"unauthorized"}`)
	if isNonRetryableUpstreamError(http.StatusUnauthorized, plain) {
		t.Fatal("plain 401 without credits payload should not be classified as non-retryable billing")
	}
}

func TestGetHTTPClientForTierSocks5PaidDirectDefaultUsesProxy(t *testing.T) {
	oldHTTPClient := httpClient
	oldProxies := socks5Proxies
	oldActive := activeSocks5
	oldPaidDirect := socks5PaidDirect
	oldClient := socks5Client
	oldClientAddr := socks5ClientAddr
	t.Cleanup(func() {
		httpClient = oldHTTPClient
		socks5Mu.Lock()
		socks5Proxies = oldProxies
		activeSocks5 = oldActive
		socks5PaidDirect = oldPaidDirect
		socks5Client = oldClient
		socks5ClientAddr = oldClientAddr
		socks5Mu.Unlock()
	})

	httpClient = &http.Client{Timeout: 1}
	socks5Mu.Lock()
	socks5Proxies = []Socks5Proxy{{Addr: "127.0.0.1:1080", Name: "test"}}
	activeSocks5 = "127.0.0.1:1080"
	socks5PaidDirect = false
	socks5Client = nil
	socks5ClientAddr = ""
	socks5Mu.Unlock()

	paid := getHTTPClientForTier(TierPaid)
	free := getHTTPClientForTier(TierFree)
	if paid == httpClient {
		t.Fatal("default socks5_paid_direct=false should send paid traffic through SOCKS5")
	}
	if free == httpClient {
		t.Fatal("free traffic should use SOCKS5 when active_socks5 is set")
	}
	if paid != free {
		t.Fatal("paid and free should share the cached SOCKS5 client when paid_direct is false")
	}

	socks5Mu.Lock()
	socks5PaidDirect = true
	socks5Mu.Unlock()
	if getHTTPClientForTier(TierPaid) != httpClient {
		t.Fatal("socks5_paid_direct=true should keep paid traffic on the direct client")
	}
	if getHTTPClientForTier(TierFree) == httpClient {
		t.Fatal("free traffic should still use SOCKS5 when paid_direct is true")
	}
}

func TestCallOpenCodeAPIExhausted4xxReturnsRequestedModelError(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{
			status: http.StatusUnauthorized,
			body:   `{"error":"unauthorized-1"}`,
			header: http.Header{"X-Upstream-Error": []string{"first"}},
		},
		{
			status: http.StatusUnauthorized,
			body:   `{"error":"unauthorized-2"}`,
			header: http.Header{"X-Upstream-Error": []string{"second"}},
		},
		{
			status: http.StatusUnauthorized,
			body:   `{"error":"unauthorized-3"}`,
			header: http.Header{"X-Upstream-Error": []string{"last"}},
		},
	})
	modelMu.Lock()
	modelsCache = []ModelInfo{{ID: "primary-model"}, {ID: "fallback-model-free"}}
	modelMu.Unlock()

	body, status, header, err := callOpenCodeAPI(context.Background(), []byte(`{"model":"primary-model","messages":[]}`), "primary-model", UpstreamAuth{Mode: AuthRoutePublic})
	if err == nil {
		t.Fatal("callOpenCodeAPI() error = nil, want upstream error")
	}
	if status != http.StatusUnauthorized {
		t.Fatalf("callOpenCodeAPI() status = %d, want %d", status, http.StatusUnauthorized)
	}
	if string(body) != `{"error":"unauthorized-3"}` {
		t.Fatalf("callOpenCodeAPI() body = %s, want final same-model retry body", string(body))
	}
	if header.Get("X-Upstream-Error") != "last" {
		t.Fatalf("final header = %q, want last", header.Get("X-Upstream-Error"))
	}
	wantModels := []string{"primary-model", "primary-model", "primary-model"}
	if !reflect.DeepEqual(transport.requestedModels, wantModels) {
		t.Fatalf("requested models = %#v, want %#v", transport.requestedModels, wantModels)
	}
	if transport.closeIdleCalls != 2 {
		t.Fatalf("CloseIdleConnections calls = %d, want 2", transport.closeIdleCalls)
	}
}

func TestBuildOCRequestRoutesSharedAndGoOnlyModelsByAuthMode(t *testing.T) {
	oldModelsCache := modelsCache
	oldGoModelsCache := goModelsCache
	modelMu.Lock()
	modelsCache = []ModelInfo{
		{ID: "glm-5.2"},
		{ID: "gpt-5.5"},
	}
	goModelsCache = []ModelInfo{
		{ID: "glm-5.2"},
		{ID: "kimi-k2.7-code"},
	}
	modelMu.Unlock()
	t.Cleanup(func() {
		modelMu.Lock()
		modelsCache = oldModelsCache
		goModelsCache = oldGoModelsCache
		modelMu.Unlock()
	})

	tests := []struct {
		name    string
		auth    UpstreamAuth
		modelID string
		wantURL string
	}{
		{
			name:    "public stays on zen free surface",
			auth:    UpstreamAuth{Mode: AuthRoutePublic},
			modelID: "deepseek-v4-flash-free",
			wantURL: "https://opencode.ai/zen/v1/chat/completions",
		},
		{
			name:    "bare key keeps shared model on zen",
			auth:    UpstreamAuth{Mode: AuthRouteAuto, Token: "sk-auto"},
			modelID: "glm-5.2",
			wantURL: "https://opencode.ai/zen/v1/chat/completions",
		},
		{
			name:    "go prefix sends shared model to go surface",
			auth:    UpstreamAuth{Mode: AuthRouteGo, Token: "sk-go"},
			modelID: "glm-5.2",
			wantURL: "https://opencode.ai/zen/go/v1/chat/completions",
		},
		{
			name:    "bare key still reaches go only models",
			auth:    UpstreamAuth{Mode: AuthRouteAuto, Token: "sk-auto"},
			modelID: "kimi-k2.7-code",
			wantURL: "https://opencode.ai/zen/go/v1/chat/completions",
		},
		{
			name:    "zen prefix forces zen surface",
			auth:    UpstreamAuth{Mode: AuthRouteZen, Token: "sk-zen"},
			modelID: "glm-5.2",
			wantURL: "https://opencode.ai/zen/v1/chat/completions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildOCRequest(tt.modelID, map[string]any{"messages": []any{}}, tt.auth)
			if err != nil {
				t.Fatalf("buildOCRequest() error = %v", err)
			}
			if got := req.URL.String(); got != tt.wantURL {
				t.Fatalf("buildOCRequest() URL = %q, want %q", got, tt.wantURL)
			}
			wantAuth := "Bearer public"
			if tt.auth.Mode != AuthRoutePublic {
				wantAuth = "Bearer " + tt.auth.Token
			}
			if got := req.Header.Get("Authorization"); got != wantAuth {
				t.Fatalf("buildOCRequest() Authorization = %q, want %q", got, wantAuth)
			}
		})
	}
}

func TestListModelsHandlerSeparatesPublicZenAndGoCatalogs(t *testing.T) {
	oldModelsCache := modelsCache
	oldGoModelsCache := goModelsCache
	oldModelsLoaded := modelsLoaded
	oldModelAlias := modelAlias
	modelMu.Lock()
	modelsCache = []ModelInfo{
		{ID: "deepseek-v4-flash-free"},
		{ID: "glm-5.2"},
		{ID: "gpt-5.5"},
	}
	goModelsCache = []ModelInfo{
		{ID: "glm-5.2"},
		{ID: "kimi-k2.7-code"},
	}
	modelsLoaded = true
	modelMu.Unlock()
	configMu.Lock()
	modelAlias = map[string]string{}
	configMu.Unlock()
	t.Cleanup(func() {
		modelMu.Lock()
		modelsCache = oldModelsCache
		goModelsCache = oldGoModelsCache
		modelsLoaded = oldModelsLoaded
		modelMu.Unlock()
		configMu.Lock()
		modelAlias = oldModelAlias
		configMu.Unlock()
	})

	tests := []struct {
		name       string
		authHeader string
		wantIDs    []string
	}{
		{
			name:    "public only sees free zen models",
			wantIDs: []string{"deepseek-v4-flash"},
		},
		{
			name:       "bare zen key sees zen catalog only",
			authHeader: "Bearer sk-auto0123456789abcdef",
			wantIDs:    []string{"deepseek-v4-flash", "glm-5.2", "gpt-5.5"},
		},
		{
			name:       "go prefix sees free and go catalog",
			authHeader: "Bearer go:sk-go0123456789abcdef",
			wantIDs:    []string{"deepseek-v4-flash", "glm-5.2", "kimi-k2.7-code"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()

			listModelsHandler(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("listModelsHandler() status = %d, want %d", rec.Code, http.StatusOK)
			}
			var payload struct {
				Data []ModelInfo `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("unmarshal models response: %v", err)
			}
			gotIDs := make([]string, 0, len(payload.Data))
			for _, model := range payload.Data {
				gotIDs = append(gotIDs, model.ID)
			}
			if !reflect.DeepEqual(gotIDs, tt.wantIDs) {
				t.Fatalf("listModelsHandler() ids = %#v, want %#v", gotIDs, tt.wantIDs)
			}
		})
	}
}

func TestListModelsHandlerReplacesMappedModelIDsWithAliases(t *testing.T) {
	oldModelsCache := modelsCache
	oldGoModelsCache := goModelsCache
	oldModelsLoaded := modelsLoaded
	oldModelAlias := modelAlias
	modelMu.Lock()
	modelsCache = []ModelInfo{
		{ID: "deepseek-v4-flash-free", Object: "model", OwnedBy: "opencode"},
		{ID: "gpt-5.5", Object: "model", OwnedBy: "opencode"},
	}
	goModelsCache = nil
	modelsLoaded = true
	modelMu.Unlock()
	configMu.Lock()
	modelAlias = map[string]string{
		"deepseek-v4-flash": "deepseek-v4-flash-free",
	}
	configMu.Unlock()
	t.Cleanup(func() {
		modelMu.Lock()
		modelsCache = oldModelsCache
		goModelsCache = oldGoModelsCache
		modelsLoaded = oldModelsLoaded
		modelMu.Unlock()
		configMu.Lock()
		modelAlias = oldModelAlias
		configMu.Unlock()
	})

	for _, tt := range []struct {
		name       string
		authHeader string
		wantIDs    []string
	}{
		{
			name:    "public sees free alias instead of upstream name",
			wantIDs: []string{"deepseek-v4-flash"},
		},
		{
			name:       "authenticated catalog replaces upstream name",
			authHeader: "Bearer sk-auto0123456789abcdef",
			wantIDs:    []string{"deepseek-v4-flash", "gpt-5.5"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()

			listModelsHandler(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("listModelsHandler() status = %d, want %d", rec.Code, http.StatusOK)
			}
			var payload struct {
				Data []ModelInfo `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("unmarshal models response: %v", err)
			}
			gotIDs := make([]string, 0, len(payload.Data))
			for _, model := range payload.Data {
				gotIDs = append(gotIDs, model.ID)
			}
			if !reflect.DeepEqual(gotIDs, tt.wantIDs) {
				t.Fatalf("listModelsHandler() ids = %#v, want %#v", gotIDs, tt.wantIDs)
			}
		})
	}
}

func TestListModelsHandlerStripsFreeSuffixWithoutAlias(t *testing.T) {
	oldModelsCache := modelsCache
	oldGoModelsCache := goModelsCache
	oldModelsLoaded := modelsLoaded
	oldModelAlias := modelAlias
	modelMu.Lock()
	modelsCache = []ModelInfo{
		{ID: "mimo-v2.5-free", Object: "model", OwnedBy: "opencode"},
	}
	goModelsCache = nil
	modelsLoaded = true
	modelMu.Unlock()
	configMu.Lock()
	modelAlias = map[string]string{}
	configMu.Unlock()
	t.Cleanup(func() {
		modelMu.Lock()
		modelsCache = oldModelsCache
		goModelsCache = oldGoModelsCache
		modelsLoaded = oldModelsLoaded
		modelMu.Unlock()
		configMu.Lock()
		modelAlias = oldModelAlias
		configMu.Unlock()
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	listModelsHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var payload struct {
		Data []ModelInfo `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data) != 1 || payload.Data[0].ID != "mimo-v2.5" {
		t.Fatalf("ids = %#v, want [mimo-v2.5]", payload.Data)
	}
}

func TestResolveModelMapsStrippedFreeNameBackToUpstream(t *testing.T) {
	oldModelsCache := modelsCache
	oldGoModelsCache := goModelsCache
	oldModelAlias := modelAlias
	modelMu.Lock()
	modelsCache = []ModelInfo{{ID: "mimo-v2.5-free"}}
	goModelsCache = nil
	modelMu.Unlock()
	configMu.Lock()
	modelAlias = map[string]string{}
	configMu.Unlock()
	t.Cleanup(func() {
		modelMu.Lock()
		modelsCache = oldModelsCache
		goModelsCache = oldGoModelsCache
		modelMu.Unlock()
		configMu.Lock()
		modelAlias = oldModelAlias
		configMu.Unlock()
	})

	if got := resolveModel("mimo-v2.5"); got != "mimo-v2.5-free" {
		t.Fatalf("resolveModel(mimo-v2.5) = %q, want mimo-v2.5-free", got)
	}
	if got := resolveModel("mimo-v2.5-free"); got != "mimo-v2.5-free" {
		t.Fatalf("resolveModel(mimo-v2.5-free) = %q, want unchanged", got)
	}
}

// TestResolveModelMapsFreeTwinEvenWhenPaidExists 验证付费版与免费版共存时，
// 请求去后缀名仍映射到免费版（本项目只服务免费模型）。
func TestResolveModelMapsFreeTwinEvenWhenPaidExists(t *testing.T) {
	oldModelsCache := modelsCache
	oldGoModelsCache := goModelsCache
	oldModelAlias := modelAlias
	modelMu.Lock()
	modelsCache = []ModelInfo{{ID: "hy3"}, {ID: "hy3-free"}}
	goModelsCache = nil
	modelMu.Unlock()
	configMu.Lock()
	modelAlias = map[string]string{}
	configMu.Unlock()
	t.Cleanup(func() {
		modelMu.Lock()
		modelsCache = oldModelsCache
		goModelsCache = oldGoModelsCache
		modelMu.Unlock()
		configMu.Lock()
		modelAlias = oldModelAlias
		configMu.Unlock()
	})

	if got := resolveModel("hy3"); got != "hy3-free" {
		t.Fatalf("resolveModel(hy3) with paid twin present = %q, want hy3-free", got)
	}
	// 手动别名仍优先：别名显式指向付费版时不应被自动免费映射覆盖
	configMu.Lock()
	modelAlias = map[string]string{"hy3-paid": "hy3"}
	configMu.Unlock()
	if got := resolveModel("hy3-paid"); got != "hy3" {
		t.Fatalf("resolveModel(hy3-paid) = %q, want hy3 (manual alias wins)", got)
	}
}

func TestExtractUpstreamAuthKeyValidation(t *testing.T) {
	tests := []struct {
		name       string
		authHeader string
		apiKey     string
		wantMode   AuthRouteMode
		wantToken  string
		wantSource string
	}{
		{"no header", "", "", AuthRoutePublic, "", "none"},
		{"bearer empty", "Bearer ", "", AuthRoutePublic, "", "none"},
		{"bearer public", "Bearer public", "", AuthRoutePublic, "", "authorization"},
		{"bearer no-key-required placeholder", "Bearer no-key-required", "", AuthRoutePublic, "", "authorization"},
		{"bearer random non-key", "Bearer abc123xyz", "", AuthRoutePublic, "", "authorization"},
		{"valid sk key", "Bearer sk-validkey0123456789abcdef", "", AuthRouteAuto, "sk-validkey0123456789abcdef", "authorization"},
		{"go prefix with sk key", "Bearer go:sk-gokey0123456789abcdef", "", AuthRouteGo, "sk-gokey0123456789abcdef", "authorization"},
		{"zen prefix with sk key", "Bearer zen:sk-zenkey0123456789abcdef", "", AuthRouteZen, "sk-zenkey0123456789abcdef", "authorization"},
		{"go prefix with placeholder falls to public", "Bearer go:no-key-required", "", AuthRoutePublic, "", "authorization"},
		{"bare sk- with no suffix is invalid", "Bearer sk-", "", AuthRoutePublic, "", "authorization"},
		{"x-api-key valid", "", "sk-validkey0123456789abcdef", AuthRouteAuto, "sk-validkey0123456789abcdef", "x-api-key"},
		{"x-api-key sk-local short", "", "sk-local", AuthRoutePublic, "", "x-api-key"},
		{"x-api-key sk-ant rejected", "", "sk-ant-api03-abcdefghijklmnopqrstuvwxyz", AuthRoutePublic, "", "x-api-key"},
		{"x-api-key go prefix", "", "go:sk-gokey0123456789abcdef", AuthRouteGo, "sk-gokey0123456789abcdef", "x-api-key"},
		{"x-api-key zen prefix", "", "zen:sk-zenkey0123456789abcdef", AuthRouteZen, "sk-zenkey0123456789abcdef", "x-api-key"},
		{"bearer wins over x-api-key", "Bearer sk-validkey0123456789abcdef", "sk-otherkey0123456789abcdef", AuthRouteAuto, "sk-validkey0123456789abcdef", "authorization"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			if tt.apiKey != "" {
				req.Header.Set("x-api-key", tt.apiKey)
			}
			auth := extractUpstreamAuth(req)
			if auth.Mode != tt.wantMode {
				t.Fatalf("mode = %v, want %v", auth.Mode, tt.wantMode)
			}
			if auth.Token != tt.wantToken {
				t.Fatalf("token = %q, want %q", auth.Token, tt.wantToken)
			}
			if auth.Source != tt.wantSource {
				t.Fatalf("source = %q, want %q", auth.Source, tt.wantSource)
			}
		})
	}
}

// ---- 上游 v0.4.7/v0.4.8 移植：Claude usage 缓存语义 ----

func TestBuildClaudeUsageCoreDeepSeekMissIsNotCreation(t *testing.T) {
	usage := buildClaudeUsageCore(map[string]any{
		"prompt_tokens":            float64(200),
		"prompt_cache_hit_tokens":  float64(160),
		"prompt_cache_miss_tokens": float64(40),
		"completion_tokens":        float64(35),
	})
	if got := usage["input_tokens"]; got != 40 {
		t.Fatalf("input_tokens = %v, want 40 (200-160 hit excluded)", got)
	}
	if got := usage["output_tokens"]; got != 35 {
		t.Fatalf("output_tokens = %v, want 35", got)
	}
	if got := usage["cache_read_input_tokens"]; got != 160 {
		t.Fatalf("cache_read_input_tokens = %v, want 160", got)
	}
	if _, ok := usage["cache_creation_input_tokens"]; ok {
		t.Fatalf("cache_creation_input_tokens should be absent for DeepSeek miss-only usage, got %v", usage["cache_creation_input_tokens"])
	}
}

func TestBuildClaudeUsageCoreAnthropicStyleUntouched(t *testing.T) {
	usage := buildClaudeUsageCore(map[string]any{
		"input_tokens":                float64(33),
		"cache_read_input_tokens":     float64(256),
		"cache_creation_input_tokens": float64(10),
		"output_tokens":               float64(7),
	})
	if got := usage["input_tokens"]; got != 33 {
		t.Fatalf("input_tokens = %v, want 33 (canonical Anthropic fields not subtracted)", got)
	}
}

func TestParseCacheUsageCanonicalFields(t *testing.T) {
	read, created := parseCacheUsage(map[string]any{
		"cache_read_input_tokens":     float64(64),
		"cache_creation_input_tokens": float64(8),
	})
	if read != 64 || created != 8 {
		t.Fatalf("parseCacheUsage = (%d, %d), want (64, 8)", read, created)
	}
}

func TestParseCacheUsageDeepSeekMissIsNotCreation(t *testing.T) {
	read, created := parseCacheUsage(map[string]any{
		"prompt_tokens":            float64(200),
		"prompt_cache_hit_tokens":  float64(160),
		"prompt_cache_miss_tokens": float64(40),
	})
	if read != 160 || created != 0 {
		t.Fatalf("parseCacheUsage = (%d, %d), want (160, 0)", read, created)
	}
}

func TestParseCacheUsagePrefersCanonicalRead(t *testing.T) {
	read, created := parseCacheUsage(map[string]any{
		"cache_read_input_tokens": float64(80),
		"prompt_cache_hit_tokens": float64(160),
		"prompt_tokens_details": map[string]any{
			"cached_tokens": float64(200),
		},
	})
	if read != 80 || created != 0 {
		t.Fatalf("parseCacheUsage = (%d, %d), want (80, 0)", read, created)
	}
}

// ---- 上游 v0.5.0 移植：/v1/messages/count_tokens 本地估算 ----

func TestEstimateClaudeInputTokens(t *testing.T) {
	req := ClaudeRequest{
		Model:  "hy3",
		System: "You are helpful.",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: []any{
				map[string]any{"type": "text", "text": "hi there"},
			}},
			{Role: "user", Content: []any{
				map[string]any{"type": "image", "source": map[string]any{}},
				map[string]any{"type": "text", "text": "what is this"},
			}},
		},
	}
	got := estimateClaudeInputTokens(req)
	// system(4+4) + msg(4+2) + msg(4+2) + msg(4+1600+3) = 1627
	if got != 1627 {
		t.Fatalf("estimateClaudeInputTokens = %d, want 1627", got)
	}
}

func TestEstimateTextTokensRuneBased(t *testing.T) {
	if got := estimateTextTokens(""); got != 0 {
		t.Fatalf("empty = %d, want 0", got)
	}
	if got := estimateTextTokens("ab"); got != 1 {
		t.Fatalf("short = %d, want 1 (round up)", got)
	}
	// 8 个中文字符 = 8 rune ≈ 2 token（按字节会错算成 6）
	if got := estimateTextTokens("一二三四五六七八"); got != 2 {
		t.Fatalf("cjk = %d, want 2", got)
	}
}

func TestClaudeCountTokensHandler(t *testing.T) {
	body := `{"model":"hy3","messages":[{"role":"user","content":"hello world"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	rec := httptest.NewRecorder()
	claudeCountTokensHandler(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp map[string]int
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["input_tokens"] <= 0 {
		t.Fatalf("input_tokens = %d, want > 0", resp["input_tokens"])
	}

	// 缺 model → 400 协议错误结构
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"messages":[]}`))
	rec2 := httptest.NewRecorder()
	claudeCountTokensHandler(rec2, req2)
	if rec2.Code != 400 {
		t.Fatalf("missing model status = %d, want 400", rec2.Code)
	}
	// GET → 405
	req3 := httptest.NewRequest(http.MethodGet, "/v1/messages/count_tokens", nil)
	rec3 := httptest.NewRecorder()
	claudeCountTokensHandler(rec3, req3)
	if rec3.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec3.Code)
	}
}

// ---- 上游 v0.4.5 移植：prompt_cache_retention / cache_control 注入 ----

func TestConvertRequestInjectsCacheDefaults(t *testing.T) {
	oldRetention := promptCacheRetention
	oldBreakpoints := cacheBreakpoints
	promptCacheRetention = ""
	cacheBreakpoints = true
	t.Cleanup(func() { promptCacheRetention = oldRetention; cacheBreakpoints = oldBreakpoints })

	req := &OpenAIRequest{Model: "deepseek-v4-flash-free", Messages: []Message{{Role: "user", Content: "hi"}}}
	body := convertRequest(req)
	if v, _ := body["prompt_cache_retention"].(string); v != "24h" {
		t.Fatalf("prompt_cache_retention = %#v, want 24h", body["prompt_cache_retention"])
	}
	cc, ok := body["cache_control"].(map[string]any)
	if !ok || cc["type"] != "ephemeral" || cc["ttl"] != "1h" {
		t.Fatalf("cache_control = %#v, want {type:ephemeral ttl:1h}", body["cache_control"])
	}
}

func TestConvertRequestSkipsCacheControlForGLM(t *testing.T) {
	oldBreakpoints := cacheBreakpoints
	cacheBreakpoints = true
	t.Cleanup(func() { cacheBreakpoints = oldBreakpoints })

	for _, m := range []string{"glm-5.2", "GLM-5", "zhipu-x", "z-ai-model"} {
		req := &OpenAIRequest{Model: m, Messages: []Message{{Role: "user", Content: "hi"}}}
		body := convertRequest(req)
		if _, ok := body["cache_control"]; ok {
			t.Fatalf("model %s: cache_control should be skipped, got %#v", m, body["cache_control"])
		}
		if v, _ := body["prompt_cache_retention"].(string); v != "24h" {
			t.Fatalf("model %s: retention still injected, got %#v", m, body["prompt_cache_retention"])
		}
	}
}

func TestConvertRequestExtraBodyWinsOverInjection(t *testing.T) {
	oldRetention := promptCacheRetention
	promptCacheRetention = ""
	t.Cleanup(func() { promptCacheRetention = oldRetention })

	req := &OpenAIRequest{
		Model:    "deepseek-v4-flash-free",
		Messages: []Message{{Role: "user", Content: "hi"}},
		ExtraBody: map[string]any{
			"prompt_cache_retention": "in_memory",
		},
	}
	body := convertRequest(req)
	if v, _ := body["prompt_cache_retention"].(string); v != "in_memory" {
		t.Fatalf("explicit extra_body retention lost: %#v", body["prompt_cache_retention"])
	}
}

func TestConvertRequestRetentionOff(t *testing.T) {
	oldRetention := promptCacheRetention
	oldBreakpoints := cacheBreakpoints
	promptCacheRetention = "off"
	cacheBreakpoints = false
	t.Cleanup(func() { promptCacheRetention = oldRetention; cacheBreakpoints = oldBreakpoints })

	req := &OpenAIRequest{Model: "deepseek-v4-flash-free", Messages: []Message{{Role: "user", Content: "hi"}}}
	body := convertRequest(req)
	if _, ok := body["prompt_cache_retention"]; ok {
		t.Fatalf("retention=off should not inject, got %#v", body["prompt_cache_retention"])
	}
	if _, ok := body["cache_control"]; ok {
		t.Fatalf("breakpoints=false should not inject, got %#v", body["cache_control"])
	}
}

// ---- 上游 v0.4.6 移植：SOCKS5 会话粘性出口 ----

func withStickyProxyEnv(t *testing.T, proxies []Socks5Proxy, active string, sticky bool, paidDirect bool) {
	t.Helper()
	socks5Mu.Lock()
	oldProxies := socks5Proxies
	oldActive := activeSocks5
	oldSticky := socks5Sticky
	oldPaidDirect := socks5PaidDirect
	socks5Proxies = proxies
	activeSocks5 = active
	socks5Sticky = sticky
	socks5PaidDirect = paidDirect
	socks5Mu.Unlock()
	stickyMu.Lock()
	stickyEntries = map[string]*stickyProxyEntry{}
	stickyMu.Unlock()
	t.Cleanup(func() {
		socks5Mu.Lock()
		socks5Proxies = oldProxies
		activeSocks5 = oldActive
		socks5Sticky = oldSticky
		socks5PaidDirect = oldPaidDirect
		socks5Mu.Unlock()
		stickyMu.Lock()
		stickyEntries = map[string]*stickyProxyEntry{}
		stickyMu.Unlock()
	})
}

func TestStickyKeyForRequest(t *testing.T) {
	cases := []struct {
		name string
		auth UpstreamAuth
		body map[string]any
		want string
	}{
		{"paid token wins", UpstreamAuth{Token: "sk-abc"}, nil, "tok:sk-abc"},
		{"public session user", UpstreamAuth{}, map[string]any{"user": "sess-42"}, "usr:sess-42"},
		{"public no session falls back", UpstreamAuth{}, map[string]any{}, stickyPublicFallback},
		{"public no body falls back", UpstreamAuth{}, nil, stickyPublicFallback},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stickyKeyForRequest(c.auth, c.body); got != c.want {
				t.Fatalf("stickyKeyForRequest = %q, want %q", got, c.want)
			}
		})
	}
}

func TestGetHTTPClientStickyPinsSession(t *testing.T) {
	withStickyProxyEnv(t, []Socks5Proxy{{Addr: "p1"}, {Addr: "p2"}, {Addr: "p3"}}, socks5RR, true, false)

	c1 := getHTTPClientSticky(UpstreamAuth{Token: "tok-1"}, nil)
	c2 := getHTTPClientSticky(UpstreamAuth{Token: "tok-1"}, nil)
	if c1 != c2 {
		t.Fatalf("same session got different clients")
	}
	u1 := getHTTPClientSticky(UpstreamAuth{}, map[string]any{"user": "sess-1"})
	u2 := getHTTPClientSticky(UpstreamAuth{}, map[string]any{"user": "sess-1"})
	if u1 != u2 {
		t.Fatalf("same user session got different clients")
	}
	p1 := getHTTPClientSticky(UpstreamAuth{}, map[string]any{})
	p2 := getHTTPClientSticky(UpstreamAuth{}, map[string]any{})
	if p1 != p2 {
		t.Fatalf("public fallback sessions should share one client")
	}
}

func TestInvalidateStickyProxyRebinds(t *testing.T) {
	withStickyProxyEnv(t, []Socks5Proxy{{Addr: "p1"}, {Addr: "p2"}, {Addr: "p3"}}, socks5RR, true, false)

	auth := UpstreamAuth{Token: "tok-1"}
	_ = getHTTPClientSticky(auth, nil)
	if got := len(stickyEntries); got != 1 {
		t.Fatalf("sticky entries = %d, want 1", got)
	}
	invalidateStickyProxy(auth, nil)
	if got := len(stickyEntries); got != 0 {
		t.Fatalf("sticky entries after invalidate = %d, want 0", got)
	}
	_ = getHTTPClientSticky(auth, nil)
	if _, ok := stickyEntries["tok:tok-1"]; !ok {
		t.Fatal("binding should be recreated after invalidate")
	}
}

func TestInvalidateStickyProxyRotatesEgress(t *testing.T) {
	withStickyProxyEnv(t, []Socks5Proxy{{Addr: "p1"}, {Addr: "p2"}, {Addr: "p3"}}, socks5RR, true, false)

	auth := UpstreamAuth{Token: "tok-2"}
	seen := map[int]bool{}
	for i := 0; i < 4; i++ {
		_ = getHTTPClientSticky(auth, nil)
		stickyMu.Lock()
		idx := stickyEntries["tok:tok-2"].proxyIdx
		stickyMu.Unlock()
		seen[idx] = true
		invalidateStickyProxy(auth, nil)
	}
	if len(seen) < 2 {
		t.Fatalf("egress did not rotate across rebinds: %v", seen)
	}
}

func TestGetHTTPClientStickySkipsWhenDisabled(t *testing.T) {
	withStickyProxyEnv(t, []Socks5Proxy{{Addr: "p1"}, {Addr: "p2"}}, socks5RR, false, false)

	getHTTPClientSticky(UpstreamAuth{Token: "tok-1"}, nil)
	if got := len(stickyEntries); got != 0 {
		t.Fatalf("sticky disabled but entries = %d", got)
	}
}

// ---- 上游 v0.4.9 移植：text_only_models 多模态降级 ----

func TestIsTextOnlyModelPrefixMatch(t *testing.T) {
	old := textOnlyModels
	textOnlyModels = []string{"deepseek"}
	t.Cleanup(func() { textOnlyModels = old })

	for _, m := range []string{"deepseek-v4-flash", "DeepSeek-V4-Pro", "deepseek-v4-flash-free"} {
		if !isTextOnlyModel(m) {
			t.Fatalf("isTextOnlyModel(%q) = false, want true", m)
		}
	}
	if isTextOnlyModel("glm-5.2") || isTextOnlyModel("") {
		t.Fatal("non-deepseek models should not be text-only")
	}
}

func TestDowngradeMultimodalContent(t *testing.T) {
	content := []any{
		map[string]any{"type": "text", "text": "look at this"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,xx"}},
		map[string]any{"type": "file", "file": map[string]any{}},
	}
	got := downgradeMultimodalContent(content, true).([]any)
	if len(got) != 3 {
		t.Fatalf("parts = %d, want 3 (order preserved)", len(got))
	}
	if got[0].(map[string]any)["text"] != "look at this" {
		t.Fatalf("text part lost: %#v", got[0])
	}
	if got[1].(map[string]any)["text"] != "[image attached]" {
		t.Fatalf("image not downgraded: %#v", got[1])
	}
	if got[2].(map[string]any)["text"] != "[document attached]" {
		t.Fatalf("file not downgraded: %#v", got[2])
	}
	// 非 text-only 模型原样返回
	same := downgradeMultimodalContent(content, false).([]any)
	if len(same) != 3 || same[1].(map[string]any)["type"] != "image_url" {
		t.Fatal("non-text-only should return content unchanged")
	}
}

func TestConvertRequestDowngradesForTextOnlyModel(t *testing.T) {
	old := textOnlyModels
	textOnlyModels = []string{"deepseek"}
	t.Cleanup(func() { textOnlyModels = old })

	req := &OpenAIRequest{
		Model: "deepseek-v4-flash-free",
		Messages: []Message{{
			Role: "user",
			Content: []any{
				map[string]any{"type": "text", "text": "hi"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "x"}},
			},
		}},
	}
	body := convertRequest(req)
	msgs := body["messages"].([]map[string]any)
	content := msgs[0]["content"].([]any)
	if content[1].(map[string]any)["text"] != "[image attached]" {
		t.Fatalf("upstream still has image part: %#v", content[1])
	}
}

// ---- 上游 v0.4.4 移植：原始 DSML/Qwen 工具调用转换 ----

func TestParseRawToolCalls(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		want  int
		first string
		args  string
	}{
		{
			name: "dsml name parameters",
			in:   `<｜DSML｜tool_calls><name>ls</name><parameters>{"path":"."}</parameters></｜DSML｜tool_calls>`,
			want: 1, first: "ls", args: `{"path":"."}`,
		},
		{
			name: "dsml invoke",
			in:   `<｜DSML｜tool_calls><｜DSML｜invoke name="read_file"><｜DSML｜parameter name="path">/tmp/a.txt</｜DSML｜parameter></｜DSML｜invoke></｜DSML｜tool_calls>`,
			want: 1, first: "read_file", args: `{"path":"/tmp/a.txt"}`,
		},
		{
			name: "qwen",
			in:   `<tool_call><name>search</name><parameters>{"q":"go"}</parameters></tool_call>`,
			want: 1, first: "search", args: `{"q":"go"}`,
		},
		{
			name: "qwen function",
			in:   `<function=search><parameter=q>go</parameter></function>`,
			want: 1, first: "search", args: `{"q":"go"}`,
		},
		{
			name: "multiple",
			in:   `<｜DSML｜tool_calls><name>a</name><parameters>{}</parameters><name>b</name><parameters>{"x":1}</parameters></｜DSML｜tool_calls>`,
			want: 2, first: "a", args: `{}`,
		},
		{
			name: "malformed",
			in:   `<｜DSML｜tool_calls><name>ls</name></｜DSML｜tool_calls>`,
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseRawToolCalls(tt.in)
			if len(got) != tt.want {
				t.Fatalf("parseRawToolCalls = %d calls, want %d: %#v", len(got), tt.want, got)
			}
			if tt.want > 0 {
				if got[0].Function.Name != tt.first || got[0].Function.Arguments != tt.args {
					t.Fatalf("first call = %#v, want name=%q args=%q", got[0], tt.first, tt.args)
				}
				if !strings.HasPrefix(got[0].ID, "call_") {
					t.Fatalf("call ID = %q, want call_ prefix", got[0].ID)
				}
			}
		})
	}
}

func TestConvertRawToolCallsInBody(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"<tool_call><name>ls</name><parameters>{\"path\":\".\"}</parameters></tool_call>"},"finish_reason":"stop"}]}`)
	out := convertRawToolCallsInBody(body)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msg := m["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	tcs := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(tcs))
	}
	tc := tcs[0].(map[string]any)
	if tc["type"] != "function" {
		t.Fatalf("type = %#v, want function", tc["type"])
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "ls" || fn["arguments"] != `{"path":"."}` {
		t.Fatalf("function = %#v", fn)
	}
	fr := m["choices"].([]any)[0].(map[string]any)["finish_reason"]
	if fr != "tool_calls" {
		t.Fatalf("finish_reason = %v, want tool_calls", fr)
	}
}

func TestConvertRawToolCallsInBodySkipsNative(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"see <tool_call> doc","tool_calls":[{"id":"call_x","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`)
	out := convertRawToolCallsInBody(body)
	if string(out) != string(body) {
		t.Fatal("native tool_calls body must not be modified")
	}
}

func TestWrapRawSSEConvertsSplitDSML(t *testing.T) {
	sse := "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"<tool\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"_call><name>ls</name><parameters>{\\\"path\\\":\\\".\\\"}</parameters></tool_call>\"}}]}\n\n" +
		"data: [DONE]\n\n"
	r := wrapRawSSE(io.NopCloser(strings.NewReader(sse)))
	outBytes, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	out := string(outBytes)
	if !strings.Contains(out, `"tool_calls"`) {
		t.Fatalf("output missing synthesized tool_calls:\n%s", out)
	}
	if strings.Contains(out, "<tool_call>") && !strings.Contains(out, "arguments") {
		t.Fatalf("raw markup leaked to client:\n%s", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Fatal("[DONE] lost")
	}
}

func TestWrapRawSSENativePassThrough(t *testing.T) {
	sse := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\"}]}}]}\n\n" +
		"data: [DONE]\n\n"
	r := wrapRawSSE(io.NopCloser(strings.NewReader(sse)))
	outBytes, _ := io.ReadAll(r)
	if string(outBytes) != sse {
		t.Fatalf("native stream must pass through unchanged, got:\n%s", string(outBytes))
	}
}
