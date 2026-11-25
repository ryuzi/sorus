package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/mcp"
)

// DefaultSyncAPIPath は Config.SyncAPIPath を省略したときの、ローカル CMS と公開サーバーがコンテンツを
// やり取りする同期 API のパス。誰でも推測できるため、公開サーバーでは NewSyncAPIPath で生成した秘匿パスを使う。
const DefaultSyncAPIPath = "/api/sync"

const (
	// secretSyncAPIPathPrefix は NewSyncAPIPath が生成するパスの接頭辞。
	secretSyncAPIPathPrefix = DefaultSyncAPIPath + "-"
	// secretSyncAPIPathBytes は NewSyncAPIPath が生成するパスの乱数部分のバイト数（16進数で32桁）。
	secretSyncAPIPathBytes = 16
	// maxSyncAPIPathLength は同期 API のパスの最大長。
	maxSyncAPIPathLength = 200
)

// syncAPIPathPattern は同期 API のパスに使える形。URL の非予約文字だけの区間を / でつなぐ。
// Echo のルート記法（:param・*）や空の区間（//）を含むパスは拒否する。
var syncAPIPathPattern = regexp.MustCompile(`^(?:/[A-Za-z0-9._~-]+)+$`)

// reservedSyncAPIPathPrefixes は同期 API のパスに使えない、既存の機能が使うパス。
// /admin は公開専用モードで PublicGuardMiddleware が 404 にするため、同期 API に届かない。
var reservedSyncAPIPathPrefixes = []string{
	"/admin", "/static", strings.TrimSuffix(UploadURLPrefix, "/"), strings.TrimSuffix(AttachmentURLPrefix, "/"),
	strings.TrimSuffix(OGImageURLPrefix, "/"), mcp.DefaultBasePath,
}

// ErrInvalidSyncAPIPath は同期 API のパスの形式が正しくないことを示す。
var ErrInvalidSyncAPIPath = errors.New("invalid sync api path")

// NewSyncAPIPath は第三者に推測できない同期 API のパス（/api/sync-<32桁の16進数>）を生成する。
func NewSyncAPIPath() (string, error) {
	buf := make([]byte, secretSyncAPIPathBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("server: generate sync api path: %w", err)
	}
	return secretSyncAPIPathPrefix + hex.EncodeToString(buf), nil
}

// NormalizeSyncAPIPath は同期 API のパスを検査し、前後の空白と末尾の / を除き、先頭の / を補った形にする。
// 形式が正しくなければ ErrInvalidSyncAPIPath を包んだエラーを返す。
func NormalizeSyncAPIPath(raw string) (string, error) {
	p := strings.TrimRight(strings.TrimSpace(raw), "/")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if p == "/" || len(p) > maxSyncAPIPathLength || !syncAPIPathPattern.MatchString(p) || path.Clean(p) != p {
		return "", fmt.Errorf("%w %q: use a path such as %s<random hex> (letters, digits and . _ ~ - only)",
			ErrInvalidSyncAPIPath, raw, secretSyncAPIPathPrefix)
	}
	for _, reserved := range reservedSyncAPIPathPrefixes {
		if p == reserved || strings.HasPrefix(p, reserved+"/") {
			return "", fmt.Errorf("%w %q: %s is used by another feature", ErrInvalidSyncAPIPath, raw, reserved)
		}
	}
	return p, nil
}

// 同期 API のアクション。SyncRequest.Action に指定する。
const (
	// SyncActionSubmitDraft は記事と添付画像を提出し、校閲待ちとして保存する（writer, editor, admin）。
	SyncActionSubmitDraft = "submit_draft"
	// SyncActionFetchPending は校閲待ちの記事の一覧を本文とともに返す（editor, admin）。
	SyncActionFetchPending = "fetch_pending"
	// SyncActionPublishArticle は校閲待ちの記事を公開する（editor, admin）。Article・Images を付けると記事更新と公開を一度に行う。
	SyncActionPublishArticle = "publish_article"
	// SyncActionSyncSettings はサイト基本設定を更新する（admin のみ）。
	SyncActionSyncSettings = "sync_settings"
	// SyncActionIssueInvite は新しいトークンを発行し、招待コードにして返す（admin のみ）。
	SyncActionIssueInvite = "issue_invite"
	// SyncActionListTokens は公開サーバーが記録している発行済みのトークンの一覧を返す（admin のみ）。
	SyncActionListTokens = "list_tokens"
	// SyncActionRevokeToken は発行済みのトークンを取り消す（admin のみ）。
	SyncActionRevokeToken = "revoke_token"
	// SyncActionScheduleArticle は校閲待ちの記事を、公開日時（PublishAt）を決めて予約する（editor, admin）。
	// 予約した記事は状態を公開、公開日時を PublishAt として保存し、その時刻から読者に見え始める（article.IsScheduled）。
	// 予約を知らない古い公開サーバーは未対応の action として 400 で断るため、即時公開になる事故は起きない。
	SyncActionScheduleArticle = "schedule_article"
	// SyncActionUnscheduleArticle は予約中の記事を校閲待ちに戻す（editor, admin）。日時を変えるときは、
	// 取り消してから予約し直す。
	SyncActionUnscheduleArticle = "unschedule_article"
	// SyncActionUnpublishArticle は公開中・予約中の記事を下書きに戻す（editor, admin）。
	SyncActionUnpublishArticle = "unpublish_article"
	// SyncActionServerInfo は公開サーバーが対応している機能を返す（writer, editor, admin）。ローカル CMS は
	// 予約の選択肢を出す前にこれで確かめる。知らない古い公開サーバーは 400 で断り、予約に対応していないと分かる。
	SyncActionServerInfo = "server_info"
	// SyncActionGetSiteAccess は公開サーバー自身の閲覧制限（Basic 認証）の状態を返す（admin のみ）。
	// パスワードとそのハッシュは返さず、設定済みかだけを返す。
	SyncActionGetSiteAccess = "get_site_access"
	// SyncActionSetSiteAccess は公開サーバー自身の閲覧制限を変更する（admin のみ）。SiteAccess を指定する。
	// PC の閲覧制限とは別の設定で、sync_settings（サイト基本設定の反映）では変わらない。
	SyncActionSetSiteAccess = "set_site_access"
	// SyncActionGetMaintenance は公開サーバーのメンテナンス表示の状態を返す（admin のみ）。
	SyncActionGetMaintenance = "get_maintenance"
	// SyncActionSetMaintenance は公開サーバーのメンテナンス表示を始める・終える（admin のみ）。Maintenance を指定する。
	// 始めるときは選択中の画面（Maintenance.Page）もいっしょに送り、ON と画面を一体で反映する。画面だけを送ると、
	// 表示中の画面を更新し、ON/OFF は変えない。sync_settings（テーマを含むサイト基本設定の反映）や set_site_access
	// では変わらない。知らない古い公開サーバーは未対応の action として 400 で断る（何も変わらない）。応答に page が
	// 無ければ読み取れない（結果不明）として扱う。
	SyncActionSetMaintenance = "set_maintenance"
	// SyncActionSyncMedia はメディアライブラリの画像の記録（media の行）と画像ファイルを受け取り、同じ ID で保存する
	// （追加・更新。editor, admin）。公開サイトのフォトギャラリー（トップページのギャラリー・本文の :::gallery）は
	// タグで media を引くため、記事の添付画像の転送だけでは表示されない。Media を指定し、公開サーバーに無い画像
	// ファイルだけを Images に付ける（応答の Missing を見て送り直す）。知らない古い公開サーバーは未対応の action
	// として 400 で断る（何も変わらない）。
	SyncActionSyncMedia = "sync_media"
	// SyncActionSyncAttachments は記事の添付資料（PDF など）の記録（attachments の行）とファイルを受け取り、同じ ID で
	// 保存する（追加・更新。editor, admin。handlers_sync_attachments.go）。ArticleID の記事が公開サーバーに無ければ 404。
	// Attachments にはその記事の添付資料をすべて並べ、ファイルの中身（Data）は公開サーバーに無いものだけを付ける（応答の
	// Missing を見て1件ずつ送り直す）。PruneAttachments を付けると、一覧に無い添付資料を公開サーバーから消す（PC で削除した
	// 資料を公開サイトから取り下げる）。知らない古い公開サーバーは未対応の action として 400 で断る（何も変わらない）。
	SyncActionSyncAttachments = "sync_attachments"
	// SyncActionCheckout は記事 ArticleID に、PC の sorus で編集中の印（チェックアウト）を付ける（editor, admin）。
	// 印は article.CheckoutTTL の間有効で、その間はほかの人の提出・公開・予約を 409 で断る（Force で上書きできる）。
	// ほかの人の有効な印があれば、Force を付けない限り 409 を返す。知らない古い公開サーバーは未対応の action として 400 で断る。
	SyncActionCheckout = "checkout"
	// SyncActionReleaseCheckout は記事 ArticleID の編集中の印を外す（editor, admin）。
	SyncActionReleaseCheckout = "release_checkout"
	// SyncActionResolveProposal は記事 ArticleID に届いている AI アプリからの変更案（mcp.Proposal）を、ProposalAction
	// （SyncProposalAccept・SyncProposalDiscard）に従って記事に取り込むか破棄する（editor, admin）。ProposalID を付けると、
	// 確かめた案が新しい案に置き換わっていたときに 409 で断る。
	SyncActionResolveProposal = "resolve_proposal"
	// SyncActionPullArticle は記事 ArticleID 1件を本文と添付画像のファイル名とともに返す（writer, editor, admin）。
	// PC の sorus で続きを書くために取り込む下書き・校閲待ちの記事に限り、それ以外の状態（公開中・予約中など）は 409、
	// 記事が無ければ 404 を返す。知らない古い公開サーバーは未対応の action として 400 で断る。
	SyncActionPullArticle = "pull_article"
)

