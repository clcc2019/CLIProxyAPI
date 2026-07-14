package management

import "context"

const codexManagementUpstreamConcurrency = 4

// acquireCodexUpstreamSlot limits aggregate Codex management requests across
// credentials. Waiting follows the caller's context, while each upstream
// operation starts its own timeout only after it obtains a slot.
func (h *Handler) acquireCodexUpstreamSlot(ctx context.Context) (func(), error) {
	if h == nil {
		return func() {}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	h.mu.Lock()
	if h.codexUpstreamSlots == nil {
		h.codexUpstreamSlots = make(chan struct{}, codexManagementUpstreamConcurrency)
	}
	slots := h.codexUpstreamSlots
	h.mu.Unlock()

	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
