package importer

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/image"
	"sorus/internal/markdown"
	"sorus/internal/search"
)

const (
	// DefaultAuthor は dc:creator が空の記事を割り当てるユーザー名の既定値。
	DefaultAuthor = "admin"
	// uncategorizedName / uncategorizedSlug はカテゴリ未指定の記事を入れるカテゴリ。
	uncategorizedName = "未分類"
	uncategorizedSlug = "uncategorized"
	// untitled はタイトルが空の記事に付けるタイトル。
	untitled = "無題"
	// redirectStatus は旧パーマリンクから登録する転送のステータスコード。
	redirectStatus = http.StatusMovedPermanently
	// reservedPageSlugSuffix は固定ページのスラッグが予約語（Options.ReservedPageSlug）のときに付ける接尾辞。
	reservedPageSlugSuffix = "-page"
)

// Options はインポートの設定。
type Options struct {
	// DefaultAuthor は dc:creator が空の記事の著者ユーザー名。空なら DefaultAuthor。Account を指定したときは使わない。
	DefaultAuthor string
	// Account は、すべての記事の著者にする既存のユーザーの ID。空でなければ WordPress の著者にかかわらず
	// この1人の記事として取り込み、ユーザーを作らない（デスクトップモードは1人1アカウントのため）。
	Account string
	// DownloadImages が true なら本文・アイキャッチの外部画像をダウンロードして WebP 化する。
	DownloadImages bool
	// DryRun が true なら DB の変更をコミットせず、画像のダウンロードと検索インデックス登録も行わない。
	DryRun bool
	// SkipPages が true なら固定ページ (post_type=page) を取り込まない。false なら kind = page の記事として取り込む。
	SkipPages bool
	// UploadDir は WebP の保存先。DownloadImages のとき必須。
	UploadDir string
	// HTTPClient は画像のダウンロードに使う。nil ならタイムアウト 10 秒のクライアント。
	HTTPClient *http.Client
	// MaxImageSize は画像1枚の上限バイト数。0 以下なら image.DefaultMaxUploadSize。
	MaxImageSize int64
	// Location は wp:post_date_gmt が無い記事の日時を解釈するタイムゾーン。nil なら UTC。
	Location *time.Location
	// Logger は警告の出力先。nil なら log.Default()。
	Logger *log.Logger
	// ReservedPageSlug は固定ページのスラッグに使えないか（予約語か）を返す。保存と同じ規則にするため、
	// 同期 API のパスを加えた article.Service の IsReservedPageSlug を渡す。nil なら固定の予約語
	// （article.IsReservedPageSlug）だけを検査する。
	ReservedPageSlug func(slug string) bool
}

// Summary はインポート結果の統計。
type Summary struct {
	Articles          int // 登録した記事数（固定ページを含む）
	Published         int
	Drafts            int
	Skipped           int // 取り込み済み・ゴミ箱などで登録しなかった記事数
	Pages             int // Articles のうち固定ページ (kind = page) として登録した数
	PageSlugsRenamed  int // 予約語と重なるため reservedPageSlugSuffix を付けた固定ページのスラッグの数
	CategoriesCreated int
	UsersCreated      int
	AccountName       string // Options.Account の表示名。Account を指定しなかったときは空
	ImagesSucceeded   int
	ImagesFailed      int
	Redirects         int
	Indexed           int
	DryRun            bool
	Duration          time.Duration
	Warnings          []string
}

