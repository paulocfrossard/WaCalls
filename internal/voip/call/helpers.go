package call

import (
	"context"
	"strings"
	"wacalls/internal/voip/core"
	"wacalls/internal/voip/wanode"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
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

// sameDeviceJID reports whether two JID strings are the same DEVICE (or the
// same LID/PN alias of that device). Different devices of the same account are
// NOT the same. Returns true on uncertainty so a legitimate terminate from the
// answering device is never dropped.
func (m *CallManager) sameDeviceJID(a, b string) bool {
	if a == "" || b == "" || m.sock == nil {
		return true
	}
	if a == b {
		return true
	}
	ja, ea := types.ParseJID(a)
	jb, eb := types.ParseJID(b)
	if ea != nil || eb != nil {
		return true
	}
	// Only when BOTH sides carry a concrete device number can we prove they
	// are different devices. Device 0 (bare JID) is treated as unknown so a
	// terminate from the answering device is never dropped just because one
	// spelling omits the device suffix.
	if ja.Device != 0 && jb.Device != 0 && ja.Device != jb.Device {
		return false
	}
	ca, cb := wanode.CleanJID(a), wanode.CleanJID(b)
	if ca == cb {
		return true
	}
	// Same device number: tolerate LID/PN spelling of the same account.
	la := m.sock.ResolveLIDForPN(context.Background(), ja.ToNonAD())
	lb := m.sock.ResolveLIDForPN(context.Background(), jb.ToNonAD())
	return wanode.CleanJID(la.String()) == wanode.CleanJID(lb.String())
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
