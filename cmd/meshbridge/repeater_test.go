package main

import (
	"bufio"
	"strings"
	"testing"

	"github.com/dpup/sierra-meshrelay/internal/admin"
)

func nearby(names ...string) []admin.NearbyRepeater {
	var out []admin.NearbyRepeater
	for i, n := range names {
		out = append(out, admin.NearbyRepeater{Name: n, Pubkey: strings.Repeat(string("0123456789abcdef"[i]), 64)})
	}
	return out
}

func TestPickNearby(t *testing.T) {
	near := nearby("zeta RPT", "Ridge RPT", "alpha", "Ridgecrest", "Mission Hill")
	pick := func(want, input string, interactive bool) (string, error) {
		r, err := pickNearby(near, want, bufio.NewReader(strings.NewReader(input)), interactive)
		return r.Name, err
	}
	cases := []struct {
		want, input string
		interactive bool
		name, err   string
	}{
		{"ridge rpt", "", false, "Ridge RPT", ""},             // exact name, any case
		{"4444", "", false, "Mission Hill", ""},               // pubkey prefix
		{"mission", "", false, "Mission Hill", ""},            // one name contains it
		{"ridge", "", false, "", "name the repeater"},         // several: needs a terminal
		{"ridge", "2\n", true, "Ridgecrest", ""},              // narrowed, sorted: Ridge RPT, Ridgecrest
		{"nowhere", "", false, "", "doesn't know a repeater"}, // no match
		{"", "1\n", true, "alpha", ""},                        // sorted by name, ignoring case
		{"", "5\n", true, "zeta RPT", ""},                     //
		{"", "hill\n1\n", true, "Mission Hill", ""},           // search, then pick
		{"", "xyz\nzeta\n1\n", true, "zeta RPT", ""},          // a search with no hits keeps asking
		{"", "9\n", true, "", "no repeater chosen"},           // out of range, then EOF
	}
	for _, c := range cases {
		name, err := pick(c.want, c.input, c.interactive)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%q/%q: got %q, %v; want error %q", c.want, c.input, name, err, c.err)
			}
			continue
		}
		if err != nil || name != c.name {
			t.Errorf("%q/%q: got %q, %v; want %q", c.want, c.input, name, err, c.name)
		}
	}
	if near[0].Name != "zeta RPT" {
		t.Error("pickNearby reordered the caller's slice")
	}
}
