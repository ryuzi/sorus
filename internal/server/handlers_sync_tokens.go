package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"

	"sorus/internal/auth"
)

// 公開サーバーの招待（トークン）の管理。発行したトークンは sync_tokens にトークン ID（jti）で記録し、
// 取り消したトークンは同期 API で拒否する。
// トークン自体は保存しない。

const (
	// defaultInviteTTLDays は issue_invite で有効日数を省略したときの日数（sorus token create の既定と同じ）。
	defaultInviteTTLDays = 90
	// maxInviteTTLDays は issue_invite で指定できる有効日数の上限（sorus token create と同じ10年）。
	maxInviteTTLDays = 3650
	// maxInvitesPerHour は issue_invite で1時間に発行できる招待の数（公開サーバー全体）。
	// 管理者のトークンが漏れたときに、大量の招待を作られるのを抑える。
	maxInvitesPerHour = 30
	// maxListedTokens は list_tokens が返す記録の上限。
	maxListedTokens = 500
)

// RecordIssuedToken は発行したトークンの claims を db の sync_tokens に記録する。via は発行の経路
// （TokenIssuedViaCLI・TokenIssuedViaAPI・TokenIssuedViaSeen）、issuedBy は発行した管理者の名前（無ければ空）。
// 同じ jti の記録が既にあれば何もしない（取り消しの記録を上書きしない）。sorus token create は公開サーバーの
// 稼働中にも呼ぶため、1文で書き込む。
func RecordIssuedToken(ctx context.Context, db *sqlx.DB, claims *auth.Claims, via, issuedBy string) error {
	if claims == nil || !auth.ValidTokenID(claims.ID) {
		return fmt.Errorf("server: record token: %w", auth.ErrMalformedToken)
	}
	issuedAt := time.Now().UTC().Truncate(time.Second)
	if claims.IssuedAt > 0 {
		issuedAt = time.Unix(claims.IssuedAt, 0).UTC()
	}
	_, err := db.ExecContext(ctx, `INSERT INTO sync_tokens (jti, subject, role, issued_at, expires_at, issued_via, issued_by)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(jti) DO NOTHING`,
		claims.ID, claims.Subject, claims.Role, issuedAt, time.Unix(claims.ExpiresAt, 0).UTC(), via, issuedBy)
	if err != nil {
		return fmt.Errorf("server: record token: %w", err)
	}
	return nil
}

// syncTokenRevoked は検証済みのトークンが sync_tokens で取り消されているかを返す。発行の記録が無いトークン（公開
// サーバーの DB が無い場所で発行したものなど）は、初めて使われたときに記録し、以後は一覧に出して取り消せるようにする。
// ID が不正なトークンや記録を読めない・書けないときはエラーを返し、
// 呼び出し側はトークンを受け付けない。同期 API（checkSyncTokenRecord）と、閲覧制限中の画像の取得
// （syncTokenReadsUpload）が同じ記録と取り消しを使う。
func (s *Server) syncTokenRevoked(ctx context.Context, claims *auth.Claims) (bool, error) {
	if !auth.ValidTokenID(claims.ID) {
		return false, fmt.Errorf("server: check token: %w", auth.ErrMalformedToken)
	}
	var revokedAt sql.NullTime
	err := s.db.GetContext(ctx, &revokedAt, `SELECT revoked_at FROM sync_tokens WHERE jti = ?`, claims.ID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, RecordIssuedToken(ctx, s.db, claims, TokenIssuedViaSeen, "")
	case err != nil:
		return false, fmt.Errorf("server: check token: %w", err)
	}
	return revokedAt.Valid, nil
}

// checkSyncTokenRecord は検証済みのトークンが取り消されていないかを sync_tokens で確かめ（syncTokenRevoked）、
// 取り消されていれば 401 を返す。記録を読めない・書けないときはエラー（500）にして受け付けない。
func (s *Server) checkSyncTokenRecord(c echo.Context, claims *auth.Claims) error {
	revoked, err := s.syncTokenRevoked(c.Request().Context(), claims)
	if err != nil {
		return fmt.Errorf("server: sync api: %w", err)
	}
	if revoked {
		return syncUnauthorized(c, "invalid_token",
			"この招待は取り消されています。サイト管理者に新しい招待コードを発行してもらい、"+InviteJoinHint)
	}
	return nil
}

