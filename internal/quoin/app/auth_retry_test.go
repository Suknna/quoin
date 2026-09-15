package app

import (
	"errors"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/danielgtaylor/huma/v2"
)

func TestAuthenticationRetryHeaders(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		delay time.Duration
		want  string
	}{
		{"password", auth.ErrRateLimited, 1500 * time.Millisecond, "2"},
		{"challenge", auth.ErrChallengeRateLimited, 60 * time.Second, "60"},
		{"minimum", auth.ErrRateLimited, 0, "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := authenticationRetryError(tc.err, tc.delay)
			var headers huma.HeadersError
			if !errors.As(err, &headers) || headers.GetHeaders().Get("Retry-After") != tc.want {
				t.Fatalf("missing or incorrect retry header: %v", err)
			}
			var status huma.StatusError
			if !errors.As(err, &status) || status.GetStatus() != 429 {
				t.Fatalf("expected rate-limit status: %v", err)
			}
		})
	}
	var headers huma.HeadersError
	if errors.As(authenticationRetryError(auth.ErrOtpInvalid, time.Minute), &headers) {
		t.Fatal("invalid OTP must not receive a retry header")
	}
}
