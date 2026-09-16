package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPythonRuntimeManagerUsesAuthenticatedFixedRoutes(t *testing.T) {
	var mu sync.Mutex
	seen := make([]string, 0, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "manager-secret", r.Header.Get("x-api-key"))
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/acquire":
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "neonix-register-job_1", body["lease_id"])
			require.Equal(t, true, body["python"])
			require.Equal(t, true, body["captcha"])
			_, _ = io.WriteString(w, `{"python":{"running":true,"port":7788},"captcha":{"running":true,"port":8877},"leases":1}`)
		case "/status":
			_, _ = io.WriteString(w, `{"python":{"running":true,"port":7788},"captcha":{"running":false,"port":8877},"leases":2}`)
		case "/release":
			var body map[string]string
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "neonix-register-job_1", body["lease_id"])
			_, _ = io.WriteString(w, `{"python":{"running":true},"leases":1}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	manager := NewPythonRuntimeManagerWithConfig(server.URL, "manager-secret", server.Client())
	status, err := manager.Acquire(context.Background(), "neonix-register-job_1", true)
	require.NoError(t, err)
	require.True(t, status.Python.Running)
	status, err = manager.Status(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, status.Leases)
	require.NoError(t, manager.Release(context.Background(), "neonix-register-job_1"))

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"POST /acquire", "GET /status", "POST /release"}, seen)
}

func TestPythonRuntimeManagerRejectsInvalidLeaseAndResponse(t *testing.T) {
	manager := NewPythonRuntimeManagerWithConfig("http://manager.test", "key", &http.Client{})
	_, err := manager.Acquire(context.Background(), "../escape", false)
	require.Error(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"leases":1}`)
	}))
	t.Cleanup(server.Close)
	manager = NewPythonRuntimeManagerWithConfig(server.URL, "key", server.Client())
	_, err = manager.Status(context.Background())
	require.Error(t, err)
	require.NotContains(t, err.Error(), "key")
}

type cancellingRuntimeManagerTransport struct{}

func (cancellingRuntimeManagerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	<-request.Context().Done()
	return nil, request.Context().Err()
}

func TestPythonRuntimeManagerHonorsCallerCancellation(t *testing.T) {
	manager := NewPythonRuntimeManagerWithConfig("http://manager.test", "key", &http.Client{Transport: cancellingRuntimeManagerTransport{}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	_, err := manager.Acquire(ctx, "neonix-register-job_1", false)
	require.Error(t, err)
	require.Less(t, time.Since(started), time.Second)
	require.True(t, errors.Is(err, context.Canceled))
}

func TestPythonRegisterRuntimeLeaseLifecycle(t *testing.T) {
	var mu sync.Mutex
	requests := make([]string, 0, 8)
	jobStatus := "running"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/acquire":
			_, _ = io.WriteString(w, `{"python":{"running":true},"captcha":{"running":true},"leases":1}`)
		case "/release":
			_, _ = io.WriteString(w, `{"python":{"running":true},"leases":0}`)
		case "/job/start":
			_, _ = io.WriteString(w, `{"job_id":"job_lease","status":"pending"}`)
		case "/job/job_lease/status":
			_, _ = io.WriteString(w, `{"job_id":"job_lease","status":"`+jobStatus+`"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	manager := NewPythonRuntimeManagerWithConfig(server.URL, "key", server.Client())
	runtime := NewPythonRegisterRuntimeWithConfig(server.URL, "key", server.Client(), "").WithRuntimeManager(manager)
	_, err := runtime.Start(context.Background(), PythonRegisterStartRequest{
		JobID: "job_lease", Type: "register", RegisterConfig: &PythonRegisterConfig{TargetProvider: "kiro", RegisterMethod: "email"},
	})
	require.NoError(t, err)
	_, err = runtime.Status(context.Background(), "job_lease")
	require.NoError(t, err)
	jobStatus = "done"
	_, err = runtime.Status(context.Background(), "job_lease")
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{
		"POST /acquire", "POST /job/start",
		"GET /job/job_lease/status", "GET /job/job_lease/status", "POST /release",
	}, requests)
}

func TestPythonRegisterRuntimeReleasesLeaseOnStartFailureAndCancel(t *testing.T) {
	var mu sync.Mutex
	releases := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/acquire":
			_, _ = io.WriteString(w, `{"python":{"running":true},"leases":1}`)
		case "/release":
			mu.Lock()
			releases++
			mu.Unlock()
			_, _ = io.WriteString(w, `{"python":{"running":true},"leases":0}`)
		case "/job/start", "/job/job_cancel/cancel":
			http.Error(w, "refresh-token-should-not-leak", http.StatusBadGateway)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	manager := NewPythonRuntimeManagerWithConfig(server.URL, "key", server.Client())
	runtime := NewPythonRegisterRuntimeWithConfig(server.URL, "key", server.Client(), "").WithRuntimeManager(manager)

	_, err := runtime.Start(context.Background(), PythonRegisterStartRequest{JobID: "job_start", Type: "register"})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "refresh-token")
	_, err = runtime.Cancel(context.Background(), "job_cancel")
	require.Error(t, err)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 2, releases)
}

func TestPythonRegisterRuntimeStatusDistinguishesManagerAndChild(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			_, _ = io.WriteString(w, `{"python":{"running":true},"leases":3}`)
		case "/health":
			http.Error(w, "starting", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	manager := NewPythonRuntimeManagerWithConfig(server.URL, "key", server.Client())
	runtime := NewPythonRegisterRuntimeWithConfig(server.URL, "key", server.Client(), "").WithRuntimeManager(manager)
	status := runtime.RuntimeStatus(context.Background())
	require.True(t, status.ManagerConfigured)
	require.True(t, status.ManagerRunning)
	require.True(t, status.ChildRunning)
	require.False(t, status.ChildReady)
	require.Equal(t, 3, status.Leases)
}

func TestPythonMailboxRuntimeAcquiresAndReleasesIndependentLease(t *testing.T) {
	var mu sync.Mutex
	acquired, released := "", ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/acquire":
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, false, body["captcha"])
			mu.Lock()
			acquired, _ = body["lease_id"].(string)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"python":{"running":true},"leases":1}`)
		case "/api/mailbox/poll":
			_, _ = io.WriteString(w, `{"status":"complete","email":"owner@example.com"}`)
		case "/release":
			var body map[string]string
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			mu.Lock()
			released = body["lease_id"]
			mu.Unlock()
			_, _ = io.WriteString(w, `{"python":{"running":true},"leases":0}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	manager := NewPythonRuntimeManagerWithConfig(server.URL, "key", server.Client())
	runtime := NewPythonMailboxRuntimeWithConfig(server.URL, "key", server.Client(), "").WithRuntimeManager(manager)
	_, err := runtime.Poll(context.Background(), MailboxPollInput{
		Email: "owner@example.com", ClientID: "client", RefreshToken: "refresh", Timeout: 3 * time.Second,
	})
	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	require.True(t, strings.HasPrefix(acquired, "neonix-mailbox-"))
	require.Equal(t, acquired, released)
}
