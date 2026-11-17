// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
)

// peeked is what PlainRoute reads out of a request body. It is deliberately
// narrow: only the model name and the stream flag are needed to route a request.
// Everything else stays exactly as the client sent it.
type peeked struct {
	Model  string
	Stream bool

	// Byte range of the model value in the original body, so a model alias can
	// be rewritten without re-serializing the request.
	modelStart int
	modelEnd   int
}

// peekBody reads the top-level model and stream fields without rebuilding the
// document. Unknown fields are not validated, not re-encoded and not dropped.
func peekBody(data []byte) (*peeked, error) {
	dec := json.NewDecoder(bytes.NewReader(data))

	token, err := dec.Token()
	if err != nil {
		return nil, errors.New("request body must be a JSON object")
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return nil, errors.New("request body must be a JSON object")
	}

	p := &peeked{}
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errors.New("object key must be a string")
		}

		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		// InputOffset is the position after the value, so the value bytes are the
		// range [end-len(raw), end]. Counting from the end keeps the separator
		// between the key and the value intact.
		end := int(dec.InputOffset())
		start := end - len(raw)

		switch key {
		case "model":
			var model string
			if err := json.Unmarshal(raw, &model); err != nil {
				return nil, errors.New(`field "model" must be a string`)
			}
			p.Model = model
			p.modelStart, p.modelEnd = int(start), int(end)
		case "stream":
			var stream bool
			if err := json.Unmarshal(raw, &stream); err == nil {
				p.Stream = stream
			}
			// A non-boolean stream value is left alone: the gateway does not
			// decide the response shape, the upstream content type does.
		}
	}

	return p, nil
}

// withModel returns the body with only the model value replaced. The bytes around
// it are kept, so a mapped alias is the single change PlainRoute ever makes to a
// request.
func (p *peeked) withModel(body []byte, upstreamModel string) []byte {
	if p.Model == upstreamModel || p.modelStart == 0 && p.modelEnd == 0 {
		return body
	}
	encoded, err := json.Marshal(upstreamModel)
	if err != nil {
		return body
	}
	out := make([]byte, 0, len(body))
	out = append(out, body[:p.modelStart]...)
	out = append(out, encoded...)
	out = append(out, body[p.modelEnd:]...)
	return out
}
