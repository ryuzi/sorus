package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"sorus/internal/article"
	"sorus/internal/auth"
)

// 変更案（Proposal）は、AI アプリ（MCP）から既存の記事の内容を置き換えようとしたときに、記事を変えずに
// 預かっておく案。AI アプリの自動承認では人が確かめたことを保証できないため、案は sorus の編集画面で
// 人が差分を確かめ、編集欄に反映してから普段どおり保存したときにだけ記事に入る（internal/server）。
//
// 案は settings テーブルに記事ごとに1件だけ置く（キー ProposalKeyPrefix + 記事 ID、値は Proposal の JSON）。
// 同じ記事に新しい案が来たら前の案を置き換える。案には公開前の本文が含まれるため、公開サイト・静的出力・
// 公開サーバーとの同期・ログには出さない（settings は決まったキーだけを読み書きする）。

// ProposalKeyPrefix は変更案を置く settings のキーの接頭辞。
const ProposalKeyPrefix = "article_proposal:"

// 変更案の種類。
const (
	// ProposalEdit は cms_save_draft による既存の記事のタイトル・本文・抜粋などの変更。
	ProposalEdit = "edit"
	// ProposalTranslation は cms_create_translation による既存の翻訳の置き換え。
	ProposalTranslation = "translation"
	// ProposalTranslationSync は cms_sync_translation による原文に合わせた再翻訳。
	ProposalTranslationSync = "translation_sync"
)

// Proposal は記事 ArticleID への変更案。Changes には AI アプリが指定した項目のうち、案を作ったときの記事と
// 違っていたものだけを入れる。反映するときは、その時点の記事に Changes を重ねる（指定していない項目を
// 案を作ったときの古い値に戻さない）。
type Proposal struct {
	ID         string    `json:"id"`
	ArticleID  string    `json:"article_id"`
	Kind       string    `json:"kind"`
	ProposedBy string    `json:"proposed_by"`
	CreatedAt  time.Time `json:"created_at"`
	// SourceArticleID は翻訳の案で使った原文の記事 ID。
	SourceArticleID string `json:"source_article_id,omitempty"`
	// SourceBase は翻訳の案で訳した原文の印（SourceFingerprint）。案を作った後に原文が変わったかを、
	// 反映する前に sorus の画面で確かめるのに使う。
	SourceBase string `json:"source_base,omitempty"`
	// SourceLang は翻訳の案で訳した原文の言語。これが無い案は、言語を含めずに印を作っていた古い案
	// （原文の言語が変わったかを確かめられない）。
	SourceLang string          `json:"source_lang,omitempty"`
	Changes    ProposalChanges `json:"changes"`
}

// IsTranslation は案が原文を訳した翻訳の案（cms_create_translation・cms_sync_translation）かを返す。
func (p *Proposal) IsTranslation() bool {
	return p.Kind == ProposalTranslation || p.Kind == ProposalTranslationSync
}

