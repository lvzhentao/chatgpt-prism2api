package prism

import (
	"context"
	"net/http"

	"prism-2api/internal/adapter"
	"prism-2api/internal/egress"
)

// mockClient lets --mock boot the admin SPA and /v1 without a real vendor.
type mockClient struct{}

func newMockClient() *mockClient { return &mockClient{} }

func (c *mockClient) SetBaseURL(string)               {}
func (c *mockClient) SetHTTPClient(*http.Client)      {}
func (c *mockClient) SetEgress(egress.Settings) error { return nil }
func (c *mockClient) ListModels(context.Context) ([]adapter.ModelInfo, error) {
	out := make([]adapter.ModelInfo, 0, len(staticModels()))
	for _, m := range staticModels() {
		out = append(out, *m)
	}
	return out, nil
}
func (c *mockClient) FetchUsage(context.Context, string) (adapter.UsageSnapshot, error) {
	p := 12.5
	return adapter.UsageSnapshot{Email: "mock@example.com", PlanLabel: "mock", TotalPercentUsed: &p, Unlimited: false}, nil
}
func (c *mockClient) Stream(_ context.Context, _ *adapter.NativeRequest, emit func(adapter.Event) bool) error {
	emit(adapter.Event{Text: "Hello from " + DisplayName + " mock.\n"})
	emit(adapter.Event{Ended: true, OutputTokens: 8, InputTokens: 4})
	return nil
}

// MockBackend is kept so extracted server.go field types still compile.
// The in-process mock client is preferred; this type is a no-op host.
type MockBackend struct{ Addr string }

func NewMockBackend(addr string) *MockBackend { return &MockBackend{Addr: addr} }
func (m *MockBackend) Start() error           { return nil }
func (m *MockBackend) Close()                 {}
