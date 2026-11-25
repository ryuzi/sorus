package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func newTestKeyPair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	return pub, priv
}

// signRaw は任意の header/payload JSON に正しい署名を付けたトークンを作る。
// 署名は正当だが中身が不正なトークンの検証に使う。
func signRaw(t *testing.T, priv ed25519.PrivateKey, headerJSON, payloadJSON string) string {
	t.Helper()
	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString([]byte(headerJSON)) + "." + enc.EncodeToString([]byte(payloadJSON))
	sig := ed25519.Sign(priv, []byte(signingInput))
	return signingInput + "." + enc.EncodeToString(sig)
}

func mustDecodeSegment(t *testing.T, seg string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatalf("decode segment %q: %v", seg, err)
	}
	return b
}

func TestCapabilityGenerateKeyPair(t *testing.T) {
	pub1, priv1 := newTestKeyPair(t)
	pub2, _ := newTestKeyPair(t)

	if len(pub1) != ed25519.PublicKeySize {
		t.Errorf("public key size = %d, want %d", len(pub1), ed25519.PublicKeySize)
	}
	if len(priv1) != ed25519.PrivateKeySize {
		t.Errorf("private key size = %d, want %d", len(priv1), ed25519.PrivateKeySize)
	}
	if !priv1.Public().(ed25519.PublicKey).Equal(pub1) {
		t.Error("private key does not correspond to public key")
	}
	if pub1.Equal(pub2) {
		t.Error("two generated key pairs must differ")
	}
}

func TestCapabilityKeyEncoding(t *testing.T) {
	pub, priv := newTestKeyPair(t)

	t.Run("public key round trip", func(t *testing.T) {
		got, err := DecodePublicKey(EncodePublicKey(pub))
		if err != nil {
			t.Fatalf("DecodePublicKey: %v", err)
		}
		if !got.Equal(pub) {
			t.Error("decoded public key differs from original")
		}
	})

	t.Run("private key round trip", func(t *testing.T) {
		got, err := DecodePrivateKey(EncodePrivateKey(priv))
		if err != nil {
			t.Fatalf("DecodePrivateKey: %v", err)
		}
		if !got.Equal(priv) {
			t.Error("decoded private key differs from original")
		}
	})

	t.Run("surrounding whitespace is ignored", func(t *testing.T) {
		if _, err := DecodePublicKey("  " + EncodePublicKey(pub) + "\n"); err != nil {
			t.Errorf("DecodePublicKey with whitespace: %v", err)
		}
		if _, err := DecodePrivateKey("\t" + EncodePrivateKey(priv) + "\r\n"); err != nil {
			t.Errorf("DecodePrivateKey with whitespace: %v", err)
		}
	})

	t.Run("decoded keys can sign and verify tokens", func(t *testing.T) {
		decPub, err := DecodePublicKey(EncodePublicKey(pub))
		if err != nil {
			t.Fatalf("DecodePublicKey: %v", err)
		}
		decPriv, err := DecodePrivateKey(EncodePrivateKey(priv))
		if err != nil {
			t.Fatalf("DecodePrivateKey: %v", err)
		}
		token, err := IssueToken(decPriv, "alice", "writer", time.Hour)
		if err != nil {
			t.Fatalf("IssueToken: %v", err)
		}
		if _, err := VerifyToken(decPub, token); err != nil {
			t.Errorf("VerifyToken: %v", err)
		}
	})

	// 後半32バイト（公開鍵部分）を書き換えた秘密鍵。
	corrupted := make([]byte, len(priv))
	copy(corrupted, priv)
	corrupted[len(corrupted)-1] ^= 0xff

	invalidPublic := map[string]string{
		"empty":          "",
		"not base64":     "!!!not-base64!!!",
		"too short":      base64.StdEncoding.EncodeToString(pub[:31]),
		"too long":       base64.StdEncoding.EncodeToString(append(append([]byte{}, pub...), 0)),
		"private as pub": EncodePrivateKey(priv),
		"unpadded url64": base64.RawURLEncoding.EncodeToString(pub),
	}
	for name, in := range invalidPublic {
		t.Run("invalid public key: "+name, func(t *testing.T) {
			if _, err := DecodePublicKey(in); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("DecodePublicKey(%q) error = %v, want ErrInvalidKey", in, err)
			}
		})
	}

	invalidPrivate := map[string]string{
		"empty":          "",
		"not base64":     "%%%",
		"seed only":      base64.StdEncoding.EncodeToString(priv.Seed()),
		"public as priv": EncodePublicKey(pub),
		"corrupted":      base64.StdEncoding.EncodeToString(corrupted),
	}
	for name, in := range invalidPrivate {
		t.Run("invalid private key: "+name, func(t *testing.T) {
			if _, err := DecodePrivateKey(in); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("DecodePrivateKey(%q) error = %v, want ErrInvalidKey", in, err)
			}
		})
	}
}

