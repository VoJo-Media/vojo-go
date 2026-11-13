package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	secret = "vwsec_current"
	now    = int64(1_800_000_000)
	body   = `{"event_type":"wrapper.ready","event_id":"evt_1","attempt_id":"att_1","data":{"id":"w1"}}`
)

var at = WithNow(func() time.Time { return time.Unix(now, 0) })

func sig(s, b, t string) string {
	m := hmac.New(sha256.New, []byte(s))
	fmt.Fprintf(m, "%s.%s", t, b)
	return hex.EncodeToString(m.Sum(nil))
}

type vector struct {
	Name      string   `json:"name"`
	Body      string   `json:"body"`
	Header    *string  `json:"header"`
	Secrets   []string `json:"secrets"`
	Now       int64    `json:"now"`
	Tolerance int64    `json:"tolerance_seconds"`
	Expect    struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Event struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			AttemptID string `json:"attempt_id"`
		} `json:"event"`
	} `json:"expect"`
}

func TestSharedVectors(t *testing.T) {
	var data []byte
	for _, dir := range []string{"../shared", "../../shared"} {
		if b, err := os.ReadFile(filepath.Join(dir, "webhook-vectors.json")); err == nil {
			data = b
		}
	}
	if data == nil {
		t.Fatal("webhook-vectors.json not found in ../shared/ or ../../shared/: the vectors must be alongside the package")
	}
	var file struct {
		Vectors []vector `json:"vectors"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Vectors) == 0 {
		t.Fatal("no vectors")
	}
	for _, v := range file.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			header := ""
			if v.Header != nil {
				header = *v.Header
			}
			opts := []Option{
				WithTolerance(time.Duration(v.Tolerance) * time.Second),
				WithNow(func() time.Time { return time.Unix(v.Now, 0) }),
			}
			for _, extra := range v.Secrets[1:] {
				opts = append(opts, WithAdditionalSecret(extra))
			}
			event, err := ConstructEvent([]byte(v.Body), header, v.Secrets[0], opts...)
			if v.Expect.OK {
				if err != nil {
					t.Fatal(err)
				}
				if event.Type != v.Expect.Event.Type || event.ID != v.Expect.Event.ID || event.AttemptID != v.Expect.Event.AttemptID {
					t.Fatalf("got %+v", event)
				}
				return
			}
			var failure *VerificationError
			if !errors.As(err, &failure) || failure.Kind != v.Expect.Error {
				t.Fatalf("want %s, got %v", v.Expect.Error, err)
			}
		})
	}
}

func TestEventDecodesIntoGeneratedTypes(t *testing.T) {
	e, err := ConstructEvent([]byte(body), GenerateTestHeader([]byte(body), secret, time.Unix(now, 0)), secret, at)
	if err != nil {
		t.Fatal(err)
	}
	var ready WrapperEvent
	if err := e.Decode(&ready); err != nil || e.Type != TypeWrapperReady {
		t.Fatalf("%v %s", err, e.Type)
	}
	if string(e.Payload) != body {
		t.Fatal("payload changed")
	}
}

func TestUsesTheTimestampAsSent(t *testing.T) {
	sent := fmt.Sprintf("0%d", now)
	if err := VerifySignature([]byte(body), "t="+sent+",v1="+sig(secret, body, sent), secret, at); err != nil {
		t.Fatal(err)
	}
}

func TestSignatureIsCheckedBeforeTheTimestamp(t *testing.T) {
	err := VerifySignature([]byte(body), fmt.Sprintf("t=%d,v1=bad", now-9999), secret, at)
	var failure *VerificationError
	if !errors.As(err, &failure) || failure.Kind != KindNoMatchingSignature {
		t.Fatalf("got %v", err)
	}
}

func TestOddHeadersRaiseOnlyTheVerificationError(t *testing.T) {
	for _, h := range []string{
		fmt.Sprintf("t=-%d,v1=x", now),
		"t=١٢٣,v1=x",
		"t=" + strings.Repeat("9", 5000) + ",v1=x",
		"t=99999999999999999999,v1=x",
		fmt.Sprintf("t=%d,v1=é", now),
	} {
		if err := VerifySignature([]byte(body), h, secret, at); !errors.Is(err, ErrInvalidSignature) {
			t.Errorf("%q: got %v", h, err)
		}
	}
}

func TestToleranceOfZeroOrLessIsAProgrammingError(t *testing.T) {
	h := GenerateTestHeader([]byte(body), secret, time.Unix(now, 0))
	for _, d := range []time.Duration{0, -time.Second} {
		if err := VerifySignature([]byte(body), h, secret, at, WithTolerance(d)); err == nil || errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("got %v", err)
		}
	}
}

func TestMissingHeaderPointsAtRollingTheSecret(t *testing.T) {
	if err := VerifySignature([]byte(body), "", secret, at); err == nil || !strings.Contains(err.Error(), "rolled") {
		t.Fatalf("got %v", err)
	}
}

func TestEmptySecretIsASetupErrorNotAPass(t *testing.T) {
	h := fmt.Sprintf("t=%d,v1=%s", now, sig("", body, fmt.Sprint(now)))
	if err := VerifySignature([]byte(body), h, "", at); err == nil || errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("got %v", err)
	}
}

func TestToleranceThatWouldTurnTheCheckOffIsRefused(t *testing.T) {
	body := []byte(`{"eventType":"x"}`)
	at := time.Unix(1_800_000_000, 0)
	header := GenerateTestHeader(body, "s", at)
	now := WithNow(func() time.Time { return at })
	for _, d := range []time.Duration{MaxTolerance + time.Second, time.Duration(1<<63 - 1)} {
		err := VerifySignature(body, header, "s", now, WithTolerance(d))
		if err == nil || errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("%v: got %v", d, err)
		}
	}
	if err := VerifySignature(body, header, "s", now, WithTolerance(MaxTolerance)); err != nil {
		t.Fatalf("the largest tolerance should pass: %v", err)
	}
}
