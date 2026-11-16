package externalcli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
)

func TestAGYBridgeBoundariesAndResult(t *testing.T) {
	bridge, err := newAGYBridge(t.Context(), []llm.ToolSchema{{Name: "echo", InputSchema: map[string]any{"type": "object"}}}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.close()
	client := &http.Client{Timeout: 3 * time.Second}
	for _, test := range []struct {
		url, origin string
		status      int
	}{{bridge.URL + "wrong", "", 403}, {bridge.URL, "https://example.org", 403}} {
		req, _ := http.NewRequest("POST", test.url, bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		if test.origin != "" {
			req.Header.Set("Origin", test.origin)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != test.status {
			t.Fatal("MCP capability/origin check failed")
		}
	}
	done := make(chan error, 1)
	go func() {
		response, e := client.Post(bridge.URL, "application/json", bytes.NewBufferString(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"echo","arguments":{"value":"hello"}}}`))
		if e != nil {
			done <- e
			return
		}
		defer response.Body.Close()
		var result struct {
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		e = json.NewDecoder(response.Body).Decode(&result)
		if e == nil && (!result.Result.IsError || len(result.Result.Content) != 1 || result.Result.Content[0].Text != "guard denied") {
			e = errors.New("MCP error result was lost")
		}
		done <- e
	}()
	select {
	case call := <-bridge.Calls:
		if call.Name != "echo" || string(call.Arguments) != `{"value":"hello"}` {
			t.Fatal("tool arguments changed")
		}
		if err := call.answer(t.Context(), llm.ToolResultText(call.ID, "guard denied", true)); err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("MCP call was not dispatched")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
