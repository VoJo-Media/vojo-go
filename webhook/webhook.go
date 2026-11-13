// Package webhook verifies deliveries from Vojo. It needs no API key or client.
//
// Pass the raw request body, exactly as received. Decoding and re-encoding the
// JSON changes the bytes and the signature will not match.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// SignatureHeader is the HTTP header carrying a delivery's signature.
const SignatureHeader = "X-Vojo-Signature"

// DefaultTolerance is how far a signature's timestamp may be from the current
// time, in either direction.
const DefaultTolerance = 300 * time.Second

// MaxTolerance is the largest tolerance accepted. The timestamp check cannot be
// turned off.
const MaxTolerance = 24 * time.Hour

// Why a delivery failed verification: the values of VerificationError.Kind.
const (
	KindMissingHeader           = "missing_header"
	KindMalformedHeader         = "malformed_header"
	KindNoMatchingSignature     = "no_matching_signature"
	KindTimestampOutOfTolerance = "timestamp_out_of_tolerance"
	KindInvalidPayload          = "invalid_payload"
)

// ErrInvalidSignature is matched by every VerificationError, via errors.Is.
var ErrInvalidSignature = errors.New("vojo webhook: verification failed")

// VerificationError means a delivery is not authentic. Reject it with a 400.
type VerificationError struct {
	Kind    string
	Message string
}

func (e *VerificationError) Error() string { return "vojo webhook: " + e.Message }

// Is matches ErrInvalidSignature.
func (e *VerificationError) Is(target error) bool { return target == ErrInvalidSignature }

var timestampPattern = regexp.MustCompile(`^[0-9]{1,18}$`)

func invalid(kind, format string, args ...any) error {
	return &VerificationError{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

// Event is a verified delivery.
type Event struct {
	// Type is the event type, such as "wrapper.ready".
	Type string
	// ID is stable across retries of the same event. Dedupe on it.
	ID string
	// AttemptID changes on every delivery attempt.
	AttemptID string
	// Payload is the whole body, as received.
	Payload json.RawMessage
}

// Decode unmarshals the whole body into v, such as the matching event type here:
//
//	var ready webhook.WrapperEvent
//	err := event.Decode(&ready)
func (e *Event) Decode(v any) error {
	return json.Unmarshal(e.Payload, v)
}

type config struct {
	secrets   []string
	tolerance time.Duration
	now       func() time.Time
}

// Option changes how a delivery is verified.
type Option func(*config)

// WithTolerance changes DefaultTolerance. It must be positive and at most MaxTolerance.
func WithTolerance(d time.Duration) Option {
	return func(c *config) { c.tolerance = d }
}

// WithAdditionalSecret also accepts signatures from another secret, for use
// while rolling: a delivery matching any secret you hold is accepted.
func WithAdditionalSecret(secret string) Option {
	return func(c *config) { c.secrets = append(c.secrets, secret) }
}

// WithNow overrides the current time, for tests.
func WithNow(now func() time.Time) Option {
	return func(c *config) { c.now = now }
}

// VerifySignature returns a *VerificationError unless the delivery is authentic.
// During a secret roll a delivery carries a signature from each secret, and any
// match is accepted.
func VerifySignature(body []byte, header, secret string, opts ...Option) error {
	c := config{secrets: []string{secret}, tolerance: DefaultTolerance, now: time.Now}
	for _, opt := range opts {
		opt(&c)
	}
	for _, s := range c.secrets {
		if s == "" {
			return errors.New("vojo webhook: the signing secret is empty")
		}
	}
	if c.tolerance <= 0 || c.tolerance > MaxTolerance {
		return fmt.Errorf("vojo webhook: tolerance must be positive and at most %s", MaxTolerance)
	}
	if header == "" {
		return invalid(KindMissingHeader, "the %s header is missing; an endpoint whose secret predates "+
			"signing sends no header until its secret is rolled", SignatureHeader)
	}

	timestamp, signatures, err := parseHeader(header)
	if err != nil {
		return err
	}

	// Every comparison runs even after a match, so timing reveals nothing about
	// which signature matched or how many were sent.
	matched := false
	for _, s := range c.secrets {
		expected := sign(s, timestamp, body)
		for _, candidate := range signatures {
			if hmac.Equal(candidate, expected) {
				matched = true
			}
		}
	}
	if !matched {
		return invalid(KindNoMatchingSignature, "no signature matches; check the signing secret, and that "+
			"you passed the raw request body rather than decoded or re-encoded JSON")
	}

	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return invalid(KindMalformedHeader, "the %s header has a bad timestamp", SignatureHeader)
	}
	age := c.now().Unix() - seconds
	if age < 0 {
		age = -age
	}
	if age > int64(c.tolerance/time.Second) {
		return invalid(KindTimestampOutOfTolerance, "the signature timestamp is outside the %s tolerance; "+
			"check this machine's clock, or this may be a replayed delivery", c.tolerance)
	}
	return nil
}

// ConstructEvent verifies a delivery and returns it as an Event.
func ConstructEvent(body []byte, header, secret string, opts ...Option) (*Event, error) {
	if err := VerifySignature(body, header, secret, opts...); err != nil {
		return nil, err
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return nil, invalid(KindInvalidPayload, "the signed body is not a JSON object")
	}
	return &Event{
		Type:      firstString(fields, "eventType", "event_type"),
		ID:        firstString(fields, "eventId", "event_id"),
		AttemptID: firstString(fields, "attemptId", "attempt_id"),
		Payload:   append(json.RawMessage(nil), body...),
	}, nil
}

// GenerateTestHeader signs a payload as Vojo would, to test your own webhook handler.
func GenerateTestHeader(body []byte, secret string, timestamp time.Time) string {
	t := strconv.FormatInt(timestamp.Unix(), 10)
	return "t=" + t + ",v1=" + string(sign(secret, t, body))
}

// sign covers the timestamp exactly as it was sent.
func sign(secret, timestamp string, body []byte) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	return []byte(hex.EncodeToString(mac.Sum(nil)))
}

func parseHeader(header string) (string, [][]byte, error) {
	var (
		timestamp    string
		hasTimestamp bool
		signatures   [][]byte
	)
	for _, part := range strings.Split(header, ",") {
		name, value, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch name {
		case "t":
			if hasTimestamp || !timestampPattern.MatchString(value) {
				return "", nil, invalid(KindMalformedHeader, "the %s header has a bad timestamp", SignatureHeader)
			}
			timestamp, hasTimestamp = value, true
		case "v1":
			signatures = append(signatures, []byte(value))
		}
	}
	if !hasTimestamp {
		return "", nil, invalid(KindMalformedHeader, "the %s header has no timestamp", SignatureHeader)
	}
	if len(signatures) == 0 {
		return "", nil, invalid(KindMalformedHeader, "the %s header has no v1 signature", SignatureHeader)
	}
	return timestamp, signatures, nil
}

func firstString(fields map[string]any, names ...string) string {
	for _, name := range names {
		if value, ok := fields[name].(string); ok && value != "" {
			return value
		}
	}
	return ""
}
