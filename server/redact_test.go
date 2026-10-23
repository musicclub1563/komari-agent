package server

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/komari-monitor/komari-agent/dnsresolver"
)

const logMarkerToken = "MARKER-LEAK-TOKEN-9f3a"

func TestRedactCredentialsRemovesTokenValues(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "rpc token in url error",
			input: `Post "http://panel.example/api/clients/v2/rpc?token=` + logMarkerToken + `": dial tcp 127.0.0.1:1: connect: connection refused`,
			want:  `Post "http://panel.example/api/clients/v2/rpc?token=REDACTED": dial tcp 127.0.0.1:1: connect: connection refused`,
		},
		{
			name:  "transfer url carries two credentials",
			input: `Post "http://panel.example/api/clients/transfer/abc?token=` + logMarkerToken + `&transfer_token=` + logMarkerToken + `": EOF`,
			want:  `Post "http://panel.example/api/clients/transfer/abc?token=REDACTED&transfer_token=REDACTED": EOF`,
		},
		{
			name:  "credential followed by other parameters",
			input: `http://panel.example/api/clients/v2/rpc?token=` + logMarkerToken + `&id=42`,
			want:  `http://panel.example/api/clients/v2/rpc?token=REDACTED&id=42`,
		},
		{
			name:  "no credential is left untouched",
			input: `dial tcp 127.0.0.1:1: connect: connection refused`,
			want:  `dial tcp 127.0.0.1:1: connect: connection refused`,
		},
		{
			name:  "empty input",
			input: "",
			want:  "",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := redactCredentials(testCase.input); got != testCase.want {
				t.Fatalf("redactCredentials() = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestLogErrorHidesTokenInURLError asserts the production logging helper never
// writes the agent token, while also pinning that net/http really does embed it
// in the raw error (otherwise this test would no longer prove anything).
func TestLogErrorHidesTokenInURLError(t *testing.T) {
	rawErr := &url.Error{
		Op:  "Post",
		URL: "http://panel.example/api/clients/v2/rpc?token=" + logMarkerToken,
		Err: errors.New("dial tcp 127.0.0.1:1: connect: connection refused"),
	}
	if !strings.Contains(rawErr.Error(), logMarkerToken) {
		t.Fatalf("precondition failed: net/http error does not embed the token: %v", rawErr)
	}

	var buffer bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buffer)
	defer log.SetOutput(previous)

	logError("Failed to POST v2 report", rawErr)

	logged := buffer.String()
	if strings.Contains(logged, logMarkerToken) {
		t.Fatalf("log leaked the token: %s", logged)
	}
	if !strings.Contains(logged, "token=REDACTED") {
		t.Fatalf("log is missing the redacted form: %s", logged)
	}
	if !strings.Contains(logged, "Failed to POST v2 report") {
		t.Fatalf("log is missing the operation name: %s", logged)
	}
}

// TestTaskExecutionLogOmitsCommandBody pins that a task's command text, which
// routinely carries credentials, never reaches the log.
func TestTaskExecutionLogOmitsCommandBody(t *testing.T) {
	var buffer bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buffer)
	defer log.SetOutput(previous)

	logTaskExecution("task-42", "psql \"postgres://user:"+logMarkerToken+"@db/prod\" --command 'select 1'")

	logged := buffer.String()
	if strings.Contains(logged, logMarkerToken) {
		t.Fatalf("log leaked the command body: %s", logged)
	}
	if !strings.Contains(logged, "task-42") {
		t.Fatalf("log is missing the task id: %s", logged)
	}
}

// TestCrossHostRedirectDoesNotForwardToken documents the redirect behaviour of
// the HTTP client the agent uses: a 30x to another origin must not carry the
// query credential with it.
func TestCrossHostRedirectDoesNotForwardToken(t *testing.T) {
	var (
		mu       sync.Mutex
		received string
		sawAny   bool
	)
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		received = request.URL.RawQuery
		sawAny = true
		mu.Unlock()
		writer.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	frontend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, backend.URL+"/capture", http.StatusFound)
	}))
	defer frontend.Close()

	client := dnsresolver.GetHTTPClientWithPreference(5*time.Second, "")
	request, err := http.NewRequest(http.MethodPost,
		frontend.URL+"/api/clients/v2/rpc?token="+logMarkerToken, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("follow redirect: %v", err)
	}
	_ = response.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if !sawAny {
		t.Fatal("redirect target was never reached")
	}
	if strings.Contains(received, logMarkerToken) {
		t.Fatalf("redirect forwarded the token to another origin: %q", received)
	}
}