// Write はサマリーを人が読める形式で w に書く。
func (s *Summary) Write(w io.Writer) {
	mode := ""
	if s.DryRun {
		mode = "（ドライラン: 変更はコミットしていません）"
	}
	fmt.Fprintf(w, "=== WordPress インポート完了%s ===\n", mode)
	fmt.Fprintf(w, "記事:         %d 件（公開 %d / 下書き %d / スキップ %d）\n", s.Articles, s.Published, s.Drafts, s.Skipped)
	fmt.Fprintf(w, "固定ページ:   %d 件（予約語のためスラッグを変更 %d 件）\n", s.Pages, s.PageSlugsRenamed)
	fmt.Fprintf(w, "作成カテゴリ: %d 件\n", s.CategoriesCreated)
	if s.AccountName != "" {
		fmt.Fprintf(w, "著者:         記事 %d 件を「%s」さんの記事として取り込みました\n", s.Articles, s.AccountName)
	} else {
		fmt.Fprintf(w, "作成ユーザー: %d 件\n", s.UsersCreated)
	}
	fmt.Fprintf(w, "変換画像:     成功 %d / 失敗 %d\n", s.ImagesSucceeded, s.ImagesFailed)
	fmt.Fprintf(w, "301転送:      %d 件\n", s.Redirects)
	fmt.Fprintf(w, "検索登録:     %d 件\n", s.Indexed)
	fmt.Fprintf(w, "警告:         %d 件\n", len(s.Warnings))
	fmt.Fprintf(w, "処理時間:     %s\n", s.Duration.Round(time.Millisecond))
}

// Importer は WXR を DB に取り込む。
type Importer struct {
	db    *sqlx.DB
	index *search.Index
	opts  Options
	log   *log.Logger
}

// New は Importer を返す。db は database.Migrate 適用済みであること。index が nil なら検索登録を行わない。
func New(db *sqlx.DB, index *search.Index, opts Options) *Importer {
	if opts.DefaultAuthor == "" {
		opts.DefaultAuthor = DefaultAuthor
	}
	if opts.ReservedPageSlug == nil {
		opts.ReservedPageSlug = article.IsReservedPageSlug
	}
	l := opts.Logger
	if l == nil {
		l = log.Default()
	}
	return &Importer{db: db, index: index, opts: opts, log: l}
}

// ImportFile は WXR ファイルを取り込む。
func (im *Importer) ImportFile(ctx context.Context, path string) (*Summary, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("importer: open %s: %w", path, err)
	}
	defer f.Close()
	return im.Import(ctx, f)
}

// Import は r の WXR を解析して取り込む。
func (im *Importer) Import(ctx context.Context, r io.Reader) (*Summary, error) {
	exp, err := Parse(r, ParseOptions{Location: im.opts.Location})
	if err != nil {
		return nil, err
	}
	return im.ImportExport(ctx, exp)
}

// pending は DB 登録前に変換を終えた記事。
type pending struct {
	post          Post
	markdown      string
	excerpt       string
	featuredImage string
	images        []*FetchedImage // この記事で初めて使う画像（media に登録する）
	oldPaths      []string
}

