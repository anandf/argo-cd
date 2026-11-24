package kubetransport

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	utilnet "k8s.io/apimachinery/pkg/util/net"
)

type roundTripperFunc func(req *http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// TestRoundTripper just copy request headers to the resposne.
type TestRoundTripper struct{}

func (rt TestRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp := http.Response{}
	resp.Header = http.Header{}
	for k, vs := range req.Header {
		for _, v := range vs {
			resp.Header.Add(k, v)
		}
	}
	return &resp, nil
}

func TestWithServerSideTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		url             string
		timeout         time.Duration
		expectedTimeout string
	}{
		{
			name:            "regular request",
			url:             "https://kubernetes.example/api/v1/pods",
			timeout:         time.Minute,
			expectedTimeout: "1m0s",
		},
		{
			name:            "preserves existing query parameters",
			url:             "https://kubernetes.example/api/v1/pods?limit=500&resourceVersion=123",
			timeout:         30 * time.Second,
			expectedTimeout: "30s",
		},
		{
			name:            "watch is left for the API server to classify",
			url:             "https://kubernetes.example/api/v1/pods?watch=true",
			timeout:         time.Minute,
			expectedTimeout: "1m0s",
		},
		{
			name:            "preserves explicit request timeout",
			url:             "https://kubernetes.example/api/v1/pods?timeout=5s",
			timeout:         time.Minute,
			expectedTimeout: "5s",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var receivedRequest *http.Request
			inner := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				receivedRequest = req
				return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
			})
			wrapped := WithServerSideTimeout(tt.timeout)(inner)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tt.url, http.NoBody)
			require.NoError(t, err)
			originalRawQuery := req.URL.RawQuery

			resp, err := wrapped.RoundTrip(req)
			require.NoError(t, err)
			require.NotNil(t, receivedRequest)
			assert.Equal(t, tt.expectedTimeout, receivedRequest.URL.Query().Get("timeout"))
			assert.Equal(t, req.URL.Query().Get("limit"), receivedRequest.URL.Query().Get("limit"))
			assert.Equal(t, req.URL.Query().Get("resourceVersion"), receivedRequest.URL.Query().Get("resourceVersion"))
			_, hasDeadline := receivedRequest.Context().Deadline()
			assert.False(t, hasDeadline)
			assert.Equal(t, originalRawQuery, req.URL.RawQuery, "the original request must not be mutated")
			require.NoError(t, resp.Body.Close())
		})
	}
}

func TestWithServerSideTimeoutIsTransparentWrapper(t *testing.T) {
	t.Parallel()

	inner := &TestRoundTripper{}
	wrapped := WithServerSideTimeout(time.Minute)(inner)
	wrapper, ok := wrapped.(utilnet.RoundTripperWrapper)
	require.True(t, ok)
	assert.Same(t, inner, wrapper.WrappedRoundTripper())
}

func TestWithServerSideTimeoutUsesDefaultTransportWhenInnerIsNil(t *testing.T) {
	t.Parallel()

	wrapped := WithServerSideTimeout(time.Minute)(nil)
	wrapper, ok := wrapped.(utilnet.RoundTripperWrapper)
	require.True(t, ok)
	assert.Same(t, http.DefaultTransport, wrapper.WrappedRoundTripper())
}

func TestWithServerSideTimeoutDisabledReturnsInnerTransport(t *testing.T) {
	t.Parallel()

	inner := &TestRoundTripper{}
	assert.Same(t, inner, WithServerSideTimeout(0)(inner))
}
