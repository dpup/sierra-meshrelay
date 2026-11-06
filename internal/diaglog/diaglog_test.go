package diaglog

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestRingTeesInfoAndAbove(t *testing.T) {
	var out bytes.Buffer
	r := NewRing(3)
	log := slog.New(r.Handler(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelWarn})))
	log = log.With("gateway", "cabin").WithGroup("node")
	log.Debug("not kept")
	log.Info("node link opened", "addr", "10.0.0.5:5000", "control_token", "abc", "pin", 123456,
		"ping_interval", "25s", "expected_pubkey", "ab12")
	log.Warn("scan found nothing", "err", strings.Repeat("x", 500))

	es, lost := r.Since(0)
	if len(es) != 2 || lost != 0 {
		t.Fatalf("entries %+v lost %d", es, lost)
	}
	a := es[0].Attrs
	if es[0].Seq != 1 || es[0].Level != "INFO" || a["gateway"] != "cabin" || a["node.addr"] != "10.0.0.5:5000" ||
		a["node.control_token"] != "[redacted]" || a["node.pin"] != "[redacted]" || a["node.ping_interval"] != "25s" ||
		a["node.expected_pubkey"] != "ab12" {
		t.Fatalf("entry: %+v", es[0])
	}
	if v := es[1].Attrs["node.err"]; len(v) > MaxAttrLen+len("…") {
		t.Fatalf("not truncated: %d", len(v))
	}
	// The inner handler still filters by its own level.
	if strings.Contains(out.String(), "node link opened") || !strings.Contains(out.String(), "scan found nothing") {
		t.Fatalf("inner output: %s", out.String())
	}

	ch := r.Changed()
	log.Info("a")
	<-ch
	log.Info("b")
	log.Info("c")
	es, lost = r.Since(1)
	if lost != 1 || len(es) != 3 || es[0].Msg != "a" {
		t.Fatalf("after overflow: %+v lost %d", es, lost)
	}
	if es, lost := r.Since(5); len(es) != 0 || lost != 0 {
		t.Fatalf("up to date: %+v %d", es, lost)
	}
}
