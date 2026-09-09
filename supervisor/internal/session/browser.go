package session

// BrowserVerification is whether the session can drive the developer's own
// browser right now. It carries no error: an unavailable result is an ordinary
// answer, so a caller cannot mistake the absence of this capability for a
// failure and abandon what it was doing (9.12).
type BrowserVerification struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// InteractiveBrowserVerification decides, at the moment of asking, whether the
// agent may verify through the developer's browser (9.11).
//
// Two conditions have to hold and neither is knowable ahead of time: the
// browser extension lives on the developer's machine and is reached through
// their attached client, and Claude Code disables the integration outright on a
// session authenticated with a long-lived credential — which is how the
// platform starts every workspace, so this is normally unavailable and the
// autonomous loop must not lean on it.
func (s *Supervisor) InteractiveBrowserVerification() BrowserVerification {
	if s.LongLivedAuth {
		return BrowserVerification{Reason: "session authenticated with a long-lived credential"}
	}
	clients, err := s.Tmux.ListClients()
	if err != nil {
		return BrowserVerification{Reason: "cannot determine attached clients: " + err.Error()}
	}
	if firstWritable(clients) == nil {
		return BrowserVerification{Reason: "no developer client holds the session"}
	}
	return BrowserVerification{Available: true}
}
