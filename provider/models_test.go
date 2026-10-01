package provider

import (
	"encoding/json"
	"testing"
	"time"
)

func TestScrobTimeUnmarshalsNaiveTimestamps(t *testing.T) {
	// Regression test: Scrob serializes timestamps with Python's naive
	// datetime.isoformat(), no timezone suffix - encoding/json's default
	// time.Time unmarshaler rejects that outright.
	var got scrobTime
	if err := json.Unmarshal([]byte(`"2026-10-01T12:06:08"`), &got); err != nil {
		t.Fatalf("UnmarshalJSON() error: %v", err)
	}
	want := time.Date(2026, 10, 1, 12, 6, 8, 0, time.UTC)
	if !got.Time().Equal(want) {
		t.Fatalf("Time() = %v, want %v", got.Time(), want)
	}

	var tzAware scrobTime
	if err := json.Unmarshal([]byte(`"2026-10-01T12:06:08Z"`), &tzAware); err != nil {
		t.Fatalf("UnmarshalJSON() error on RFC3339 input: %v", err)
	}
	if !tzAware.Time().Equal(want) {
		t.Fatalf("Time() = %v, want %v", tzAware.Time(), want)
	}

	var empty scrobTime
	if err := json.Unmarshal([]byte(`""`), &empty); err != nil {
		t.Fatalf("UnmarshalJSON() error on empty input: %v", err)
	}
	if !empty.Time().IsZero() {
		t.Fatalf("Time() = %v, want zero", empty.Time())
	}
}

func TestScrobTimeMarshalRoundTrips(t *testing.T) {
	in := scrobTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	encoded, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("MarshalJSON() error: %v", err)
	}
	var out scrobTime
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatalf("UnmarshalJSON() error: %v", err)
	}
	if !out.Time().Equal(in.Time()) {
		t.Fatalf("round trip = %v, want %v", out.Time(), in.Time())
	}
}
