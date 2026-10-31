package decisions

import (
	"encoding/json"
	"strings"
	"testing"
)

func req(t *testing.T, s string) map[string]any {
	t.Helper()
	var r map[string]any
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDecisions(t *testing.T) {
	mcp := req(t, `{"agent":"bot","on":"mcp","at":1,"server":"crm","tool":"refund","args":{"order_id":"1182","amount":240,"note":"a”,\"x\":\"<b>"}}`)
	http := req(t, `{"agent":"bot","on":"http","at":1,"method":"POST","path":"/refunds","body":[1,2]}`)
	k8s := req(t, `{"agent":"bot","on":"k8s","at":1,"verb":"delete","kind":"Pod","namespace":"ops","name":"web-1"}`)

	for _, c := range []struct{ got, want string }{
		{Target(mcp), "crm/refund"},
		{Target(http), "POST /refunds"},
		{Target(k8s), "delete Pod ops/web-1"},
		{marshal(Arguments(mcp, false)), `["amount","note","order_id"]`},
		{marshal(Arguments(http, false)), `[]`}, // a body that isn't an object has no fields
		{marshal(Arguments(k8s, true)), `{}`},
		// The quote lookalike is escaped, so the note can't pass for another argument
		{Summary(mcp), `crm/refund {"amount":240,"note":"a\u201d,\"x\":\"<b>","order_id":"1182"}`},
	} {
		if c.got != c.want {
			t.Errorf("got  %s\nwant %s", c.got, c.want)
		}
	}

	long := req(t, `{"on":"mcp","server":"s","tool":"t","args":{"a":"`+strings.Repeat("x", 5000)+`","b":"​hidden","z":"last"}}`)
	if n := Arguments(req(t, `{"on":"mcp","args":{"`+strings.Repeat("k", 5000)+`":1}}`), false).([]string)[0]; len(n) > 302 {
		t.Errorf("name not clipped: %d bytes", len(n))
	}
	if v := Arguments(long, true).(map[string]string)["a"]; len([]rune(v)) != 300 || !strings.HasSuffix(v, "…") {
		t.Errorf("value not clipped to 300: %d", len([]rune(v)))
	}
	s := Summary(long)
	if len([]rune(s)) > 2000 || !strings.Contains(s, `"z":"last"`) || !strings.Contains(s, `\u200bhidden`) {
		t.Errorf("summary must keep every argument, escape invisible characters and stay ≤2000: %s", s)
	}
	many := map[string]any{"on": "mcp", "args": map[string]any{}}
	for i := 0; i < 1000; i++ {
		many["args"].(map[string]any)[strings.Repeat("k", 50)+string(rune('a'+i%26))+strings.Repeat("y", i)] = i
	}
	if n := len([]rune(Summary(many))); n > 2000 {
		t.Errorf("summary of 1000 arguments is %d characters", n)
	}

	// Same request, different time and key order: same hash. Different arguments: different hash.
	again := req(t, `{"args":{"note":"a”,\"x\":\"<b>","amount":240,"order_id":"1182"},"tool":"refund","server":"crm","on":"mcp","at":99,"agent":"bot"}`)
	if RequestHash(mcp) != RequestHash(again) || len(RequestHash(mcp)) != 64 {
		t.Error("hash must ignore at and key order")
	}
	again["args"].(map[string]any)["amount"] = 241.0
	if RequestHash(mcp) == RequestHash(again) {
		t.Error("hash must change with the arguments")
	}
	if _, ok := mcp["at"]; !ok {
		t.Error("RequestHash must not change the request")
	}

	ev, _ := json.Marshal(Event{On: "mcp", Decision: Unauthenticated, Arguments: Arguments(mcp, false)})
	if !strings.Contains(string(ev), `"agent":null`) || !strings.Contains(string(ev), `"upstream_status":null`) {
		t.Errorf("unset fields must be null: %s", ev)
	}
}
