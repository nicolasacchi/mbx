package commands

import "strings"

// sandboxAgentHost is the documented operational trap W7 targets: the
// Metabase host mbx was built against sits outside a Claude Code agent's
// default Bash sandbox network allowlist. A forgotten dangerouslyDisableSandbox
// surfaces as a raw dial/connection failure with no hint pointing at the
// actual cause unless we name it here — see CLAUDE.md's "Sandbox" section.
const sandboxAgentHost = "metabase.1000farmacie.it"

// networkFailureHint returns a hint naming the sandbox setting when a
// network-class failure's request URL targets the production Metabase
// host — "" otherwise (a local dev instance, a different host, or no URL
// available at all).
func networkFailureHint(rawURL string) string {
	if !strings.Contains(rawURL, sandboxAgentHost) {
		return ""
	}
	return "network failure reaching " + sandboxAgentHost + " — if running under an agent sandbox (e.g. Claude Code's Bash tool), this host needs dangerouslyDisableSandbox: true; a forgotten sandbox flag is the most common cause of this error"
}