// SourceFingerprint は翻訳で訳す原文（記事 ID・言語・タイトル・本文）の印を返す。翻訳の案を作るときに訳した原文の印を残し、
// 反映する前に今の原文の印と比べる（同じ秒の変更も見分けられるよう、内容そのものから作る）。
func SourceFingerprint(src *article.Article) string {
	h := sha256.New()
	for _, v := range []string{src.ID, src.Lang, src.Title, src.ContentMarkdown} {
		h.Write([]byte(v))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ProposalChanges は変更案で変える項目。nil の項目は変えない。
type ProposalChanges struct {
	Title              *string       `json:"title,omitempty"`
	ContentMarkdown    *string       `json:"content_markdown,omitempty"`
	Excerpt            *string       `json:"excerpt,omitempty"`
	Slug               *string       `json:"slug,omitempty"`
	CategoryID         *int64        `json:"category_id,omitempty"`
	Kind               *article.Kind `json:"kind,omitempty"`
	Lang               *string       `json:"lang,omitempty"`
	TranslationGroupID *string       `json:"translation_group_id,omitempty"`
	SourceArticleID    *string       `json:"source_article_id,omitempty"`
}

// ReplacesContent は案が記事の文章（タイトル・本文・抜粋）を変えるかを返す。
func (c ProposalChanges) ReplacesContent() bool {
	return c.Title != nil || c.ContentMarkdown != nil || c.Excerpt != nil
}

// Empty は変える項目が1つも無いかを返す。
func (c ProposalChanges) Empty() bool {
	return !c.ReplacesContent() && c.Slug == nil && c.CategoryID == nil && c.Kind == nil &&
		c.Lang == nil && c.TranslationGroupID == nil && c.SourceArticleID == nil
}

// Fields は変える項目の名前（MCP の引数名）を返す。
func (c ProposalChanges) Fields() []string {
	var out []string
	add := func(ok bool, name string) {
		if ok {
			out = append(out, name)
		}
	}
	add(c.Title != nil, "title")
	add(c.ContentMarkdown != nil, "content_markdown")
	add(c.Excerpt != nil, "excerpt")
	add(c.Slug != nil, "slug")
	add(c.CategoryID != nil, "category_id")
	add(c.Kind != nil, "kind")
	add(c.Lang != nil, "lang")
	add(c.TranslationGroupID != nil, "translation_group_id")
	add(c.SourceArticleID != nil, "source_article_id")
	return out
}

// Apply は a に案を重ねた記事を返す（a は変えない）。
func (c ProposalChanges) Apply(a article.Article) article.Article {
	if c.Title != nil {
		a.Title = *c.Title
	}
	if c.ContentMarkdown != nil {
		a.ContentMarkdown = *c.ContentMarkdown
	}
	if c.Excerpt != nil {
		a.Excerpt = *c.Excerpt
	}
	if c.Slug != nil {
		a.Slug = *c.Slug
	}
	if c.CategoryID != nil {
		a.CategoryID = *c.CategoryID
	}
	if c.Kind != nil {
		a.Kind = *c.Kind
	}
	if c.Lang != nil {
		a.Lang = *c.Lang
	}
	if c.TranslationGroupID != nil {
		a.TranslationGroupID = *c.TranslationGroupID
	}
	if c.SourceArticleID != nil {
		a.SourceArticleID = sql.NullString{String: *c.SourceArticleID, Valid: true}
	}
	return a
}

// LoadProposal は記事 articleID の変更案を返す。案が無ければ nil, nil を返す。
func LoadProposal(ctx context.Context, db sqlx.QueryerContext, articleID string) (*Proposal, error) {
	var raw string
	err := sqlx.GetContext(ctx, db, &raw, `SELECT value FROM settings WHERE key = ?`, ProposalKeyPrefix+articleID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mcp: load proposal: %w", err)
	}
	var p Proposal
	if err := json.Unmarshal([]byte(raw), &p); err != nil || p.ID == "" || p.ArticleID != articleID {
		// 読めない値は案が無いものとして扱う（中身はログに出さない）。
		return nil, nil
	}
	return &p, nil
}

// DeleteProposal は記事 articleID の変更案が proposalID のときだけ消し、消したかを返す。確認した後に新しい案に
// 置き換わっていれば消さない（新しい案はもう一度確かめてもらう）。
func DeleteProposal(ctx context.Context, db sqlx.ExecerContext, articleID, proposalID string) (bool, error) {
	if proposalID == "" {
		return false, nil
	}
	res, err := db.ExecContext(ctx, `DELETE FROM settings WHERE key = ? AND json_extract(value, '$.id') = ?`,
		ProposalKeyPrefix+articleID, proposalID)
	if err != nil {
		return false, fmt.Errorf("mcp: delete proposal: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mcp: delete proposal: %w", err)
	}
	return n > 0, nil
}

// saveProposal は p を記事の変更案として保存し、前の案を置き換えたかを返す。p.ID と p.CreatedAt はここで決める。
func saveProposal(ctx context.Context, db *sqlx.DB, p *Proposal) (replaced bool, err error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return false, fmt.Errorf("mcp: proposal id: %w", err)
	}
	p.ID = hex.EncodeToString(b)
	p.CreatedAt = time.Now().UTC().Truncate(time.Second)
	value, err := json.Marshal(p)
	if err != nil {
		return false, fmt.Errorf("mcp: encode proposal: %w", err)
	}
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("mcp: save proposal: %w", err)
	}
	defer tx.Rollback()
	var n int
	if err := tx.GetContext(ctx, &n, `SELECT COUNT(*) FROM settings WHERE key = ?`, ProposalKeyPrefix+p.ArticleID); err != nil {
		return false, fmt.Errorf("mcp: save proposal: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`, ProposalKeyPrefix+p.ArticleID, string(value)); err != nil {
		return false, fmt.Errorf("mcp: save proposal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("mcp: save proposal: %w", err)
	}
	return n > 0, nil
}

// proposalSummary は MCP の結果に返す変更案の概要。本文などの中身は返さない。
type proposalSummary struct {
	ID            string    `json:"id"`
	Kind          string    `json:"kind"`
	ProposedBy    string    `json:"proposed_by"`
	CreatedAt     time.Time `json:"created_at"`
	ChangedFields []string  `json:"changed_fields"`
	// ReplacedPrevious は同じ記事の前の案を置き換えたか（stageProposal の結果だけ）。
	ReplacedPrevious bool `json:"replaced_previous,omitempty"`
	// ConfirmPath・ConfirmURL は人が差分を確かめる sorus の編集画面。管理画面の無い公開サーバー（PublicOnly）では空。
	ConfirmPath string `json:"confirm_path,omitempty"`
	ConfirmURL  string `json:"confirm_url,omitempty"`
}

// proposalAnchor は編集画面で変更案の確認を開く位置（templates/admin/article_edit.html）。
const proposalAnchor = "#article-proposal"

// summarize は p の概要を返す。
func (s *Server) summarize(ctx context.Context, p *Proposal) (*proposalSummary, error) {
	var name string
	err := s.db.GetContext(ctx, &name, `SELECT COALESCE(NULLIF(display_name, ''), username, '') FROM users WHERE id = ?`, p.ProposedBy)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("mcp: proposal author: %w", err)
	}
	if name == "" {
		name = "不明"
	}
	summary := &proposalSummary{
		ID: p.ID, Kind: p.Kind, ProposedBy: name, CreatedAt: p.CreatedAt, ChangedFields: p.Changes.Fields(),
	}
	if path, url := s.editLinks(p.ArticleID); path != "" {
		summary.ConfirmPath, summary.ConfirmURL = path+proposalAnchor, url+proposalAnchor
	}
	return summary, nil
}

// pendingProposal は記事 id の確認待ちの変更案の概要を返す。無ければ nil。
func (s *Server) pendingProposal(ctx context.Context, id string) (*proposalSummary, error) {
	p, err := LoadProposal(ctx, s.db, id)
	if err != nil || p == nil {
		return nil, err
	}
	return s.summarize(ctx, p)
}

// stageProposal は記事 a への変更 changes を、記事を変えずに変更案として預ける。他のユーザーが編集中なら預からない。
// 翻訳の案なら、訳した原文 src（AI に渡した時点の原文）とその印を残す。翻訳でなければ src は nil。
func (s *Server) stageProposal(ctx context.Context, user *auth.User, a *article.Article, kind string, src *article.Article, changes ProposalChanges) (*proposalSummary, error) {
	if err := s.checkNotLockedByOther(ctx, user, a.ID); err != nil {
		return nil, err
	}
	// 呼び出し側が読んだ後に PC の sorus で編集中になっていることがあるため、案を預ける直前に読み直して確かめる。
	if err := s.recheckNotCheckedOut(ctx, a.ID); err != nil {
		return nil, err
	}
	p := &Proposal{ArticleID: a.ID, Kind: kind, ProposedBy: user.ID, Changes: changes}
	if src != nil {
		p.SourceArticleID, p.SourceBase, p.SourceLang = src.ID, SourceFingerprint(src), src.Lang
	}
	replaced, err := saveProposal(ctx, s.db, p)
	if err != nil {
		return nil, err
	}
	summary, err := s.summarize(ctx, p)
	if err != nil {
		return nil, err
	}
	summary.ReplacedPrevious = replaced
	return summary, nil
}

// proposalMessage は変更案を預かったときに AI エージェントへ返す説明。
// 管理画面の無い公開サーバー（PublicOnly）では confirm_url を返さないため、PC の sorus の「届いた原稿」へ案内する。
func (s *Server) proposalMessage(p *proposalSummary) string {
	msg := "既存の記事の内容を置き換える変更のため、記事はまだ変えていません。変更案として預かりました。" +
		"sorus の編集画面（confirm_url）で人が差分を確かめ、編集欄に反映して保存したときに記事に入ります。" +
		"AI アプリの側で承認しても反映されません。利用者に confirm_url を開いて確認するよう伝えてください。"
	if s.cfg.PublicOnly {
		msg = "既存の記事の内容を置き換える変更のため、記事はまだ変えていません。変更案として預かりました。" +
			"PC の sorus の「届いた原稿」で人が差分を確かめて取り込んだときに記事に入ります。" +
			"AI アプリの側で承認しても反映されません。利用者に、PC の sorus の「届いた原稿」で確認するよう伝えてください。"
	}
	if p.ReplacedPrevious {
		msg += "この記事にあった前の変更案は、この案に置き換えました。"
	} else {
		msg += "同じ記事に新しい案を送ると、この案は置き換わります。"
	}
	return msg
}
