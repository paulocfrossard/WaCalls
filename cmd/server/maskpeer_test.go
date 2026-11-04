package main

import "testing"

func TestMaskPeer(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "****"},
		{"abc@x", "****@x"},
		{"1234@s.whatsapp.net", "****@s.whatsapp.net"},
		{"41129277939839:0@lid", "**********9839@lid"},
		{"5532999999999@s.whatsapp.net", "*********9999@s.whatsapp.net"},
		// Multi-byte user part must be sliced by rune, not byte.
		{"èèèèè@x", "*èèèè@x"},
	}
	for _, c := range cases {
		if got := maskPeer(c.in); got != c.want {
			t.Errorf("maskPeer(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