// ImportExport は解析済みの WXR を取り込む。
//
// 1. 取り込み済み（旧パスが redirects に登録済み）・ゴミ箱の記事を除外する。
// 2. 本文を Markdown に変換し、外部画像をダウンロードして WebP 化する（DB のロックを取らずに行う）。
// 3. 著者・カテゴリ・記事・media・redirects を1つの書き込みトランザクションで登録する。
// 4. コミット後、公開記事を検索インデックスに登録する。
//
// DB 登録に失敗した場合は全体をロールバックし、生成した WebP も削除する。
// 画像1枚のダウンロード失敗では中断せず、警告を記録して元の URL を残す。
func (im *Importer) ImportExport(ctx context.Context, exp *Export) (*Summary, error) {
	start := time.Now()
	sum := &Summary{DryRun: im.opts.DryRun}

	existing, err := im.existingRedirects(ctx)
	if err != nil {
		return nil, err
	}

	attachments := make(map[int64]string, len(exp.Attachments))
	for _, a := range exp.Attachments {
		if a.ID > 0 && a.AttachmentURL != "" {
			attachments[a.ID] = a.AttachmentURL
		}
	}

	var fetcher *MediaFetcher
	if im.opts.DownloadImages && !im.opts.DryRun {
		if im.opts.UploadDir == "" {
			return nil, errors.New("importer: UploadDir is required to download images")
		}
		fetcher = NewMediaFetcher(im.opts.HTTPClient, im.opts.UploadDir, im.opts.MaxImageSize)
	}

	var items []*pending
	var fetched []*FetchedImage
	seenPaths := map[string]bool{}
	for _, p := range exp.Posts {
		if err := ctx.Err(); err != nil {
			removeAll(fetched)
			return nil, err
		}
		if skip, reason := im.shouldSkip(p); skip {
			sum.Skipped++
			im.warnf(sum, "記事 %d「%s」をスキップしました: %s", p.ID, p.Title, reason)
			continue
		}
		paths := oldPaths(p)
		if alreadyImported(paths, existing) {
			sum.Skipped++
			im.warnf(sum, "記事 %d「%s」は取り込み済みのためスキップしました", p.ID, p.Title)
			continue
		}
		// 同じファイル内で旧パスが重複する場合は先の記事を優先する。
		var unique []string
		for _, op := range paths {
			if !seenPaths[op] {
				seenPaths[op] = true
				unique = append(unique, op)
			}
		}

		it := &pending{
			post:     p,
			markdown: HTMLToMarkdown(p.ContentHTML),
			excerpt:  plainText(p.ExcerptHTML),
			oldPaths: unique,
		}
		if thumb := attachments[parseInt(p.Meta["_thumbnail_id"])]; thumb != "" {
			it.featuredImage = thumb
		}
		if fetcher != nil {
			fetched = append(fetched, im.localizeImages(ctx, fetcher, it, sum)...)
		}
		items = append(items, it)
	}

	created, err := im.write(ctx, exp, items, sum)
	if err != nil {
		removeAll(fetched)
		return nil, err
	}

	var indexErrs []error
	if !im.opts.DryRun && im.index != nil {
		for _, c := range created {
			if c.article.Status != article.StatusPublished {
				continue
			}
			if err := im.index.IndexArticle(c.article, c.authorName, c.categoryName); err != nil {
				im.warnf(sum, "記事「%s」の検索インデックス登録に失敗しました: %v", c.article.Title, err)
				indexErrs = append(indexErrs, fmt.Errorf("article %q: %w", c.article.Title, err))
				continue
			}
			sum.Indexed++
		}
	}

	sum.Duration = time.Since(start)
	if len(indexErrs) > 0 {
		return sum, fmt.Errorf("importer: committed articles, but %d failed to index: %w", len(indexErrs), errors.Join(indexErrs...))
	}
	return sum, nil
}

// shouldSkip はゴミ箱・自動下書き・対象外の投稿タイプを除外する。
func (im *Importer) shouldSkip(p Post) (bool, string) {
	switch {
	case p.Type == "page" && im.opts.SkipPages:
		return true, "固定ページは取り込み対象外です"
	case p.Status == "trash":
		return true, "ゴミ箱の記事です"
	case p.Status == "auto-draft":
		return true, "自動下書きです"
	}
	return false, ""
}

// localizeImages は本文とアイキャッチの外部画像を取得し、本文の URL をローカルの WebP に置き換える。
// 新たに取得した画像を返す。失敗した画像は警告を記録して元の URL のまま残す。
func (im *Importer) localizeImages(ctx context.Context, f *MediaFetcher, it *pending, sum *Summary) []*FetchedImage {
	urls := ExternalImageURLs(it.markdown)
	if strings.HasPrefix(it.featuredImage, "http://") || strings.HasPrefix(it.featuredImage, "https://") {
		urls = append(urls, it.featuredImage)
	}
	var fresh []*FetchedImage
	replacements := map[string]string{}
	for _, u := range urls {
		img, cached, err := f.Fetch(ctx, u)
		if err != nil {
			if !cached {
				sum.ImagesFailed++
				im.warnf(sum, "画像の取り込みに失敗しました（元の URL を維持します）: %v", err)
			}
			continue
		}
		if !cached {
			sum.ImagesSucceeded++
			fresh = append(fresh, img)
		}
		replacements[u] = img.URL
	}
	it.images = fresh
	it.markdown = ReplaceURLs(it.markdown, replacements)
	if r, ok := replacements[it.featuredImage]; ok {
		it.featuredImage = r
	}
	return fresh
}

