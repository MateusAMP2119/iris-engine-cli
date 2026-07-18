package store_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store/storetest"
)

// The #215 plugin ledgers ride both turn-run mints atomically: run_plugins pins
// and run_plugin_calls rows land in the same CTE as the run row.

// pluginTurnRecord is sampleTurnRecord plus one pin and two calls.
func pluginTurnRecord() store.TurnRunRecord {
	rec := sampleTurnRecord([]int64{11})
	rec.Plugins = []store.RunPluginPin{{Alias: "mail", Name: "smtp-send", Version: "1.0", Digest: "abc123", InstanceID: "smtp-send@1.0#3"}}
	rec.Calls = []store.RunPluginCall{
		{Seq: 1, Alias: "mail", Verb: "send", ArgsDigest: "d1", Outcome: "ok", ResponseDigest: "r1"},
		{Seq: 2, Alias: "mail", Verb: "send", ArgsDigest: "d2", Outcome: "err", Error: "timeout"},
	}
	return rec
}

func TestCreateTurnRunCarriesPluginLedgers(t *testing.T) {
	rec := storetest.NewWriteRecorder()
	w := store.NewWriter(rec)
	if err := w.CreateTurnRun(context.Background(), pluginTurnRecord()); err != nil {
		t.Fatalf("CreateTurnRun: %v", err)
	}
	stmt := rec.Transactions()[0][0]
	if !strings.Contains(stmt.SQL, "INSERT INTO run_plugins") || !strings.Contains(stmt.SQL, "INSERT INTO run_plugin_calls") {
		t.Errorf("mint lacks the plugin ledger legs:\n%s", stmt.SQL)
	}
	if len(stmt.Args) != 20 {
		t.Fatalf("args = %d, want 20", len(stmt.Args))
	}
	if got := stmt.Args[8]; !reflect.DeepEqual(got, []string{"mail"}) {
		t.Errorf("pin alias arg = %v", got)
	}
	if got := stmt.Args[11]; !reflect.DeepEqual(got, []string{"abc123"}) {
		t.Errorf("pin digest arg = %v", got)
	}
	if got := stmt.Args[12]; !reflect.DeepEqual(got, []string{"smtp-send@1.0#3"}) {
		t.Errorf("pin instance arg = %v", got)
	}
	if got := stmt.Args[13]; !reflect.DeepEqual(got, []int64{1, 2}) {
		t.Errorf("call seq arg = %v", got)
	}
	if got := stmt.Args[17]; !reflect.DeepEqual(got, []string{"ok", "err"}) {
		t.Errorf("call outcome arg = %v", got)
	}
	if got := stmt.Args[19]; !reflect.DeepEqual(got, []string{"", "timeout"}) {
		t.Errorf("call error arg = %v", got)
	}
}

func TestDeadLetterTurnRunCarriesPluginLedgers(t *testing.T) {
	rec := storetest.NewWriteRecorder()
	w := store.NewWriter(rec)
	if err := w.DeadLetterTurnRun(context.Background(), pluginTurnRecord(), store.ReasonFailed, "boom"); err != nil {
		t.Fatalf("DeadLetterTurnRun: %v", err)
	}
	stmt := rec.Transactions()[0][0]
	if !strings.Contains(stmt.SQL, "INSERT INTO dead_letters") || !strings.Contains(stmt.SQL, "INSERT INTO run_plugin_calls") {
		t.Errorf("dead-letter mint lacks the plugin ledger legs:\n%s", stmt.SQL)
	}
	if len(stmt.Args) != 22 {
		t.Fatalf("args = %d, want 22", len(stmt.Args))
	}
	if got := stmt.Args[10]; !reflect.DeepEqual(got, []string{"mail"}) {
		t.Errorf("pin alias arg = %v", got)
	}
	if got := stmt.Args[14]; !reflect.DeepEqual(got, []string{"smtp-send@1.0#3"}) {
		t.Errorf("pin instance arg = %v", got)
	}
	if got := stmt.Args[15]; !reflect.DeepEqual(got, []int64{1, 2}) {
		t.Errorf("call seq arg = %v", got)
	}
}
