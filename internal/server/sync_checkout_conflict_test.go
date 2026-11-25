package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/mcp"
)

// getArticle は記事 id を読み直す。
func (e *syncEnv) getArticle(id string) *article.Article {
	e.t.Helper()
	a, err := e.server.articles.GetByID(id)
	if err != nil {
		e.t.Fatalf("GetByID(%s): %v", id, err)
	}
	return a
}

// seedProposal は記事 articleID に変更案を直接置き、案の ID を返す。
func (e *syncEnv) seedProposal(articleID, proposalID string, changes mcp.ProposalChanges) string {
	e.t.Helper()
	p := mcp.Proposal{ID: proposalID, ArticleID: articleID, Kind: mcp.ProposalEdit, ProposedBy: studentA.ID,
		CreatedAt: time.Now().UTC().Truncate(time.Second), Changes: changes}
	b, err := json.Marshal(p)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, mcp.ProposalKeyPrefix+articleID, string(b)); err != nil {
		e.t.Fatalf("seed proposal: %v", err)
	}
	return proposalID
}

func (e *syncEnv) hasProposal(articleID string) bool {
	e.t.Helper()
	p, err := mcp.LoadProposal(e.t.Context(), e.db, articleID)
	if err != nil {
		e.t.Fatalf("LoadProposal: %v", err)
	}
	return p != nil
}

func findPending(list []SyncPendingArticle, id string) *SyncPendingArticle {
	for i := range list {
		if list[i].ID == id {
			return &list[i]
		}
	}
	return nil
}

