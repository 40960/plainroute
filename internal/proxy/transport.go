// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"net"
	"net/http"
	"time"

	"github.com/40960/plainroute/internal/config"
)

// newTransport builds the upstream transport. Timeouts are separated so a long
// reasoning stream is not killed by an ordinary request timeout.
//
// Compression is left to the client and the upstream: PlainRoute never
// decompresses, so the bytes it copies are the bytes the upstream sent.
func newTransport(cfg *config.Config) *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   cfg.Timeouts.Connect.Std(),
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: cfg.Timeouts.ResponseHeader.Std(),
		IdleConnTimeout:       cfg.Server.IdleTimeout.Std(),
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		DisableCompression:    true,
		ForceAttemptHTTP2:     true,
	}
}

// noRedirect stops the gateway from following a redirect quietly: the client sees
// the upstream response as it is, and the gateway never sends a request to a URL
// that was not in its configuration.
func noRedirect(_ *http.Request, via []*http.Request) error {
	return http.ErrUseLastResponse
}
