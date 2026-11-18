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
//
// The model name is treated as a route name, not as a model: the model a request gets is the
// one carried by the target that is picked, so the name in the request is the name the client
// chose, and the name in the response is the one the upstream wrote.
type peeked struct {
	Model  string
	Stream bool

	// Byte range of the model value in the original body, so it can be replaced with the model
	// of the selected target without re-serializing the request.
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
// it are kept, so the model value is the single change PlainRoute ever makes to a
// request, and the model it writes is the one carried by the selected target.
func (p *peeked) withModel(body []byte, model string) []byte {
	if p.Model == model || p.modelStart == 0 && p.modelEnd == 0 {
		return body
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return body
	}
	out := make([]byte, 0, len(body))
	out = append(out, body[:p.modelStart]...)
	out = append(out, encoded...)
	out = append(out, body[p.modelEnd:]...)
	return out
}
