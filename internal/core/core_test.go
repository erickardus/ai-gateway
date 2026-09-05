package core

import (
	"errors"
	"net/http"
	"testing"
)

func TestIsUpstreamCredential(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"sk-ant-oat01-token", true},
		{"Bearer sk-ant-oat01-token", true},
		{"bearer sk-ant-oat01-token", true},
		{"  Bearer   sk-ant-oat01-token  ", true},
		{"sk-ant-api03-key", true},
		{"Bearer sk-ant-api03-key", true},
		{"sk-vk-gatewaykey", false},
		{"Bearer sk-vk-gatewaykey", false},
		{"", false},
		{"random", false},
		// Near misses must not be classified as upstream credentials.
		{"sk-ant-", false},
		{"prefixed-sk-ant-oat01", false},
	}
	for _, tc := range tests {
		if got := IsUpstreamCredential(tc.value); got != tc.want {
			t.Errorf("IsUpstreamCredential(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestStripScheme(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Bearer token", "token"},
		{"bearer token", "token"},
		{"BEARER token", "token"},
		{"Basic token", "token"},
		{"token", "token"},
		{"  spaced  ", "spaced"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := StripScheme(tc.in); got != tc.want {
			t.Errorf("StripScheme(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStatusFor(t *testing.T) {
	tests := []struct {
		err  error
		want int
	}{
		{nil, http.StatusOK},
		{ErrKeyInvalid, http.StatusUnauthorized},
		{ErrKeyBlocked, http.StatusForbidden},
		{ErrModelNotAllowed, http.StatusForbidden},
		{ErrPassthroughNotAllowed, http.StatusForbidden},
		{ErrUpstreamHostNotAllowed, http.StatusForbidden},
		{ErrModelNotFound, http.StatusNotFound},
		{ErrRateLimited, http.StatusTooManyRequests},
		{ErrNoHealthyDeployment, http.StatusServiceUnavailable},
		{ErrBodyTooLarge, http.StatusRequestEntityTooLarge},
		{errors.New("something else"), http.StatusInternalServerError},
	}
	for _, tc := range tests {
		if got := StatusFor(tc.err); got != tc.want {
			t.Errorf("StatusFor(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
	// Wrapped sentinels must still map correctly.
	if got := StatusFor(errors.Join(errors.New("ctx"), ErrRateLimited)); got != http.StatusTooManyRequests {
		t.Errorf("wrapped ErrRateLimited = %d, want 429", got)
	}
}

func TestUpstreamErrorRetryable(t *testing.T) {
	tests := []struct {
		status int
		want   bool
	}{
		{http.StatusRequestTimeout, true},
		{http.StatusConflict, true},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusNotFound, false},
		{http.StatusOK, false},
	}
	for _, tc := range tests {
		ue := &UpstreamError{StatusCode: tc.status}
		if got := ue.Retryable(); got != tc.want {
			t.Errorf("status %d: Retryable = %v, want %v", tc.status, got, tc.want)
		}
	}
	if msg := (&UpstreamError{StatusCode: 503, Deployment: "d1"}).Error(); msg == "" {
		t.Error("UpstreamError.Error must not be empty")
	}
}

func TestUsageTotal(t *testing.T) {
	if got := (Usage{InputTokens: 10, OutputTokens: 32}).Total(); got != 42 {
		t.Errorf("Total = %d, want 42", got)
	}
}

func TestPricingCacheSavings(t *testing.T) {
	p := Pricing{InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3, CacheWritePer1M: 3.75}

	tests := []struct {
		name  string
		price Pricing
		usage Usage
		want  float64
	}{
		{
			name:  "cache reads are the difference from ordinary input",
			price: p,
			usage: Usage{InputTokens: 100, CacheReadTokens: 1_000_000},
			want:  2.7,
		},
		{
			// A cache write costs a premium and saves nothing; only reads do.
			name:  "cache writes save nothing",
			price: p,
			usage: Usage{CacheWriteTokens: 1_000_000},
			want:  0,
		},
		{
			name:  "an unpriced deployment saves nothing it can report",
			price: Pricing{},
			usage: Usage{CacheReadTokens: 1_000_000},
			want:  0,
		},
		{
			// Pricing a cache read above input is a configuration mistake, not
			// a loss to report.
			name:  "a cache read priced above input is not a negative saving",
			price: Pricing{InputPer1M: 1, CacheReadPer1M: 5},
			usage: Usage{CacheReadTokens: 1_000_000},
			want:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.price.CacheSavings(tt.usage)
			if diff := got - tt.want; diff > 1e-12 || diff < -1e-12 {
				t.Errorf("CacheSavings = %v, want %v", got, tt.want)
			}
		})
	}
}
