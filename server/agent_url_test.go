package server

import (
	"net/url"
	"strings"
	"testing"

	pkg_flags "github.com/komari-monitor/komari-agent/cmd/flags"
)

func TestBuildAgentURLPreservesExistingShapeForPlainTokens(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		path     string
		params   map[string]string
		want     string
	}{
		{
			name:     "plain endpoint",
			endpoint: "http://panel.example",
			path:     "/api/clients/v2/rpc",
			params:   map[string]string{"token": "abc123"},
			want:     "http://panel.example/api/clients/v2/rpc?token=abc123",
		},
		{
			name:     "trailing slash is not doubled",
			endpoint: "http://panel.example/",
			path:     "/api/clients/v2/rpc",
			params:   map[string]string{"token": "abc123"},
			want:     "http://panel.example/api/clients/v2/rpc?token=abc123",
		},
		{
			name:     "endpoint with base path",
			endpoint: "https://panel.example/komari/",
			path:     "/api/clients/v2/rpc",
			params:   map[string]string{"token": "abc123"},
			want:     "https://panel.example/komari/api/clients/v2/rpc?token=abc123",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := buildAgentURL(testCase.endpoint, testCase.path, testCase.params); got != testCase.want {
				t.Fatalf("buildAgentURL() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestBuildAgentURLEscapesParameterValues(t *testing.T) {
	token := "a&b=c d#e"
	built := buildAgentURL("http://panel.example", "/api/clients/v2/rpc", map[string]string{"token": token})

	if strings.Contains(built, "a&b=c d") {
		t.Fatalf("token was not escaped: %q", built)
	}
	parsed, err := url.Parse(built)
	if err != nil {
		t.Fatalf("parse built url: %v", err)
	}
	query := parsed.Query()
	if got := query.Get("token"); got != token {
		t.Fatalf("token round trip = %q, want %q", got, token)
	}
	if len(query) != 1 {
		t.Fatalf("parameter injection produced %d parameters: %v", len(query), query)
	}
}

func TestBuildAgentWebSocketURLMapsScheme(t *testing.T) {
	if got := buildAgentWebSocketURL("http://panel.example", "/api/clients/v2/rpc", map[string]string{"token": "t"}); got != "ws://panel.example/api/clients/v2/rpc?token=t" {
		t.Fatalf("http mapping = %q", got)
	}
	if got := buildAgentWebSocketURL("https://panel.example", "/api/clients/terminal", map[string]string{"token": "t", "id": "x"}); got != "wss://panel.example/api/clients/terminal?id=x&token=t" {
		t.Fatalf("https mapping = %q", got)
	}
}

// TestBuildFileTransferURLEscapesCredentials covers the file transfer endpoint,
// which already used url.Values; the assertion keeps that guarantee pinned.
func TestBuildFileTransferURLEscapesCredentials(t *testing.T) {
	previous := *pkg_flags.GlobalConfig
	*pkg_flags.GlobalConfig = previous
	pkg_flags.GlobalConfig.Endpoint = "http://panel.example"
	pkg_flags.GlobalConfig.Token = "agent&token"
	t.Cleanup(func() { *pkg_flags.GlobalConfig = previous })

	built, err := buildFileTransferURL(map[string]interface{}{
		"transfer_id":    "abc/def",
		"transfer_token": "tt&v",
	})
	if err != nil {
		t.Fatalf("buildFileTransferURL: %v", err)
	}
	parsed, err := url.Parse(built)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := parsed.Query().Get("token"); got != "agent&token" {
		t.Fatalf("token = %q, want %q", got, "agent&token")
	}
	if got := parsed.Query().Get("transfer_token"); got != "tt&v" {
		t.Fatalf("transfer_token = %q, want %q", got, "tt&v")
	}
	if !strings.Contains(parsed.Path, "/api/clients/transfer/") {
		t.Fatalf("unexpected path %q", parsed.Path)
	}
}
