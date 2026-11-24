// Package kubetransport provides client-go transport wrappers used when building a REST config
// for a managed cluster: a bounded retry wrapper and a server-side timeout wrapper.
//
// These were previously in util/http. They live in their own package because the Argo CD API
// types are their only consumer, and util/http pulls in the common package (and through it
// go-redis and grpc), which the API types must not depend on.
package kubetransport

import (
	"bytes"
	"io"
	"net/http"
	"time"

	log "github.com/sirupsen/logrus"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/transport"
)

const (
	// respReadLimit bounds how much of a discarded response body is drained before retrying.
	respReadLimit = int64(524288)
	retryWaitMax  = time.Duration(10) * time.Second

	// EnvRetryMax is the number of retries the Kubernetes client performs on retryable errors.
	EnvRetryMax = "ARGOCD_K8SCLIENT_RETRY_MAX"
	// EnvRetryBaseBackoff is the base backoff between those retries.
	EnvRetryBaseBackoff = "ARGOCD_K8SCLIENT_RETRY_BASE_BACKOFF"
)

func WithRetry(maxRetries int64, baseRetryBackoff time.Duration) transport.WrapperFunc {
	return func(rt http.RoundTripper) http.RoundTripper {
		return &retryTransport{
			inner:      rt,
			maxRetries: maxRetries,
			backoff:    baseRetryBackoff,
		}
	}
}

// WithServerSideTimeout adds the timeout query parameter understood by the
// Kubernetes API server without imposing a client-side deadline.
func WithServerSideTimeout(timeout time.Duration) transport.WrapperFunc {
	return func(rt http.RoundTripper) http.RoundTripper {
		if timeout <= 0 {
			return rt
		}
		if rt == nil {
			rt = http.DefaultTransport
		}
		return &serverSideTimeoutTransport{
			inner:   rt,
			timeout: timeout,
		}
	}
}

type serverSideTimeoutTransport struct {
	inner   http.RoundTripper
	timeout time.Duration
}

var _ utilnet.RoundTripperWrapper = (*serverSideTimeoutTransport)(nil)

func (t *serverSideTimeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	query := req.URL.Query()
	if query.Get("timeout") != "" {
		return t.inner.RoundTrip(req)
	}

	clonedReq := req.Clone(req.Context())
	query.Set("timeout", t.timeout.String())
	clonedReq.URL.RawQuery = query.Encode()
	return t.inner.RoundTrip(clonedReq)
}

func (t *serverSideTimeoutTransport) WrappedRoundTripper() http.RoundTripper {
	return t.inner
}

type retryTransport struct {
	inner      http.RoundTripper
	maxRetries int64
	backoff    time.Duration
}

func isRetriable(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if resp.StatusCode == 0 || (resp.StatusCode >= 500 && resp.StatusCode != http.StatusNotImplemented) {
		return true
	}
	return false
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var resp *http.Response
	var err error
	backoff := t.backoff
	var bodyBytes []byte
	if req.Body != nil {
		bodyBytes, _ = io.ReadAll(req.Body)
	}
	for i := 0; i <= int(t.maxRetries); i++ {
		req.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
		resp, err = t.inner.RoundTrip(req)
		if i < int(t.maxRetries) && (err != nil || isRetriable(resp)) {
			if resp != nil && resp.Body != nil {
				drainBody(resp.Body)
			}
			if backoff > retryWaitMax {
				backoff = retryWaitMax
			}
			select {
			case <-time.After(backoff):
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
			backoff *= 2
			continue
		}
		break
	}
	return resp, err
}

func drainBody(body io.ReadCloser) {
	defer body.Close()
	_, err := io.Copy(io.Discard, io.LimitReader(body, respReadLimit))
	if err != nil {
		log.Warnf("error reading response body: %s", err.Error())
	}
}
