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

// TestParseClockHourRoundsUp 锁住 auto_checkin_at 的分钟不会被静默丢弃。
//
// 排程粒度是整点，10:35 必须落到 11:00：签到窗口常常 10:30 才开，
// 按 10:00 执行等于每天都跑空。23:xx 取整后落到次日 00:00，仍不早于设定时刻。
func TestParseClockHourRoundsUp(t *testing.T) {
	cases := []struct {
		value string
		hour  int
		ok    bool
	}{
		{"10:00", 10, true},
		{" 09:07 ", 10, true},
		{"23:30", 0, true},
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
