package management

import "context"

const codexManagementUpstreamConcurrency = 12

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

	h.codexUpstreamSlotsOnce.Do(func() {
		h.codexUpstreamSlots = make(chan struct{}, codexManagementUpstreamConcurrency)
	})
	slots := h.codexUpstreamSlots

	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