func TestCapabilityIssueAndVerify(t *testing.T) {
	pub, priv := newTestKeyPair(t)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

	for _, role := range []Role{RoleWriter, RoleEditor, RoleAdmin} {
		t.Run(string(role), func(t *testing.T) {
			token, _, err := issueTokenAt(priv, "user-"+string(role), string(role), 24*time.Hour, now)
			if err != nil {
				t.Fatalf("issueTokenAt: %v", err)
			}

			claims, err := verifyTokenAt(pub, token, now.Add(time.Hour))
			if err != nil {
				t.Fatalf("verifyTokenAt: %v", err)
			}
			if !ValidTokenID(claims.ID) {
				t.Errorf("jti = %q, want 32 lowercase hex digits", claims.ID)
			}
			want := Claims{
				Subject:   "user-" + string(role),
				Role:      role,
				ExpiresAt: now.Add(24 * time.Hour).Unix(),
				IssuedAt:  now.Unix(),
				ID:        claims.ID,
			}
			if *claims != want {
				t.Errorf("claims = %+v, want %+v", *claims, want)
			}
		})
	}

	t.Run("token format is header.payload.signature in unpadded base64url", func(t *testing.T) {
		token, _, err := issueTokenAt(priv, "alice", "writer", time.Hour, now)
		if err != nil {
			t.Fatalf("issueTokenAt: %v", err)
		}
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			t.Fatalf("token has %d segments, want 3: %q", len(parts), token)
		}
		for i, p := range parts {
			if p == "" {
				t.Errorf("segment %d is empty", i)
			}
			if strings.ContainsAny(p, "=+/") {
				t.Errorf("segment %d contains non base64url or padding characters: %q", i, p)
			}
		}

		var header map[string]any
		if err := json.Unmarshal(mustDecodeSegment(t, parts[0]), &header); err != nil {
			t.Fatalf("header is not JSON: %v", err)
		}
		if header["alg"] != "EdDSA" || header["typ"] != "sorus-cap" {
			t.Errorf("header = %v, want alg EdDSA and typ sorus-cap", header)
		}

		var payload map[string]any
		if err := json.Unmarshal(mustDecodeSegment(t, parts[1]), &payload); err != nil {
			t.Fatalf("payload is not JSON: %v", err)
		}
		for _, key := range []string{"sub", "role", "exp", "iat", "jti"} {
			if _, ok := payload[key]; !ok {
				t.Errorf("payload is missing %q: %v", key, payload)
			}
		}

		if sig := mustDecodeSegment(t, parts[2]); len(sig) != ed25519.SignatureSize {
			t.Errorf("signature size = %d, want %d", len(sig), ed25519.SignatureSize)
		}
	})

	t.Run("public API with real clock", func(t *testing.T) {
		before := time.Now().Unix()
		token, err := IssueToken(priv, "bob", "editor", time.Hour)
		if err != nil {
			t.Fatalf("IssueToken: %v", err)
		}
		after := time.Now().Unix()

		claims, err := VerifyToken(pub, token)
		if err != nil {
			t.Fatalf("VerifyToken: %v", err)
		}
		if claims.Subject != "bob" || claims.Role != RoleEditor {
			t.Errorf("claims = %+v, want sub bob and role editor", *claims)
		}
		if claims.IssuedAt < before || claims.IssuedAt > after {
			t.Errorf("iat = %d, want within [%d, %d]", claims.IssuedAt, before, after)
		}
		if got := claims.ExpiresAt - claims.IssuedAt; got != int64(time.Hour/time.Second) {
			t.Errorf("exp - iat = %d, want %d", got, int64(time.Hour/time.Second))
		}
	})
}

