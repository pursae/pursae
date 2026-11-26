package server

import (
	"math"
	"strconv"
	"testing"
	"time"
)

// Person counts must be capped so a huge per-type count cannot wrap the sum past the
// max-persons check or the per-person cost multiplier.
func TestT2GatesBookingPersonsCapped(t *testing.T) {
	_, _, srv := newTestServer(t, false)
	conf := &bkConf{DurationType: "fixed", Duration: 1, Unit: "day", MinDuration: 1, MaxDuration: 1,
		HasPersons: true, MinPersons: 1, MaxPersons: 4, PersonCostMult: true, BlockCost: 10,
		PersonTypes: []bkPersonType{{ID: "a", Name: "Adult"}, {ID: "b", Name: "Child"}}}
	day := time.Now().AddDate(0, 0, 7).Format("2006-01-02")
	big := strconv.Itoa(math.MaxInt - 1)
	cases := []struct {
		name string
		a, b string
		ok   bool
	}{
		{"normal", "2", "1", true},
		{"over max", "3", "2", false},
		{"wrapping", big, "3", false},
		{"huge single", big, "0", false},
		{"global cap", "10001", "0", false},
	}
	for _, tc := range cases {
		form := map[string][]string{"wc_bookings_field_start_date": {day},
			"wc_bookings_field_persons_a": {tc.a}, "wc_bookings_field_persons_b": {tc.b}}
		_, _, persons, err := srv.bkParseRequest(conf, form)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err=%v persons=%v", tc.name, err, persons)
		}
	}
	unbounded := *conf
	unbounded.MaxPersons = 0
	form := map[string][]string{"wc_bookings_field_start_date": {day},
		"wc_bookings_field_persons_a": {"6000"}, "wc_bookings_field_persons_b": {"6000"}}
	if _, _, _, err := srv.bkParseRequest(&unbounded, form); err == nil {
		t.Error("a total above the global cap must be rejected")
	}
}

func TestT2GatesBookingPersonSums(t *testing.T) {
	m := map[string]int{"a": math.MaxInt, "b": math.MaxInt}
	if n := bkSumPersons(m); n != bkMaxPersons {
		t.Errorf("sum = %d, want %d", n, bkMaxPersons)
	}
	if n := bkSumPersons(map[string]int{"a": -5, "b": 3}); n != 3 {
		t.Errorf("negative counts must not lower the sum: %d", n)
	}
	conf := &bkConf{HasPersons: true, PersonCostMult: true, BlockCost: 10,
		PersonTypes: []bkPersonType{{ID: "a"}, {ID: "b"}}, Duration: 1, Unit: "day"}
	cost := gooBkCostWith(conf, time.Now(), 1, map[string]int{"a": math.MaxInt, "b": 2}, 0)
	if cost != 10*bkMaxPersons {
		t.Errorf("wrapped person total lowered the cost: %v", cost)
	}
}
