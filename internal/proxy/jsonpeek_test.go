// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"bytes"
	"testing"
)

func TestPeekBodyAcceptsOneCompleteObjectWithUnknownFields(t *testing.T) {
	body := []byte(" {\n  \"model\": \"demo\", \"future\": {\"x\": [1,2]}, \"stream\": true\n}\n")
	peek, err := peekBody(body)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if peek.Model != "demo" || !peek.Stream {
		t.Errorf("peek: %+v", peek)
	}
	// Unknown fields are byte-preserving: replacing the model changes only those bytes.
	out := peek.withModel(body, "target-model")
	if want := []byte(" {\n  \"model\": \"target-model\", \"future\": {\"x\": [1,2]}, \"stream\": true\n}\n"); !bytes.Equal(out, want) {
		t.Errorf("withModel changed bytes other than the model:\n%s", out)
	}
}

func TestPeekBodyRefusesAnythingButOneCompleteObject(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"truncated", `{"model":"m"`},
		{"trailing garbage", `{"model":"m"} garbage`},
		{"second document", `{"model":"m"}{"x":1}`},
		{"duplicate model", `{"model":"a","model":"b"}`},
		{"not an object", `["m"]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := peekBody([]byte(c.body)); err == nil {
				t.Errorf("%s was accepted: %q", c.name, c.body)
			}
		})
	}
}
