package v1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type pythonAutomationRoute struct {
	Method        string `json:"method"`
	Path          string `json:"path"`
	Auth          string `json:"auth"`
	SuccessStatus int    `json:"successStatus"`
}

type pythonAutomationFixture struct {
	Version        string `json:"version"`
	Authentication struct {
		Header          string `json:"header"`
		SharedSecretEnv string `json:"sharedSecretEnv"`
		MaxHeaderBytes  int    `json:"maxHeaderBytes"`
	} `json:"authentication"`
	Limits struct {
		RequestBodyBytes  int `json:"requestBodyBytes"`
		ResponseBodyBytes int `json:"responseBodyBytes"`
		JobIDBytes        int `json:"jobIdBytes"`
	} `json:"limits"`
	SupportedJobTypes []string `json:"supportedJobTypes"`
	Worker            struct {
		BaseURLEnv     string                  `json:"baseUrlEnv"`
		CallbackURLEnv string                  `json:"callbackUrlEnv"`
		Routes         []pythonAutomationRoute `json:"routes"`
	} `json:"worker"`
	RuntimeManager struct {
		BaseURLEnv        string                  `json:"baseUrlEnv"`
		SecretEnv         string                  `json:"secretEnv"`
		SecretFallbackEnv string                  `json:"secretFallbackEnv"`
		Routes            []pythonAutomationRoute `json:"routes"`
	} `json:"runtimeManager"`
	JobStatusValues                        []string `json:"jobStatusValues"`
	SecretFieldsForbiddenInPublicResponses []string `json:"secretFieldsForbiddenInPublicResponses"`
}

func TestPythonAutomationHTTPContractFixture(t *testing.T) {
	path := filepath.Join("..", "..", "..", "contracts", "fixtures", "python-automation-http-contract.v1.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got pythonAutomationFixture
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != "neonix-python-automation-v1" {
		t.Fatalf("unexpected contract version %q", got.Version)
	}
	if got.Authentication.Header != "x-api-key" || got.Authentication.SharedSecretEnv != "PYAUTO_INTERNAL_API_KEY" || got.Authentication.MaxHeaderBytes <= 0 {
		t.Fatalf("invalid internal authentication contract: %+v", got.Authentication)
	}
	if got.Limits.RequestBodyBytes <= 0 || got.Limits.ResponseBodyBytes <= 0 || got.Limits.JobIDBytes != 128 {
		t.Fatalf("invalid automation limits: %+v", got.Limits)
	}
	if strings.Join(got.SupportedJobTypes, ",") != "register,register_subscribe,relogin,inject" {
		t.Fatalf("unexpected supported job types: %v", got.SupportedJobTypes)
	}
	if got.Worker.BaseURLEnv != "PYAUTO_BASE_URL" || got.Worker.CallbackURLEnv != "PYAUTO_BACKEND_CALLBACK_URL" {
		t.Fatalf("invalid worker environment contract: %+v", got.Worker)
	}
	if got.RuntimeManager.BaseURLEnv != "RUNTIME_MANAGER_URL" || got.RuntimeManager.SecretFallbackEnv != "PYAUTO_INTERNAL_API_KEY" {
		t.Fatalf("invalid runtime manager environment contract: %+v", got.RuntimeManager)
	}

	required := []string{
		"GET /health none",
		"POST /job/start internal",
		"GET /job/{job_id}/status internal",
		"GET /job/{job_id}/logs internal",
		"POST /job/{job_id}/cancel internal",
		"GET /job/bfs-lockout internal",
		"POST /api/mailbox/poll internal",
		"manager:GET /health none",
		"manager:GET /status internal",
		"manager:POST /acquire internal",
		"manager:POST /release internal",
	}
	actual := make([]string, 0, len(got.Worker.Routes)+len(got.RuntimeManager.Routes))
	for _, route := range got.Worker.Routes {
		validatePythonAutomationRoute(t, route)
		actual = append(actual, route.Method+" "+route.Path+" "+route.Auth)
	}
	for _, route := range got.RuntimeManager.Routes {
		validatePythonAutomationRoute(t, route)
		actual = append(actual, "manager:"+route.Method+" "+route.Path+" "+route.Auth)
	}
	sort.Strings(required)
	sort.Strings(actual)
	if strings.Join(required, "|") != strings.Join(actual, "|") {
		t.Fatalf("automation route contract mismatch\nwant: %v\n got: %v", required, actual)
	}
	for _, requiredSecret := range []string{"access_token", "refresh_token", "password", "api_key", "callback_url", "code_verifier"} {
		if !containsString(got.SecretFieldsForbiddenInPublicResponses, requiredSecret) {
			t.Fatalf("missing forbidden public secret field %q", requiredSecret)
		}
	}

	checksumPath := path + ".sha256"
	checksumData, err := os.ReadFile(checksumPath)
	if err != nil {
		t.Fatal(err)
	}
	wantChecksum := strings.Fields(string(checksumData))
	if len(wantChecksum) == 0 {
		t.Fatal("empty automation contract checksum")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != wantChecksum[0] {
		t.Fatal("automation contract checksum is stale")
	}
}

func validatePythonAutomationRoute(t *testing.T, route pythonAutomationRoute) {
	t.Helper()
	if route.Method == "" || route.Path == "" || (route.Auth != "none" && route.Auth != "internal") {
		t.Fatalf("invalid automation route: %+v", route)
	}
	if route.SuccessStatus < 200 || route.SuccessStatus > 299 {
		t.Fatalf("invalid automation success status: %+v", route)
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
