package opencode

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func TestParseFreeModelsUsesNamesPricingAndKnownFallbacks(t *testing.T) {
	models, err := ParseFreeModels([]byte(`{"data":[{"id":"paid","name":"Paid","pricing":{"input":1,"output":0}},{"id":"new-free","name":"New Free","pricing":{"input":1}},{"id":"zero-cost","name":"Zero","cost":{"input":"$0","output":0}},{"id":"big-pickle","name":""}]}`))
	require.NoError(t, err)
	require.Len(t, models, 3)
	require.Equal(t, "new-free", models[0].ID)
	require.Equal(t, "zero-cost", models[1].ID)
	require.Equal(t, "Big Pickle", models[2].Name)
	require.Equal(t, 0, models[2].SortOrder)
}

func TestParseFreeModelsRejectsMixedOrMissingPricing(t *testing.T) {
	models, err := ParseFreeModels([]byte(`{"data":[
		{"id":"paid","name":"Paid","pricing":{"input":0,"output":0.25}},
		{"id":"unknown","name":"Unknown"},
		{"id":"free-by-string","name":"Free by string","pricing":{"input":"free","output":"-"}}
	]}`))
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.Equal(t, "free-by-string", models[0].ID)
}

func TestFetchFreeModelsUsesFixedURLAndRejectsFailure(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		require.Equal(t, ModelsURL, req.URL.String())
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"free-model","name":"Free Model"}]}`))}, nil
	})}
	models, err := FetchFreeModels(context.Background(), client)
	require.NoError(t, err)
	require.Equal(t, "free-model", models[0].ID)

	client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("upstream unavailable"))}, nil
	})
	_, err = FetchFreeModels(context.Background(), client)
	require.ErrorIs(t, err, ErrCatalogueUnavailable)
}
