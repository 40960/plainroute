// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"strings"
	"testing"
)

// The needle is a value that must never be printed anywhere. It is not a real key, and it is not
// the value a test uses to make something happen: the point is only whether it comes back out.
const secret = "PLAINROUTE_TEST_SECRET_DO_NOT_LEAK_918273645"

// A config file is a secret now, so the error a bad file produces has to be safe to print: it is
// what a terminal shows, what a log line carries, and what a bug report pastes.
//
// What is measured here, and what the package comment claims:
//   - the yaml decoder reports the position and the key that is wrong, not the text on the line,
//     so a bad key name or a bad node says where it is and not what it holds;
//   - validate names the field and the problem it found, and echoes back only the value it
//     rejected, which is a duration or a mode name;
//   - a key value cannot be a rejected value, because nothing in this package can find an api key
//     wrong except that it is empty.
//
// So printing a parse error is safe, and this test is the pin on that. If a future message echoed
// the value it was complaining about, this fails.
func TestAValidationErrorDoesNotPrintTheKey(t *testing.T) {
	_, err := Parse([]byte(`
logging:
  level: ` + secret + `
routes:
  demo:
    targets:
      - name: account-one
        model: provider/model-one
        base_url: https://example.invalid/v1
        api_key: ` + secret + `
`))
	if err == nil {
		t.Fatal("expected the bad level to be rejected")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the error printed a value from the file: %v", err)
	}

	// The same file with a value validate does echo: a duration it refuses. The number comes back,
	// which is what makes the message useful, and the key still does not.
	_, err = Parse([]byte(`
timeouts:
  connect: -1s
routes:
  demo:
    targets:
      - name: account-one
        model: provider/model-one
        base_url: https://example.invalid/v1
        api_key: ` + secret + `
`))
	if err == nil {
		t.Fatal("expected the negative duration to be rejected")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the error printed a value from the file: %v", err)
	}
}

// A decode error names the key that is unknown and where it is, and here it names the wrong key
// without repeating the value sitting under it.
func TestADecodeErrorDoesNotPrintTheKey(t *testing.T) {
	_, err := Parse([]byte(`
logging:
  nope: ` + secret + `
routes:
  demo:
    targets:
      - name: account-one
        model: provider/model-one
        base_url: https://example.invalid/v1
        api_key: ` + secret + `
`))
	if err == nil {
		t.Fatal("expected the unknown key to be rejected")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the error printed a value from the file: %v", err)
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("the error must name the key it refused: %v", err)
	}
}
