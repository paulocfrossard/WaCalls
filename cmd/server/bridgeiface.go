package main

// browserBridge is the minimal surface the session registry needs from a
// browser-leg bridge. Keeping it as an interface lets tests inject a fake
// without a real PeerConnection.
type browserBridge interface {
	WritePCM(pcm []float32) error
	Detach()
	Close()
}

// *Bridge satisfies browserBridge.
var _ browserBridge = (*Bridge)(nil)
