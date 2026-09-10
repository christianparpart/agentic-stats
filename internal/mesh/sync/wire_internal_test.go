package sync

import (
	"reflect"
	"testing"

	"github.com/christianparpart/agentic-stats/internal/store"
)

// Every column of a record must survive the wire.
//
// By reflection rather than by a hand-written list, because a hand-written list
// is exactly what failed: pr_repo and pr_number were absent from wireRecord for
// as long as they existed, so a pull request opened on one machine was invisible
// on every other one, and nothing anywhere reported a problem. A test that
// enumerates the struct itself cannot fall behind it.
func TestEveryRecordColumnSurvivesTheWire(t *testing.T) {
	var original store.Record
	v := reflect.ValueOf(&original).Elem()
	for i := range v.NumField() {
		// A distinct value per field, so a toWire that fills the right column
		// from the wrong source is caught as well as one that fills nothing.
		switch f := v.Field(i); f.Kind() {
		case reflect.String:
			f.SetString("value-" + v.Type().Field(i).Name)
		case reflect.Int64:
			f.SetInt(int64(i) + 1)
		case reflect.Slice:
			f.SetBytes([]byte{byte(i) + 1})
		default:
			t.Fatalf("store.Record.%s is a %s, which this test does not know how to fill",
				v.Type().Field(i).Name, f.Kind())
		}
	}

	if got := fromWire(toWire(original)); !reflect.DeepEqual(got, original) {
		t.Errorf("a record did not survive the round trip:\n got %+v\nwant %+v", got, original)
	}
}
