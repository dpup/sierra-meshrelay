package svc

import (
	"slices"
	"strings"
	"testing"
)

func TestICACLSArgs(t *testing.T) {
	got := strings.Join(icaclsArgs(`C:\ProgramData\SIERRA\meshbridge`, false), " ")
	want := `C:\ProgramData\SIERRA\meshbridge /inheritance:r /grant:r *S-1-5-18:(OI)(CI)F *S-1-5-32-544:(OI)(CI)F ` +
		`/remove:g *S-1-5-32-545 *S-1-5-11 *S-1-3-0 *S-1-1-0 /T /C /Q`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if args := icaclsArgs("d", true); !slices.Contains(args, `NT SERVICE\meshbridge:(OI)(CI)M`) {
		t.Fatalf("service account not granted: %v", args)
	}
}
