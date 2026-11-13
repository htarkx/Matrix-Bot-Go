// Package human formats sizes and durations for chat messages (Chinese units).
package human

import (
	"fmt"
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
