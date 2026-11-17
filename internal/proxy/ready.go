// SPDX-License-Identifier: AGPL-3.0-only

package proxy

// MarkReady reports that the config loaded and the HTTP server can serve. The
// gateway is not ready before it is listening, and one upstream returning 429
// never makes the whole gateway unready.
func (h *Handler) MarkReady() { h.ready.Store(true) }