// resolve_proposal の ProposalAction。
const (
	// SyncProposalAccept は変更案を記事に取り込み、案を消す。
	SyncProposalAccept = "accept"
	// SyncProposalDiscard は記事を変えずに変更案を消す。
	SyncProposalDiscard = "discard"
)

// SyncConflictCode は、記事が PC で最後に取得したときから公開サーバーで変わっていたため保存しなかった 409 の応答の Code。
const SyncConflictCode = "conflict"

// SyncCheckedOutCode は、ほかの人の編集中の印（チェックアウト）があるため断った 409 の応答の code。応答の checked_out_by に
// 編集中の人を入れる。PC は Force を付けて送り直せば引き継げる。
const SyncCheckedOutCode = "checked_out"

// MinScheduleLead は予約の公開日時に指定できる、今からの最短の間隔。これより近い日時の予約は断る。
const MinScheduleLead = time.Minute

// SyncRequest は同期 API（POST Config.SyncAPIPath）のリクエストボディ（application/json）。
// Action ごとに使う項目が異なり、使わない項目は無視する。
type SyncRequest struct {
	Action string `json:"action"`
	// Article は submit_draft で提出する記事。publish_article に付けると記事を更新して直接公開する。
	Article *SyncArticle `json:"article,omitempty"`
	// Images は submit_draft・publish_article で記事とともに保存する添付画像。sync_media では Media の記録が参照する
	// 画像ファイルのうち、公開サーバーに無いもの。
	Images []SyncImage `json:"images,omitempty"`
	// ArticleID は publish_article で公開・更新する記事、schedule_article で予約する記事、
	// unschedule_article で予約を取り消す記事の ID。
	ArticleID string `json:"article_id,omitempty"`
	// PublishAt は schedule_article で予約する公開日時（RFC 3339、ローカル CMS は UTC で送る）。
	// 今から MinScheduleLead 以上先でなければならない。
	PublishAt *time.Time `json:"publish_at,omitempty"`
	// Settings は sync_settings で更新するサイト基本設定。
	Settings *SyncSettings `json:"settings,omitempty"`
	// Role・Subject・TTLDays は issue_invite で発行するトークンのロール・名前（sub）・有効日数（1〜3650、0 なら90日）。
	Role    string `json:"role,omitempty"`
	Subject string `json:"sub,omitempty"`
	TTLDays int    `json:"ttl_days,omitempty"`
	// JTI は revoke_token で取り消すトークンの ID。
	JTI string `json:"jti,omitempty"`
	// ConfirmSelf は revoke_token で、この要求に使っているトークン自体を取り消すことを確認済みか。
	ConfirmSelf bool `json:"confirm_self,omitempty"`
	// SiteAccess は set_site_access で保存する閲覧制限の設定。
	SiteAccess *SyncSiteAccessUpdate `json:"site_access,omitempty"`
	// Maintenance は set_maintenance で指定するメンテナンス表示の状態。
	Maintenance *SyncMaintenanceUpdate `json:"maintenance,omitempty"`
	// MCP は MCP 連携の操作（mcp_issue_token・mcp_revoke_token・mcp_revoke_oauth_app）の内容（handlers_sync_mcp.go）。
	MCP *SyncMCPRequest `json:"mcp,omitempty"`
	// Media は sync_media で保存するメディアライブラリの画像の記録（maxSyncMediaItems 件まで）。画像ファイルは Images に付ける。
	Media []SyncMedia `json:"media,omitempty"`
	// TagMediaIDs は sync_media で「いまこのタグが付いている PC の画像」をタグごとに示す（タグ → PC の画像 ID の
	// すべて）。公開サーバーは、このトークンが送って保存した画像のうち、そのタグが付いているのに一覧に無い画像から
	// タグだけを外す（PC でタグを外した・画像を削除したことを公開サイトのギャラリーに反映する。画像の記録とファイルは
	// 消さず、公開サーバーで直接登録した画像やほかの招待で送られた画像には触れない）。Media と同じ要求で送るか、
	// 外すだけなら Media を空にして送る。知らない古い公開サーバーは無視する（応答の Untagged が無い）。
	TagMediaIDs map[string][]string `json:"tag_media_ids,omitempty"`
	// Attachments は sync_attachments で保存する、ArticleID の記事の添付資料（maxAttachmentsPerArticle 件まで）。
	Attachments []SyncAttachment `json:"attachments,omitempty"`
	// PruneAttachments は sync_attachments で、Attachments をその記事の添付資料のすべてとして扱い、一覧に無い添付資料を
	// 公開サーバーから消すか。ファイルの中身だけを送り直す要求では付けない。
	PruneAttachments bool `json:"prune_attachments,omitempty"`
	// BaseUpdatedAt は submit_draft・publish_article（Article 付きと承認）・schedule_article で、PC が最後に取得したときの公開サーバーの記事の
	// 更新日時。公開サーバーの今の更新日時と1秒より大きくずれていれば、その後にほかの人が変えたものとして保存せず、
	// SyncConflictResponse（409）を返す。省略すると確かめない（古いローカル CMS）。
	BaseUpdatedAt *time.Time `json:"base_updated_at,omitempty"`
	// Force は、ほかの人の編集中の印（チェックアウト）があっても提出・公開・予約・チェックアウトを行うか。
	// BaseUpdatedAt の食い違いは Force では無視しない（上書きするときは BaseUpdatedAt を送らない）。
	Force bool `json:"force,omitempty"`
	// ClientID は要求を出した PC の sorus の識別子（PC ごとに一度だけ作って保存する remote_client_id）。同じ人・同じ
	// トークンを複数の PC で使っていても、checkout で付けた編集中の印がどの PC のものかを見分ける。checkout・
	// release_checkout・submit_draft・publish_article・schedule_article・resolve_proposal で送る。省略すると（古い PC）
	// 名前（sub）だけで見分ける。
	ClientID string `json:"client_id,omitempty"`
	// ProposalAction は resolve_proposal で行う操作（SyncProposalAccept・SyncProposalDiscard）。
	ProposalAction string `json:"proposal_action,omitempty"`
	// ProposalID は resolve_proposal で確かめた変更案の ID。省略すると今ある案を対象にする。
	ProposalID string `json:"proposal_id,omitempty"`
}

