package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ケーパビリティ・トークンは公開サーバーの秘密鍵で署名され、ローカルCMSが保持する。
// 形式は JWS 風の header.payload.signature で、各セグメントはパディングなしの Base64URL。
// 署名対象は "header.payload" の ASCII バイト列で、アルゴリズムは Ed25519 に固定する。

var (
	ErrInvalidKey       = errors.New("invalid ed25519 key")
	ErrInvalidSubject   = errors.New("invalid token subject")
	ErrInvalidTTL       = errors.New("invalid token ttl")
	ErrMalformedToken   = errors.New("malformed capability token")
	ErrInvalidSignature = errors.New("invalid capability token signature")
	ErrTokenExpired     = errors.New("capability token expired")
)

const (
	tokenAlg = "EdDSA"
	tokenTyp = "sorus-cap"
	// maxTokenLength は検証前に弾くトークン長の上限。正規のトークンは数百バイトに収まる。
	maxTokenLength = 4096
	// tokenIDBytes はトークン ID（jti）の乱数部分のバイト数。16進数で32桁になる。
	tokenIDBytes = 16
)

// segmentEncoding はトークン各セグメントの符号化方式。Strict により末尾の余剰ビットが
// 0 でない非正規表現を拒否し、同じトークンの別表記を作れないようにする。
var segmentEncoding = base64.RawURLEncoding.Strict()

// tokenHeader はトークンの header セグメント。
type tokenHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// Claims はトークンの payload セグメント。時刻は UNIX 秒。
type Claims struct {
	Subject   string `json:"sub"`
	Role      Role   `json:"role"`
	ExpiresAt int64  `json:"exp"`
	IssuedAt  int64  `json:"iat"`
	// ID はトークンを1つずつ見分ける ID（jti、32桁の16進数）。公開サーバーはこの ID で発行の記録と取り消しを管理する。
	// ID を持たない古い形式のトークンも有効だが、個別には取り消せない。
	ID string `json:"jti,omitempty"`
}

// GenerateKeyPair は署名用の Ed25519 キーペアを crypto/rand で生成する。
func GenerateKeyPair() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// EncodePublicKey は公開鍵を標準 Base64 文字列にする。
func EncodePublicKey(pub ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pub)
}

// EncodePrivateKey は秘密鍵（64バイト）を標準 Base64 文字列にする。
func EncodePrivateKey(priv ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(priv)
}

// DecodePublicKey は EncodePublicKey の出力を公開鍵に戻す。前後の空白は無視する。
func DecodePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, ErrInvalidKey
	}
	return ed25519.PublicKey(b), nil
}

// DecodePrivateKey は EncodePrivateKey の出力を秘密鍵に戻す。前後の空白は無視する。
// 後半32バイトの公開鍵部分がシードから導出したものと一致しない場合は破損として拒否する。
func DecodePrivateKey(s string) (ed25519.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PrivateKeySize {
		return nil, ErrInvalidKey
	}
	priv := ed25519.PrivateKey(b)
	if !ed25519.NewKeyFromSeed(priv.Seed()).Equal(priv) {
		return nil, ErrInvalidKey
	}
	return priv, nil
}

// IssueToken は sub と role を持ち ttl 後に失効するトークンを privKey で署名して発行する。
// role は writer, editor, admin のいずれか、ttl は1秒以上でなければならない。
// トークンには新しいトークン ID（jti）を入れる。
func IssueToken(privKey ed25519.PrivateKey, sub, role string, ttl time.Duration) (string, error) {
	token, _, err := issueTokenAt(privKey, sub, role, ttl, time.Now())
	return token, err
}

// IssueTokenClaims は IssueToken と同じくトークンを発行し、トークンに書いた Claims も返す。
// 公開サーバーが発行を記録する（トークン ID・有効期限を保存する）ときに使う。
func IssueTokenClaims(privKey ed25519.PrivateKey, sub, role string, ttl time.Duration) (string, *Claims, error) {
	return issueTokenAt(privKey, sub, role, ttl, time.Now())
}

