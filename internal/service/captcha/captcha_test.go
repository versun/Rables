package captcha

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestIssueAndVerify(t *testing.T) {
	c := New("secret", TTL)
	for range 50 {
		ch, token := c.IssueChallenge()

		if ch.Question != fmt.Sprintf("%d %s %d =", ch.A, ch.Op, ch.B) {
			t.Errorf("question %q does not match challenge %+v", ch.Question, ch)
		}
		if ch.A < 0 || ch.A > Max || ch.B < 0 || ch.B > Max {
			t.Errorf("out-of-bounds challenge %+v", ch)
		}
		expected, ok := c.Expected(token)
		if !ok {
			t.Fatalf("fresh token rejected: %+v", ch)
		}
		var want int
		if ch.Op == "+" {
			want = ch.A + ch.B
		} else {
			want = ch.A - ch.B
		}
		if expected != want {
			t.Errorf("expected = %d, want %d (%+v)", expected, want, ch)
		}
		if want < 0 || want > Max {
			t.Errorf("answer %d out of bounds (%+v)", want, ch)
		}
		if !c.Verify(token, fmt.Sprintf("%d", want)) {
			t.Error("correct answer rejected")
		}
		if !c.Verify(token, fmt.Sprintf(" %d ", want)) {
			t.Error("answer with surrounding whitespace rejected")
		}
	}
}

func TestVerifyBadAnswers(t *testing.T) {
	c := New("secret", TTL)
	_, token := c.Issue()
	expected, _ := c.Expected(token)

	tests := []struct {
		name   string
		token  string
		answer string
	}{
		{name: "wrong answer", token: token, answer: fmt.Sprintf("%d", expected+1)},
		{name: "empty answer", token: token, answer: ""},
		{name: "non-numeric answer", token: token, answer: "abc"},
		{name: "float answer", token: token, answer: "3.0"},
		{name: "empty token", token: "", answer: fmt.Sprintf("%d", expected)},
		{name: "garbage token", token: "not-a-token", answer: fmt.Sprintf("%d", expected)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if c.Verify(tt.token, tt.answer) {
				t.Error("Verify accepted an invalid input")
			}
		})
	}
}

func TestTamperedToken(t *testing.T) {
	c := New("secret", TTL)
	_, token := c.Issue()

	body, sig, _ := strings.Cut(token, ".")

	tampered := []struct {
		name  string
		token string
	}{
		{name: "altered signature", token: body + "." + sig[:len(sig)-2] + "xx"},
		{name: "altered body", token: body[:len(body)-2] + "xx." + sig},
		{name: "foreign signature", token: New("other-secret", TTL).sign(payload{A: 1, B: 2, Op: "+", Exp: time.Now().Add(time.Hour).Unix()})},
		{name: "out-of-bounds challenge", token: c.sign(payload{A: 99, B: 1, Op: "+", Exp: time.Now().Add(time.Hour).Unix()})},
		{name: "out-of-range answer", token: c.sign(payload{A: 1, B: 5, Op: "-", Exp: time.Now().Add(time.Hour).Unix()})},
		{name: "unknown operator", token: c.sign(payload{A: 1, B: 2, Op: "*", Exp: time.Now().Add(time.Hour).Unix()})},
	}
	for _, tt := range tampered {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := c.Expected(tt.token); ok {
				t.Error("Expected accepted token")
			}
			if c.Verify(tt.token, "3") {
				t.Error("Verify accepted token")
			}
		})
	}
}

func TestExpiredToken(t *testing.T) {
	c := New("secret", -time.Hour) // already expired on issue
	_, token := c.Issue()
	if _, ok := c.Expected(token); ok {
		t.Error("expired token accepted by Expected")
	}
	expected := 0 // any answer must fail
	if c.Verify(token, fmt.Sprintf("%d", expected)) {
		t.Error("expired token accepted by Verify")
	}
}