// SyncAttachment は sync_attachments で送る添付資料1件の記録（attachments の行）。公開サーバーは同じ ID で保存し、
// 保存するファイル名（ID + 拡張子）と MIME タイプは Filename の拡張子から自分で決める（PC の保存先のパスは送らない）。
type SyncAttachment struct {
	// ID は PC の添付資料の ID（32桁の16進数）。公開サイトのダウンロード URL（/attachments/<ID>/download）にも使う。
	ID string `json:"id"`
	// Filename はダウンロードさせるときのファイル名（表示用）。拡張子は添付を許可した形式でなければならない。
	Filename string `json:"filename"`
	// Size はファイルのバイト数、SHA256 はファイルの SHA-256（小文字の16進数）。公開サーバーは自分のファイルと比べ、
	// 同じなら中身を受け取らずに記録だけを保存する。PC にファイルが無いときは SHA256 を空にする（公開サーバーにファイルが
	// あれば記録だけを保存し、無ければ Missing として返す）。
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
	// CreatedAt は PC で添付した日時（一覧の並び順に使う）。
	CreatedAt time.Time `json:"created_at"`
	// Data はファイルの中身（JSON では標準の Base64 文字列）。公開サーバーに無いと言われたものだけを付ける。
	Data []byte `json:"data,omitempty"`
}

// SyncAttachmentsResponse は sync_attachments の応答。
type SyncAttachmentsResponse struct {
	Action string `json:"action"`
	// Saved は保存（追加・更新）した記録の ID。要求の順で、無ければ空配列。
	Saved []string `json:"saved"`
	// Missing はファイルが公開サーバーに無い（または中身が違う）ため保存できなかった記録の ID。PC はファイルの中身を
	// Data に付けて送り直す。無ければ空配列。
	Missing []string `json:"missing"`
	// Removed は PruneAttachments に従って公開サーバーから消した添付資料の ID。無ければ空配列。
	Removed []string `json:"removed"`
}

