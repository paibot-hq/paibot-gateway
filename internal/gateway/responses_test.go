package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func responsesOK(w http.ResponseWriter, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"id":"resp_1","object":"response","model":%q,"output":[],"usage":{"input_tokens":11,"input_tokens_details":{"cached_tokens":4},"output_tokens":7,"output_tokens_details":{"reasoning_tokens":3},"total_tokens":18}}`, body["model"])
}

func TestResponses(t *testing.T) {
	var got seen
	var store any
	up := fakeUpstream(t, &got, func(w http.ResponseWriter, body map[string]any) {
		store = body["store"]
		responsesOK(w, body)
	})
	var logs bytes.Buffer
	g := newTestGateway(t, config(up.URL), &logs)

	rec := do(g, "POST", "/v1/responses", testKey, `{"model":"chat","input":"top secret prompt"}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got.path != "/v1/responses" || got.model != "real-model-0" || got.auth != "Bearer upstream-secret" {
		t.Errorf("upstream got %+v", got)
	}
	if store != false {
		t.Errorf("store = %v, want false", store)
	}
	if l := logs.String(); !strings.Contains(l, `"prompt_tokens":11`) || !strings.Contains(l, `"completion_tokens":7`) || strings.Contains(l, "top secret") {
		t.Errorf("log: %s", l)
	}

	for _, p := range []string{"/v1/responses/compact", "/v1/responses/input_tokens"} {
		if rec := do(g, "POST", p, testKey, `{"model":"chat","input":"x"}`); rec.Code != 200 || got.path != p {
			t.Errorf("%s: %d, upstream path %q", p, rec.Code, got.path)
		}
	}
	if rec := do(g, "GET", "/v1/responses/resp_1", testKey, ""); rec.Code != 404 {
		t.Errorf("GET by id: %d", rec.Code)
	}
}

func TestResponsesStorageRefused(t *testing.T) {
	calls := 0
	up := fakeUpstream(t, nil, func(w http.ResponseWriter, body map[string]any) {
		calls++
		responsesOK(w, body)
	})
	g := newTestGateway(t, config(up.URL), nil)
	for _, body := range []string{
		`{"model":"chat","input":"x","store":true}`,
		`{"model":"chat","input":"x","previous_response_id":"resp_1"}`,
		`{"model":"chat","input":"x","conversation":"conv_1"}`,
	} {
		rec := do(g, "POST", "/v1/responses", testKey, body)
		if rec.Code != 400 || errCode(t, rec) != "invalid_request" {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
	for _, body := range []string{
		`{"model":"chat","input":"x","store":false}`,
		`{"model":"chat","input":"x","previous_response_id":null}`,
	} {
		if rec := do(g, "POST", "/v1/responses", testKey, body); rec.Code != 200 {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
	if calls != 2 {
		t.Errorf("upstream called %d times, want 2", calls)
	}
}

func TestResponsesStorageAllowed(t *testing.T) {
	var store any
	stored := fakeUpstream(t, nil, func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	other := fakeUpstream(t, nil, func(w http.ResponseWriter, body map[string]any) {
		store = body["store"]
		responsesOK(w, body)
	})
	cfg := strings.Replace(config(stored.URL, other.URL), `"upstreams"`, `"allow_response_storage": true, "upstreams"`, 1)
	g := newTestGateway(t, cfg, nil)

	// A new conversation may fall back, and store is left to the client.
	if rec := do(g, "POST", "/v1/responses", testKey, `{"model":"chat","input":"x"}`); rec.Code != 200 || store != nil {
		t.Errorf("new conversation: %d, store %v", rec.Code, store)
	}
	// Continuing one stays on the first target, which holds it.
	rec := do(g, "POST", "/v1/responses", testKey, `{"model":"chat","input":"x","previous_response_id":"resp_1"}`)
	if rec.Code != 503 {
		t.Errorf("continued conversation fell back: %d", rec.Code)
	}
}

func TestResponsesOnAnthropicModel(t *testing.T) {
	up := fakeAnthropic(t, nil, anthropicOK)
	g := newTestGateway(t, anthropicConfig(up.URL), nil)
	rec := do(g, "POST", "/v1/responses", testKey, `{"model":"sonnet","input":"x"}`)
	if rec.Code != 400 || errCode(t, rec) != "invalid_request" {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

func TestResponsesStreaming(t *testing.T) {
	up := fakeUpstream(t, nil, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"usage\":null}}\n\n")
		fl.Flush()
		io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
		fl.Flush()
		io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[],\"usage\":{\"input_tokens\":9,\"output_tokens\":4,\"total_tokens\":13}}}\n\n")
	})
	var logs bytes.Buffer
	g := newTestGateway(t, config(up.URL), &logs)
	srv := httptest.NewServer(g)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/responses", strings.NewReader(`{"model":"chat","input":"x","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"delta":"hi"`) || !strings.Contains(string(b), "response.completed") {
		t.Fatalf("stream not relayed: %q", b)
	}
	srv.Close()
	if l := logs.String(); !strings.Contains(l, `"prompt_tokens":9`) || !strings.Contains(l, `"completion_tokens":4`) {
		t.Errorf("stream usage not logged: %s", l)
	}
}

// A long answer must not slip past budgets: Responses streams repeat it in
// their last event, making a line longer than sseUsage keeps.
func TestUsageFromOversizedLine(t *testing.T) {
	text, _ := json.Marshal(strings.Repeat(`a "usage":{"input_tokens":0} `, maxLine/20))
	ev := fmt.Sprintf(`data: {"type":"response.completed","response":{"output":[{"text":%s}],"usage":{"input_tokens":5,"output_tokens":300000},"metadata":{"usage":"0"}}}`+"\n\n", text)
	if len(ev) <= maxLine {
		t.Fatal("event too short for the test")
	}
	s := sseUsage{api: formatResponses}
	for i := 0; i < len(ev); i += 4096 {
		s.Write([]byte(ev[i:min(i+4096, len(ev))]))
	}
	if s.usage == nil || s.usage.CompletionTokens != 300000 {
		t.Fatalf("usage = %+v", s.usage)
	}
}

func TestUsageFromOversizedBody(t *testing.T) {
	text, _ := json.Marshal(strings.Repeat("x", maxCapture))
	up := fakeUpstream(t, nil, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%s}}],"usage":{"prompt_tokens":2,"completion_tokens":1000000,"total_tokens":1000002}}`, text)
	})
	var logs bytes.Buffer
	g := newTestGateway(t, config(up.URL), &logs)
	if rec := do(g, "POST", "/v1/chat/completions", testKey, `{"model":"chat"}`); rec.Code != 200 || rec.Body.Len() < maxCapture {
		t.Fatalf("%d, %d bytes", rec.Code, rec.Body.Len())
	}
	if l := logs.String(); !strings.Contains(l, `"completion_tokens":1000000`) {
		t.Errorf("usage of an oversized body not logged: %s", l)
	}
}

func TestTailBuffer(t *testing.T) {
	var tb tailBuffer
	var all []byte
	for i := range 5000 {
		p := []byte(fmt.Sprintf("%d,", i))
		tb.Write(p)
		all = append(all, p...)
	}
	big := bytes.Repeat([]byte("z"), maxTail+10)
	tb.Write(big)
	all = append(all, big...)
	tb.Write([]byte("end"))
	all = append(all, "end"...)
	if !bytes.Equal(tb.Bytes(), all[len(all)-maxTail:]) {
		t.Error("tail differs")
	}
}
