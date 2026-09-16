package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type registerRuntimeCancelledTransport struct{}

func (registerRuntimeCancelledTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	<-request.Context().Done()
	return nil, request.Context().Err()
}

func TestPythonRegisterRuntimeHonorsCancellation(t *testing.T) {
	runtime := NewPythonRegisterRuntimeWithConfig("http://worker.test", "key", &http.Client{Transport: registerRuntimeCancelledTransport{}}, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	_, err := runtime.Status(ctx, "job_cancelled")
	require.Error(t, err)
	require.Less(t, time.Since(started), time.Second)
	require.Equal(t, "REGISTER_RUNTIME_UNAVAILABLE", RegisterRuntimeErrorCode(err))
}

func TestPythonRegisterRuntimeUsesAuthenticatedFixedWorkerRoutes(t *testing.T) {
	var mu sync.Mutex
	seen := make([]string, 0, 5)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "worker-secret", r.Header.Get("x-api-key"))
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.EscapedPath())
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/job/start":
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "job_123", body["job_id"])
			require.Equal(t, "register", body["type"])
			config := body["register_config"].(map[string]any)
			require.Equal(t, "antigravity", config["target_provider"])
			_, _ = io.WriteString(w, `{"job_id":"job_123","status":"pending"}`)
		case "/job/job_123/status":
			_, _ = io.WriteString(w, `{"job_id":"job_123","status":"running","logs":["started"]}`)
		case "/job/job_123/logs":
			_, _ = io.WriteString(w, `{"logs":["started"]}`)
		case "/job/job_123/cancel":
			_, _ = io.WriteString(w, `{"cancelled":true}`)
		case "/job/bfs-lockout":
			_, _ = io.WriteString(w, `{"bfs_blocked_until":12345}`)
		case "/capabilities":
			_, _ = io.WriteString(w, `{"camoufox":{"available":true,"packageInstalled":true,"binaryAvailable":true,"supportedProviders":["grok"]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	runtime := NewPythonRegisterRuntimeWithConfig(server.URL, "worker-secret", server.Client(), "")
	started, err := runtime.Start(context.Background(), PythonRegisterStartRequest{
		JobID: "job_123", Type: "register",
		RegisterConfig: &PythonRegisterConfig{TargetProvider: "antigravity", RegisterMethod: "google", Count: 1},
	})
	require.NoError(t, err)
	require.Equal(t, "pending", started.Status)

	status, err := runtime.Status(context.Background(), "job_123")
	require.NoError(t, err)
	require.Equal(t, []string{"started"}, status.Logs)
	logs, err := runtime.Logs(context.Background(), "job_123")
	require.NoError(t, err)
	require.Equal(t, []string{"started"}, logs.Logs)
	cancelled, err := runtime.Cancel(context.Background(), "job_123")
	require.NoError(t, err)
	require.True(t, cancelled.Cancelled)
	lockout, err := runtime.BFSLockout(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(12345), lockout.BFSBlockedUntil)
	capabilities, err := runtime.Capabilities(context.Background())
	require.NoError(t, err)
	require.True(t, capabilities["camoufox"].Available)
	require.Equal(t, []string{"grok"}, capabilities["camoufox"].SupportedProviders)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{
		"POST /job/start",
		"GET /job/job_123/status",
		"GET /job/job_123/logs",
		"POST /job/job_123/cancel",
		"GET /job/bfs-lockout",
		"GET /capabilities",
	}, seen)
}

func TestPythonRegisterRuntimeRejectsInvalidJobIDsBeforeNetwork(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	runtime := NewPythonRegisterRuntimeWithConfig(server.URL, "key", server.Client(), "")

	for _, jobID := range []string{"", "../secret", "job/other", strings.Repeat("a", 129)} {
		_, err := runtime.Status(context.Background(), jobID)
		require.Equal(t, "REGISTER_PAYLOAD_INVALID", RegisterRuntimeErrorCode(err))
	}
	_, err := runtime.Start(context.Background(), PythonRegisterStartRequest{JobID: "valid-id", Type: "arbitrary"})
	require.Equal(t, "REGISTER_PAYLOAD_INVALID", RegisterRuntimeErrorCode(err))
	require.Zero(t, requests.Load())
}

func TestPythonRegisterRuntimeClassifiesFailuresWithoutLeakingWorkerBody(t *testing.T) {
	for _, test := range []struct {
		status int
		code   string
	}{
		{http.StatusBadRequest, "REGISTER_PAYLOAD_INVALID"},
		{http.StatusUnprocessableEntity, "REGISTER_PAYLOAD_INVALID"},
		{http.StatusUnauthorized, "REGISTER_RUNTIME_UNAUTHORIZED"},
		{http.StatusForbidden, "REGISTER_RUNTIME_UNAUTHORIZED"},
		{http.StatusNotFound, "REGISTER_JOB_NOT_FOUND"},
		{http.StatusConflict, "REGISTER_JOB_CONFLICT"},
		{http.StatusTooManyRequests, "REGISTER_BFS_LOCKOUT"},
		{http.StatusBadGateway, "REGISTER_RUNTIME_UNAVAILABLE"},
	} {
		t.Run(fmt.Sprintf("status_%d", test.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, `{"detail":"refresh-token-must-never-leak"}`)
			}))
			t.Cleanup(server.Close)
			runtime := NewPythonRegisterRuntimeWithConfig(server.URL, "key", server.Client(), "")
			_, err := runtime.Status(context.Background(), "job_1")
			require.Equal(t, test.code, RegisterRuntimeErrorCode(err))
			require.NotContains(t, err.Error(), "refresh-token")
		})
	}
}

func TestPythonRegisterRuntimeRejectsOversizedAndInvalidResponses(t *testing.T) {
	for _, body := range []string{strings.Repeat("x", registerRuntimeBodyLimit+1), `{not-json}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, body)
		}))
		runtime := NewPythonRegisterRuntimeWithConfig(server.URL, "key", server.Client(), "")
		_, err := runtime.Status(context.Background(), "job_1")
		server.Close()
		if body == "{not-json}" {
			require.Equal(t, "REGISTER_RUNTIME_PROTOCOL_INVALID", RegisterRuntimeErrorCode(err))
		} else {
			require.Equal(t, "REGISTER_RUNTIME_UNAVAILABLE", RegisterRuntimeErrorCode(err))
		}
	}
}

func TestPythonRegisterRuntimeRejectsMismatchedAndInvalidJobStatus(t *testing.T) {
	for _, body := range []string{
		"{\"job_id\":\"other_job\",\"status\":\"running\"}",
		"{\"job_id\":\"job_1\",\"status\":\"mystery\"}",
		"{\"status\":\"running\"}",
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, body)
		}))
		runtime := NewPythonRegisterRuntimeWithConfig(server.URL, "key", server.Client(), "")
		_, err := runtime.Status(context.Background(), "job_1")
		server.Close()
		require.Equal(t, "REGISTER_RUNTIME_PROTOCOL_INVALID", RegisterRuntimeErrorCode(err))
		require.Equal(t, http.StatusBadGateway, RegisterRuntimePublicStatus(err))
	}
}

func TestRegisterRuntimeInternalUnauthorizedMapsToServiceUnavailable(t *testing.T) {
	err := registerRuntimeError("REGISTER_RUNTIME_UNAUTHORIZED", http.StatusUnauthorized, nil)
	require.Equal(t, http.StatusServiceUnavailable, RegisterRuntimePublicStatus(err))
}