// SyncMedia は sync_media で送るメディアライブラリの画像1件の記録（media の行）。公開サイトのフォトギャラリーに使う。
// ファイルの場所は PC の保存先を含めず、uploads/ 直下のファイル名（syncImageNamePattern の形）だけを送る。
// 公開サーバーは自分のアップロード先のパスにして保存する。
type SyncMedia struct {
	// ID は PC のメディアライブラリの画像 ID。公開サーバーでも同じ ID で保存し、同じ ID の再送は更新になる。
	ID string `json:"id"`
	// Filename はアップロードしたときの元のファイル名（表示用）。
	Filename string `json:"filename"`
	// PathOriginal・PathLarge・PathMedium・PathSmall は原寸と各サイズの画像のファイル名。無いサイズは空。
	PathOriginal string `json:"path_original,omitempty"`
	PathLarge    string `json:"path_large,omitempty"`
	PathMedium   string `json:"path_medium,omitempty"`
	PathSmall    string `json:"path_small,omitempty"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	MimeType     string `json:"mime_type"`
	Alt          string `json:"alt"`
	Caption      string `json:"caption"`
	// AITags は画像のタグ（「, 」区切り）。公開サーバーのギャラリーはこのタグで画像を選ぶ。
	AITags string `json:"ai_tags"`
	// CreatedAt は PC で画像を登録した日時。ギャラリーの並び順（新しい順・古い順）に使う。
	CreatedAt time.Time `json:"created_at"`
}

// files は記録が参照する画像のファイル名（空・重複を除く）を返す。
func (m SyncMedia) files() []string {
	var names []string
	for _, n := range []string{m.PathOriginal, m.PathLarge, m.PathMedium, m.PathSmall} {
		if n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	return names
}

// SyncMediaResponse は sync_media の応答。
type SyncMediaResponse struct {
	Action string `json:"action"`
	// Saved は保存（追加・更新）した記録の ID。要求の順で、無ければ空配列。
	Saved []string `json:"saved"`
	// Missing は公開サーバーに無いため記録を保存できなかった画像のファイル名。PC はこのファイルを Images に付けて、
	// その記録を送り直す。無ければ空配列。
	Missing []string `json:"missing"`
	// Untagged は TagMediaIDs に従ってタグを外した画像の ID。無ければ空配列。TagMediaIDs を知らない古い公開サーバーの
	// 応答には無い（nil）ため、PC はタグを外したことを反映できなかったと判断できる。
	Untagged []string `json:"untagged"`
}

// SyncMaintenanceUpdate は set_maintenance で指定する、公開サーバーのメンテナンス表示の状態。
type SyncMaintenanceUpdate struct {
	// Enabled はメンテナンス表示を始める（true）か終える（false）か。切り替え（反転）ではなく状態を指定する。
	// 画面だけを更新するときは省略する（表示中でなければ何も変えずに断る。ON/OFF は変えない）。
	// Enabled も Page も無ければ 400（何も変えない）。
	Enabled *bool `json:"enabled"`
	// Page は訪問者に出す画面。始める（オフからオン）ときだけ反映し、すでにオンなら無視する（画面の更新は
	// Enabled を付けずに Page だけを送る）。オフにする要求では無視する。
	Page *SyncMaintenancePage `json:"page,omitempty"`
}

const (
	// SyncMaintenancePageCustom はエンジニアが用意した単一 HTML を表示する。
	SyncMaintenancePageCustom = "custom"
	// SyncMaintenancePageDefault は組み込みの標準画面を表示する。
	SyncMaintenancePageDefault = "default"
)

// SyncMaintenancePage は set_maintenance で送る、訪問者に出す画面。
type SyncMaintenancePage struct {
	// Kind は custom（HTML を使う）か default（標準画面）。必須。
	Kind string `json:"kind"`
	// Name は custom のとき、元のファイル名（表示用。パスには使わない）。
	Name string `json:"name,omitempty"`
	// HTML は custom のときの UTF-8 の単一 HTML。標準画面のときは空。
	HTML string `json:"html,omitempty"`
}

// SyncMaintenancePageInfo は get_maintenance・set_maintenance の応答に付ける、保存済みの画面の識別。
// HTML 本文は返さない。この項目が無い応答は読み取れない。
type SyncMaintenancePageInfo struct {
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
	// SHA256 は保存した HTML の SHA-256（小文字の 16 進）。標準画面のときは空。
	SHA256 string `json:"sha256,omitempty"`
	// UpdatedAt は画面を最後に保存した日時（UTC）。一度も保存していなければ無い。
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// SyncMaintenanceResponse は get_maintenance・set_maintenance の応答。公開サーバーのメンテナンス表示の状態を表す。
type SyncMaintenanceResponse struct {
	Action string `json:"action"`
	// Enabled は公開サーバーがいまメンテナンス表示をしているか（公開サイトの訪問者に 503 を返しているか）。
	// 公開サーバーの DB に保存した設定で、再起動しても続く。
	Enabled bool `json:"enabled"`
	// ChangedAt はメンテナンス表示を最後に始めた・終えた日時（UTC）。一度も切り替えていなければ無い。
	ChangedAt *time.Time `json:"changed_at,omitempty"`
	// Changed は set_maintenance で ON/OFF が変わったか（すでにその状態だったなら false）。
	Changed bool `json:"changed"`
	// Page は保存済みの画面。対応している公開サーバーは必ず付ける（標準画面でも kind=default）。
	Page *SyncMaintenancePageInfo `json:"page,omitempty"`
	// PageChanged は set_maintenance で保存済みの画面が変わったか。get_maintenance では常に false。
	PageChanged bool `json:"page_changed"`
}

// SyncSiteAccessUpdate は set_site_access で保存する、公開サーバー自身の閲覧制限（Basic 認証）の設定。
// 検査の規則は管理画面の「サイト設定」と同じ（validateBasicAuthInput）。
type SyncSiteAccessUpdate struct {
	// Enabled は閲覧制限を有効にするか。有効にするときは User と、Password または保存済みのパスワードが要る。
	Enabled bool `json:"enabled"`
	// User は閲覧用のユーザー名（前後の空白は除く）。
	User string `json:"user"`
	// Password は新しい閲覧用のパスワード。空なら保存済みのパスワードを使い続ける。公開サーバーは bcrypt の
	// ハッシュだけを保存し、応答・ログには出さない。
	Password string `json:"password,omitempty"`
	// ExpectedRevision は変更の前提にした設定の版（get_site_access の応答の Revision）。必須。保存済みの版と
	// 違えば（ほかの PC が先に変更していれば）何も変えずに 409 を返す。
	ExpectedRevision *string `json:"expected_revision"`
}

// SyncArticle は submit_draft で提出する記事。
type SyncArticle struct {
	// ID はローカル CMS の記事 ID。指定すると公開サーバーでも同じ ID で保存し、同じ ID の再提出は更新になる。
	// 省略すると公開サーバーが新しい ID を発行する。
	ID              string `json:"id,omitempty"`
	Title           string `json:"title"`
	Slug            string `json:"slug,omitempty"`
	ContentMarkdown string `json:"content_markdown"`
	Excerpt         string `json:"excerpt,omitempty"`
	// FeaturedImageURL はアイキャッチ画像の URL（/uploads/… の相対パスか http(s) の絶対 URL）。nil（項目なし）は
	// 公開サーバーの今の値を保ち（アイキャッチを知らない古いローカル CMS からの提出で消さない）、空文字は外す。
	// 画像ファイル自体は Images で送る。
	FeaturedImageURL *string `json:"featured_image_url,omitempty"`
	// Category はカテゴリのスラッグまたは名前。公開サーバーに無ければ既定のカテゴリ（更新では現在のカテゴリ）にする。
	Category string `json:"category,omitempty"`
	// Kind は記事の種類（post・page）。省略すると新しい記事は post、更新では現在の種類を保つ
	// （種類を知らない古いローカル CMS からの提出を記事として扱う）。
	Kind article.Kind `json:"kind,omitempty"`
	// 多言語メタデータ（言語コード、翻訳グループID、翻訳元の記事ID）
	Lang               string `json:"lang,omitempty"`
	TranslationGroupID string `json:"translation_group_id,omitempty"`
	SourceArticleID    string `json:"source_article_id,omitempty"`
	// PublishedAt は PC での公開日時（publish_article で直接公開するときだけ使う）。今より前なら公開サーバーでも同じ
	// 公開日時にし、一括同期で送った古い記事が「今日の記事」として並ばないようにする。未来の日時・省略したときは、
	// 公開サーバーの今までどおり（初めて公開する記事なら今の時刻）にする。知らない古い公開サーバーは無視する。
	PublishedAt *time.Time `json:"published_at,omitempty"`
}

// SyncImage は submit_draft で転送する添付画像1件。
type SyncImage struct {
	// Name はローカル CMS の uploads/ でのファイル名（例: <32桁の16進数>-1200.webp）。公開サーバーにも同じ名前で保存する。
	Name string `json:"name"`
	// Data は WebP ファイルの中身。JSON では標準の Base64 文字列で表す。
	Data []byte `json:"data"`
}

// SyncSettings は sync_settings で更新するサイト基本設定とサイト構成。nil の項目は変更しない
// （サイト構成を知らない古いローカル CMS が反映しても、公開サーバーのメニュー・トップページは消えない）。
type SyncSettings struct {
	SiteName        *string `json:"site_name,omitempty"`
	SiteDescription *string `json:"site_description,omitempty"`
	BaseURL         *string `json:"base_url,omitempty"`
	Copyright       *string `json:"copyright,omitempty"`
	AnalyticsTag    *string `json:"analytics_tag,omitempty"`
	Theme           *string `json:"theme,omitempty"`
	// SiteMenu はナビゲーションメニュー。空の配列はメニューを空にする。項目の Children（ドロップダウンの子項目）と
	// 種類 group（見出しのみ）も送る。子項目を知らない古い公開サーバーは Children を捨て、group は未知の種類として
	// 断る（422）。
	SiteMenu *[]MenuItem `json:"site_menu,omitempty"`
	// SiteFooterMenu はフッターのサイトマップ（最上位の項目が列の見出し、Children が列のリンク）。空の配列はサイトマップを
	// 出さなくする。知らない古い公開サーバーは無視する（応答の SyncSettingsValues.SiteFooterMenu が nil）。
	SiteFooterMenu *[]MenuItem `json:"site_footer_menu,omitempty"`
	// FrontPage はトップに出すページのスラッグ。空文字は解除（トップは記事の一覧）。
	FrontPage *string `json:"front_page,omitempty"`
	// FrontCards はトップに並べるページのスラッグ。空の配列はカードを出さない。
	FrontCards      *[]string `json:"front_cards,omitempty"`
	TopGalleryTag   *string   `json:"top_gallery_tag,omitempty"`
	TopGalleryLimit *int      `json:"top_gallery_limit,omitempty"`
	TopGalleryTitle *string   `json:"top_gallery_title,omitempty"`
	// TopGalleryLayout はトップページのフォトギャラリーの並べ方（justified・masonry・cards）、TopGalleryLinkURL・
	// TopGalleryLinkLabel はその下の「すべて見る」リンクの行き先と文言（空文字はリンクを出さない・既定の文言）。
	// 知らない古い公開サーバーはこれらを無視する（応答の SyncSettingsValues.TopGalleryLayout が nil）。
	TopGalleryLayout    *string `json:"top_gallery_layout,omitempty"`
	TopGalleryLinkURL   *string `json:"top_gallery_link_url,omitempty"`
	TopGalleryLinkLabel *string `json:"top_gallery_link_label,omitempty"`
	// ShowFrontCardImages・ShowArticleCardImages・ShowRelatedCardImages は公開カード3種の
	// アイキャッチ画像を表示するか（テーマ設定画面の「公開カードのアイキャッチ画像表示」）。
	ShowFrontCardImages   *bool `json:"show_front_card_images,omitempty"`
	ShowArticleCardImages *bool `json:"show_article_card_images,omitempty"`
	ShowRelatedCardImages *bool `json:"show_related_card_images,omitempty"`
	// MultilingualEnabled は多言語対応（/en 以下の英語ページ・言語切り替え・hreflang）を有効にするか（基本設定の
	// multilingual_enabled）。知らない古い公開サーバーはこの項目を無視する（応答の Settings.MultilingualEnabled が nil）。
	MultilingualEnabled *bool `json:"multilingual_enabled,omitempty"`
	// ShowAuthor は記事や一覧に執筆者名を表示するか（サイト基本設定の「記事や一覧に執筆者名を表示する」）。
	// 知らない古い公開サーバーはこの項目を無視する（応答の SyncSettingsValues.ShowAuthor が nil になる）。
	ShowAuthor *bool `json:"show_author,omitempty"`
	// AIEnabled は AI 機能を使うか（PC の基本設定・「AI・外部連携」の ai_enabled）。公開サーバーは自分の settings の
	// ai_enabled に保存し、OFF なら外部の AI アプリとの連携（MCP・OAuth・同期 API の mcp_*）を無いものとして 404 にする。
	// 公開サーバーの環境変数 SORUS_AI=off は、この値より優先する（応答の SyncSettingsValues.AIForcedOff）。知らない古い
	// 公開サーバーは無視する（応答の SyncSettingsValues.AIEnabled が nil になり、PC は反映されなかったと分かる）。
	AIEnabled *bool `json:"ai_enabled,omitempty"`
	// RevokeAllMCP は公開サーバーで発行済みの MCP の API トークンと、OAuth 2.1 で許可した AI アプリの連携をすべて
	// 取り消すか（「AI・外部連携」を OFF にするときの「許可済みの連携も解除する」）。AIEnabled といっしょに送る。
	RevokeAllMCP bool `json:"revoke_all_mcp,omitempty"`
	// ThemePackage は Theme に指定した独自テーマのファイル。付けると公開サーバーはテーマを受け取って配置し
	// （同じ ID のテーマは置き換える）、設定とともに適用する。ID は Theme と同じでなければならない。
	// 知らない古い公開サーバーはこの項目を無視するため、ローカル CMS は先に server_info の CustomThemes で確かめる。
	ThemePackage *SyncThemePackage `json:"theme_package,omitempty"`
}

// SyncThemePackage は sync_settings で送る独自テーマ1つ。PC で取り込んだテーマのうち、公開サーバーが使うファイル
// （theme.json と、配信するテーマのファイル）だけを Zip にしたもの。公開サーバーはテーマの取り込みと同じ検査
// （上限・危険なパス・シンボリックリンク・theme.json と theme.css の検証）を通してから配置する。
type SyncThemePackage struct {
	// ID はテーマ ID。公開サーバーでも同じ ID で配置する（Zip の中の名前からは決めない）。
	ID string `json:"id"`
	// Archive は Zip の中身（theme.MaxArchiveSize まで）。JSON では標準の Base64 文字列で表す。
	Archive []byte `json:"archive"`
	// SHA256 は Archive の SHA-256（16進数の小文字）。公開サーバーは一致を確かめ、受け取ったものを応答で返す。
	SHA256 string `json:"sha256"`
}

// SyncThemePackageResult は sync_settings で受け取って配置した独自テーマ。ThemePackage を知らない古い公開サーバーの
// 応答には無い（読んだ側では nil になる）。
type SyncThemePackageResult struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
}

// SyncArticleRef は提出・公開した記事の公開サーバー上の状態。
type SyncArticleRef struct {
	ID       string         `json:"id"`
	Slug     string         `json:"slug"`
	Status   article.Status `json:"status"`
	Kind     article.Kind   `json:"kind"`
	Category string         `json:"category"`
	// URL は公開ページの URL。BaseURL が未設定ならサイトルートからのパス。
	URL         string     `json:"url"`
	UpdatedAt   time.Time  `json:"updated_at"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
}

