package server

import (
	"net/url"
	"strings"
)

// buildAgentURL joins the configured endpoint with a control-plane path and its
// query parameters. The agent authenticates with a token in the query string
// (the server contract), so every value must be escaped: a token containing '&'
// or '#' would otherwise inject or truncate parameters.
func buildAgentURL(endpoint, path string, params map[string]string) string {
	base, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		// Keep the previous concatenation behaviour so an unparsable endpoint
		// still surfaces as a connection error instead of panicking.
		return strings.TrimSuffix(endpoint, "/") + path
	}
	base.Path = strings.TrimRight(base.Path, "/") + path
	base.RawPath = ""
	query := base.Query()
	for key, value := range params {
		query.Set(key, value)
	}
	base.RawQuery = query.Encode()
	return base.String()
}

// buildAgentWebSocketURL is buildAgentURL with the http(s) scheme mapped to
// ws(s). Endpoints with any other scheme are returned unchanged.
func buildAgentWebSocketURL(endpoint, path string, params map[string]string) string {
	built := buildAgentURL(endpoint, path, params)
	switch {
	case strings.HasPrefix(built, "https://"):
		return "wss://" + strings.TrimPrefix(built, "https://")
	case strings.HasPrefix(built, "http://"):
		return "ws://" + strings.TrimPrefix(built, "http://")
	}
	return built
}
