// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"os"
	"sort"
	"strings"
	"testing"
)

// The hot-reload table in README.md is a claim about what a running process can act on, so it is
// checked against the code rather than trusted. A table that a document alone maintains can say a
// field is hot while the code reads it only on the startup path, and nothing else in this repository
// would notice. This test reads the table as a reader reads it — the field name in the first column
// and the decision in the second — and compares it with the boundary in boundary.go.
//
// It fails from either side: a row moved from restart to hot reload fails, and a field dropped from
// the code's set fails, because the restart rows have to be exactly the code's set and not a subset
// of it.
func TestTheHotReloadTableMatchesTheBoundary(t *testing.T) {
	data, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}

	rows := map[string]string{}
	lines := strings.Split(string(data), "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "| Setting | Change |") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatal("no Setting to Change table in README.md, so this test read nothing")
	}

	for _, line := range lines[start+1:] {
		if !strings.HasPrefix(line, "| ") {
			break
		}
		if strings.HasPrefix(line, "| ---") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 3 {
			continue
		}
		name := strings.Trim(strings.TrimSpace(cells[1]), "`")
		decision := strings.TrimSpace(cells[2])
		if decision != "hot reload" && decision != "restart" {
			t.Fatalf("row %q says %q, which is not one of the two decisions the table makes", name, decision)
		}
		if _, seen := rows[name]; seen {
			t.Fatalf("the table names %s twice, so a reader cannot tell which row a field is on", name)
		}
		rows[name] = decision
	}

	if len(rows) == 0 {
		t.Fatal("no table rows found in README.md, so this test read nothing")
	}

	restart := []string{}
	for name, decision := range rows {
		_, inBoundary := processLevelFields(&Config{})[name]
		if decision == "restart" {
			restart = append(restart, name)
			if !inBoundary {
				t.Errorf("the table says %s needs a restart, and the boundary does not name it", name)
			}
		} else if inBoundary {
			t.Errorf("the boundary says %s cannot be acted on by a running process, and the table calls it hot",
				name)
		}
	}

	sort.Strings(restart)
	want := []string{}
	for name := range processLevelFields(&Config{}) {
		want = append(want, name)
	}
	sort.Strings(want)

	if strings.Join(restart, ",") != strings.Join(want, ",") {
		t.Errorf("the restart rows are %v, and the boundary is %v", restart, want)
	}
}