func TestSyncFetchPendingDraftsCheckoutAndProposal(t *testing.T) {
	env := newSyncEnv(t)
	editor := env.token("desk", auth.RoleEditor)

	older := seedArticle(t, env.db, article.Article{Title: "古い下書き", ContentMarkdown: "古い本文", Status: article.StatusDraft})
	newer := seedArticle(t, env.db, article.Article{Title: "新しい下書き", Status: article.StatusDraft})
	review := env.seedReview("校閲待ち", "review")
	seedArticle(t, env.db, article.Article{Title: "公開中", Status: article.StatusPublished})
	if _, err := env.db.Exec(`UPDATE articles SET updated_at = ? WHERE id = ?`, time.Now().UTC().Add(-time.Hour), older); err != nil {
		t.Fatal(err)
	}
	// 有効な印（older）と、期限の切れた印（newer）。
	if err := env.server.articles.SetCheckout(t.Context(), older, "佐藤", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := env.server.articles.SetCheckout(t.Context(), newer, "鈴木", "", time.Now().Add(-article.CheckoutTTL-time.Minute)); err != nil {
		t.Fatal(err)
	}
	env.seedProposal(older, "prop1", mcp.ProposalChanges{ContentMarkdown: strPtr("AI の本文")})

	rec := env.call(editor, SyncRequest{Action: SyncActionFetchPending})
	expectSyncStatus(t, rec, http.StatusOK)
	res := decodeSync[SyncPendingResponse](t, rec)
	if len(res.Drafts) != 2 || res.Drafts[0].ID != older || res.Drafts[1].ID != newer {
		t.Fatalf("drafts = %+v, want [older newer]", res.Drafts)
	}
	if len(res.Articles) != 1 || res.Articles[0].ID != review {
		t.Errorf("articles = %+v, want only the review article", res.Articles)
	}
	d := res.Drafts[0]
	if d.CheckedOutBy != "佐藤" || d.CheckedOutAt == nil {
		t.Errorf("active checkout not reported: %+v", d)
	}
	if res.Drafts[1].CheckedOutBy != "" || res.Drafts[1].CheckedOutAt != nil {
		t.Errorf("expired checkout must be hidden: %+v", res.Drafts[1])
	}
	pp := d.PendingProposal
	if pp == nil || pp.ID != "prop1" || pp.Kind != mcp.ProposalEdit || pp.ProposedBy != studentA.DisplayName ||
		!pp.HasContent || pp.HasTitle || pp.HasExcerpt || pp.ContentMarkdown != "AI の本文" {
		t.Errorf("pending proposal = %+v", pp)
	}
	if res.Drafts[1].PendingProposal != nil {
		t.Errorf("article without a proposal got %+v", res.Drafts[1].PendingProposal)
	}
}

func TestSyncCheckoutReleaseAndForce(t *testing.T) {
	env := newSyncEnv(t)
	sato := env.token("佐藤", auth.RoleEditor)
	suzuki := env.token("鈴木", auth.RoleEditor)
	writer := env.token("田中", auth.RoleWriter)
	id := env.seedReview("記事", "a")

	// writer は権限が無い。
	expectSyncStatus(t, env.call(writer, SyncRequest{Action: SyncActionCheckout, ArticleID: id}), http.StatusForbidden)
	expectSyncStatus(t, env.call(sato, SyncRequest{Action: SyncActionCheckout, ArticleID: "missing"}), http.StatusNotFound)

	before := env.getArticle(id).UpdatedAt
	rec := env.call(sato, SyncRequest{Action: SyncActionCheckout, ArticleID: id})
	expectSyncStatus(t, rec, http.StatusOK)
	res := decodeSync[SyncCheckoutResponse](t, rec)
	if res.CheckedOutBy != "佐藤" || res.CheckedOutAt == nil || !res.UpdatedAt.Equal(before) {
		t.Errorf("checkout response = %+v (updated_at before %v)", res, before)
	}
	if a := env.getArticle(id); !a.IsCheckedOut(time.Now()) || a.CheckedOutBy != "佐藤" || !a.UpdatedAt.Equal(before) {
		t.Errorf("article after checkout = by %q at %v updated %v", a.CheckedOutBy, a.CheckedOutAt, a.UpdatedAt)
	}
	// 同じ人は付け直せる。
	expectSyncStatus(t, env.call(sato, SyncRequest{Action: SyncActionCheckout, ArticleID: id}), http.StatusOK)

	// ほかの人は付けられず、外せず、提出・公開もできない。
	rec = env.call(suzuki, SyncRequest{Action: SyncActionCheckout, ArticleID: id})
	expectSyncStatus(t, rec, http.StatusConflict)
	if msg := syncErrorMessage(t, rec); !strings.Contains(msg, "佐藤さんが PC で編集中") {
		t.Errorf("message = %q", msg)
	}
	expectSyncStatus(t, env.call(suzuki, SyncRequest{Action: SyncActionReleaseCheckout, ArticleID: id}), http.StatusConflict)
	expectSyncStatus(t, env.call(suzuki, submitRequest(SyncArticle{ID: id, Title: "上書き"})), http.StatusConflict)
	expectSyncStatus(t, env.call(suzuki, publishRequest(id)), http.StatusConflict)
	expectSyncStatus(t, env.call(suzuki, SyncRequest{Action: SyncActionScheduleArticle, ArticleID: id,
		PublishAt: timePtr(time.Now().Add(time.Hour))}), http.StatusConflict)
	if a := env.getArticle(id); a.Title != "記事" || a.Status != article.StatusReview {
		t.Fatalf("article changed despite checkout: %q %s", a.Title, a.Status)
	}

	// 期限の切れた印は無いものとして扱う。
	if err := env.server.articles.SetCheckout(t.Context(), id, "佐藤", "", time.Now().Add(-article.CheckoutTTL-time.Second)); err != nil {
		t.Fatal(err)
	}
	expectSyncStatus(t, env.call(suzuki, SyncRequest{Action: SyncActionCheckout, ArticleID: id}), http.StatusOK)

	// force で奪える。
	rec = env.call(sato, SyncRequest{Action: SyncActionCheckout, ArticleID: id, Force: true})
	expectSyncStatus(t, rec, http.StatusOK)
	if got := env.getArticle(id).CheckedOutBy; got != "佐藤" {
		t.Errorf("forced checkout by = %q, want 佐藤", got)
	}

	// 本人は外せる。外した後の外す要求も成功する。
	expectSyncStatus(t, env.call(sato, SyncRequest{Action: SyncActionReleaseCheckout, ArticleID: id}), http.StatusOK)
	if a := env.getArticle(id); a.CheckedOutBy != "" || a.CheckedOutAt != nil {
		t.Errorf("checkout not released: %q %v", a.CheckedOutBy, a.CheckedOutAt)
	}
	expectSyncStatus(t, env.call(suzuki, SyncRequest{Action: SyncActionReleaseCheckout, ArticleID: id}), http.StatusOK)

	// force ならほかの人の印も外せる。
	if err := env.server.articles.SetCheckout(t.Context(), id, "佐藤", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	expectSyncStatus(t, env.call(suzuki, SyncRequest{Action: SyncActionReleaseCheckout, ArticleID: id, Force: true}), http.StatusOK)
	if env.getArticle(id).CheckedOutBy != "" {
		t.Error("forced release did not clear the checkout")
	}

	// force の提出はほかの人の印があっても通り、印を外す。
	if err := env.server.articles.SetCheckout(t.Context(), id, "佐藤", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	req := submitRequest(SyncArticle{ID: id, Title: "強制で提出"})
	req.Force = true
	expectSyncStatus(t, env.call(suzuki, req), http.StatusOK)
	if a := env.getArticle(id); a.Title != "強制で提出" || a.CheckedOutBy != "" {
		t.Errorf("forced submit: title %q checked_out_by %q", a.Title, a.CheckedOutBy)
	}
}

func TestSyncSubmitClearsCheckout(t *testing.T) {
	env := newSyncEnv(t)
	sato := env.token("佐藤", auth.RoleEditor)
	id := env.seedReview("記事", "a")
	other := env.seedReview("別の記事", "b")
	sched := env.seedReview("予約する記事", "c")

	for _, aid := range []string{id, other, sched} {
		expectSyncStatus(t, env.call(sato, SyncRequest{Action: SyncActionCheckout, ArticleID: aid}), http.StatusOK)
	}
	expectSyncStatus(t, env.call(sato, submitRequest(SyncArticle{ID: id, Title: "提出した", ContentMarkdown: "本文"})), http.StatusOK)
	if a := env.getArticle(id); a.Title != "提出した" || a.CheckedOutBy != "" || a.CheckedOutAt != nil {
		t.Errorf("after submit: title %q checkout %q %v", a.Title, a.CheckedOutBy, a.CheckedOutAt)
	}
	expectSyncStatus(t, env.call(sato, publishRequest(other)), http.StatusOK)
	if a := env.getArticle(other); a.Status != article.StatusPublished || a.CheckedOutBy != "" {
		t.Errorf("after publish: status %s checkout %q", a.Status, a.CheckedOutBy)
	}
	expectSyncStatus(t, env.call(sato, SyncRequest{Action: SyncActionScheduleArticle, ArticleID: sched,
		PublishAt: timePtr(time.Now().Add(time.Hour))}), http.StatusOK)
	if a := env.getArticle(sched); a.CheckedOutBy != "" {
		t.Errorf("after schedule: checkout %q", a.CheckedOutBy)
	}
}

func TestSyncSubmitConflictOnStaleBase(t *testing.T) {
	env := newSyncEnv(t)
	editor := env.token("desk", auth.RoleEditor)
	id := seedArticle(t, env.db, article.Article{Title: "サーバーの題", ContentMarkdown: "サーバーの本文",
		Status: article.StatusReview, AuthorID: studentA.ID})
	base := env.getArticle(id).UpdatedAt

	// 1秒以内のずれは同じ版とみなす。
	req := submitRequest(SyncArticle{ID: id, Title: "PC の題", ContentMarkdown: "PC の本文"})
	req.BaseUpdatedAt = timePtr(base.Add(500 * time.Millisecond))
	expectSyncStatus(t, env.call(editor, req), http.StatusOK)

	// PC が古い版から編集していたら 409 で、公開サーバーの今の内容を返す。
	cur := env.getArticle(id)
	stale := submitRequest(SyncArticle{ID: id, Title: "古い版からの題", ContentMarkdown: "古い版からの本文"})
	stale.BaseUpdatedAt = timePtr(cur.UpdatedAt.Add(-time.Hour))
	rec := env.call(editor, stale)
	expectSyncStatus(t, rec, http.StatusConflict)
	res := decodeSync[SyncConflictResponse](t, rec)
	if res.Code != SyncConflictCode || res.ArticleID != id || res.ServerTitle != "PC の題" ||
		res.ServerContentMarkdown != "PC の本文" || !res.ServerUpdatedAt.Equal(cur.UpdatedAt) || res.ServerAuthorName == "" {
		t.Errorf("conflict response = %+v (server updated %v)", res, cur.UpdatedAt)
	}
	if a := env.getArticle(id); a.Title != "PC の題" {
		t.Errorf("stale submit overwrote the article: %q", a.Title)
	}

	// force でも版の食い違いは無視しない。
	stale.Force = true
	expectSyncStatus(t, env.call(editor, stale), http.StatusConflict)

	// publish_article（記事付き）も同じく確かめる。
	pub := SyncRequest{Action: SyncActionPublishArticle, Article: &SyncArticle{ID: id, Title: "公開"},
		BaseUpdatedAt: timePtr(cur.UpdatedAt.Add(-time.Hour))}
	expectSyncStatus(t, env.call(editor, pub), http.StatusConflict)
	pub.BaseUpdatedAt = timePtr(cur.UpdatedAt)
	expectSyncStatus(t, env.call(editor, pub), http.StatusOK)
}

func TestSyncResolveProposal(t *testing.T) {
	env := newSyncEnv(t)
	editor := env.token("desk", auth.RoleEditor)
	writer := env.token("田中", auth.RoleWriter)
	id := seedArticle(t, env.db, article.Article{Title: "元の題", ContentMarkdown: "元の本文", Excerpt: "元の抜粋", Status: article.StatusDraft})

	resolve := func(action, proposalID string) SyncRequest {
		return SyncRequest{Action: SyncActionResolveProposal, ArticleID: id, ProposalAction: action, ProposalID: proposalID}
	}
	expectSyncStatus(t, env.call(writer, resolve(SyncProposalAccept, "")), http.StatusForbidden)
	expectSyncStatus(t, env.call(editor, resolve("merge", "")), http.StatusBadRequest)
	expectSyncStatus(t, env.call(editor, resolve(SyncProposalAccept, "")), http.StatusNotFound)

	// 破棄: 記事は変わらず、案だけが消える。
	env.seedProposal(id, "p1", mcp.ProposalChanges{Title: strPtr("AI の題")})
	expectSyncStatus(t, env.call(editor, resolve(SyncProposalDiscard, "other")), http.StatusConflict)
	if !env.hasProposal(id) {
		t.Fatal("proposal removed despite a stale proposal_id")
	}
	rec := env.call(editor, resolve(SyncProposalDiscard, "p1"))
	expectSyncStatus(t, rec, http.StatusOK)
	res := decodeSync[SyncResolveProposalResponse](t, rec)
	if res.ProposalAction != SyncProposalDiscard || res.Article.Title != "元の題" || res.Article.PendingProposal != nil {
		t.Errorf("discard response = %+v", res)
	}
	if env.hasProposal(id) || env.getArticle(id).Title != "元の題" {
		t.Error("discard must remove the proposal and keep the article")
	}

	// 取り込み: 案の項目だけを今の記事に重ねて保存し、版を残して案を消す。
	env.seedProposal(id, "p2", mcp.ProposalChanges{Title: strPtr("AI の題"), ContentMarkdown: strPtr("AI の本文")})
	rec = env.call(editor, resolve(SyncProposalAccept, "p2"))
	expectSyncStatus(t, rec, http.StatusOK)
	res = decodeSync[SyncResolveProposalResponse](t, rec)
	if res.ProposalAction != SyncProposalAccept || res.Article.ID != id || res.Article.Title != "AI の題" ||
		res.Article.ContentMarkdown != "AI の本文" || res.Article.Excerpt != "元の抜粋" || res.Article.PendingProposal != nil {
		t.Errorf("accept response = %+v", res.Article)
	}
	if a := env.getArticle(id); a.Title != "AI の題" || a.ContentMarkdown != "AI の本文" || a.Excerpt != "元の抜粋" ||
		a.Status != article.StatusDraft {
		t.Errorf("article after accept = %q %q %q %s", a.Title, a.ContentMarkdown, a.Excerpt, a.Status)
	}
	if env.hasProposal(id) {
		t.Error("accepted proposal must be removed")
	}
	var revs int
	if err := env.db.Get(&revs, `SELECT COUNT(*) FROM article_revisions WHERE article_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if revs == 0 {
		t.Error("accept must record a revision")
	}

	// ほかの人の編集中の印があれば、force を付けない限り取り込まない。
	env.seedProposal(id, "p3", mcp.ProposalChanges{Title: strPtr("次の題")})
	if err := env.server.articles.SetCheckout(t.Context(), id, "佐藤", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	expectSyncStatus(t, env.call(editor, resolve(SyncProposalAccept, "p3")), http.StatusConflict)
	req := resolve(SyncProposalAccept, "p3")
	req.Force = true
	expectSyncStatus(t, env.call(editor, req), http.StatusOK)
	if env.getArticle(id).Title != "次の題" {
		t.Error("forced accept did not apply the proposal")
	}
}

func timePtr(t time.Time) *time.Time { return &t }

// pull_article は writer のトークンでも、下書き・校閲待ちの記事1件を本文・印・公開サーバーにある参照画像とともに返す。
// 公開中・予約中・削除済みの記事は返さない。
func TestSyncPullArticle(t *testing.T) {
	env := newSyncEnv(t)
	writer := env.token("田中", auth.RoleWriter)

	const hash = "0123456789abcdef0123456789abcdef"
	const featHash = "fedcba9876543210fedcba9876543210"
	for _, name := range []string{hash + "-original.webp", hash + "-1200.webp", featHash + "-800.webp"} {
		if err := os.WriteFile(filepath.Join(env.server.cfg.UploadDir, name), []byte("webp"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	src := seedArticle(t, env.db, article.Article{Title: "原文", Status: article.StatusDraft, Lang: "ja"})
	draft := seedArticle(t, env.db, article.Article{
		Title: "英語の下書き", Slug: "en-draft", Excerpt: "抜粋", Kind: article.KindPage, Status: article.StatusDraft, Lang: "en",
		ContentMarkdown:    "![図](/uploads/" + hash + "-1200.webp)\n![無い](/uploads/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-800.webp)",
		FeaturedImage:      "/uploads/" + featHash + "-800.webp",
		TranslationGroupID: src, SourceArticleID: sql.NullString{String: src, Valid: true},
	})
	if err := env.server.articles.SetCheckout(t.Context(), draft, "佐藤", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	review := env.seedReview("校閲待ち", "review")

	rec := env.call(writer, SyncRequest{Action: SyncActionPullArticle, ArticleID: draft})
	expectSyncStatus(t, rec, http.StatusOK)
	res := decodeSync[SyncPullArticleResponse](t, rec)
	a := res.Article
	if res.Action != SyncActionPullArticle || a.ID != draft || a.Title != "英語の下書き" || a.Slug != "en-draft" ||
		a.Excerpt != "抜粋" || a.Kind != article.KindPage || a.Lang != "en" || a.TranslationGroupID != src ||
		a.SourceArticleID != src || a.Category == "" || a.UpdatedAt.IsZero() || !strings.Contains(a.ContentMarkdown, hash) {
		t.Errorf("pull_article draft = %+v", res)
	}
	if a.CheckedOutBy != "佐藤" || a.CheckedOutAt == nil {
		t.Errorf("checkout = %q %v", a.CheckedOutBy, a.CheckedOutAt)
	}
	wantImages := []string{hash + "-original.webp", hash + "-1200.webp", featHash + "-800.webp"}
	if strings.Join(res.Images, ",") != strings.Join(wantImages, ",") {
		t.Errorf("images = %v, want %v", res.Images, wantImages)
	}

	rec = env.call(writer, SyncRequest{Action: SyncActionPullArticle, ArticleID: review})
	expectSyncStatus(t, rec, http.StatusOK)
	if res := decodeSync[SyncPullArticleResponse](t, rec); res.Article.ID != review || res.Article.Status != article.StatusReview || res.Images == nil {
		t.Errorf("pull_article review = %+v", res)
	}

	published := seedArticle(t, env.db, article.Article{Title: "公開中", Status: article.StatusPublished})
	scheduled := seedArticle(t, env.db, article.Article{Title: "予約中", Status: article.StatusPublished, PublishedAt: timePtr(time.Now().Add(time.Hour))})
	for _, id := range []string{published, scheduled} {
		expectSyncStatus(t, env.call(writer, SyncRequest{Action: SyncActionPullArticle, ArticleID: id}), http.StatusConflict)
	}
	if _, err := env.db.Exec(`UPDATE articles SET status = ? WHERE id = ?`, article.StatusDeleted, review); err != nil {
		t.Fatal(err)
	}
	expectSyncStatus(t, env.call(writer, SyncRequest{Action: SyncActionPullArticle, ArticleID: review}), http.StatusNotFound)
	expectSyncStatus(t, env.call(writer, SyncRequest{Action: SyncActionPullArticle, ArticleID: "missing"}), http.StatusNotFound)
	expectSyncStatus(t, env.call(writer, SyncRequest{Action: SyncActionPullArticle}), http.StatusBadRequest)
}

// checkSyncCheckout は編集中の印を PC の識別子（client_id）で見分ける。同じ人・同じトークンでも別の PC なら断り、
// 識別子を送らない古い PC は、印にも識別子が無いときだけ名前で見分ける。
func TestCheckSyncCheckoutClientID(t *testing.T) {
	now := time.Now()
	at := now.Add(-time.Minute)
	mark := func(by, client string) *article.Article {
		return &article.Article{CheckedOutBy: by, CheckedOutClient: client, CheckedOutAt: &at}
	}
	cases := []struct {
		name     string
		a        *article.Article
		actor    string
		clientID string
		force    bool
		ok       bool
		message  string
	}{
		{"same PC", mark("佐藤", "pc-a"), "佐藤", "pc-a", false, true, ""},
		{"same actor, other PC", mark("佐藤", "pc-a"), "佐藤", "pc-b", false, false, "佐藤さんが別の PC で編集中のため変更できません"},
		{"same actor, legacy PC against client mark", mark("佐藤", "pc-a"), "佐藤", "", false, false, "佐藤さんが別の PC で編集中のため変更できません"},
		{"same actor, new PC against legacy mark", mark("佐藤", ""), "佐藤", "pc-a", false, false, "佐藤さんが別の PC で編集中のため変更できません"},
		{"legacy PC against legacy mark", mark("佐藤", ""), "佐藤", "", false, true, ""},
		{"other actor, same client id", mark("鈴木", "pc-a"), "佐藤", "pc-a", false, true, ""},
		{"other actor", mark("鈴木", "pc-a"), "佐藤", "pc-b", false, false, "鈴木さんが PC で編集中のため変更できません"},
		{"other actor, legacy", mark("鈴木", ""), "佐藤", "", false, false, "鈴木さんが PC で編集中のため変更できません"},
		{"force", mark("佐藤", "pc-a"), "佐藤", "pc-b", true, true, ""},
		{"no mark", &article.Article{}, "佐藤", "pc-b", false, true, ""},
	}
	for _, tc := range cases {
		err := checkSyncCheckout(tc.a, tc.actor, tc.clientID, tc.force, now)
		if tc.ok {
			if err != nil {
				t.Errorf("%s: err = %v, want nil", tc.name, err)
			}
			continue
		}
		var he *echo.HTTPError
		if !errors.As(err, &he) || he.Code != http.StatusConflict {
			t.Errorf("%s: err = %v, want 409", tc.name, err)
			continue
		}
		body, _ := he.Message.(map[string]any)
		if body["code"] != SyncCheckedOutCode || body["message"] != tc.message || body["checked_out_by"] != tc.a.CheckedOutBy {
			t.Errorf("%s: body = %v, want message %q", tc.name, body, tc.message)
		}
	}
}

// 同じ人・同じトークンを2台の PC（client_id が違う）で同時に使っても、編集中の印はそれを付けた PC のものになり、
// もう1台の PC からの提出・公開・予約・変更案の操作・印の外し・付け直しは、引き継ぐ（force）まで 409 で断る。
func TestSyncCheckoutSameTokenTwoClients(t *testing.T) {
	env := newSyncEnv(t)
	tok := env.token("佐藤", auth.RoleEditor)
	id := env.seedReview("記事", "two-pcs")
	const pcA, pcB = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	rec := env.call(tok, SyncRequest{Action: SyncActionCheckout, ArticleID: id, ClientID: pcA})
	expectSyncStatus(t, rec, http.StatusOK)
	if res := decodeSync[SyncCheckoutResponse](t, rec); res.CheckedOutBy != "佐藤" || res.CheckedOutClient != pcA {
		t.Fatalf("checkout = %+v, want 佐藤 on %s", res, pcA)
	}
	if a := env.getArticle(id); a.CheckedOutClient != pcA {
		t.Fatalf("stored client = %q, want %s", a.CheckedOutClient, pcA)
	}

	// fetch_pending・pull_article は印を付けた PC の識別子を返す。
	pending := decodeSync[SyncPendingResponse](t, env.call(tok, SyncRequest{Action: SyncActionFetchPending}))
	if p := findPending(pending.Articles, id); p == nil || p.CheckedOutBy != "佐藤" || p.CheckedOutClient != pcA {
		t.Errorf("fetch_pending item = %+v, want client %s", p, pcA)
	}
	pulled := decodeSync[SyncPullArticleResponse](t, env.call(tok, SyncRequest{Action: SyncActionPullArticle, ArticleID: id}))
	if pulled.Article.CheckedOutClient != pcA {
		t.Errorf("pull_article client = %q, want %s", pulled.Article.CheckedOutClient, pcA)
	}

	// 同じトークンでも PC B からの操作はすべて断る（識別子を送らない古い PC も同じ）。
	env.seedProposal(id, "p1", mcp.ProposalChanges{Title: strPtr("AI の題")})
	submit := submitRequest(SyncArticle{ID: id, Title: "PC B の版"})
	submit.ClientID = pcB
	publish := publishRequest(id)
	publish.ClientID = pcB
	for name, req := range map[string]SyncRequest{
		"checkout":         {Action: SyncActionCheckout, ArticleID: id, ClientID: pcB},
		"release_checkout": {Action: SyncActionReleaseCheckout, ArticleID: id, ClientID: pcB},
		"submit_draft":     submit,
		"publish_article":  publish,
		"schedule_article": {Action: SyncActionScheduleArticle, ArticleID: id, ClientID: pcB, PublishAt: timePtr(time.Now().Add(time.Hour))},
		"resolve_proposal": {Action: SyncActionResolveProposal, ArticleID: id, ClientID: pcB, ProposalAction: SyncProposalDiscard},
		"legacy checkout":  {Action: SyncActionCheckout, ArticleID: id},
	} {
		rec := env.call(tok, req)
		expectSyncStatus(t, rec, http.StatusConflict)
		var body struct {
			Code         string `json:"code"`
			Message      string `json:"message"`
			CheckedOutBy string `json:"checked_out_by"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		if body.Code != SyncCheckedOutCode || body.CheckedOutBy != "佐藤" || body.Message != "佐藤さんが別の PC で編集中のため変更できません" {
			t.Errorf("%s: 409 body = %+v", name, body)
		}
	}
	if a := env.getArticle(id); a.Title != "記事" || a.Status != article.StatusReview || a.CheckedOutClient != pcA || !env.hasProposal(id) {
		t.Fatalf("article changed by PC B: title %q status %s client %q proposal %v", a.Title, a.Status, a.CheckedOutClient, env.hasProposal(id))
	}

	// PC A 自身は付け直せ、変更案も扱える。
	expectSyncStatus(t, env.call(tok, SyncRequest{Action: SyncActionCheckout, ArticleID: id, ClientID: pcA}), http.StatusOK)
	expectSyncStatus(t, env.call(tok, SyncRequest{Action: SyncActionResolveProposal, ArticleID: id, ClientID: pcA,
		ProposalAction: SyncProposalDiscard}), http.StatusOK)

	// PC B が引き継ぐ（force）と、印は PC B のものになり、今度は PC A が断られる。
	expectSyncStatus(t, env.call(tok, SyncRequest{Action: SyncActionCheckout, ArticleID: id, ClientID: pcB, Force: true}), http.StatusOK)
	if a := env.getArticle(id); a.CheckedOutBy != "佐藤" || a.CheckedOutClient != pcB {
		t.Fatalf("after takeover: by %q client %q, want 佐藤 on %s", a.CheckedOutBy, a.CheckedOutClient, pcB)
	}
	submitA := submitRequest(SyncArticle{ID: id, Title: "PC A の版"})
	submitA.ClientID = pcA
	expectSyncStatus(t, env.call(tok, submitA), http.StatusConflict)

	// PC B の提出は通り、印（識別子も）を外す。
	expectSyncStatus(t, env.call(tok, submit), http.StatusOK)
	if a := env.getArticle(id); a.Title != "PC B の版" || a.CheckedOutBy != "" || a.CheckedOutClient != "" || a.CheckedOutAt != nil {
		t.Errorf("after PC B submit: title %q checkout %q %q %v", a.Title, a.CheckedOutBy, a.CheckedOutClient, a.CheckedOutAt)
	}
}