// syncIssueInvite は新しいトークンを発行して記録し、この要求の宛先（同期 API の URL）と束ねた招待コードを返す。
// 招待コードの URL は、管理者のアプリが実際に接続できた URL（要求のスキームとホスト）を使う。
func (s *Server) syncIssueInvite(c echo.Context, claims *auth.Claims, req *SyncRequest) error {
	if s.cfg.SyncSigningKey == nil {
		return echo.NewHTTPError(http.StatusNotImplemented,
			"この公開サーバーでは招待を発行できません。公開サーバーの sorus を最新版にして再起動するか、サーバーで sorus token create を使ってください")
	}
	role := auth.Role(strings.TrimSpace(req.Role))
	switch role {
	case auth.RoleWriter, auth.RoleEditor, auth.RoleAdmin:
	default:
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "招待する人の立場（writer・editor・admin）を指定してください")
	}
	sub := strings.TrimSpace(req.Subject)
	if sub == "" {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "招待する人の名前を入力してください")
	}
	if utf8.RuneCountInString(sub) > maxSyncDisplayNameRunes || strings.ContainsFunc(sub, unicode.IsControl) {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, fmt.Sprintf("名前は改行を含まない%d文字以内で入力してください", maxSyncDisplayNameRunes))
	}
	if strings.HasPrefix(strings.ToLower(sub), "sync-") {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "「sync-」で始まる名前は招待に使用できません")
	}
	var existingUserCount int
	if err := s.db.GetContext(c.Request().Context(), &existingUserCount, `SELECT COUNT(*) FROM users WHERE username = ? OR id = ?`, sub, sub); err != nil {
		return fmt.Errorf("server: sync issue invite: check existing user: %w", err)
	}
	if existingUserCount > 0 {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "既存のユーザーと同じ名前は招待に使用できません")
	}
	days := req.TTLDays
	if days == 0 {
		days = defaultInviteTTLDays
	}
	if days < 1 || days > maxInviteTTLDays {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, fmt.Sprintf("有効期間は1日から%d日までで指定してください", maxInviteTTLDays))
	}
	// 公開 URL（BaseURL）のホスト名が要求の Host と同じなら、そのスキーム・ポートを使う。それ以外は、接続自体の
	// TLS か TRUSTED_PROXIES（Config.TrustedProxies）のリバースプロキシが付けた X-Forwarded-Proto だけでスキームを
	// 決める。ALLOWED_HOSTS を指定したサイトでは、一覧に無い Host を招待コードに載せない（advertisedOrigin）。
	// クライアントが書いた転送ヘッダーや Host で、平文の接続を https と偽ったり、別のホストを指す招待コードを作らせない。
	syncURL := s.advertisedOrigin(c) + s.cfg.SyncAPIPath
	if _, _, err := normalizeRemoteURL(syncURL); err != nil {
		return echo.NewHTTPError(http.StatusUnprocessableEntity,
			"この公開サーバーへの接続が https ではないため、招待コードを作れません。https:// で始まる URL で接続し直してください")
	}

	token, issued, err := auth.IssueTokenClaims(s.cfg.SyncSigningKey, sub, string(role), time.Duration(days)*24*time.Hour)
	if err != nil {
		return fmt.Errorf("server: sync issue invite: %w", err)
	}
	code, err := EncodeInviteCode(syncURL, token)
	if err != nil {
		return fmt.Errorf("server: sync issue invite: %w", err)
	}
	// 記録できなかった招待は渡さない（一覧に出ず、取り消せない招待を作らない）。
	recorded, err := s.recordInvite(c.Request().Context(), issued, claims.Subject)
	if err != nil {
		return err
	}
	if !recorded {
		return echo.NewHTTPError(http.StatusTooManyRequests,
			fmt.Sprintf("招待の発行は1時間に%d件までです。しばらく待ってからもう一度お試しください", maxInvitesPerHour))
	}
	return c.JSON(http.StatusOK, SyncIssueInviteResponse{
		Action:     SyncActionIssueInvite,
		InviteCode: code,
		JTI:        issued.ID,
		Subject:    issued.Subject,
		Role:       issued.Role,
		ExpiresAt:  time.Unix(issued.ExpiresAt, 0).UTC(),
	})
}

