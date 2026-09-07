package httpjsonreceiver // import "httpjsonreceiver"

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// timeTemplate matches {{now}}, {{now-5m}}, {{now+1h|unix}} and so on. It is a
// deliberately tiny substitution rather than text/template: the only thing a
// request needs to compute at scrape time is a timestamp relative to now, and a
// full template engine in a config string is a much larger surface to get wrong.
var timeTemplate = regexp.MustCompile(`\{\{\s*now\s*(?:([+-])\s*([0-9]+(?:\.[0-9]+)?(?:ns|us|ms|s|m|h)(?:[0-9]+(?:\.[0-9]+)?(?:ns|us|ms|s|m|h))*)\s*)?(?:\|\s*([a-zA-Z0-9]+)\s*)?\}\}`)

// renderTimeTemplates substitutes every {{now...}} expression in s.
//
// Formats: rfc3339 (the default), rfc3339nano, minute, unix, unixmilli, date.
// An unparseable duration or an unknown format is an error rather than a
// pass-through, because the alternative is sending an API a request containing
// a literal "{{now-5m}}" and having to work backwards from its rejection.
func renderTimeTemplates(s string, now time.Time) (string, error) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}

	var firstErr error
	out := timeTemplate.ReplaceAllStringFunc(s, func(match string) string {
		groups := timeTemplate.FindStringSubmatch(match)
		sign, offset, format := groups[1], groups[2], groups[3]

		ts := now
		if offset != "" {
			d, err := time.ParseDuration(offset)
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("invalid duration %q in %q: %w", offset, match, err)
				}
				return match
			}
			if sign == "-" {
				d = -d
			}
			ts = ts.Add(d)
		}

		switch strings.ToLower(format) {
		case "", "rfc3339":
			return ts.Format(time.RFC3339)
		case "rfc3339nano":
			return ts.Format(time.RFC3339Nano)
		case "minute":
			// Truncated to the start of the minute. An API queried for a rolling
			// window returns overlapping or gapped windows as the scrape ticker
			// drifts; aligning both ends of the range to a minute boundary makes
			// consecutive scrapes cover exactly adjacent windows instead.
			return ts.Truncate(time.Minute).Format(time.RFC3339)
		case "unix":
			return fmt.Sprintf("%d", ts.Unix())
		case "unixmilli":
			return fmt.Sprintf("%d", ts.UnixMilli())
		case "date":
			return ts.Format("2006-01-02")
		default:
			if firstErr == nil {
				firstErr = fmt.Errorf("unknown time format %q in %q", format, match)
			}
			return match
		}
	})

	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}
