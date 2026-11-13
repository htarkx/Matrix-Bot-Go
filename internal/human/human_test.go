package human

import (
	"testing"
	"time"
)

func TestBytes(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0B"}, {1023, "1023B"}, {1024, "1K"}, {1536, "2K"},
		{5 << 20, "5.0M"}, {1.5 * (1 << 30), "1.5G"}, {3 << 50, "3072.0T"},
	}
	for _, c := range cases {
		if got := Bytes(c.in); got != c.want {
			t.Errorf("Bytes(%v) = %s, want %s", c.in, got, c.want)
		}
	}
	if got := Bytes(uint64(2048)); got != "2K" {
		t.Errorf("uint64: %s", got)
	}
}

func TestDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                             "1 分钟",
		90 * time.Second:              "1 分钟",
		time.Hour - time.Nanosecond:   "59 分钟",
		61 * time.Minute:              "1 小时 1 分",
		23*time.Hour + 59*time.Minute: "23 小时 59 分",
		24 * time.Hour:                "1 天 0 小时",
		50*time.Hour + 30*time.Minute: "2 天 2 小时",
	}
	for in, want := range cases {
		if got := Duration(in); got != want {
			t.Errorf("Duration(%v) = %s, want %s", in, got, want)
		}
	}
}
