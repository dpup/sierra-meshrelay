package relay

import "testing"

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"SIERRA Elmer":     "sierra-elmer",
		"Jay's PC":         "jay-s-pc",
		"  --Node__42!!  ": "node-42",
		"🛰 Café Node":      "caf-node",
		"":                 "",
		"a-very-long-node-name-that-goes-on-and-on-forever": "a-very-long-node-name-that-goes-on-a",
	} {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}
