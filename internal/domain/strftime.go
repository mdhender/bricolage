// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A strftime formatter (DESIGN.md 5.3).
//
// URI formats are strftime strings. Go's reference-time layouts cannot express
// them and translating one into the other is lossy in both directions -- "%m"
// and "01" agree, "%j" and "002" agree, and then "%e" has no layout at all and
// "Jan" has no token. DESIGN.md 5.3 says to write the formatter rather than
// the translator, and this is it: forty lines and a table, against a
// translation nobody could debug from either side.
//
// The token set is the one the system we learned from accepted in a URI
// format, which is POSIX strftime minus the locale-dependent and
// timezone-dependent conversions. %c, %x and %X are deliberately absent: a URI
// that changes shape with the server's locale is a URI that changes shape when
// somebody changes the server's locale.

// UnknownTokenError reports a strftime conversion this formatter does not
// implement.
//
// It is an error rather than a passthrough because a URI format is
// configuration a person typed, and silently emitting "%q" into every URI on
// the site is the failure mode where nobody notices for a month.
type UnknownTokenError struct {
	// Token is the conversion, including the leading '%'.
	Token string
}

func (e *UnknownTokenError) Error() string {
	return fmt.Sprintf("strftime: %s is not a conversion this system implements", e.Token)
}

// Is makes an unknown token answer to ErrInvalid, so the transport edge maps
// it to 422 through the one mapping function it has.
func (e *UnknownTokenError) Is(target error) bool { return target == ErrInvalid }

// Strftime expands the POSIX conversions in format against t.
//
// A conversion this system does not implement is an *UnknownTokenError rather
// than a passthrough, and a trailing '%' is the same. Braced extensions --
// %{categories} and %{slug} -- are not handled here: they are substituted by
// BuildURI before the date conversions run, because one of them consumes the
// slash that follows it and that is a URI rule rather than a date rule.
func Strftime(format string, t time.Time) (string, error) {
	var b strings.Builder
	b.Grow(len(format) + 16)

	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			b.WriteByte(format[i])
			continue
		}
		if i+1 >= len(format) {
			return "", &UnknownTokenError{Token: "%"}
		}
		i++
		verb := format[i]
		out, ok := strftimeVerb(verb, t)
		if !ok {
			return "", &UnknownTokenError{Token: "%" + string(verb)}
		}
		b.WriteString(out)
	}
	return b.String(), nil
}

// strftimeVerb expands one conversion. The zero-padded numeric ones are the
// only ones a URI format normally uses; the rest are here because they were
// accepted before and dropping them silently would change existing addresses.
func strftimeVerb(verb byte, t time.Time) (string, bool) {
	switch verb {
	case '%':
		return "%", true
	case 'n':
		return "\n", true
	case 't':
		return "\t", true

	// Dates.
	case 'Y':
		return strconv.Itoa(t.Year()), true
	case 'y':
		return pad2(t.Year() % 100), true
	case 'C':
		return pad2(t.Year() / 100), true
	case 'm':
		return pad2(int(t.Month())), true
	case 'd':
		return pad2(t.Day()), true
	case 'e':
		return space2(t.Day()), true
	case 'j':
		return fmt.Sprintf("%03d", t.YearDay()), true
	case 'b', 'h':
		return t.Format("Jan"), true
	case 'B':
		return t.Format("January"), true
	case 'a':
		return t.Format("Mon"), true
	case 'A':
		return t.Format("Monday"), true
	case 'u':
		// ISO 8601 weekday, Monday = 1 .. Sunday = 7.
		if d := int(t.Weekday()); d == 0 {
			return "7", true
		} else {
			return strconv.Itoa(d), true
		}
	case 'w':
		return strconv.Itoa(int(t.Weekday())), true
	case 'F':
		return t.Format("2006-01-02"), true
	case 'D':
		return t.Format("01/02/06"), true

	// Times.
	case 'H':
		return pad2(t.Hour()), true
	case 'I':
		return pad2(hour12(t.Hour())), true
	case 'M':
		return pad2(t.Minute()), true
	case 'S':
		return pad2(t.Second()), true
	case 'p':
		if t.Hour() < 12 {
			return "AM", true
		}
		return "PM", true
	case 'P':
		if t.Hour() < 12 {
			return "am", true
		}
		return "pm", true
	case 'R':
		return t.Format("15:04"), true
	case 'T':
		return t.Format("15:04:05"), true
	case 's':
		return strconv.FormatInt(t.Unix(), 10), true
	case 'Z':
		return t.Format("MST"), true
	case 'z':
		return t.Format("-0700"), true
	}
	return "", false
}

func pad2(n int) string {
	if n < 0 {
		n = -n
	}
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

func space2(n int) string {
	if n < 10 {
		return " " + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

func hour12(h int) int {
	h %= 12
	if h == 0 {
		return 12
	}
	return h
}

// HasDateConversion reports whether format contains any conversion that reads
// the instant.
//
// BuildURI asks, because a document with no cover date can still have a URI --
// a fixed-URI page is exactly that -- and the difference between "this format
// needs a date and there is none" and "this format needs no date" is the
// difference between a refusal and an address.
func HasDateConversion(format string) bool {
	for i := 0; i < len(format); i++ {
		if format[i] != '%' || i+1 >= len(format) {
			continue
		}
		i++
		switch format[i] {
		case '%', 'n', 't', '{':
			// Literals and the braced extensions read no clock.
		default:
			return true
		}
	}
	return false
}
