package api_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
)

// readoutPayloads is every read-API readout document the epic's invariants close
// over (specification section 11): the stats rollup, the engine-info readout, the
// engine-inspect DDL dump, and the pipeline-show readout. The invariant tests walk
// these types, so a field added to any readout is checked by construction.
func readoutPayloads() map[string]reflect.Type {
	return map[string]reflect.Type{
		"stats":   reflect.TypeOf(api.StatsPayload{}),
		"info":    reflect.TypeOf(api.InfoPayload{}),
		"inspect": reflect.TypeOf(api.InspectPayload{}),
		"show":    reflect.TypeOf(api.PipelineShowResult{}),
	}
}

// walkStructFields visits every struct field reachable from t (through pointers,
// slices, arrays, and maps), calling visit with the owning type and the field.
func walkStructFields(t reflect.Type, seen map[reflect.Type]bool, visit func(owner reflect.Type, f reflect.StructField)) {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		walkStructFields(t.Elem(), seen, visit)
	case reflect.Map:
		walkStructFields(t.Key(), seen, visit)
		walkStructFields(t.Elem(), seen, visit)
	case reflect.Struct:
		if seen[t] {
			return
		}
		seen[t] = true
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			visit(t, f)
			walkStructFields(f.Type, seen, visit)
		}
	}
}

// jsonName is the field's wire name: the json tag's name part, or the Go name
// lowercased when untagged.
func jsonName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if name, _, _ := strings.Cut(tag, ","); name != "" {
		return name
	}
	return strings.ToLower(f.Name)
}

// TestUptimeSoleWallClock proves uptime in the engine-info readout is the engine's
// one and only wall-clock readout, and display-only (specification section 11): the
// InfoPayload's Uptime is a rendered string a caller cannot compute on -- never a
// time.Time or a duration -- and no other field in any readout (stats, info,
// inspect, show) is time-typed or clock-named. Everything else is a count, a
// last-value, or identity.
//
// spec: S11/uptime-sole-wall-clock
func TestUptimeSoleWallClock(t *testing.T) {
	uptime, ok := reflect.TypeOf(api.InfoPayload{}).FieldByName("Uptime")
	if !ok {
		t.Fatal("InfoPayload carries no Uptime field; info must report uptime")
	}
	if uptime.Type.Kind() != reflect.String {
		t.Errorf("InfoPayload.Uptime is %s, want a rendered display string (display-only, never computable)", uptime.Type)
	}
	if jsonName(uptime) != "uptime" {
		t.Errorf("InfoPayload.Uptime wire name = %q, want %q", jsonName(uptime), "uptime")
	}

	timeType := reflect.TypeOf(time.Time{})
	durationType := reflect.TypeOf(time.Duration(0))
	// Clock-suggesting wire-name fragments: any field so named is a second
	// wall-clock readout unless it IS info's uptime.
	clockFragments := []string{"time", "clock", "_at", "timestamp", "duration", "elapsed", "started", "when"}

	for name, typ := range readoutPayloads() {
		walkStructFields(typ, map[reflect.Type]bool{}, func(owner reflect.Type, f reflect.StructField) {
			if f.Type == timeType || f.Type == durationType {
				t.Errorf("%s readout: %s.%s is %s; uptime is the engine's sole wall-clock readout and it is a display string", name, owner.Name(), f.Name, f.Type)
			}
			wire := jsonName(f)
			if owner == reflect.TypeOf(api.InfoPayload{}) && wire == "uptime" {
				return // the one permitted wall-clock readout
			}
			for _, frag := range clockFragments {
				if strings.Contains(wire, frag) {
					t.Errorf("%s readout: field %s.%s (wire %q) suggests a wall-clock; uptime in info is the only one permitted", name, owner.Name(), f.Name, wire)
				}
			}
		})
	}
}

// TestNoLivenessReadouts proves no readout -- stats, info, inspect, or pipeline
// show -- carries a last-heartbeat or last-seen field: connection state is the
// only liveness signal (specification section 11). The check walks every field of
// every readout payload, so a liveness field added anywhere fails here.
//
// spec: S11/no-liveness-readouts
func TestNoLivenessReadouts(t *testing.T) {
	livenessFragments := []string{"heartbeat", "last_seen", "lastseen", "seen", "liveness", "alive"}
	for name, typ := range readoutPayloads() {
		walkStructFields(typ, map[reflect.Type]bool{}, func(owner reflect.Type, f reflect.StructField) {
			wire := jsonName(f)
			lowered := strings.ToLower(f.Name)
			for _, frag := range livenessFragments {
				if strings.Contains(wire, frag) || strings.Contains(lowered, frag) {
					t.Errorf("%s readout: field %s.%s (wire %q) is a liveness readout; connection state is the only liveness signal", name, owner.Name(), f.Name, wire)
				}
			}
		})
	}
}
