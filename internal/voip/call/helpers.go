package call

import (
	"strings"
	"wacalls/internal/voip/core"
	"wacalls/internal/voip/wanode"

	waBinary "go.mau.fi/whatsmeow/binary"
)

func hasChildTag(n *waBinary.Node, tag string) bool {
	for _, c := range wanode.NodeChildren(n) {
		if c.Tag == tag {
			return true
		}
	}
	return false
}

func ensureDeviceJid(jid string) string {
	if strings.Contains(jid, ":") {

		if at := strings.Index(jid, "@"); at > strings.Index(jid, ":") {
			return jid
		}
	}
	return strings.Replace(jid, "@", ":0@", 1)
}

// isOwnBase reports whether jid (device or not) resolves to one of our own
// identities (LID and/or PN).
func isOwnBase(jid string, ownBases []string) bool {
	clean := wanode.CleanJID(jid)
	for _, b := range ownBases {
		if b != "" && clean == b {
			return true
		}
	}
	return false
}

func findOurDevice(participants []string, ownBases []string, fallback string) string {
	// Prefer the device form of the first identity (ownBaseJIDs lists the
	// credential JID first), then any other own identity.
	for _, base := range ownBases {
		for _, jid := range participants {
			if strings.Contains(jid, ":") && wanode.CleanJID(jid) == base {
				return jid
			}
		}
	}
	return fallback
}

func firstPeerDevice(participants []string, ownBases []string) string {
	for _, jid := range participants {
		if !isOwnBase(jid, ownBases) {
			return jid
		}
	}
	return ""
}

func firstSsrc(s []uint32) uint32 {
	if len(s) > 0 {
		return s[0]
	}
	return 0
}

// childTagSummary renders a node's immediate children as "tag[subtag,subtag]"
// for diagnostics — e.g. seeing whether an offer carries a structured
// "relay[key,token,te2]" node or no relay node at all.
func childTagSummary(n *waBinary.Node) string {
	if n == nil {
		return "<nil>"
	}
	var parts []string
	for _, c := range wanode.NodeChildren(n) {
		tag := c.Tag
		if sub := wanode.NodeChildren(&c); len(sub) > 0 {
			subtags := make([]string, 0, len(sub))
			for _, s := range sub {
				subtags = append(subtags, s.Tag)
			}
			tag += "[" + strings.Join(subtags, ",") + "]"
		}
		parts = append(parts, tag)
	}
	return strings.Join(parts, " ")
}

func relayEndpointCount(rd *core.RelayData) int {
	if rd == nil {
		return 0
	}
	return len(rd.Endpoints)
}

func containsSsrc(s []uint32, v uint32) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