// createdArticle はコミット後に検索インデックスへ登録する記事。
type createdArticle struct {
	article      *article.Article
	authorName   string
	categoryName string
}

type userRef struct {
	id          string
	displayName string
}

type categoryRef struct {
	id   int64
	name string
}

// write は記事一式を BEGIN IMMEDIATE のトランザクションで登録する。DryRun なら最後にロールバックする。
func (im *Importer) write(ctx context.Context, exp *Export, items []*pending, sum *Summary) ([]createdArticle, error) {
	conn, err := im.db.Connx(ctx)
	if err != nil {
		return nil, fmt.Errorf("importer: acquire conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, fmt.Errorf("importer: begin: %w", err)
	}
	done := false
	defer func() {
		if !done {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	tx := &txWriter{conn: conn, ctx: ctx, exp: exp, sum: sum, users: map[string]userRef{}, categories: map[string]categoryRef{},
		warnf: func(format string, args ...any) { im.warnf(sum, format, args...) }, reserved: im.opts.ReservedPageSlug}
	if err := tx.loadCategories(); err != nil {
		return nil, err
	}
	var account *userRef
	if im.opts.Account != "" {
		u, err := tx.account(im.opts.Account)
		if err != nil {
			return nil, err
		}
		account = &u
		sum.AccountName = u.displayName
	}

	created := make([]createdArticle, 0, len(items))
	for _, it := range items {
		c, skipped, err := tx.writeArticle(it, im.opts.DefaultAuthor, account)
		if err != nil {
			return nil, fmt.Errorf("importer: post %d %q: %w", it.post.ID, it.post.Title, err)
		}
		if skipped {
			continue
		}
		created = append(created, c)
	}

	if im.opts.DryRun {
		return created, nil // defer でロールバックする
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("importer: commit: %w", err)
	}
	done = true
	return created, nil
}

// txWriter はトランザクション内の登録処理と、作成済みの著者・カテゴリのキャッシュ。
type txWriter struct {
	conn       *sqlx.Conn
	ctx        context.Context
	exp        *Export
	sum        *Summary
	users      map[string]userRef     // ユーザー名 → ユーザー
	categories map[string]categoryRef // "name:" + 名前 / "slug:" + スラッグ → カテゴリ
	slugsTaken map[string]bool        // カテゴリのスラッグ
	warnf      func(format string, args ...any)
	reserved   func(slug string) bool // 固定ページのスラッグに使えないか（Options.ReservedPageSlug）
}

// writeArticle は記事を1件登録する。account が nil でなければ WordPress の著者にかかわらずその記事にする。
func (tx *txWriter) writeArticle(it *pending, defaultAuthor string, account *userRef) (createdArticle, bool, error) {
	// トランザクション内で重複（他のプロセスが先行して同じ旧URLをインポートした場合など）を再チェック
	if len(it.oldPaths) > 0 {
		query, args, err := sqlx.In("SELECT COUNT(*) FROM redirects WHERE old_path IN (?)", it.oldPaths)
		if err != nil {
			return createdArticle{}, false, fmt.Errorf("check existing redirects: %w", err)
		}
		var count int
		if err := tx.conn.QueryRowContext(tx.ctx, tx.conn.Rebind(query), args...).Scan(&count); err != nil {
			return createdArticle{}, false, fmt.Errorf("scan existing redirects count: %w", err)
		}
		if count > 0 {
			tx.sum.Skipped++
			return createdArticle{}, true, nil
		}
	}

	p := it.post
	var author userRef
	if account != nil {
		author = *account
	} else {
		login := p.Creator
		if login == "" {
			login = defaultAuthor
		}
		var err error
		if author, err = tx.user(login); err != nil {
			return createdArticle{}, false, err
		}
	}
	var term Term
	if len(p.Categories) > 0 {
		term = p.Categories[0]
	}
	cat, err := tx.category(term)
	if err != nil {
		return createdArticle{}, false, err
	}

	title := p.Title
	if title == "" {
		title = untitled
	}
	rendered, err := markdown.Render(it.markdown)
	if err != nil {
		return createdArticle{}, false, fmt.Errorf("render markdown: %w", err)
	}
	id, err := newHexID()
	if err != nil {
		return createdArticle{}, false, err
	}
	kind := article.KindPost
	if p.Type == "page" {
		kind = article.KindPage
	}
	base := p.Slug
	if strings.TrimSpace(base) == "" {
		base = title
	}
	base = article.NormalizeSlug(base)
	// 公開サイトの決まった URL（/admin・/search など）と重なる固定ページは、決まった接尾辞を付けたスラッグにする
	// （同じファイルを何度取り込んでも同じスラッグになる）。旧 URL からの転送は新しいスラッグへ向く。
	if kind == article.KindPage && tx.reserved(base) {
		renamed := base + reservedPageSlugSuffix
		tx.sum.PageSlugsRenamed++
		tx.warnf("固定ページ %d「%s」のスラッグ「%s」はサイトの決まった URL と重なるため「%s」にしました", p.ID, title, base, renamed)
		base = renamed
	}
	slug, err := tx.uniqueArticleSlug(base)
	if err != nil {
		return createdArticle{}, false, err
	}

	now := time.Now().UTC()
	date := p.Date
	if date.IsZero() {
		date = now
	}
	a := &article.Article{
		ID:              id,
		Title:           title,
		Slug:            slug,
		ContentMarkdown: it.markdown,
		ContentHTML:     rendered.HTML,
		Excerpt:         it.excerpt,
		FeaturedImage:   it.featuredImage,
		AuthorID:        author.id,
		CategoryID:      cat.id,
		Status:          article.StatusDraft,
		Kind:            kind,
		CreatedAt:       date,
		UpdatedAt:       now,
	}
	if p.Status == "publish" {
		a.Status = article.StatusPublished
		a.PublishedAt = &date
	}
	q, args, err := sqlx.Named(`INSERT INTO articles (id, title, slug, content_markdown, content_html, excerpt,
			featured_image_url, author_id, category_id, status, kind, published_at, created_at, updated_at)
		VALUES (:id, :title, :slug, :content_markdown, :content_html, :excerpt,
			:featured_image_url, :author_id, :category_id, :status, :kind, :published_at, :created_at, :updated_at)`, a)
	if err != nil {
		return createdArticle{}, false, fmt.Errorf("bind article: %w", err)
	}
	if _, err := tx.conn.ExecContext(tx.ctx, tx.conn.Rebind(q), args...); err != nil {
		return createdArticle{}, false, fmt.Errorf("insert article: %w", err)
	}
	if a.Status == article.StatusPublished {
		tx.sum.Published++
	} else {
		tx.sum.Drafts++
	}
	tx.sum.Articles++
	if kind == article.KindPage {
		tx.sum.Pages++
	}

	for _, img := range it.images {
		pi := img.Processed
		if _, err := tx.conn.ExecContext(tx.ctx, `INSERT INTO media (id, filename, path_original, path_large,
				path_medium, path_small, width, height, mime_type, uploaded_by, taken_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			img.MediaID, img.Filename, pi.OriginalPath, pi.LargePath, pi.MediumPath, pi.SmallPath,
			pi.Width, pi.Height, pi.MimeType, author.id, image.TakenAtValue(pi.TakenAt, date)); err != nil {
			return createdArticle{}, false, fmt.Errorf("insert media: %w", err)
		}
	}

	newPath := "/articles/" + url.PathEscape(slug)
	for _, op := range it.oldPaths {
		if op == newPath {
			continue
		}
		res, err := tx.conn.ExecContext(tx.ctx, `INSERT INTO redirects (old_path, new_path, status_code)
			VALUES (?, ?, ?) ON CONFLICT(old_path) DO NOTHING`, op, newPath, redirectStatus)
		if err != nil {
			return createdArticle{}, false, fmt.Errorf("insert redirect: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			tx.sum.Redirects++
		}
	}
	return createdArticle{article: a, authorName: author.displayName, categoryName: cat.name}, false, nil
}

// account は ID が id の既存のユーザーを返す。見つからなければエラーにする（作成はしない）。
func (tx *txWriter) account(id string) (userRef, error) {
	var row struct {
		ID          string `db:"id"`
		DisplayName string `db:"name"`
	}
	err := tx.conn.GetContext(tx.ctx, &row, `SELECT id, COALESCE(NULLIF(display_name, ''), username) AS name FROM users WHERE id = ?`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return userRef{}, fmt.Errorf("importer: account %q not found", id)
	}
	if err != nil {
		return userRef{}, fmt.Errorf("importer: find account %q: %w", id, err)
	}
	return userRef{id: row.ID, displayName: row.DisplayName}, nil
}

// user はユーザー名 login のユーザーを返す。存在しなければランダムなパスワードの writer として作成する。
func (tx *txWriter) user(login string) (userRef, error) {
	if u, ok := tx.users[login]; ok {
		return u, nil
	}
	var row struct {
		ID          string `db:"id"`
		DisplayName string `db:"display_name"`
	}
	err := tx.conn.GetContext(tx.ctx, &row, `SELECT id, display_name FROM users WHERE username = ?`, login)
	if err == nil {
		u := userRef{id: row.ID, displayName: row.DisplayName}
		tx.users[login] = u
		return u, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return userRef{}, fmt.Errorf("find user %q: %w", login, err)
	}

	display := login
	for _, a := range tx.exp.Authors {
		if a.Login == login && a.DisplayName != "" {
			display = a.DisplayName
			break
		}
	}
	id, err := newHexID()
	if err != nil {
		return userRef{}, err
	}
	// ログインできないよう、誰も知らないランダムなパスワードを設定する。管理者がリセットして配布する。
	secret := make([]byte, 24)
	if _, err := rand.Read(secret); err != nil {
		return userRef{}, fmt.Errorf("generate password: %w", err)
	}
	hash, err := auth.HashPassword(hex.EncodeToString(secret))
	if err != nil {
		return userRef{}, fmt.Errorf("hash password: %w", err)
	}
	if _, err := tx.conn.ExecContext(tx.ctx, `INSERT INTO users (id, username, display_name, password_hash, role)
		VALUES (?, ?, ?, ?, ?)`, id, login, display, hash, auth.RoleWriter); err != nil {
		return userRef{}, fmt.Errorf("create user %q: %w", login, err)
	}
	tx.sum.UsersCreated++
	u := userRef{id: id, displayName: display}
	tx.users[login] = u
	return u, nil
}

func (tx *txWriter) loadCategories() error {
	var rows []struct {
		ID   int64  `db:"id"`
		Name string `db:"name"`
		Slug string `db:"slug"`
	}
	if err := tx.conn.SelectContext(tx.ctx, &rows, `SELECT id, name, slug FROM categories`); err != nil {
		return fmt.Errorf("importer: load categories: %w", err)
	}
	tx.slugsTaken = make(map[string]bool, len(rows))
	for _, r := range rows {
		ref := categoryRef{id: r.ID, name: r.Name}
		tx.categories["name:"+r.Name] = ref
		tx.categories["slug:"+r.Slug] = ref
		tx.slugsTaken[r.Slug] = true
	}
	return nil
}

// category は名前、次にスラッグで既存カテゴリと照合し、無ければ作成する。名前が空なら「未分類」。
func (tx *txWriter) category(t Term) (categoryRef, error) {
	if t.Name == "" {
		t = Term{Name: uncategorizedName, Slug: uncategorizedSlug}
	}
	if c, ok := tx.categories["name:"+t.Name]; ok {
		return c, nil
	}
	base := t.Slug
	if base == "" {
		base = t.Name
	}
	base = article.NormalizeSlug(base)
	if c, ok := tx.categories["slug:"+base]; ok {
		return c, nil
	}
	slug := base
	for n := 2; tx.slugsTaken[slug]; n++ {
		slug = base + "-" + strconv.Itoa(n)
	}
	res, err := tx.conn.ExecContext(tx.ctx, `INSERT INTO categories (name, slug) VALUES (?, ?)`, t.Name, slug)
	if err != nil {
		return categoryRef{}, fmt.Errorf("create category %q: %w", t.Name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return categoryRef{}, fmt.Errorf("create category %q: %w", t.Name, err)
	}
	tx.sum.CategoriesCreated++
	ref := categoryRef{id: id, name: t.Name}
	tx.categories["name:"+t.Name] = ref
	tx.categories["slug:"+slug] = ref
	tx.slugsTaken[slug] = true
	return ref, nil
}

// uniqueArticleSlug は base が既存の記事（論理削除済み・同じトランザクションで登録した記事を含む）と
// 重複しなければそのまま、重複すれば base-2, base-3, ... のうち最初の空きを返す。
func (tx *txWriter) uniqueArticleSlug(base string) (string, error) {
	var taken []string
	err := tx.conn.SelectContext(tx.ctx, &taken, `SELECT slug FROM articles WHERE slug = ? OR slug LIKE ? ESCAPE '\'`,
		base, likeEscaper.Replace(base)+"-%")
	if err != nil {
		return "", fmt.Errorf("find slugs: %w", err)
	}
	used := make(map[string]bool, len(taken))
	for _, t := range taken {
		used[t] = true
	}
	slug := base
	for n := 2; used[slug]; n++ {
		slug = base + "-" + strconv.Itoa(n)
	}
	return slug, nil
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// existingRedirects は登録済みの旧パスの集合を返す。取り込み済み記事の判定に使う。
func (im *Importer) existingRedirects(ctx context.Context) (map[string]bool, error) {
	var paths []string
	if err := im.db.SelectContext(ctx, &paths, `SELECT old_path FROM redirects`); err != nil {
		return nil, fmt.Errorf("importer: load redirects: %w", err)
	}
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[p] = true
	}
	return set, nil
}

func alreadyImported(paths []string, existing map[string]bool) bool {
	for _, p := range paths {
		if existing[p] {
			return true
		}
	}
	return false
}

// oldPaths は記事の旧 URL から redirects に登録するパスを返す。
// パーマリンク（例: /2023/04/post-name/）と、ID 指定の /?p=123（固定ページは /?page_id=123 も）。
// サイトのトップ（/）は登録しない。
func oldPaths(p Post) []string {
	var paths []string
	add := func(s string) {
		if s == "" || s == "/" {
			return
		}
		for _, e := range paths {
			if e == s {
				return
			}
		}
		paths = append(paths, s)
	}
	if u, err := url.Parse(p.Link); err == nil && p.Link != "" {
		pth := u.Path
		if pth == "" {
			pth = "/"
		}
		if u.RawQuery != "" {
			pth += "?" + u.RawQuery
		}
		add(pth)
	}
	if p.ID > 0 {
		id := strconv.FormatInt(p.ID, 10)
		add("/?p=" + id)
		if p.Type == "page" {
			add("/?page_id=" + id)
		}
	}
	return paths
}

// plainText は抜粋の HTML からタグを除いた1行のテキストを返す。
func plainText(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	nodes, err := html.ParseFragment(strings.NewReader(s), &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body})
	if err != nil {
		return strings.TrimSpace(s)
	}
	var b strings.Builder
	for _, n := range nodes {
		b.WriteString(textContent(n))
		b.WriteByte(' ')
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func removeAll(imgs []*FetchedImage) {
	for _, img := range imgs {
		RemoveImageFiles(img.Processed)
	}
}

func (im *Importer) warnf(sum *Summary, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	sum.Warnings = append(sum.Warnings, msg)
	im.log.Printf("importer: warning: %s", msg)
}
