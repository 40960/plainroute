// SPDX-License-Identifier: AGPL-3.0-only

// plainroute-fakeupstream is the controllable upstream used by the end-to-end script
// and by a local run when a gateway should not be pointed at a real provider.
//
// Behaviour comes from the API key the gateway presents, because a key is what
// identifies a target, or from the path part of the target base URL, which is
// how the script checks one case at a time. GET /stats reports what the server saw, so
// a script can check that client requests and upstream attempts are the same number, and
// GET /last?key= reports the last request received for one key as one line per fact, which
// is what a script greps when it needs to know what the gateway forwarded.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/40960/plainroute/internal/fakeupstream"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8091", "address to listen on")
	behaviors := flag.String("behaviors", "", "JSON file mapping an API key value to a behaviour")
	flag.Parse()

	server := fakeupstream.New()
	configured := 0
	if *behaviors != "" {
		loaded, err := fakeupstream.LoadBehaviors(*behaviors)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fakeupstream: %v\n", err)
			os.Exit(1)
		}
		for key, behavior := range loaded {
			server.Set(key, behavior)
			configured++
		}
	}

	fmt.Printf("fakeupstream on http://%s: %d keyed behaviours, named paths, stats at /stats, "+
		"the last request for a key at /last?key=\n", *listen, configured)

	if err := http.ListenAndServe(*listen, server); err != nil {
		fmt.Fprintf(os.Stderr, "fakeupstream: %v\n", err)
		os.Exit(1)
	}
}
