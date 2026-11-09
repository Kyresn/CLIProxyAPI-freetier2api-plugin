package tasks

import "testing"

func TestNormalizeHoursAndNextWake(t *testing.T) {
	if got := NormalizeHours([]int{9, 9, 25, -1, 21}); len(got) != 2 || got[0] != 9 || got[1] != 21 {
		t.Fatalf("NormalizeHours = %#v", got)
	}
}

func TestNormalizeHoursKeepsValidValues(t *testing.T) {
	if got := NormalizeHours([]int{0, 23}); len(got) != 2 || got[0] != 0 || got[1] != 23 {
		t.Fatalf("NormalizeHours = %#v", got)
	}
}

// TestParseClockHourUsesConfiguredHour locks the wake hour for minute-specific
// auto_checkin_at. checkinMinuteDelay waits within this hour, preventing an
// early checkin without moving the wake to the next hour.
func TestParseClockHourUsesConfiguredHour(t *testing.T) {
	cases := []struct {
		value string
		hour  int
		ok    bool
	}{
		{"10:00", 10, true},
		{" 09:07 ", 9, true},
		{"23:30", 23, true},
		{"24:00", 0, false},
		{"10:60", 0, false},
		{"10", 0, false},
	}
	for _, tc := range cases {
		hour, ok := parseClockHour(tc.value)
		if ok != tc.ok || hour != tc.hour {
			t.Fatalf("parseClockHour(%q) = (%d, %v)，want (%d, %v)", tc.value, hour, ok, tc.hour, tc.ok)
		}
	}
}
