package oidc

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestAuthorize(t *testing.T) {
	parse := func(js string) claims {
		var c claims
		if err := json.Unmarshal([]byte(js), &c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	tests := []struct {
		name    string
		require bool
		claims  string
		denied  bool
	}{
		{"verified", true, `{"email":"a@b.c","email_verified":true}`, false},
		{"unverified", true, `{"email":"a@b.c","email_verified":false}`, true},
		{"claim missing", true, `{"email":"a@b.c"}`, false},
		{"claim null", true, `{"email_verified":null}`, false},
		{"string false", true, `{"email_verified":"false"}`, true},
		{"string true", true, `{"email_verified":"true"}`, false},
		{"option off", false, `{"email_verified":false}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{cfg: Config{RequireVerifiedEmail: tc.require}}
			err := h.authorize(parse(tc.claims))
			if tc.denied != errors.Is(err, errDenied) {
				t.Errorf("denied=%v, err=%v", tc.denied, err)
			}
		})
	}
}

func TestClaimsRejectsGarbageBool(t *testing.T) {
	var c claims
	if err := json.Unmarshal([]byte(`{"email_verified":"maybe"}`), &c); err == nil {
		t.Error("expected error")
	}
}
