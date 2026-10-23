package server

import (
	"log"
	"regexp"
)

// credentialQuery matches query parameters that carry agent credentials. The
// agent authenticates through the URL query string (the server contract), so
// any error derived from net/http carries that secret inside `*url.Error`.
var credentialQuery = regexp.MustCompile(`(?i)([?&;])(token|transfer_token|access_token|api_key)=[^&\s"'#]*`)

const redactedPlaceholder = "REDACTED"

// redactCredentials replaces credential values in free-form text so it can be
// logged or reported without exposing the agent token.
func redactCredentials(text string) string {
	if text == "" {
		return text
	}
	return credentialQuery.ReplaceAllString(text, "${1}${2}="+redactedPlaceholder)
}

// redactError returns err's message with credential values removed.
func redactError(err error) string {
	if err == nil {
		return ""
	}
	return redactCredentials(err.Error())
}

// logError records a transport or protocol error. net/http embeds the full
// request URL in its errors, and that URL contains the agent token, so the
// message is always redacted before it reaches the log.
func logError(prefix string, err error) {
	log.Printf("%s: %s", prefix, redactError(err))
}