// SyncSubmitResponse は submit_draft の応答。
type SyncSubmitResponse struct {
	Action  string         `json:"action"`
	Created bool           `json:"created"`
	Article SyncArticleRef `json:"article"`
	// Images は保存した（または同じ内容で保存済みだった）画像の配信 URL。
	Images []string `json:"images"`
	// Attachments は公開サーバーにあるその記事の添付資料の数（publish_article の応答だけ）。PC の記事にも公開サーバーにも
	// 添付資料が無ければ、PC は sync_attachments を送らない（数百件の一括同期で要求の数を半分にする）。無い（古い公開サーバー）
	// ときは数が分からないため、今までどおり送る。
	Attachments *int `json:"attachments,omitempty"`
}

// SyncPendingArticle は fetch_pending が返す校閲待ちの記事、または予約中の記事1件。
type SyncPendingArticle struct {
	ID              string         `db:"id" json:"id"`
	Title           string         `db:"title" json:"title"`
	Slug            string         `db:"slug" json:"slug"`
	ContentMarkdown string         `db:"content_markdown" json:"content_markdown"`
	Excerpt         string         `db:"excerpt" json:"excerpt"`
	Status          article.Status `db:"status" json:"status"`
	Kind            article.Kind   `db:"kind" json:"kind,omitempty"`
	Category        string         `db:"category_slug" json:"category"`
	CategoryName    string         `db:"category_name" json:"category_name"`
	AuthorID        string         `db:"author_id" json:"author_id"`
	AuthorName      string         `db:"author_name" json:"author_name"`
	CreatedAt       time.Time      `db:"created_at" json:"created_at"`
	UpdatedAt       time.Time      `db:"updated_at" json:"updated_at"`
	// PublishedAt は公開日時。予約中の記事では予約した日時。校閲待ちの記事では、一度公開した記事の
	// 最初の公開日時（公開したことが無ければ無い）。
	PublishedAt *time.Time `db:"published_at" json:"published_at,omitempty"`
	// 多言語メタデータ
	Lang               string `db:"lang" json:"lang,omitempty"`
	TranslationGroupID string `db:"translation_group_id" json:"translation_group_id,omitempty"`
	SourceArticleID    string `db:"source_article_id" json:"source_article_id,omitempty"`
	// CheckedOutBy・CheckedOutAt は PC の sorus でこの記事を編集中の人と、その開始日時（有効な印があるときだけ）。
	CheckedOutBy string     `db:"checked_out_by" json:"checked_out_by,omitempty"`
	CheckedOutAt *time.Time `db:"checked_out_at" json:"checked_out_at,omitempty"`
	// CheckedOutClient は編集中の印を付けた PC の sorus の識別子（SyncRequest.ClientID。古い PC の印では無い）。
	// PC は自分の識別子と比べ、「この PC で編集中」と「別の PC で編集中」を見分ける。
	CheckedOutClient string `db:"checked_out_client" json:"checked_out_client,omitempty"`
	// PendingProposal はこの記事に届いている、AI アプリからの確認待ちの変更案（無ければ無い）。
	PendingProposal *SyncProposalSummary `db:"-" json:"pending_proposal,omitempty"`
}

