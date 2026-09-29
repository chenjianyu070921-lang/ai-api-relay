package relay

import (
	"encoding/json"
	"testing"
)

func TestInjectStreamUsage(t *testing.T) {
	body := []byte(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	out, isStream, err := InjectStreamUsage(body)
	if err != nil {
		t.Fatal(err)
	}
	if !isStream {
		t.Fatal("expected stream=true")
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	opts, ok := m["stream_options"].(map[string]any)
	if !ok || opts["include_usage"] != true {
		t.Fatalf("include_usage not injected: %s", out)
	}
	t.Logf("injected: %s", out)
}
