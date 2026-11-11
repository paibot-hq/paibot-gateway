package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
)

// formatResponses is how token usage is read from OpenAI Responses API
// answers. Requests to those endpoints are otherwise in the OpenAI format
// and go to OpenAI-format upstreams.
const formatResponses = "responses"

// responsesPaths are the Responses API endpoints Paibot Gateway serves. Each one
// names its model in the body. Endpoints that look up a stored response
// by id (GET and DELETE /responses/{id}, input_items, cancel) are not
// served: Paibot Gateway cannot tell which upstream holds the response.
var responsesPaths = []string{"/responses", "/responses/compact", "/responses/input_tokens"}

// prepareResponses applies Paibot Gateway's rule for the Responses API: unless
// the config allows it, upstreams must not keep conversations. It then
// sets "store": false on /responses, and refuses requests that build on
// a stored conversation. It reports whether the request continues a
// stored conversation; such a request must not fall back to another
// target, which would not have it.
func prepareResponses(allowStorage bool, suffix string, req map[string]json.RawMessage) (continues bool, err error) {
	continues = present(req["previous_response_id"]) || present(req["conversation"])
	if allowStorage {
		return continues, nil
	}
	if continues {
		return false, errors.New(`This gateway does not let upstreams store conversations, so "previous_response_id" and "conversation" cannot be used. Send the whole conversation in "input".`)
	}
	if suffix == "/responses" {
		if present(req["store"]) && string(bytes.TrimSpace(req["store"])) != "false" {
			return false, errors.New(`"store" must be false: this gateway does not let upstreams store conversations.`)
		}
		req["store"] = json.RawMessage("false")
	}
	return false, nil
}

// present reports whether a JSON field was sent with a value other than null.
func present(raw json.RawMessage) bool {
	v := bytes.TrimSpace(raw)
	return len(v) > 0 && string(v) != "null"
}

// responsesUsage is the token counts of the Responses API. Input includes
// cached tokens and output includes reasoning tokens, as OpenAI's prompt
// and completion counts do.
type responsesUsage struct {
	Input  *int `json:"input_tokens"`
	Output *int `json:"output_tokens"`
}

func (r *responsesUsage) usage() *usage {
	if r == nil || (r.Input == nil && r.Output == nil) {
		return nil
	}
	n := func(p *int) int {
		if p == nil {
			return 0
		}
		return *p
	}
	return &usage{PromptTokens: n(r.Input), CompletionTokens: n(r.Output), TotalTokens: n(r.Input) + n(r.Output)}
}

// responsesEvent covers a response body, which has usage at the top, and
// the stream events that end a response (response.completed, .incomplete
// and .failed), which nest it in "response".
type responsesEvent struct {
	Usage    *responsesUsage `json:"usage"`
	Response *struct {
		Usage *responsesUsage `json:"usage"`
	} `json:"response"`
}

func (e *responsesEvent) usage() *usage {
	if u := e.Usage.usage(); u != nil {
		return u
	}
	if e.Response != nil {
		return e.Response.Usage.usage()
	}
	return nil
}

// usageObject reads the value of a "usage" key in the given format.
func usageObject(format string, raw []byte) *usage {
	switch format {
	case apiAnthropic:
		var a, total anthropicUsage
		if json.Unmarshal(raw, &a) != nil {
			return nil
		}
		total.merge(&a)
		return total.usage()
	case formatResponses:
		var r responsesUsage
		if json.Unmarshal(raw, &r) != nil {
			return nil
		}
		return r.usage()
	}
	var o struct {
		Prompt     *int `json:"prompt_tokens"`
		Completion *int `json:"completion_tokens"`
	}
	if json.Unmarshal(raw, &o) != nil || (o.Prompt == nil && o.Completion == nil) {
		return nil
	}
	u := &usage{}
	if o.Prompt != nil {
		u.PromptTokens = *o.Prompt
	}
	if o.Completion != nil {
		u.CompletionTokens = *o.Completion
	}
	u.TotalTokens = u.PromptTokens + u.CompletionTokens
	return u
}

// tailUsage finds token usage at the end of a response too large to parse
// whole, so that a huge answer cannot slip past budgets. Every format puts
// usage after the generated content, and the key "usage" cannot appear
// unescaped inside a JSON string, so the last "usage" key whose value
// reads as usage is the response's.
func tailUsage(format string, tail []byte) *usage {
	key := []byte(`"usage":`)
	for end := len(tail); ; {
		i := bytes.LastIndex(tail[:end], key)
		if i < 0 {
			return nil
		}
		var raw json.RawMessage
		if json.NewDecoder(bytes.NewReader(tail[i+len(key):])).Decode(&raw) == nil {
			if u := usageObject(format, raw); u != nil {
				return u
			}
		}
		end = i
	}
}

// maxTail is how much of the end of an oversized response or stream line
// is kept to find usage in.
const maxTail = 64 << 10

// tailBuffer keeps the last maxTail bytes written to it.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	if len(p) >= maxTail {
		t.b = append(t.b[:0], p[len(p)-maxTail:]...)
		return len(p), nil
	}
	t.b = append(t.b, p...)
	if len(t.b) > 2*maxTail {
		t.b = append(t.b[:0], t.b[len(t.b)-maxTail:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) Bytes() []byte {
	if len(t.b) > maxTail {
		return t.b[len(t.b)-maxTail:]
	}
	return t.b
}

func (t *tailBuffer) Reset() { t.b = t.b[:0] }