// recordInvite は issue_invite で発行したトークンを sync_tokens に記録する。直近1時間の issue_invite による
// 記録が maxInvitesPerHour 件に達していれば記録せず false を返す。数えることと記録を1文で行う。SQLite の
// 書き込みの文は、読む前に書き込みのロックを取るため、同時に多数の発行が来ても（別のプロセスからの書き込みが
// あっても）数えてから記録するまでの間に他の記録は入らず、上限を超えて記録しない。
func (s *Server) recordInvite(ctx context.Context, claims *auth.Claims, issuedBy string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO sync_tokens (jti, subject, role, issued_at, expires_at, issued_via, issued_by)
		SELECT ?, ?, ?, ?, ?, ?, ?
		WHERE (SELECT COUNT(*) FROM sync_tokens WHERE issued_via = ? AND julianday(issued_at) > julianday(?)) < ?`,
		claims.ID, claims.Subject, claims.Role, time.Unix(claims.IssuedAt, 0).UTC(), time.Unix(claims.ExpiresAt, 0).UTC(),
		TokenIssuedViaAPI, issuedBy,
		TokenIssuedViaAPI, time.Now().UTC().Add(-time.Hour), maxInvitesPerHour)
	if err != nil {
		return false, fmt.Errorf("server: sync issue invite: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("server: sync issue invite: %w", err)
	}
	return n == 1, nil
}

const syncTokenColumns = `jti, subject, role, issued_at, expires_at, revoked_at, issued_via, issued_by`

// syncListTokens は記録している発行済みのトークンを、発行日時の新しい順に返す。
func (s *Server) syncListTokens(c echo.Context, claims *auth.Claims) error {
	tokens := []SyncTokenRecord{}
	err := s.db.SelectContext(c.Request().Context(), &tokens, `SELECT `+syncTokenColumns+` FROM sync_tokens
		ORDER BY julianday(issued_at) DESC, jti LIMIT ?`, maxListedTokens)
	if err != nil {
		return fmt.Errorf("server: sync list tokens: %w", err)
	}
	for i := range tokens {
		tokens[i].Current = claims.ID != "" && tokens[i].JTI == claims.ID
	}
	return c.JSON(http.StatusOK, SyncListTokensResponse{Action: SyncActionListTokens, Tokens: tokens, CurrentJTI: claims.ID})
}

// syncRevokeToken はトークン jti を取り消す。取り消し済みならそのまま記録を返す。
// 締め出しを防ぐため、使えるサイト管理者のトークン（取り消されておらず期限内の admin）が他に無くなる取り消しは断り、
// この要求に使っているトークン自体の取り消しは confirm_self が無ければ断る。
func (s *Server) syncRevokeToken(c echo.Context, claims *auth.Claims, req *SyncRequest) error {
	jti := strings.TrimSpace(req.JTI)
	if !auth.ValidTokenID(jti) {
		return echo.NewHTTPError(http.StatusBadRequest, "取り消す招待の ID（jti）を正しく指定してください")
	}
	ctx := c.Request().Context()
	rec, err := s.syncTokenRecord(ctx, jti)
	if err != nil {
		return err
	}
	if rec.RevokedAt != nil {
		rec.Current = jti == claims.ID
		return c.JSON(http.StatusOK, SyncRevokeTokenResponse{Action: SyncActionRevokeToken, Token: *rec})
	}
	if jti == claims.ID && !req.ConfirmSelf {
		return echo.NewHTTPError(http.StatusConflict,
			"いま使っている招待を取り消そうとしています。取り消すと、このアプリから公開サーバーに接続できなくなります。続けるには確認したうえで confirm_self を指定してください")
	}

	now := time.Now().UTC().Truncate(time.Second)
	// 残りのサイト管理者の確認と取り消しを1文で行い、同時に2つの取り消しが通って管理者がいなくなることを防ぐ。
	res, err := s.db.ExecContext(ctx, `UPDATE sync_tokens SET revoked_at = ?, revoked_by = ?
		WHERE jti = ? AND revoked_at IS NULL
		  AND (role <> ? OR EXISTS (SELECT 1 FROM sync_tokens o
		       WHERE o.role = ? AND o.revoked_at IS NULL AND julianday(o.expires_at) > julianday(?) AND o.jti <> ?))`,
		now, claims.Subject, jti, auth.RoleAdmin, auth.RoleAdmin, now, jti)
	if err != nil {
		return fmt.Errorf("server: sync revoke token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("server: sync revoke token: %w", err)
	}
	if rec, err = s.syncTokenRecord(ctx, jti); err != nil {
		return err
	}
	if n == 0 && rec.RevokedAt == nil {
		return echo.NewHTTPError(http.StatusConflict,
			"最後のサイト管理者の招待は取り消せません。先に別のサイト管理者の招待を発行し、その招待で参加してから取り消してください")
	}
	rec.Current = jti == claims.ID
	return c.JSON(http.StatusOK, SyncRevokeTokenResponse{Action: SyncActionRevokeToken, Token: *rec})
}

// syncTokenRecord は jti の発行の記録を返す。無ければ 404 を返す。
func (s *Server) syncTokenRecord(ctx context.Context, jti string) (*SyncTokenRecord, error) {
	var rec SyncTokenRecord
	err := s.db.GetContext(ctx, &rec, `SELECT `+syncTokenColumns+` FROM sync_tokens WHERE jti = ?`, jti)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, echo.NewHTTPError(http.StatusNotFound, "この招待は公開サーバーに記録されていません")
	}
	if err != nil {
		return nil, fmt.Errorf("server: sync token: %w", err)
	}
	return &rec, nil
}
