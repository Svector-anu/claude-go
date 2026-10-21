package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func echoServer() *Server {
	return &Server{
		Name: "t", Version: "1", Instructions: "be nice",
		Tools: []Tool{
			{Name: "echo", Description: "echoes", InputSchema: map[string]any{"type": "object"},
				Annotations: map[string]any{"readOnlyHint": true},
				Handler: func(_ context.Context, args json.RawMessage) Result {
					var a struct{ Text string }
					if err := json.Unmarshal(args, &a); err != nil {
						return Result{Text: "bad arguments", IsError: true}
					}
					return Result{Text: "echo: " + a.Text}
				}},
			{Name: "boom", Description: "panics", InputSchema: map[string]any{"type": "object"},
				Handler: func(context.Context, json.RawMessage) Result { panic("kaboom") }},
			{Name: "big", Description: "huge output", InputSchema: map[string]any{"type": "object"},
				Handler: func(context.Context, json.RawMessage) Result {
					return Result{Text: strings.Repeat("x", maxResultBytes*2)}
				}},
		},
	}
}

// exchange feeds the input lines to a fresh server and returns every response line.
func exchange(t *testing.T, s *Server, input string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	var resps []map[string]any
	sc := bufio.NewScanner(&out)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("server wrote a line that is not JSON: %q", sc.Text())
		}
		resps = append(resps, m)
	}
	return resps
}

func result(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	r, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", m)
	}
	return r
}

func errCode(m map[string]any) float64 {
	e, _ := m["error"].(map[string]any)
	c, _ := e["code"].(float64)
	return c
}

func TestInitializeNegotiatesTheProtocolVersion(t *testing.T) {
	for in, want := range map[string]string{"2025-03-26": "2025-03-26", "2024-11-05": "2024-11-05", "2025-06-18": "2025-06-18", "1999-01-01": "2025-06-18", "": "2025-06-18"} {
		r := exchange(t, echoServer(), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+in+`","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`+"\n")
		res := result(t, r[0])
		if res["protocolVersion"] != want {
			t.Errorf("client asked %q, got %v, want %q", in, res["protocolVersion"], want)
		}
		if res["instructions"] != "be nice" || res["serverInfo"].(map[string]any)["name"] != "t" {
			t.Errorf("result = %v", res)
		}
		if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
			t.Error("tools capability not advertised")
		}
	}
}

func TestNotificationsGetNoResponseAndOrderIsPreserved(t *testing.T) {
	in := `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`{"jsonrpc":"2.0","id":7,"method":"ping"}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}` + "\n" +
		`{"jsonrpc":"2.0","id":"abc","method":"ping"}` + "\n"
	r := exchange(t, echoServer(), in)
	if len(r) != 2 || r[0]["id"].(float64) != 7 || r[1]["id"] != "abc" {
		t.Errorf("responses = %v", r)
	}
}

func TestToolsListCarriesSchemasAndAnnotations(t *testing.T) {
	r := exchange(t, echoServer(), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n")
	tools := result(t, r[0])["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("tools = %v", tools)
	}
	first := tools[0].(map[string]any)
	if first["name"] != "echo" || first["inputSchema"].(map[string]any)["type"] != "object" || first["annotations"].(map[string]any)["readOnlyHint"] != true {
		t.Errorf("tool = %v", first)
	}
	if _, has := tools[1].(map[string]any)["annotations"]; has {
		t.Error("annotations should be omitted when empty")
	}
}

func TestToolCallSuccessAndToolLevelErrors(t *testing.T) {
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":"not an object"}}` + "\n" +
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo"}}` + "\n"
	r := exchange(t, echoServer(), in)
	text := func(m map[string]any) (string, bool) {
		res := result(t, m)
		return res["content"].([]any)[0].(map[string]any)["text"].(string), res["isError"].(bool)
	}
	if txt, isErr := text(r[0]); txt != "echo: hi" || isErr {
		t.Errorf("success = %q err=%v", txt, isErr)
	}
	if txt, isErr := text(r[1]); !isErr || txt != "bad arguments" {
		t.Errorf("bad arguments = %q err=%v (a tool-level failure is a result, not a protocol error)", txt, isErr)
	}
	if _, isErr := text(r[2]); isErr {
		t.Error("missing arguments should be treated as an empty object")
	}
}

func TestProtocolErrors(t *testing.T) {
	cases := []struct {
		name, line string
		code       float64
	}{
		{"malformed json", `{not json`, codeParse},
		{"batch", `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, codeInvalidRequest},
		{"wrong version", `{"jsonrpc":"1.0","id":1,"method":"ping"}`, codeInvalidRequest},
		{"no method", `{"jsonrpc":"2.0","id":1}`, codeInvalidRequest},
		{"unknown method", `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`, codeMethodNotFound},
		{"unknown tool", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope"}}`, codeInvalidParams},
		{"call without a name", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`, codeInvalidParams},
		{"call with junk params", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"x"}`, codeInvalidParams},
	}
	for _, c := range cases {
		r := exchange(t, echoServer(), c.line+"\n")
		if len(r) != 1 || errCode(r[0]) != c.code {
			t.Errorf("%s: got %v, want code %v", c.name, r, c.code)
		}
	}
}

func TestAPanickingToolIsContainedAndTheServerKeepsServing(t *testing.T) {
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"boom"}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n"
	r := exchange(t, echoServer(), in)
	if len(r) != 2 || !result(t, r[0])["isError"].(bool) {
		t.Fatalf("responses = %v", r)
	}
	if strings.Contains(r[0]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string), "kaboom") {
		t.Error("the panic message leaked to the client")
	}
}

func TestHugeToolOutputIsTruncated(t *testing.T) {
	r := exchange(t, echoServer(), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"big"}}`+"\n")
	txt := result(t, r[0])["content"].([]any)[0].(map[string]any)["text"].(string)
	if len(txt) > maxResultBytes+100 || !strings.HasSuffix(txt, "[output truncated]") {
		t.Errorf("output of %d bytes", len(txt))
	}
}

func TestAnOversizedMessageIsRejectedWithoutDesynchronisingTheStream(t *testing.T) {
	huge := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"` + strings.Repeat("x", maxMessageBytes+10) + `"}}`
	in := huge + "\n" + `{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n"
	r := exchange(t, echoServer(), in)
	if len(r) != 2 || errCode(r[0]) != codeInvalidRequest || r[1]["id"].(float64) != 2 {
		t.Errorf("responses = %v", r)
	}
}

func TestBlankLinesAndWindowsLineEndingsAreTolerated(t *testing.T) {
	in := "\n\r\n" + `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\r\n\n"
	if r := exchange(t, echoServer(), in); len(r) != 1 {
		t.Errorf("responses = %v", r)
	}
}

func TestEOFWithoutATrailingNewlineStillAnswersTheLastRequest(t *testing.T) {
	if r := exchange(t, echoServer(), `{"jsonrpc":"2.0","id":1,"method":"ping"}`); len(r) != 1 {
		t.Errorf("responses = %v", r)
	}
}

func TestServeStopsWhenTheContextIsCancelledEvenWithNoInputArriving(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- echoServer().Serve(ctx, pr, io.Discard) }()
	time.Sleep(50 * time.Millisecond) // Serve is now blocked waiting for input
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after cancellation while idle")
	}
}

func TestResponsesAreSingleLines(t *testing.T) {
	var out bytes.Buffer
	echoServer().Serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"line1\nline2"}}}`+"\n"), &out)
	if strings.Count(out.String(), "\n") != 1 {
		t.Errorf("a response must be exactly one line (the transport is newline delimited): %q", out.String())
	}
}