func issueTokenAt(privKey ed25519.PrivateKey, sub, role string, ttl time.Duration, now time.Time) (string, *Claims, error) {
	if len(privKey) != ed25519.PrivateKeySize {
		return "", nil, ErrInvalidKey
	}
	if strings.TrimSpace(sub) == "" {
		return "", nil, ErrInvalidSubject
	}
	if !isValidRole(Role(role)) {
		return "", nil, ErrInvalidRole
	}
	// exp は秒単位に切り捨てられるため、1秒未満の ttl は発行直後に失効しうる。
	if ttl < time.Second {
		return "", nil, ErrInvalidTTL
	}
	id, err := newTokenID()
	if err != nil {
		return "", nil, err
	}

	headerJSON, err := json.Marshal(tokenHeader{Alg: tokenAlg, Typ: tokenTyp})
	if err != nil {
		return "", nil, err
	}
	claims := Claims{
		Subject:   sub,
		Role:      Role(role),
		ExpiresAt: now.Add(ttl).Unix(),
		IssuedAt:  now.Unix(),
		ID:        id,
	}
	payloadJSON, err := json.Marshal(claims)
	if err != nil {
		return "", nil, err
	}

	signingInput := segmentEncoding.EncodeToString(headerJSON) + "." + segmentEncoding.EncodeToString(payloadJSON)
	sig := ed25519.Sign(privKey, []byte(signingInput))
	return signingInput + "." + segmentEncoding.EncodeToString(sig), &claims, nil
}

// newTokenID は crypto/rand でトークン ID（32桁の16進数）を作る。
func newTokenID() (string, error) {
	buf := make([]byte, tokenIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: generate token id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// ValidTokenID は s がトークン ID の形式（小文字の32桁の16進数）かを判定する。
func ValidTokenID(s string) bool {
	if len(s) != hex.EncodedLen(tokenIDBytes) {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// VerifyToken は tokenStr の形式・署名・有効期限を検証し、正当なら Claims を返す。
// 形式異常は ErrMalformedToken、署名不一致は ErrInvalidSignature、
// 期限切れは ErrTokenExpired、pubKey の長さ異常は ErrInvalidKey を返す。
func VerifyToken(pubKey ed25519.PublicKey, tokenStr string) (*Claims, error) {
	return verifyTokenAt(pubKey, tokenStr, time.Now())
}

func verifyTokenAt(pubKey ed25519.PublicKey, tokenStr string, now time.Time) (*Claims, error) {
	// ed25519.Verify は長さ不正の公開鍵で panic するため先に弾く。
	if len(pubKey) != ed25519.PublicKeySize {
		return nil, ErrInvalidKey
	}
	if tokenStr == "" || len(tokenStr) > maxTokenLength {
		return nil, ErrMalformedToken
	}
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return nil, ErrMalformedToken
	}
	headerJSON, err := segmentEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, ErrMalformedToken
	}
	payloadJSON, err := segmentEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrMalformedToken
	}
	sig, err := segmentEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, ErrMalformedToken
	}

	// 署名を確認してから中身を解釈する。header の改ざんもここで検出される。
	signingInput := parts[0] + "." + parts[1]
	if !ed25519.Verify(pubKey, []byte(signingInput), sig) {
		return nil, ErrInvalidSignature
	}

	var header tokenHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, ErrMalformedToken
	}
	if header.Alg != tokenAlg || header.Typ != tokenTyp {
		return nil, ErrMalformedToken
	}
	var claims Claims
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return nil, ErrMalformedToken
	}
	if strings.TrimSpace(claims.Subject) == "" || !isValidRole(claims.Role) || claims.ExpiresAt <= 0 {
		return nil, ErrMalformedToken
	}
	// jti は省略可（古い形式のトークン）。あるときは発行の記録と照合できる形式に限る。
	if claims.ID != "" && !ValidTokenID(claims.ID) {
		return nil, ErrMalformedToken
	}
	if now.Unix() >= claims.ExpiresAt {
		return nil, ErrTokenExpired
	}
	return &claims, nil
}

func isValidRole(role Role) bool {
	return role == RoleWriter || role == RoleEditor || role == RoleAdmin
}

// CanSubmitDraft は記事下書きを提出できるかを判定する（writer, editor, admin）。
func CanSubmitDraft(role Role) bool {
	return role == RoleWriter || role == RoleEditor || role == RoleAdmin
}

// CanFetchPending は校閲待ち記事を取得できるかを判定する（editor, admin）。
func CanFetchPending(role Role) bool {
	return role == RoleEditor || role == RoleAdmin
}

// CanPublish は記事を本番公開できるかを判定する（editor, admin）。
func CanPublish(role Role) bool {
	return role == RoleEditor || role == RoleAdmin
}

// CanSyncSettings はサイト設定を同期できるかを判定する（admin のみ）。
func CanSyncSettings(role Role) bool {
	return role == RoleAdmin
}

// CanManageInvites は招待（トークン）の発行・一覧・取り消しができるかを判定する（admin のみ）。
func CanManageInvites(role Role) bool {
	return role == RoleAdmin
}
