// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"sort"
	"strconv"
)

// This is the line the hot-reload table in README.md draws, and it is drawn from the code and
// not from intent: a value a new request reads from the configuration in force can change while
// the process runs, and a value the process captured when it started cannot, because the
// listener, the server object, the logger and what the startup line reported were all built from
// the file as it was at startup.
//
// The keys are the names a reader writes in the file, and they are the names a report shows,
// because the file is a secret: the values are compared here and never printed. The table in
// README.md is compared with this set by a test, so the code and the docs cannot drift apart.
func processLevelFields(cfg *Config) map[string]string {
	return map[string]string{
		// Captured by net.Listen and by the http.Server this process created.
		"server.listen":              cfg.Server.Listen,
		"server.read_header_timeout": cfg.Server.ReadHeaderTimeout.String(),
		"server.read_timeout":        cfg.Server.ReadTimeout.String(),
		"server.write_timeout":       cfg.Server.WriteTimeout.String(),
		"server.idle_timeout":        cfg.Server.IdleTimeout.String(),
		"server.shutdown_grace":      cfg.Server.ShutdownGrace.String(),
		"server.max_header_bytes":    strconv.Itoa(cfg.Server.MaxHeaderBytes),

		// Captured by the logger, which is built once. There is no runtime logger swap, so a
		// change here cannot be acted on without starting the process again.
		"logging.format": cfg.Logging.Format,
		"logging.level":  cfg.Logging.Level,

		// Nothing in the request path reads the retry block: the gateway does not retry inside one
		// client request, so these values decide only what the startup line reports, and the
		// startup line is not run again. A field the running process cannot act on is not
		// half-applied; it is refused.
		"retry.enabled":      strconv.FormatBool(cfg.Retry.Enabled),
		"retry.max_attempts": strconv.Itoa(cfg.Retry.MaxAttempts),
	}
}

// RestartRequiredChanges names the process-level fields in which two configurations differ, in a
// stable order, so a refusal says which field it refused and a reader can compare two reports.
// It reports names only: a value read from the file never comes back out.
//
// The comparison is between the configuration in force and the one the file now says, so a change
// that was refused stays refused: writing a hot-reloadable field does not quietly apply the
// process-level change that was refused with it.
//
// Comparing parsed configurations is what makes an absent field and its default the same value: a
// file that stopped writing server.listen is not a change, because both sides resolve to the value
// in force.
func RestartRequiredChanges(inForce, next *Config) []string {
	if inForce == nil || next == nil {
		return nil
	}

	var names []string
	before := processLevelFields(inForce)
	for name, value := range processLevelFields(next) {
		if before[name] != value {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
