package models

import "strings"

func weekdayOn(c *CalendarService, day int) bool {
	if c == nil {
		return false
	}
	var v string
	switch day {
	case 1:
		v = c.Monday
	case 2:
		v = c.Tuesday
	case 3:
		v = c.Wednesday
	case 4:
		v = c.Thursday
	case 5:
		v = c.Friday
	case 6:
		v = c.Saturday
	case 7:
		v = c.Sunday
	default:
		return false
	}
	v = strings.TrimSpace(v)
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "t")
}

// ServiceWeekdayLabel returns a short chip label and optional description from calendar flags.
func ServiceWeekdayLabel(c *CalendarService) (label, description string) {
	if c == nil {
		return "", ""
	}
	wd := 0
	for d := 1; d <= 7; d++ {
		if weekdayOn(c, d) {
			wd |= 1 << (d - 1)
		}
	}
	const (
		mo = 1 << 0
		tu = 1 << 1
		we = 1 << 2
		th = 1 << 3
		fr = 1 << 4
		sa = 1 << 5
		su = 1 << 6
	)
	weekday5 := mo | tu | we | th | fr
	weekend := sa | su
	switch {
	case wd == weekday5:
		return "Weekday", "Monday–Friday"
	case wd == sa:
		return "Saturday", ""
	case wd == su:
		return "Sunday", ""
	case wd == weekend:
		return "Weekend", "Saturday & Sunday"
	case wd == weekday5|sa|su:
		return "Daily", "All days"
	case wd != 0:
		return c.ServiceID, "Custom weekly pattern"
	default:
		return c.ServiceID, ""
	}
}
