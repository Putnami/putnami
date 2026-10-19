package registrycred

import (
	"net/url"
	"regexp"
	"strings"
	"sync"
)

// A missing credential matters only when a registry refuses a request. Most
// installs and builds fetch public packages and modules, which need none, so an
// installer holds the warning EnsureNativeCredential returned (HoldWarning) and
// logs it only when the evidence of a failed fetch shows the registry answered
// 401 or 403 (ReportRefusal).

// existingCredentialTail ends every warning EnsureNativeCredential returns. It
// describes a run that goes on, and the run it follows a refusal of has failed.
const existingCredentialTail = "; continuing with the existing credential"

// refusalPattern matches the way a registry says no: HTTP 401 or 403 as a
// status word of its own, or the reason phrase. A version such as 1.4.401 and a
// port such as :4013 do not match.
var refusalPattern = regexp.MustCompile(`(?i)(?:^|[\s(:])(?:401|403)(?:[\s:,)]|$)|\bunauthori[sz]ed\b|\bforbidden\b`)

// urlHostPattern finds the host of every http(s) URL in a line.
var urlHostPattern = regexp.MustCompile(`(?i)https?://[^\s"'<>)]+`)

type heldWarning struct {
	host    string
	message string
}

var (
	heldMu       sync.Mutex
	heldWarnings []heldWarning
)

// HoldWarning keeps the warning message about host until ReportRefusal finds a
// refusal from it. The state is per process, like EnsureNativeCredential's memo:
// it reports each host once, and several steps of one job may ask for the same
// host.
func HoldWarning(host, message string) {
	host = strings.ToLower(strings.TrimSpace(host))
	heldMu.Lock()
	defer heldMu.Unlock()
	for _, held := range heldWarnings {
		if held.host == host && held.message == message {
			return
		}
	}
	heldWarnings = append(heldWarnings, heldWarning{host: host, message: message})
}

// ReportRefusal logs, at the warn level, the held warnings that a registry
// refusal in evidence makes relevant, and forgets them so each is logged once.
// evidence is the error text of a failed registry request or the output of a
// failed package manager or go command.
//
// A line of evidence that shows a 401 or 403 and names an http(s) URL concerns
// the host of that URL only. A line that shows one and names no URL concerns
// every held host. Evidence with no refusal leaves the warnings held.
func ReportRefusal(evidence string, log func(level, message string)) {
	var hosts []string
	unattributed := false
	for _, line := range strings.Split(evidence, "\n") {
		if !refusalPattern.MatchString(line) {
			continue
		}
		urls := urlHostPattern.FindAllString(line, -1)
		if len(urls) == 0 {
			unattributed = true
			continue
		}
		for _, raw := range urls {
			if parsed, err := url.Parse(raw); err == nil && parsed.Hostname() != "" {
				hosts = append(hosts, strings.ToLower(parsed.Hostname()))
			}
		}
	}
	if !unattributed && len(hosts) == 0 {
		return
	}

	heldMu.Lock()
	var report []string
	kept := heldWarnings[:0]
	for _, held := range heldWarnings {
		if unattributed || containsHost(hosts, held.host) {
			report = append(report, held.message)
			continue
		}
		kept = append(kept, held)
	}
	heldWarnings = kept
	heldMu.Unlock()

	// Log after the lock is released: log may emit an event that an observer
	// feeds straight back into ReportRefusal.
	for _, message := range report {
		log("warn", "the registry refused a request (HTTP 401 or 403); "+strings.Replace(message, existingCredentialTail, "", 1))
	}
}

func containsHost(hosts []string, host string) bool {
	for _, candidate := range hosts {
		if candidate == host {
			return true
		}
	}
	return false
}
