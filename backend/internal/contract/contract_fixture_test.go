package v1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type fixture struct {
	Version string `json:"version"`
	Routes  []struct {
		Name    string `json:"name"`
		Method  string `json:"method"`
		Path    string `json:"path"`
		Auth    string `json:"auth"`
		Success struct {
			Status      int  `json:"status"`
			Streaming   bool `json:"streaming"`
			Cancellable bool `json:"cancellable"`
			Sanitized   bool `json:"sanitized"`
		} `json:"success"`
	} `json:"routes"`
	Error struct {
		RequiredFields        []string `json:"requiredFields"`
		SecretFieldsForbidden []string `json:"secretFieldsForbidden"`
	} `json:"error"`
}

func TestNeonixHTTPContractFixture(t *testing.T) {
	path := filepath.Join("..", "..", "..", "contracts", "fixtures", "neonix-http-contract.v1.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got fixture
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != "neonix-http-v1" {
		t.Fatalf("unexpected contract version %q", got.Version)
	}
	if len(got.Routes) == 0 {
		t.Fatal("contract must define at least one route")
	}
	for _, route := range got.Routes {
		if route.Name == "" || route.Method == "" || route.Path == "" {
			t.Fatalf("route is missing identity: %+v", route)
		}
		if route.Auth != "none" && route.Auth != "operator" && route.Auth != "gateway" && route.Auth != "internal" {
			t.Fatalf("route %q has unsupported auth boundary %q", route.Name, route.Auth)
		}
		if route.Success.Status < 200 || route.Success.Status > 299 {
			t.Fatalf("route %q has invalid success status %d", route.Name, route.Success.Status)
		}
	}
	for _, field := range []string{"error", "errorCode"} {
		found := false
		for _, required := range got.Error.RequiredFields {
			if required == field {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("error contract must require %q", field)
		}
	}
}