// SyncProposalSummary は fetch_pending で記事に添える、AI アプリからの確認待ちの変更案。Has* が false の項目は案で変えない。
// Title・Excerpt・ContentMarkdown は案を今の記事に重ねた後の値。
type SyncProposalSummary struct {
	ID              string    `json:"id"`
	Kind            string    `json:"kind"`
	ProposedBy      string    `json:"proposed_by"`
	CreatedAt       time.Time `json:"created_at"`
	Title           string    `json:"title,omitempty"`
	Excerpt         string    `json:"excerpt,omitempty"`
	ContentMarkdown string    `json:"content_markdown,omitempty"`
	HasTitle        bool      `json:"has_title"`
	HasExcerpt      bool      `json:"has_excerpt"`
	HasContent      bool      `json:"has_content"`
}

// SyncConflictResponse は、PC が最後に取得したとき（SyncRequest.BaseUpdatedAt）から公開サーバーの記事が変わっていたため
// 保存しなかったときの 409 の応答。Code は SyncConflictCode。Server* は公開サーバーの今の記事。
type SyncConflictResponse struct {
	Code                  string    `json:"code"`
	Message               string    `json:"message"`
	ArticleID             string    `json:"article_id"`
	ServerUpdatedAt       time.Time `json:"server_updated_at"`
	ServerTitle           string    `json:"server_title"`
	ServerContentMarkdown string    `json:"server_content_markdown"`
	ServerAuthorName      string    `json:"server_author_name"`
}

