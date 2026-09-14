package robustness

import (
	"strings"
)

var ctrListingErrorMarkers = []string{
	"failed to dial",
	"connection refused",
	"cannot connect",
	"no such file or directory",
	"permission denied",
	"i/o timeout",
	"deadline exceeded",
	"rpc error",
	"unavailable",
	"transport is closing",
	"error response from daemon",
}

func ctrListingValid(listing string) bool {
	return ctrListingKind(listing) == "ok"
}

func ctrListingKind(listing string) string {
	stripped := strings.TrimSpace(listing)
	if stripped == "" {
		return "invalid"
	}
	var header string
	for _, line := range strings.Split(listing, "\n") {
		if strings.TrimSpace(line) != "" {
			header = line
			break
		}
	}
	headerOK := ctrHeaderOK(header)
	lower := strings.ToLower(stripped)
	if !headerOK {
		head := lower
		if len(head) > 120 {
			head = head[:120]
		}
		if strings.HasPrefix(lower, "ctr:") || strings.Contains(head, "error:") {
			return "error"
		}
		for _, m := range ctrListingErrorMarkers {
			if strings.Contains(lower, m) {
				return "error"
			}
		}
		return "invalid"
	}
	return "ok"
}

func ctrHeaderOK(header string) bool {
	var toks []string
	for _, f := range strings.Fields(header) {
		toks = append(toks, strings.ToLower(f))
	}
	hasTask, hasPID, hasStatus := false, false, false
	for _, tok := range toks {
		switch tok {
		case "task":
			hasTask = true
		case "pid":
			hasPID = true
		case "status":
			hasStatus = true
		}
	}
	return hasTask && (hasPID || hasStatus)
}

func killEvidenceShowsDeath(evidence, containerID string) bool {
	if strings.TrimSpace(evidence) == "" || containerID == "" {
		return false
	}
	short := containerID
	if len(short) > 12 {
		short = short[:12]
	}
	if !strings.Contains(evidence, short) {
		return false
	}
	var listingLines []string
	for _, line := range strings.Split(evidence, "\n") {
		trim := strings.TrimSpace(line)
		lower := strings.ToLower(trim)
		if strings.HasPrefix(lower, "kubelet stopped") || strings.HasPrefix(lower, "ctr kill") {
			continue
		}
		listingLines = append(listingLines, line)
	}
	listing := strings.Join(listingLines, "\n")
	if !ctrListingValid(listing) {
		return false
	}
	dead, _ := taskDeadFromListing(containerID, listing, true)
	return dead
}

func taskDeadFromListing(cid, listing string, seen bool) (bool, string) {
	cid = strings.TrimSpace(cid)
	short := cid
	if len(short) > 12 {
		short = short[:12]
	}
	if cid == "" || len(short) < 8 {
		return false, "empty container id"
	}
	if kind := ctrListingKind(listing); kind != "ok" {
		return false, "listing is " + kind + ", not proof of death"
	}
	for _, line := range strings.Split(listing, "\n") {
		if !strings.Contains(line, cid) && !strings.Contains(line, short) {
			continue
		}
		lower := strings.ToLower(line)
		if strings.Contains(lower, "running") {
			return false, "container still running"
		}
		if strings.Contains(lower, "stopped") || strings.Contains(lower, "exited") || strings.Contains(lower, "killed") {
			return true, "container stopped"
		}
		return false, "container still present without stopped evidence"
	}
	if seen {
		return true, "container absent from valid listing"
	}
	return false, "container id never appeared in ctr tasks list"
}
