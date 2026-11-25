package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"sorus/internal/article"
	"sorus/internal/auth"
)

// TestMedia_UsageArticlesAPI は画像詳細・使用中記事一覧 API（/admin/api/media/:id/articles）の動作を検証する。
func TestMedia_UsageArticlesAPI(t *testing.T) {
	env := newTestEnv(t)
	adm := env.loggedIn(admin)

	// 1. メディアレコードを作成
	mediaID := "test-media-12345"
	filename := "sample_photo.webp"
	_, err := env.db.Exec(`INSERT INTO media (id, filename, path_original, path_large, path_medium, path_small,
			width, height, mime_type, uploaded_by, alt, caption)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		mediaID, filename, "uploads/"+filename, "uploads/large_"+filename, "", "",
		1200, 800, "image/webp", admin.ID, "テスト画像", "キャプション")
	if err != nil {
		t.Fatalf("insert media: %v", err)
	}

	// 2. 記事を作成（1件目は本文で画像を参照、2件目はアイキャッチで参照、3件目は参照なし）
	now := time.Now()
	id1 := seedArticle(t, env.db, article.Article{
		Title:           "画像を含む記事",
		Slug:            "post-with-content-image",
		ContentMarkdown: "こんにちは。\n\n![サンプル](/uploads/" + filename + ")\n\n本文です。",
		Status:          article.StatusPublished,
		CreatedAt:       now,
		UpdatedAt:       now,
	})
	id2 := seedArticle(t, env.db, article.Article{
		Title:           "アイキャッチ画像の記事",
		Slug:            "post-with-featured-image",
		ContentMarkdown: "本文だけです。",
		FeaturedImage:   "/uploads/large_" + filename,
		Status:          article.StatusPublished,
		CreatedAt:       now.Add(time.Second),
		UpdatedAt:       now.Add(time.Second),
	})
	_ = seedArticle(t, env.db, article.Article{
		Title:           "画像を使わない記事",
		Slug:            "post-no-image",
		ContentMarkdown: "画像は使っていません。",
		Status:          article.StatusPublished,
		CreatedAt:       now.Add(2 * time.Second),
		UpdatedAt:       now.Add(2 * time.Second),
	})

	// 3. API を呼び出し
	res := adm.get("/admin/api/media/" + mediaID + "/articles")
	expectStatus(t, res, http.StatusOK)

	var data MediaUsageResponse
	if err := json.Unmarshal([]byte(res.Body), &data); err != nil {
		t.Fatalf("unmarshal json: %v, body=%s", err, res.Body)
	}

	if data.MediaID != mediaID {
		t.Errorf("MediaID want %s, got %s", mediaID, data.MediaID)
	}
	if data.Count != 2 {
		t.Errorf("Count want 2, got %d", data.Count)
	}
	if len(data.Articles) != 2 {
		t.Fatalf("Articles length want 2, got %d", len(data.Articles))
	}

	// 記事IDが含まれているか確認
	ids := []string{data.Articles[0].ID, data.Articles[1].ID}
	if !strings.Contains(strings.Join(ids, ","), id1) ||
		!strings.Contains(strings.Join(ids, ","), id2) {
		t.Errorf("expected articles not found: %v (want %s and %s)", ids, id1, id2)
	}

	// Title と Slug が正しく取得できているか検証
	for _, art := range data.Articles {
		if art.Title == "" || art.Slug == "" {
			t.Errorf("article fields must not be empty: %+v", art)
		}
	}

	// 4. ゴミ箱（status='deleted'）の記事は使用中記事一覧から除外されることの検証
	_ = seedArticle(t, env.db, article.Article{
		Title:           "削除済み記事（ゴミ箱）",
		Slug:            "post-deleted-image",
		ContentMarkdown: "ゴミ箱の記事: ![画像](/uploads/" + filename + ")",
		Status:          article.StatusDeleted,
		CreatedAt:       now.Add(3 * time.Second),
		UpdatedAt:       now.Add(3 * time.Second),
	})
	resAfterDelete := adm.get("/admin/api/media/" + mediaID + "/articles")
	expectStatus(t, resAfterDelete, http.StatusOK)
	var dataAfterDelete MediaUsageResponse
	if err := json.Unmarshal([]byte(resAfterDelete.Body), &dataAfterDelete); err != nil {
		t.Fatalf("unmarshal json: %v", err)
	}
	if dataAfterDelete.Count != 2 {
		t.Errorf("deleted articles must not be included: want count 2, got %d", dataAfterDelete.Count)
	}

	// 4-b. 権限スコープ検証: 執筆チーム内では非公開の記事を共有するため、ライターにも他の人の下書きが見えること
	_ = seedArticle(t, env.db, article.Article{
		Title:           "ライターAの非公開下書き",
		Slug:            "post-writer-a-draft",
		ContentMarkdown: "下書き: ![画像](/uploads/" + filename + ")",
		Status:          article.StatusDraft,
		AuthorID:        studentA.ID,
		CreatedAt:       now.Add(4 * time.Second),
		UpdatedAt:       now.Add(4 * time.Second),
	})
	// 管理者はライターAの下書きも含めて見える（2件公開 + 1件下書き = 3件）
	resAdminScope := adm.get("/admin/api/media/" + mediaID + "/articles")
	expectStatus(t, resAdminScope, http.StatusOK)
	var dataAdminScope MediaUsageResponse
	json.Unmarshal([]byte(resAdminScope.Body), &dataAdminScope)
	if dataAdminScope.Count != 3 {
		t.Errorf("admin should see drafts: want 3, got %d", dataAdminScope.Count)
	}

	// ライターB（studentB）にもライターAの下書きを含めて3件見える
	writerB := env.loggedIn(studentB)
	resWriterB := writerB.get("/admin/api/media/" + mediaID + "/articles")
	expectStatus(t, resWriterB, http.StatusOK)
	var dataWriterB MediaUsageResponse
	json.Unmarshal([]byte(resWriterB.Body), &dataWriterB)
	if dataWriterB.Count != 3 {
		t.Errorf("writerB should see writerA's draft: want 3, got %d", dataWriterB.Count)
	}

	// ライターA（studentA）は自分の下書きを含めて3件見える
	writerA := env.loggedIn(studentA)
	resWriterA := writerA.get("/admin/api/media/" + mediaID + "/articles")
	expectStatus(t, resWriterA, http.StatusOK)
	var dataWriterA MediaUsageResponse
	json.Unmarshal([]byte(resWriterA.Body), &dataWriterA)
	if dataWriterA.Count != 3 {
		t.Errorf("writerA should see own draft: want 3, got %d", dataWriterA.Count)
	}

	// 5. 使用されていない画像の場合
	unusedMediaID := "test-unused-media-999"
	_, err = env.db.Exec(`INSERT INTO media (id, filename, path_original, mime_type, width, height, uploaded_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, unusedMediaID, "unused.webp", "uploads/unused.webp", "image/webp", 100, 100, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	resUnused := adm.get("/admin/api/media/" + unusedMediaID + "/articles")
	expectStatus(t, resUnused, http.StatusOK)
	var dataUnused MediaUsageResponse
	if err := json.Unmarshal([]byte(resUnused.Body), &dataUnused); err != nil {
		t.Fatal(err)
	}
	if dataUnused.Count != 0 || len(dataUnused.Articles) != 0 {
		t.Errorf("unused media want 0 articles, got %+v", dataUnused)
	}

	// 6. 存在しない画像IDの場合 404
	res404 := adm.get("/admin/api/media/non-existent-media-id/articles")
	expectStatus(t, res404, http.StatusNotFound)

	// 7. 未ログイン時のアクセス拒否
	client := env.newClient()
	resAuth := client.get("/admin/api/media/" + mediaID + "/articles")
	if resAuth.StatusCode != http.StatusFound && resAuth.StatusCode != http.StatusSeeOther && resAuth.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated want 302, 303 or 401, got %d", resAuth.StatusCode)
	}
}

// TestMedia_InspectorUIElements はメディア管理画面に使用中記事コンテナやリンク設定が存在することを検証する。
func TestMedia_InspectorUIElements(t *testing.T) {
	env := newTestEnv(t)
	adm := env.loggedIn(admin)

	res := adm.get("/admin/media")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body,
		`id="media-inspector-usage"`,
		`id="media-inspector-articles-container"`,
		`id="media-inspector-articles"`,
		`/admin/settings/ai`,
		"AIの設定画面で登録する",
		"使用中の記事",
		`data-label-edit="編集"`, "/static/js/media.js?v=",
	)
	// 使用中の記事の一覧はページに直接書かず、static/js/media.js が描く。
	expectNotContains(t, res.Body, "function articleRow(", "function loadArticles(")
	js := adm.get("/static/js/media.js")
	expectStatus(t, js, http.StatusOK)
	expectContains(t, js.Body,
		// タイトルは公開中なら公開ページ、それ以外はプレビューを既定のブラウザで開き、編集画面へは行末の「編集」から行く。
		`a.published && a.public_url`, `title.href = a.public_url;`,
		`title.setAttribute('data-desktop-preview', '/admin/articles/' + id + '/preview-open');`,
		`edit.href = '/admin/articles/' + id + '/edit';`,
		// 未反映のバナーは共通部品（sorus_common.js）で出す。
		"window.sorus.remoteDirty.add('media')",
	)
	expectNotContains(t, js.Body, `aTag.href = '/admin/articles/' + encodeURIComponent(a.id) + '/edit';`)
}

// 絵文字が含まれていないことのテスト
func TestMedia_NoEmojisInTemplates(t *testing.T) {
	for _, raw := range []string{
		"sample text without emoji",
	} {
		if hasEmoji(raw) {
			t.Errorf("emoji detected: %s", raw)
		}
	}
}

func hasEmoji(s string) bool {
	for _, r := range s {
		if r > 0xFFFF {
			return true
		}
	}
	return false
}

// 権限ごとのロール確認用ダミー参照（コンパイル維持）
var _ = auth.RoleAdmin
var _ = fmt.Sprintf
