// Package human formats sizes and durations for chat messages (Chinese units).
package human

import (
	"fmt"
	"strings"
	"time"
)

// Bytes formats with binary units: 512B, 12K, 3.4M, 1.2G.
func Bytes[T ~int | ~int64 | ~uint64 | ~float64](n T) string {
	v := float64(n)
	for _, unit := range []string{"B", "K", "M", "G", "T"} {
		if (v < 1024 && v > -1024) || unit == "T" {
			if unit == "B" || unit == "K" {
				return fmt.Sprintf("%.0f%s", v, unit)
			}
			return fmt.Sprintf("%.1f%s", v, unit)
		}
		v /= 1024
	}
	panic("unreachable")
}

// Duration formats coarsely: "5 分钟", "3 小时 20 分", "2 天 4 小时".
func Duration(d time.Duration) string {
	s := int64(d / time.Second)
	switch {
	case s < 3600:
		return fmt.Sprintf("%d 分钟", max(1, s/60))
	case s < 86400:
		return fmt.Sprintf("%d 小时 %d 分", s/3600, s%3600/60)
	default:
		return fmt.Sprintf("%d 天 %d 小时", s/86400, s%86400/3600)
	}
}

// Pad right-pads s with spaces to width terminal columns, counting East
// Asian wide characters (CJK, fullwidth forms, emoji) as two. Monospace
// tables in chat need this: fmt's %-10s counts runes, not columns.
func Pad(s string, width int) string {
	if w := Width(s); w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}

// Width is s's display width in columns (see Pad).
func Width(s string) int {
	w := 0
	for _, r := range s {
		switch {
		case r >= 0x1100 && (r <= 0x115f || // Hangul Jamo
			(r >= 0x2e80 && r <= 0xa4cf) || // CJK radicals … Yi
			(r >= 0xac00 && r <= 0xd7a3) || // Hangul syllables
			(r >= 0xf900 && r <= 0xfaff) || // CJK compatibility ideographs
			(r >= 0xfe30 && r <= 0xfe4f) || // CJK compatibility forms
			(r >= 0xff00 && r <= 0xff60) || // fullwidth forms
			(r >= 0xffe0 && r <= 0xffe6) ||
			(r >= 0x1f300 && r <= 0x1faff) || // emoji
			(r >= 0x20000 && r <= 0x3fffd)):
			w += 2
		default:
			w++
		}
	}
	return w
}