// SyncCheckoutResponse は checkout・release_checkout の応答。CheckedOutBy・CheckedOutAt は操作した後の印（外したら無い）。
type SyncCheckoutResponse struct {
	Action       string     `json:"action"`
	ArticleID    string     `json:"article_id"`
	CheckedOutBy string     `json:"checked_out_by,omitempty"`
	CheckedOutAt *time.Time `json:"checked_out_at,omitempty"`
	// CheckedOutClient は印を付けた PC の sorus の識別子（SyncRequest.ClientID）。
	CheckedOutClient string    `json:"checked_out_client,omitempty"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// SyncResolveProposalResponse は resolve_proposal の応答。Article は操作した後の記事（破棄では変わらない）。
type SyncResolveProposalResponse struct {
	Action         string             `json:"action"`
	ProposalAction string             `json:"proposal_action"`
	Article        SyncPendingArticle `json:"article"`
}

// SyncPullArticleResponse は pull_article の応答。Article は記事（有効な編集中の印と確認待ちの変更案を添える）、
// Images は記事の本文・アイキャッチが参照する画像のうち、公開サーバーの uploads/ にあるファイル名（サイズ違いを含む。
// 無ければ空配列）。PC は /uploads/<名前> から取得する。
type SyncPullArticleResponse struct {
	Action  string             `json:"action"`
	Article SyncPendingArticle `json:"article"`
	Images  []string           `json:"images"`
}

// SyncPendingResponse は fetch_pending の応答。Articles は更新日時の古い順で、該当が無ければ空配列。
type SyncPendingResponse struct {
	Action   string               `json:"action"`
	Articles []SyncPendingArticle `json:"articles"`
	// Scheduled は予約中の記事（状態が公開で、公開日時が未来）。公開日時の早い順で、該当が無ければ空配列。
	// 校閲待ちとは別の項目にし、予約を知らない古いローカル CMS には校閲待ちとして出さない。予約を知らない
	// 古い公開サーバーの応答には無く、読んだ側では nil になる。
	Scheduled []SyncPendingArticle `json:"scheduled"`
	// Drafts は下書きの記事（更新日時の古い順）。公開サーバーで書かれた下書きを PC で続きから書けるようにする。
	// 知らない古い公開サーバーの応答には無い。
	Drafts []SyncPendingArticle `json:"drafts,omitempty"`
}

// SyncPublishResponse は publish_article・schedule_article・unschedule_article の応答。Article は操作した後の記事。
type SyncPublishResponse struct {
	Action  string         `json:"action"`
	Article SyncArticleRef `json:"article"`
}

// SyncServerInfoResponse は server_info の応答。公開サーバーが対応している機能を表す。
type SyncServerInfoResponse struct {
	Action string `json:"action"`
	// Schedule は schedule_article・unschedule_article と、fetch_pending の Scheduled に対応しているか。
	Schedule bool `json:"schedule"`
	// CustomThemes は sync_settings の ThemePackage（独自テーマのファイルの受け取り）に対応しているか。
	CustomThemes bool `json:"custom_themes,omitempty"`
	// MaxThemePackageBytes は ThemePackage の Archive として受け付ける最大バイト数。
	MaxThemePackageBytes int64 `json:"max_theme_package_bytes,omitempty"`
	// Attachments は sync_attachments（記事の添付資料の受け取り）に対応しているか。MaxAttachmentBytes は1件の添付資料
	// として受け付ける最大バイト数。
	Attachments        bool  `json:"attachments,omitempty"`
	MaxAttachmentBytes int64 `json:"max_attachment_bytes,omitempty"`
	// AISwitch は sync_settings の AIEnabled・RevokeAllMCP（AI 機能の ON/OFF の同期）に対応しているか。
	AISwitch bool `json:"ai_switch,omitempty"`
}

// SyncSiteAccessResponse は get_site_access・set_site_access の応答。公開サーバー自身の閲覧制限の状態を表す。
// パスワードとハッシュは含まない。
type SyncSiteAccessResponse struct {
	Action string `json:"action"`
	// Enabled・User は保存済みの設定。PasswordSet は閲覧用のパスワードを設定済みか。
	Enabled     bool   `json:"enabled"`
	User        string `json:"user"`
	PasswordSet bool   `json:"password_set"`
	// Active は公開サーバーが実際に公開サイトでパスワードを求めているか（サーバーのメモリ上の設定）。
	Active bool `json:"active"`
	// Revision は設定の版。set_site_access の ExpectedRevision に渡す。一度も変えていなければ空。
	Revision string `json:"revision"`
	// SiteURL は公開サーバーに設定されたサイトの URL（BaseURL）。未設定なら空。PC が外部から保護を確かめる先に使う。
	// PC は公開サーバーの応答として扱い、インターネットに公開されたアドレスにだけ接続する。
	SiteURL string `json:"site_url"`
	// CheckPaths は、PC がトップページに加えてパスワードなしで開いてみる公開サイトのパス（"/" から始まる）。
	// CDN などが閲覧制限を有効にする前の応答を保存していそうな、フィード・最新の記事・公開中の画像を並べる。
	CheckPaths []string `json:"check_paths,omitempty"`
	// Changed は set_site_access で設定が変わったか（同じ内容の保存なら false）。
	Changed bool `json:"changed"`
	// InsecureTransport は、この要求が公開サーバーの sorus に TLS で直接届いていないか。true なら、同じ接続では
	// set_site_access を受け付けない（site_access_insecure）。PC は保存のフォームを使えなくし、理由を表示する。
	// 前段のプロキシで TLS を終える構成や、平文の http で接続したときに true になる。
	InsecureTransport bool `json:"insecure_transport,omitempty"`
}

// SyncSettingsValues は更新後のサイト基本設定とサイト構成。
type SyncSettingsValues struct {
	SiteName        string `json:"site_name"`
	SiteDescription string `json:"site_description"`
	BaseURL         string `json:"base_url"`
	Copyright       string `json:"copyright"`
	AnalyticsTag    string `json:"analytics_tag"`
	Theme           string `json:"theme"`
	// SiteMenu・FrontCards は空でも空の配列で返す。サイト構成を知らない古い公開サーバーの応答には無く、
	// 読んだ側では nil になる（ローカル CMS は反映されなかったことを利用者に伝える）。
	SiteMenu []MenuItem `json:"site_menu"`
	// SiteFooterMenu は反映後のフッターのサイトマップ（空でも空の配列）。メニューの子項目とフッターのサイトマップを
	// 知らない古い公開サーバーの応答には無い（nil）。
	SiteFooterMenu  *[]MenuItem `json:"site_footer_menu,omitempty"`
	FrontPage       string      `json:"front_page"`
	FrontCards      []string    `json:"front_cards"`
	TopGalleryTag   string      `json:"top_gallery_tag"`
	TopGalleryLimit int         `json:"top_gallery_limit"`
	TopGalleryTitle string      `json:"top_gallery_title"`
	// TopGalleryLayout・TopGalleryLinkURL・TopGalleryLinkLabel は反映後のフォトギャラリーの並べ方と「すべて見る」リンク。
	// これらを知らない古い公開サーバーの応答には無い（nil）。
	TopGalleryLayout    *string `json:"top_gallery_layout,omitempty"`
	TopGalleryLinkURL   *string `json:"top_gallery_link_url,omitempty"`
	TopGalleryLinkLabel *string `json:"top_gallery_link_label,omitempty"`
	// MultilingualEnabled は反映後の多言語対応の ON/OFF。多言語対応の設定を知らない古い公開サーバーの応答には無い（nil）。
	MultilingualEnabled *bool `json:"multilingual_enabled,omitempty"`
	// ShowAuthor は記事や一覧に執筆者名を表示するか。この項目を知らない古い公開サーバーの応答には無く、
	// 読んだ側では nil になる。
	ShowAuthor *bool `json:"show_author,omitempty"`
	// AIEnabled は反映後に公開サーバーで AI 機能（外部の AI アプリとの連携を含む）が使えるか（環境変数の強制 OFF を
	// 含めた実際の状態）。AI 機能の同期を知らない古い公開サーバーの応答には無い（nil）。AIForcedOff は公開サーバーの
	// 環境変数 SORUS_AI=off で OFF に固定されているか（PC で ON にしても公開サーバーでは OFF のまま）。
	AIEnabled   *bool `json:"ai_enabled,omitempty"`
	AIForcedOff bool  `json:"ai_forced_off,omitempty"`
}

// SyncSettingsResponse は sync_settings の応答。
type SyncSettingsResponse struct {
	Action   string             `json:"action"`
	Settings SyncSettingsValues `json:"settings"`
	// RevokedMCPTokens・RevokedOAuthApps は RevokeAllMCP で取り消した API トークンと AI アプリの連携の数。
	RevokedMCPTokens int `json:"revoked_mcp_tokens,omitempty"`
	RevokedOAuthApps int `json:"revoked_oauth_apps,omitempty"`
	// ThemePackage は要求の ThemePackage を受け取って配置したテーマ。付けなかった要求の応答には無い。
	ThemePackage *SyncThemePackageResult `json:"theme_package,omitempty"`
}

// SyncIssueInviteResponse は issue_invite の応答。InviteCode は発行したトークンと同期 API の URL を束ねた招待コードで、
// この応答でしか受け取れない（公開サーバーはトークン自体を保存しない）。
type SyncIssueInviteResponse struct {
	Action     string    `json:"action"`
	InviteCode string    `json:"invite_code"`
	JTI        string    `json:"jti"`
	Subject    string    `json:"sub"`
	Role       auth.Role `json:"role"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// 発行の経路（SyncTokenRecord.IssuedVia）。
const (
	// TokenIssuedViaCLI は公開サーバーで sorus token create を実行して発行したトークン。
	TokenIssuedViaCLI = "cli"
	// TokenIssuedViaAPI は同期 API の issue_invite で発行したトークン。
	TokenIssuedViaAPI = "api"
	// TokenIssuedViaSeen は発行の記録が無く、初めて使われたときに記録したトークン。
	TokenIssuedViaSeen = "seen"
)

// SyncTokenRecord は公開サーバーが記録している発行済みのトークン1件。トークン自体は含まない。
type SyncTokenRecord struct {
	JTI       string     `db:"jti" json:"jti"`
	Subject   string     `db:"subject" json:"sub"`
	Role      auth.Role  `db:"role" json:"role"`
	IssuedAt  time.Time  `db:"issued_at" json:"issued_at"`
	ExpiresAt time.Time  `db:"expires_at" json:"expires_at"`
	RevokedAt *time.Time `db:"revoked_at" json:"revoked_at,omitempty"`
	// IssuedVia は発行の経路（TokenIssuedViaCLI・TokenIssuedViaAPI・TokenIssuedViaSeen）。
	IssuedVia string `db:"issued_via" json:"issued_via"`
	// IssuedBy は issue_invite で発行した管理者のトークンの sub。それ以外の経路では空。
	IssuedBy string `db:"issued_by" json:"issued_by"`
	// Current はこの要求に使ったトークンか。
	Current bool `db:"-" json:"current"`
}

// SyncListTokensResponse は list_tokens の応答。Tokens は発行日時の新しい順で、該当が無ければ空配列。
// CurrentJTI はこの要求に使ったトークンの ID。
type SyncListTokensResponse struct {
	Action     string            `json:"action"`
	Tokens     []SyncTokenRecord `json:"tokens"`
	CurrentJTI string            `json:"current_jti"`
}

// SyncRevokeTokenResponse は revoke_token の応答。Token は取り消した後の記録。
type SyncRevokeTokenResponse struct {
	Action string          `json:"action"`
	Token  SyncTokenRecord `json:"token"`
}

// 招待コード。同期 API の URL とトークンを1つの文字列に束ね、
// 参加する人が「公開サーバー」の画面に貼るだけで接続できるようにする。
// 形式は InviteCodePrefix に続けて、JSON {"v":1,"url":"<同期 API の URL>","token":"<トークン>"} の
// パディングなしの Base64URL。

// InviteJoinHint は招待コードを受け取った人への案内。画面・エラーの文言・sorus token create・
// deploy/install-server.sh で同じ言い方にする（メニューの名前は i18n の remote_sync）。
const InviteJoinHint = "sorus の「設定」→「公開先」で「公開サーバーに接続する」を開き、招待コードを貼り付けてください"

// InviteCodePrefix は招待コードの先頭に付ける文字列。
const InviteCodePrefix = "sorus-invite:"

// inviteCodeVersion は招待コードの形式の版。
const inviteCodeVersion = 1

// maxInviteCodeLength は読み取りを受け付ける招待コードの長さ。正規の招待コードは1KB 程度に収まる。
const maxInviteCodeLength = 8192

// ErrInvalidInviteCode は招待コードの形式が正しくないことを示す。Error の文言は利用者にそのまま見せる。
var ErrInvalidInviteCode = errors.New("招待コードの形式が正しくありません。受け取った sorus-invite: で始まる文字列を、全部そのまま貼り付けてください")

// InviteCode は招待コードの中身。
type InviteCode struct {
	V int `json:"v"`
	// URL は同期 API の完全な URL（例: https://media.example.jp/api/sync-<秘匿パス>）。
	URL   string `json:"url"`
	Token string `json:"token"`
}

// EncodeInviteCode は同期 API の URL syncURL とトークン token から招待コードを作る。読み取り（DecodeInviteCode）と
// 同じ検査をし、参加できない招待コード（http:// の公開サーバーなど）は作らずにエラーを返す。
func EncodeInviteCode(syncURL, token string) (string, error) {
	inv := InviteCode{V: inviteCodeVersion, URL: strings.TrimSpace(syncURL), Token: strings.TrimSpace(token)}
	if err := inv.validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(inv)
	if err != nil {
		return "", fmt.Errorf("server: invite code: %w", err)
	}
	return InviteCodePrefix + base64.RawURLEncoding.EncodeToString(data), nil
}

// DecodeInviteCode は招待コードを読み、中身を検査して返す。前後の空白・改行と、メールなどで途中に入った改行は無視する。
// 形式・版・URL（https、http はこの PC 内のサーバーだけ）・トークンの形式のどれかが正しくなければエラーを返す。
func DecodeInviteCode(raw string) (*InviteCode, error) {
	s := strings.Join(strings.Fields(raw), "")
	if len(s) > maxInviteCodeLength {
		return nil, ErrInvalidInviteCode
	}
	body, ok := strings.CutPrefix(s, InviteCodePrefix)
	if !ok {
		return nil, ErrInvalidInviteCode
	}
	data, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, ErrInvalidInviteCode
	}
	var inv InviteCode
	if err := json.Unmarshal(data, &inv); err != nil {
		return nil, ErrInvalidInviteCode
	}
	if inv.V != inviteCodeVersion {
		return nil, errors.New("この招待コードは、お使いの sorus より新しい版で作られています。sorus を更新してから貼り付けてください")
	}
	if err := inv.validate(); err != nil {
		return nil, err
	}
	return &inv, nil
}

// validate は招待コードの URL とトークンを、公開サーバーの設定と同じ規則（normalizeRemoteURL・parseRemoteToken）で検査する。
func (inv InviteCode) validate() error {
	if _, _, err := normalizeRemoteURL(inv.URL); err != nil {
		return fmt.Errorf("招待コードの公開サーバーの URL が正しくありません: %w", err)
	}
	if _, err := parseRemoteToken(inv.Token, time.Now()); err != nil {
		return ErrInvalidInviteCode
	}
	return nil
}
