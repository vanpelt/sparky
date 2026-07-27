package nodelink

import "testing"

func TestParseWANTransportTotalsAggregatesOnlyTheRequestedTransport(t *testing.T) {
	const scrape = `
# TYPE sparkbox_fleet_disconnects_total counter
sparkbox_fleet_disconnects_total{node="node-b",reason="eof",transport="ssh"} 2
sparkbox_fleet_disconnects_total{node="node-b",reason="liveness",transport="ssh"} 3
sparkbox_fleet_disconnects_total{node="node-c",reason="eof",transport="ssh"} 99
# TYPE sparkbox_fleet_control_dropped_total counter
sparkbox_fleet_control_dropped_total{kind="event",node="node-b",transport="ssh"} 5
sparkbox_fleet_control_dropped_total{kind="reply",node="node-b",transport="ssh"} 7
sparkbox_fleet_control_dropped_total{kind="event",node="node-b",transport="grpc"} 88
# TYPE sparkbox_fleet_control_write_queue_depth gauge
sparkbox_fleet_control_write_queue_depth{node="node-b",transport="ssh"} 4
`
	totals, err := parseWANTransportTotals(scrape, "node-b", "ssh")
	if err != nil {
		t.Fatal(err)
	}
	if totals.DisconnectsTotal != 5 || totals.DroppedTotal != 12 || totals.WriteQueueDepth != 4 {
		t.Fatalf("totals = %+v", totals)
	}
}

func TestParseWANTransportTotalsTreatsUnpublishedZeroSeriesAsZero(t *testing.T) {
	totals, err := parseWANTransportTotals("", "node-b", "ssh")
	if err != nil {
		t.Fatal(err)
	}
	if totals != (wanTransportTotals{}) {
		t.Fatalf("empty scrape totals = %+v", totals)
	}
}
