package server

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The upstream uses code 6004 for a model/account rate limit and embeds the
// next reset time in msg, for example:
//
//	... 将在 2026-09-11 17:58:44 UTC+8 重置 ...
//
// Keep the parser independent from the process timezone; the UTC offset in
// the message is the source of truth.
var (
	rateLimitResetPattern = regexp.MustCompile(`(?i)(\d{4}-\d{1,2}-\d{1,2})\s+(\d{1,2}:\d{2}(?::\d{2})?)\s+UTC\s*([+-])\s*(\d{1,2})(?::?(\d{2}))?`)
	rateLimitCodePattern  = regexp.MustCompile(`(?i)"code"\s*:\s*"?(\d+)"?`)
)

type rateLimitEnvelope struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

// upstreamRateLimitInfo extracts the common upstream envelope. The fallback
// regex also handles a wrapper such as our final 503 error containing the
// original upstream JSON after a prefix.
func upstreamRateLimitInfo(body string) (code int, msg string) {
	var envelope rateLimitEnvelope
	if err := json.Unmarshal([]byte(body), &envelope); err == nil {
		return envelope.Code, strings.TrimSpace(envelope.Msg)
	}
	if match := rateLimitCodePattern.FindStringSubmatch(body); len(match) == 2 {
		code, _ = strconv.Atoi(match[1])
	}
	return code, strings.TrimSpace(body)
}

// isExplicitRateLimit reports whether the body describes a deterministic
// upstream reset window rather than an ordinary transient 429.
func isExplicitRateLimit(body string) bool {
	code, msg := upstreamRateLimitInfo(body)
	if code == 6004 {
		return true
	}
	if msg == "" {
		msg = body
	}
	lower := strings.ToLower(msg)
	return rateLimitResetPattern.MatchString(msg) &&
		(strings.Contains(msg, "重置") || strings.Contains(lower, "reset"))
}

// rateLimitResetAt parses the absolute reset time from an upstream message.
// A past timestamp is rejected so a malformed/stale response falls back to
// the configured short cooldown instead of immediately retrying the account.
func rateLimitResetAt(body string, now time.Time) (time.Time, bool) {
	code, msg := upstreamRateLimitInfo(body)
	if msg == "" {
		msg = body
	}
	lower := strings.ToLower(msg)
	if code != 6004 && !(strings.Contains(msg, "重置") || strings.Contains(lower, "reset")) {
		return time.Time{}, false
	}

	match := rateLimitResetPattern.FindStringSubmatch(msg)
	if len(match) != 6 {
		return time.Time{}, false
	}
	dateParts := strings.Split(match[1], "-")
	clockParts := strings.Split(match[2], ":")
	if len(dateParts) != 3 || (len(clockParts) != 2 && len(clockParts) != 3) {
		return time.Time{}, false
	}
	year, errYear := strconv.Atoi(dateParts[0])
	month, errMonth := strconv.Atoi(dateParts[1])
	day, errDay := strconv.Atoi(dateParts[2])
	hour, errHour := strconv.Atoi(clockParts[0])
	minute, errMinute := strconv.Atoi(clockParts[1])
	second := 0
	var errSecond error
	if len(clockParts) == 3 {
		second, errSecond = strconv.Atoi(clockParts[2])
	}
	if errYear != nil || errMonth != nil || errDay != nil || errHour != nil || errMinute != nil || errSecond != nil {
		return time.Time{}, false
	}

	offsetHour, err := strconv.Atoi(match[4])
	if err != nil || offsetHour > 23 {
		return time.Time{}, false
	}
	offsetMinute := 0
	if match[5] != "" {
		offsetMinute, err = strconv.Atoi(match[5])
		if err != nil || offsetMinute > 59 {
			return time.Time{}, false
		}
	}
	if month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || minute > 59 || second > 59 {
		return time.Time{}, false
	}
	sign := 1
	if match[3] == "-" {
		sign = -1
	}
	offset := sign * (offsetHour*60*60 + offsetMinute*60)
	zoneName := "UTC" + match[3] + strconv.Itoa(offsetHour)
	if match[5] != "" {
		zoneName += ":" + match[5]
	}
	location := time.FixedZone(zoneName, offset)
	reset := time.Date(year, time.Month(month), day, hour, minute, second, 0, location)
	// time.Date normalizes invalid dates (for example, February 31); reject
	// those instead of turning a bad upstream value into a valid future date.
	if reset.Year() != year || int(reset.Month()) != month || reset.Day() != day ||
		reset.Hour() != hour || reset.Minute() != minute || reset.Second() != second {
		return time.Time{}, false
	}
	if !reset.After(now) {
		return time.Time{}, false
	}
	return reset, true
}

func rateLimitReason(body string, reset time.Time) string {
	code, _ := upstreamRateLimitInfo(body)
	reason := "429 rate limit"
	if code != 0 {
		reason += " code=" + strconv.Itoa(code)
	}
	return reason + "; reset_at=" + reset.Format(time.RFC3339)
}

func rateLimitFallbackReason(body string) string {
	code, _ := upstreamRateLimitInfo(body)
	reason := "429 rate limit"
	if code != 0 {
		reason += " code=" + strconv.Itoa(code)
	}
	return reason + "; reset time unavailable"
}
