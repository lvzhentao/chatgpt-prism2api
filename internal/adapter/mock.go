package adapter

// MockBackend is an optional in-process fake upstream.
// Site --mock uses an in-process Client; this type only satisfies extracted Server fields.
type MockBackend struct{ Addr string }

func NewMockBackend(addr string) *MockBackend { return &MockBackend{Addr: addr} }
func (m *MockBackend) Start() error           { return nil }
func (m *MockBackend) Close()                 {}
