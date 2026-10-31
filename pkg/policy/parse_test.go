package policy

import "testing"

// What Parse must get right beyond spec/conformance: the conformance cases only reach the schema's enums and ids.
func TestParse(t *testing.T) {
	for src, ok := range map[string]bool{
		`{"rules":[{"id":"r","on":"mcp","effect":"allow","limit":{"window_s":600.0,"max":1}}]}`:               true,
		`{"rules":[{"id":"r","on":"mcp","effect":"allow","limit":{"window_s":0.5,"max":1}}]}`:                 false,
		`{"rules":[{"id":"r","on":"mcp","effect":"allow","limit":{"window_s":60}}]}`:                          false,
		`{"default":"allow"} {"default":"deny"}`:                                                              false,
		`{"rules":[{"id":"r","on":"mcp","effect":"allow","extra":1}]}`:                                        false,
		`{"rules":[{"id":"r","on":"mcp","effect":"allow","when":{"tz":"Mars/Base"}}]}`:                        false,
		`{"rules":[{"id":"r","on":"mcp","effect":"allow","when":{"tz":"Local"}}]}`:                            false,
		`{"rules":[{"id":"r","on":"mcp","effect":"allow","when":{"from":"24:00"}}]}`:                          false,
		`{"rules":[{"id":"r","on":"mcp","effect":"allow","where":[{"field":"a..b","op":"is","value":"x"}]}]}`: false,
		`{"rules":[{"id":"r","on":"mcp","effect":"allow","where":[{"field":"a","op":"is","value":1}]}]}`:      false,
		`{"rules":[{"id":"r","on":"mcp","effect":"allow","where":[{"field":"a","op":"in","value":null}]}]}`:   false,
		`{"rules":[{"id":"r","on":"mcp","effect":"allow","where":[{"field":"a","op":"gt"}]}]}`:                false,
	} {
		if _, err := Parse([]byte(src)); (err == nil) != ok {
			t.Errorf("Parse(%s): err=%v, want ok=%v", src, err, ok)
		}
	}
}