// 発行するトークンには毎回異なる jti を入れ、IssueTokenClaims はトークンに書いたものと同じ Claims を返す。
func TestCapabilityTokenID(t *testing.T) {
	pub, priv := newTestKeyPair(t)

	token, issued, err := IssueTokenClaims(priv, "dave", "admin", time.Hour)
	if err != nil {
		t.Fatalf("IssueTokenClaims: %v", err)
	}
	claims, err := VerifyToken(pub, token)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if *claims != *issued {
		t.Errorf("verified claims = %+v, want issued %+v", *claims, *issued)
	}
	if !ValidTokenID(issued.ID) {
		t.Errorf("jti = %q", issued.ID)
	}
	seen := map[string]bool{issued.ID: true}
	for range 20 {
		_, c, err := IssueTokenClaims(priv, "dave", "admin", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if seen[c.ID] {
			t.Fatalf("duplicate jti %q", c.ID)
		}
		seen[c.ID] = true
	}

	t.Run("token without jti stays valid", func(t *testing.T) {
		old := signRaw(t, priv, `{"alg":"EdDSA","typ":"sorus-cap"}`,
			fmt.Sprintf(`{"sub":"old","role":"writer","exp":%d,"iat":1700000000}`, time.Now().Add(time.Hour).Unix()))
		claims, err := VerifyToken(pub, old)
		if err != nil {
			t.Fatalf("VerifyToken(old format): %v", err)
		}
		if claims.ID != "" || claims.Subject != "old" {
			t.Errorf("claims = %+v", *claims)
		}
	})

	t.Run("malformed jti is rejected", func(t *testing.T) {
		exp := time.Now().Add(time.Hour).Unix()
		for _, jti := range []string{"abc", strings.Repeat("A", 32), strings.Repeat("g", 32), strings.Repeat("0", 33), `../../etc`} {
			tok := signRaw(t, priv, `{"alg":"EdDSA","typ":"sorus-cap"}`,
				fmt.Sprintf(`{"sub":"x","role":"writer","exp":%d,"iat":1700000000,"jti":%q}`, exp, jti))
			if _, err := VerifyToken(pub, tok); !errors.Is(err, ErrMalformedToken) {
				t.Errorf("jti %q: err = %v, want ErrMalformedToken", jti, err)
			}
		}
	})

	for _, tc := range []struct {
		in   string
		want bool
	}{
		{strings.Repeat("0123456789abcdef", 2), true},
		{"", false},
		{strings.Repeat("0123456789ABCDEF", 2), false},
		{strings.Repeat("a", 31), false},
	} {
		if got := ValidTokenID(tc.in); got != tc.want {
			t.Errorf("ValidTokenID(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestCapabilityIssueTokenInvalidInput(t *testing.T) {
	_, priv := newTestKeyPair(t)

	tests := []struct {
		name string
		key  ed25519.PrivateKey
		sub  string
		role string
		ttl  time.Duration
		want error
	}{
		{"nil key", nil, "alice", "writer", time.Hour, ErrInvalidKey},
		{"short key", priv[:32], "alice", "writer", time.Hour, ErrInvalidKey},
		{"empty subject", priv, "", "writer", time.Hour, ErrInvalidSubject},
		{"blank subject", priv, "   ", "writer", time.Hour, ErrInvalidSubject},
		{"empty role", priv, "alice", "", time.Hour, ErrInvalidRole},
		{"unknown role", priv, "alice", "reader", time.Hour, ErrInvalidRole},
		{"role is case sensitive", priv, "alice", "Admin", time.Hour, ErrInvalidRole},
		{"zero ttl", priv, "alice", "writer", 0, ErrInvalidTTL},
		{"negative ttl", priv, "alice", "writer", -time.Hour, ErrInvalidTTL},
		{"sub second ttl", priv, "alice", "writer", 999 * time.Millisecond, ErrInvalidTTL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, err := IssueToken(tt.key, tt.sub, tt.role, tt.ttl)
			if !errors.Is(err, tt.want) {
				t.Errorf("IssueToken error = %v, want %v", err, tt.want)
			}
			if token != "" {
				t.Errorf("IssueToken returned token %q on error", token)
			}
		})
	}
}

func TestCapabilityVerifyExpired(t *testing.T) {
	pub, priv := newTestKeyPair(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ttl := 10 * time.Minute

	token, _, err := issueTokenAt(priv, "alice", "writer", ttl, now)
	if err != nil {
		t.Fatalf("issueTokenAt: %v", err)
	}

	if _, err := verifyTokenAt(pub, token, now.Add(ttl-time.Second)); err != nil {
		t.Errorf("token one second before expiry: error = %v, want nil", err)
	}
	for _, at := range []time.Duration{ttl, ttl + time.Second, 365 * 24 * time.Hour} {
		claims, err := verifyTokenAt(pub, token, now.Add(at))
		if !errors.Is(err, ErrTokenExpired) {
			t.Errorf("verify at +%v: error = %v, want ErrTokenExpired", at, err)
		}
		if claims != nil {
			t.Errorf("verify at +%v: claims = %+v, want nil", at, *claims)
		}
	}

	t.Run("public VerifyToken rejects token issued in the past", func(t *testing.T) {
		old, _, err := issueTokenAt(priv, "alice", "writer", time.Hour, time.Now().Add(-2*time.Hour))
		if err != nil {
			t.Fatalf("issueTokenAt: %v", err)
		}
		if _, err := VerifyToken(pub, old); !errors.Is(err, ErrTokenExpired) {
			t.Errorf("VerifyToken error = %v, want ErrTokenExpired", err)
		}
	})

	t.Run("expired token with bad signature reports signature error", func(t *testing.T) {
		otherPub, _ := newTestKeyPair(t)
		if _, err := verifyTokenAt(otherPub, token, now.Add(2*ttl)); !errors.Is(err, ErrInvalidSignature) {
			t.Errorf("error = %v, want ErrInvalidSignature", err)
		}
	})
}

func TestCapabilityVerifyTampered(t *testing.T) {
	pub, priv := newTestKeyPair(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	enc := base64.RawURLEncoding

	token, _, err := issueTokenAt(priv, "alice", "writer", time.Hour, now)
	if err != nil {
		t.Fatalf("issueTokenAt: %v", err)
	}
	parts := strings.Split(token, ".")

	escalatedPayload := strings.Replace(string(mustDecodeSegment(t, parts[1])), `"role":"writer"`, `"role":"admin"`, 1)
	if !strings.Contains(escalatedPayload, `"role":"admin"`) {
		t.Fatalf("failed to build escalated payload from %q", escalatedPayload)
	}
	extendedPayload, err := json.Marshal(Claims{Subject: "alice", Role: RoleWriter, ExpiresAt: now.Add(100 * 365 * 24 * time.Hour).Unix(), IssuedAt: now.Unix()})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	sig := mustDecodeSegment(t, parts[2])
	flippedSig := append([]byte{}, sig...)
	flippedSig[0] ^= 0x01

	_, otherPriv := newTestKeyPair(t)
	foreignToken, _, err := issueTokenAt(otherPriv, "alice", "writer", time.Hour, now)
	if err != nil {
		t.Fatalf("issueTokenAt: %v", err)
	}
	otherToken, _, err := issueTokenAt(priv, "mallory", "admin", time.Hour, now)
	if err != nil {
		t.Fatalf("issueTokenAt: %v", err)
	}

	tests := map[string]string{
		"role escalated in payload":    parts[0] + "." + enc.EncodeToString([]byte(escalatedPayload)) + "." + parts[2],
		"expiry extended in payload":   parts[0] + "." + enc.EncodeToString(extendedPayload) + "." + parts[2],
		"header replaced":              enc.EncodeToString([]byte(`{"alg":"none","typ":"sorus-cap"}`)) + "." + parts[1] + "." + parts[2],
		"signature bit flipped":        parts[0] + "." + parts[1] + "." + enc.EncodeToString(flippedSig),
		"signature zeroed":             parts[0] + "." + parts[1] + "." + enc.EncodeToString(make([]byte, ed25519.SignatureSize)),
		"signed by another key":        foreignToken,
		"signature from another token": parts[0] + "." + parts[1] + "." + strings.Split(otherToken, ".")[2],
		"payload from another token":   parts[0] + "." + strings.Split(otherToken, ".")[1] + "." + parts[2],
	}
	for name, tampered := range tests {
		t.Run(name, func(t *testing.T) {
			claims, err := verifyTokenAt(pub, tampered, now)
			if !errors.Is(err, ErrInvalidSignature) {
				t.Errorf("error = %v, want ErrInvalidSignature", err)
			}
			if claims != nil {
				t.Errorf("claims = %+v, want nil", *claims)
			}
		})
	}

	// 改ざん検知のテストが偽陽性でないことを確認するため、元トークンは通ることを確かめる。
	if _, err := verifyTokenAt(pub, token, now); err != nil {
		t.Errorf("original token: error = %v, want nil", err)
	}
}

func TestCapabilityVerifyMalformed(t *testing.T) {
	pub, priv := newTestKeyPair(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	enc := base64.RawURLEncoding

	token, _, err := issueTokenAt(priv, "alice", "writer", time.Hour, now)
	if err != nil {
		t.Fatalf("issueTokenAt: %v", err)
	}
	parts := strings.Split(token, ".")

	// 署名セグメント末尾の余剰4ビットを立てた非正規表現。緩い復号では同じバイト列になる。
	alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, parts[2][len(parts[2])-1])
	nonCanonicalSig := parts[2][:len(parts[2])-1] + string(alphabet[last|0x01])
	if nonCanonicalSig == parts[2] {
		t.Fatalf("failed to build non canonical signature from %q", parts[2])
	}
	if lenient, err := base64.RawURLEncoding.DecodeString(nonCanonicalSig); err != nil || string(lenient) != string(mustDecodeSegment(t, parts[2])) {
		t.Fatalf("non canonical signature should decode to the same bytes leniently: err=%v", err)
	}

	validHeader := `{"alg":"EdDSA","typ":"sorus-cap"}`
	exp := now.Add(time.Hour).Unix()
	payload := func(sub, role string, exp int64) string {
		b, err := json.Marshal(map[string]any{"sub": sub, "role": role, "exp": exp, "iat": now.Unix()})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(b)
	}

	tests := map[string]string{
		"empty":                        "",
		"one segment":                  "abc",
		"two segments":                 parts[0] + "." + parts[1],
		"four segments":                token + "." + parts[2],
		"empty segments":               "..",
		"invalid header base64":        "*" + parts[0][1:] + "." + parts[1] + "." + parts[2],
		"invalid payload base64":       parts[0] + "." + parts[1] + "*" + "." + parts[2],
		"invalid signature base64":     parts[0] + "." + parts[1] + "." + "*" + parts[2][1:],
		"padded signature":             token + "==",
		"standard base64 signature":    parts[0] + "." + parts[1] + "." + base64.StdEncoding.EncodeToString(mustDecodeSegment(t, parts[2])),
		"non canonical signature":      parts[0] + "." + parts[1] + "." + nonCanonicalSig,
		"short signature":              parts[0] + "." + parts[1] + "." + enc.EncodeToString(mustDecodeSegment(t, parts[2])[:32]),
		"long signature":               parts[0] + "." + parts[1] + "." + enc.EncodeToString(append(mustDecodeSegment(t, parts[2]), 0)),
		"oversized token":              parts[0] + "." + strings.Repeat("A", maxTokenLength) + "." + parts[2],
		"signed header not json":       signRaw(t, priv, "not-json", payload("alice", "writer", exp)),
		"signed header wrong alg":      signRaw(t, priv, `{"alg":"HS256","typ":"sorus-cap"}`, payload("alice", "writer", exp)),
		"signed header wrong typ":      signRaw(t, priv, `{"alg":"EdDSA","typ":"JWT"}`, payload("alice", "writer", exp)),
		"signed header missing fields": signRaw(t, priv, `{}`, payload("alice", "writer", exp)),
		"signed payload not json":      signRaw(t, priv, validHeader, "not-json"),
		"signed payload wrong types":   signRaw(t, priv, validHeader, `{"sub":1,"role":"writer","exp":"soon","iat":0}`),
		"signed payload missing sub":   signRaw(t, priv, validHeader, payload("", "writer", exp)),
		"signed payload blank sub":     signRaw(t, priv, validHeader, payload("  ", "writer", exp)),
		"signed payload unknown role":  signRaw(t, priv, validHeader, payload("alice", "reader", exp)),
		"signed payload missing role":  signRaw(t, priv, validHeader, payload("alice", "", exp)),
		"signed payload missing exp":   signRaw(t, priv, validHeader, `{"sub":"alice","role":"writer","iat":0}`),
		"signed payload negative exp":  signRaw(t, priv, validHeader, payload("alice", "writer", -1)),
	}
	for name, malformed := range tests {
		t.Run(name, func(t *testing.T) {
			claims, err := verifyTokenAt(pub, malformed, now)
			if !errors.Is(err, ErrMalformedToken) {
				t.Errorf("error = %v, want ErrMalformedToken", err)
			}
			if claims != nil {
				t.Errorf("claims = %+v, want nil", *claims)
			}
		})
	}

	// signRaw 自体が正しいトークンを作れることを確認し、上の「signed ...」系が
	// 署名ではなく中身の検査で弾かれていることを保証する。
	t.Run("signRaw produces verifiable token", func(t *testing.T) {
		if _, err := verifyTokenAt(pub, signRaw(t, priv, validHeader, payload("alice", "writer", exp)), now); err != nil {
			t.Errorf("error = %v, want nil", err)
		}
	})
}

func TestCapabilityVerifyInvalidKey(t *testing.T) {
	pub, priv := newTestKeyPair(t)
	token, err := IssueToken(priv, "alice", "writer", time.Hour)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	for name, key := range map[string]ed25519.PublicKey{
		"nil":       nil,
		"short":     pub[:16],
		"too long":  append(append(ed25519.PublicKey{}, pub...), 0),
		"priv size": ed25519.PublicKey(priv),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyToken(key, token); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("error = %v, want ErrInvalidKey", err)
			}
		})
	}
}

func TestCapabilityRolePermissions(t *testing.T) {
	tests := []struct {
		role         Role
		submitDraft  bool
		fetchPending bool
		publish      bool
		syncSettings bool
	}{
		{RoleWriter, true, false, false, false},
		{RoleEditor, true, true, true, false},
		{RoleAdmin, true, true, true, true},
		{"reader", false, false, false, false},
		{"", false, false, false, false},
		{"Admin", false, false, false, false},
		{"admin ", false, false, false, false},
	}
	for _, tt := range tests {
		t.Run("role="+string(tt.role), func(t *testing.T) {
			if got := CanSubmitDraft(tt.role); got != tt.submitDraft {
				t.Errorf("CanSubmitDraft(%q) = %v, want %v", tt.role, got, tt.submitDraft)
			}
			if got := CanFetchPending(tt.role); got != tt.fetchPending {
				t.Errorf("CanFetchPending(%q) = %v, want %v", tt.role, got, tt.fetchPending)
			}
			if got := CanPublish(tt.role); got != tt.publish {
				t.Errorf("CanPublish(%q) = %v, want %v", tt.role, got, tt.publish)
			}
			if got := CanSyncSettings(tt.role); got != tt.syncSettings {
				t.Errorf("CanSyncSettings(%q) = %v, want %v", tt.role, got, tt.syncSettings)
			}
			// 招待の発行・取り消しはサイト設定と同じく admin だけ。
			if got := CanManageInvites(tt.role); got != tt.syncSettings {
				t.Errorf("CanManageInvites(%q) = %v, want %v", tt.role, got, tt.syncSettings)
			}
		})
	}

	t.Run("permissions follow verified claims", func(t *testing.T) {
		pub, priv := newTestKeyPair(t)
		token, err := IssueToken(priv, "carol", "writer", time.Hour)
		if err != nil {
			t.Fatalf("IssueToken: %v", err)
		}
		claims, err := VerifyToken(pub, token)
		if err != nil {
			t.Fatalf("VerifyToken: %v", err)
		}
		if !CanSubmitDraft(claims.Role) || CanPublish(claims.Role) || CanFetchPending(claims.Role) || CanSyncSettings(claims.Role) {
			t.Errorf("writer token permissions are wrong for role %q", claims.Role)
		}
	})
}
