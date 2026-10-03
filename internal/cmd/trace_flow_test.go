package cmd

import "testing"

func TestTraceFlowMaintenanceSelection(t *testing.T) {
	for _, options := range []TraceFlowOptions{
		{}, {Status: true, Resume: true}, {Rebind: "old"}, {RebindTo: "new"},
		{ConfirmDiscard: true}, {Discard: "execution:example"},
	} {
		if err := options.Validate(); err == nil {
			t.Fatalf("accepted ambiguous or incomplete operation: %+v", options)
		}
	}
	for _, options := range []TraceFlowOptions{
		{Status: true}, {Resume: true}, {Requeue: "reason:timing"},
		{Rebind: "old", RebindTo: "new"}, {Discard: "reason:oversize", ConfirmDiscard: true},
	} {
		if err := options.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}
