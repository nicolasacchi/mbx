package commands

import (
	"fmt"

	"github.com/nicolasacchi/mbx/internal/aliases"
)

// validationError is a typed error for a malformed positional/query id
// argument (W14, SEC-002). Kind()/ExitCode() satisfy main.go's
// errors.As(interface{ ExitCode() int }) probe the same way client.APIError
// and config.NoCredentialsError do, mapping to the fleet validation exit
// code (3) instead of the generic 1 an untyped fmt.Errorf gets.
type validationError struct {
	msg string
}

func (e *validationError) Error() string { return e.msg }
func (e *validationError) Kind() string  { return "validation" }
func (e *validationError) ExitCode() int { return 3 }

// validateID rejects anything that isn't a bare non-negative integer before
// it's spliced into a URL path or query parameter. Applied at every read
// command that takes a numeric id as a positional arg (databases get,
// dashboards get/cards/related/revisions/add-card/subscriptions, tables
// get, collections items — W14): a positional id containing "/", "..", SQL,
// or anything else is almost certainly a mistake (an alias pasted where a
// numeric id was expected, wrong flag order) or, worse, a
// path-traversal/injection-shaped string. Failing closed here is cheap —
// every real Metabase id is already a bare non-negative integer, so the
// contract costs nothing on the happy path.
//
// Delegates to aliases.ParseInt (strconv.Atoi + reject negative), which
// already has exactly this contract; this wrapper's only job is turning a
// bare bool into a typed, actionable error.
func validateID(arg string) (int, error) {
	n, ok := aliases.ParseInt(arg)
	if !ok {
		return 0, &validationError{msg: fmt.Sprintf("invalid id %q: must be a non-negative integer", arg)}
	}
	return n, nil
}
