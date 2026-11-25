package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/scheduler"
)

type approvePublishArgs struct {
	ArticleID string `json:"article_id"`
	PublishAt string `json:"publish_at"`
	Comment   string `json:"comment"`
}

type approvePublishResult struct {
	ArticleID   string `json:"article_id"`
	Status      string `json:"status"`
	PublishedAt string `json:"published_at"`
	Message     string `json:"message"`
}

const minScheduleLead = time.Minute

// jst は予約公開コメントの表示用タイムゾーン。
var jst = time.FixedZone("JST", 9*60*60)

func publisherOnly(user *auth.User, message string) error {
	if user == nil || !user.HasRole(auth.RoleEditor, auth.RoleAdmin) {
		return toolErrorf("%s", message)
	}
	return nil
}

func (s *Server) toolApproveAndPublish(ctx context.Context, user *auth.User, raw json.RawMessage) (any, error) {
	if err := publisherOnly(user, "記事を承認・公開する権限がありません"); err != nil {
		return nil, err
	}
	var args approvePublishArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	args.ArticleID = strings.TrimSpace(args.ArticleID)
	if args.ArticleID == "" {
		return nil, toolErrorf("article_id を指定してください")
	}
	comment := strings.TrimSpace(normalizeNewlines(args.Comment))
	if utf8.RuneCountInString(comment) > maxNoteRunes {
		return nil, toolErrorf("comment は%d文字以内で指定してください", maxNoteRunes)
	}
	now := time.Now().UTC()
	var at time.Time
	reserved := false
	if strings.TrimSpace(args.PublishAt) == "" {
		at = now
	} else {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(args.PublishAt))
		if err != nil {
			return nil, toolErrorf("publish_at は RFC3339 形式で指定してください")
		}
		at = parsed.UTC()
	}
	a, err := s.articles.GetByID(args.ArticleID)
	if errors.Is(err, article.ErrNotFound) {
		return nil, errArticleNotFound(args.ArticleID)
	}
	if err != nil {
		return nil, fmt.Errorf("get article for approval: %w", err)
	}
	if err := checkWritable(user, a, checkApprovable); err != nil {
		return nil, err
	}
	reserved = strings.TrimSpace(args.PublishAt) != ""
	if reserved && at.Before(now.Add(minScheduleLead)) {
		return nil, toolErrorf("予約公開日時には現在から1分以上先の日時を指定してください")
	}
	if reserved && a.PublishedAt != nil && !a.PublishedAt.After(now) {
		return nil, toolErrorf("一度公開した記事は予約公開に変更できません")
	}
	if a.Kind == article.KindPage && s.articles.IsReservedPageSlug(a.Slug) {
		return nil, toolErrorf("予約語のスラッグを持つページは公開できません")
	}
	release, err := s.lockForWrite(ctx, user, a.ID)
	if err != nil {
		return nil, err
	}
	defer release(false)
	query := `UPDATE articles SET status = ?, published_at = ?, updated_at = ? WHERE id = ? AND status = ?`
	if !reserved {
		query = `UPDATE articles SET status = ?, published_at = CASE WHEN published_at IS NULL OR published_at > ? THEN ? ELSE published_at END, updated_at = ? WHERE id = ? AND status = ?`
	}
	var res sql.Result
	if reserved {
		res, err = s.db.ExecContext(ctx, query, article.StatusPublished, at, now, a.ID, article.StatusReview)
	} else {
		res, err = s.db.ExecContext(ctx, query, article.StatusPublished, now, now, now, a.ID, article.StatusReview)
	}
	if err != nil {
		return nil, fmt.Errorf("approve article: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, fmt.Errorf("approve article: %w", err)
	} else if n == 0 {
		return nil, toolErrorf("記事の状態が変わったため承認できませんでした。現在の状態を確認してください")
	}
	if updated, err := s.articles.GetByID(a.ID); err == nil {
		s.snapshot(ctx, updated, user, "承認して公開（MCP）")
	}
	if reserved {
		defaultComment := fmt.Sprintf("予約公開を設定しました（MCP: %s JST）", at.In(jst).Format("2006/01/02 15:04"))
		if comment == "" {
			comment = defaultComment
		} else {
			comment = defaultComment + "\n" + comment
		}
	} else if comment == "" {
		comment = "公開しました（MCP）"
	}
	{
		if err := s.insertComment(ctx, a.ID, user.ID, "【承認】"+comment); err != nil {
			s.logger.Printf("mcp: comment: article %s: %v", a.ID, err)
		}
	}
	s.changed(a.ID)
	if reserved {
		if err := scheduler.Track(ctx, s.db, now, a.ID); err != nil {
			s.logger.Printf("mcp: track scheduled article %s: %v", a.ID, err)
		}
	}
	status, message := "published", "記事を公開しました"
	if at.After(time.Now()) {
		status, message = "scheduled", "記事の公開を予約しました"
	} else if s.cfg.OnArticlePublished != nil {
		s.cfg.OnArticlePublished()
	}
	return approvePublishResult{ArticleID: a.ID, Status: status, PublishedAt: at.Format(time.RFC3339), Message: message}, nil
}

type publishSiteArgs struct {
	PurgeCDN    *bool `json:"purge_cdn"`
	WaitSeconds *int  `json:"wait_seconds"`
}

func (s *Server) toolPublishSite(ctx context.Context, user *auth.User, raw json.RawMessage) (any, error) {
	if err := adminOnly(user, "静的サイトを反映する権限がありません（管理者のみ）"); err != nil {
		return nil, err
	}
	var args publishSiteArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	purge := args.PurgeCDN == nil || *args.PurgeCDN
	waitSeconds := 20
	if args.WaitSeconds != nil {
		waitSeconds = *args.WaitSeconds
	}
	if waitSeconds < 0 || waitSeconds > 30 {
		return nil, toolErrorf("wait_seconds は 0〜30 の範囲で指定してください")
	}
	if s.cfg.PublishSite == nil {
		return nil, toolErrorf("公開サイトの反映機能を利用できません")
	}
	return s.cfg.PublishSite(ctx, purge, time.Duration(waitSeconds)*time.Second)
}

func (s *Server) toolGetPublishStatus(_ context.Context, user *auth.User, _ json.RawMessage) (any, error) {
	if err := publisherOnly(user, "静的サイトの反映状態を確認する権限がありません"); err != nil {
		return nil, err
	}
	if s.cfg.GetPublishStatus == nil {
		return nil, toolErrorf("公開サイトの反映状態を確認できません")
	}
	return s.cfg.GetPublishStatus(), nil
}

func (s *Server) toolCheckPublishHealth(ctx context.Context, user *auth.User, _ json.RawMessage) (any, error) {
	if err := publisherOnly(user, "公開サイトの状態を確認する権限がありません"); err != nil {
		return nil, err
	}
	if s.cfg.CheckPublishHealth == nil {
		return nil, toolErrorf("公開サイトの健全性診断を利用できません")
	}
	return s.cfg.CheckPublishHealth(ctx), nil
}

type autonomousArticleArgs struct {
	ArticleID       string `json:"article_id"`
	Title           string `json:"title"`
	ContentMarkdown string `json:"content_markdown"`
	Excerpt         string `json:"excerpt"`
}

var (
	mdImageRE        = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	mdLinkRE         = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	htmlImgRE        = regexp.MustCompile(`(?is)<img\b[^>]*>`)
	altRE            = regexp.MustCompile(`(?is)\balt\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	mdMarkRE         = regexp.MustCompile(`(?m)^[\t ]{0,3}#{1,6}[\t ]+`)
	markupRE         = regexp.MustCompile(`(?s)<[^>]*>|[*_~` + "`" + `>#]`)
	wordRE           = regexp.MustCompile(`(?i)[a-z][a-z0-9-]{2,}`)
	jpKeywordRE      = regexp.MustCompile(`\p{Han}{2,}|[\p{Katakana}\x{30FC}]{3,}`)
	sentenceEndRE    = regexp.MustCompile(`[。！？]+[」』）)"'”’】〕］｝〉》]*|[.!?]+[」』）)"'”’】〕］｝〉》]*(?:\s|$)`)
	englishStopWords = map[string]bool{
		"a": true, "an": true, "the": true, "and": true, "or": true, "but": true, "if": true, "then": true,
		"for": true, "nor": true, "so": true, "yet": true, "as": true, "at": true, "by": true, "in": true,
		"of": true, "on": true, "to": true, "up": true, "via": true, "with": true, "from": true, "into": true,
		"onto": true, "upon": true, "over": true, "under": true, "about": true, "after": true, "before": true,
		"between": true, "through": true, "during": true, "without": true, "within": true, "is": true, "am": true,
		"are": true, "was": true, "were": true, "be": true, "been": true, "being": true, "have": true, "has": true,
		"had": true, "do": true, "does": true, "did": true, "can": true, "could": true, "will": true, "would": true,
		"shall": true, "should": true, "may": true, "might": true, "must": true, "this": true, "that": true,
		"these": true, "those": true, "it": true, "its": true, "you": true, "your": true, "we": true,
		"our": true, "they": true, "their": true, "he": true, "she": true, "his": true, "her": true,
		"i": true, "me": true, "my": true, "mine": true, "us": true, "them": true, "what": true,
		"which": true, "who": true, "whom": true, "when": true, "where": true, "why": true, "how": true,
		"all": true, "any": true, "both": true, "each": true, "few": true, "many": true, "more": true,
		"most": true, "other": true, "some": true, "such": true, "no": true, "not": true, "only": true,
		"own": true, "same": true, "than": true, "too": true, "very": true, "just": true, "also": true,
		"out": true, "here": true, "there": true, "again": true, "once": true, "new": true,
	}
)

func (s *Server) autonomousArticle(user *auth.User, args *autonomousArticleArgs) error {
	if strings.TrimSpace(args.ArticleID) == "" {
		if args.Title == "" && args.ContentMarkdown == "" {
			return toolErrorf("article_id または title/content_markdown を指定してください")
		}
		return nil
	}
	a, err := s.loadArticle(user, strings.TrimSpace(args.ArticleID), "")
	if err != nil {
		return err
	}
	args.Title, args.ContentMarkdown, args.Excerpt = a.Title, a.ContentMarkdown, a.Excerpt
	return nil
}

type diagnosticIssue struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type diagnosisResult struct {
	Score             int               `json:"score"`
	CharacterCount    int               `json:"character_count"`
	EstimatedReadMins int               `json:"estimated_read_minutes"`
	Issues            []diagnosticIssue `json:"issues"`
	Suggestions       []string          `json:"suggestions"`
}

func (s *Server) toolDiagnoseArticle(_ context.Context, user *auth.User, raw json.RawMessage) (any, error) {
	var args autonomousArticleArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := s.autonomousArticle(user, &args); err != nil {
		return nil, err
	}
	issues, suggestions := []diagnosticIssue{}, []string{}
	score := 100
	add := func(code, message, suggestion string, penalty int) {
		issues = append(issues, diagnosticIssue{code, message})
		suggestions = append(suggestions, suggestion)
		score -= penalty
	}
	md := args.ContentMarkdown
	lastHeading := 0
	fenceChar, fenceLen := "", 0
	for _, line := range strings.Split(normalizeNewlines(md), "\n") {
		trimLeft := strings.TrimLeft(line, " \t")
		indent := 0
		for _, r := range line {
			if r == ' ' {
				indent++
			} else if r == '\t' {
				indent += 4 - indent%4
			} else {
				break
			}
		}
		if indent <= 3 && len(trimLeft) >= 3 && (trimLeft[0] == '`' || trimLeft[0] == '~') {
			markerLen := len(trimLeft) - len(strings.TrimLeft(trimLeft, trimLeft[:1]))
			if markerLen >= 3 {
				if fenceChar == "" {
					if trimLeft[0] == '~' || !strings.ContainsRune(trimLeft[markerLen:], '`') {
						fenceChar, fenceLen = trimLeft[:1], markerLen
					}
				} else if trimLeft[:1] == fenceChar && markerLen >= fenceLen && strings.TrimSpace(trimLeft[markerLen:]) == "" {
					fenceChar, fenceLen = "", 0
				}
				continue
			}
		}
		if fenceChar != "" || indent >= 4 {
			continue
		}
		trim := strings.TrimSpace(line)
		if !strings.HasPrefix(trim, "#") {
			continue
		}
		level := 0
		for level < len(trim) && trim[level] == '#' {
			level++
		}
		if level > 6 || (level < len(trim) && trim[level] != ' ' && trim[level] != '\t') {
			continue
		}
		if level == 1 {
			add("h1_in_body", "本文に H1 見出しがあります。", "本文の最初の見出しを H2 から始めてください。", 10)
		}
		if lastHeading > 0 && level > lastHeading+1 {
			add("heading_level_skip", "見出し階層が飛んでいます。", "見出しレベルを順番に使ってください。", 8)
		}
		lastHeading = level
	}
	for _, m := range mdImageRE.FindAllStringSubmatch(md, -1) {
		if strings.TrimSpace(m[1]) == "" {
			add("empty_image_alt", "画像の代替テキストがありません。", "画像の内容を説明する Alt テキストを追加してください。", 8)
		}
	}
	for _, tag := range htmlImgRE.FindAllString(md, -1) {
		m := altRE.FindStringSubmatch(tag)
		if len(m) < 3 || strings.TrimSpace(m[1]+m[2]) == "" {
			add("empty_image_alt", "HTML 画像の代替テキストがありません。", "画像の内容を説明する Alt テキストを追加してください。", 8)
		}
	}
	linksText := mdImageRE.ReplaceAllString(md, "$1")
	for _, m := range mdLinkRE.FindAllStringSubmatch(linksText, -1) {
		if strings.TrimSpace(m[1]) == "" {
			add("empty_link_text", "リンクテキストがありません。", "リンク先が分かるテキストを設定してください。", 5)
		}
	}
	for _, p := range strings.Split(md, "\n\n") {
		p = strings.TrimSpace(p)
		if p != "" && utf8.RuneCountInString(p) > 300 {
			add("long_paragraph", "300文字を超える段落があります。", "長い段落を複数に分けてください。", 5)
		}
	}
	if strings.TrimSpace(args.Excerpt) == "" {
		add("missing_excerpt", "抜粋がありません。", "記事の要点をまとめた抜粋を追加してください。", 10)
	} else {
		n := utf8.RuneCountInString(strings.TrimSpace(args.Excerpt))
		if n < 50 || n > 160 {
			add("excerpt_length", "抜粋は50〜160文字が目安です。", "抜粋の長さを50〜160文字に調整してください。", 5)
		}
	}
	plain := stripMarkdown(md)
	chars := utf8.RuneCountInString(plain)
	mins := chars / 500
	if mins < 1 && chars > 0 {
		mins = 1
	}
	return diagnosisResult{Score: max(score, 0), CharacterCount: chars, EstimatedReadMins: mins, Issues: issues, Suggestions: suggestions}, nil
}

type seoResult struct {
	SEOTitle        string   `json:"seo_title"`
	MetaDescription string   `json:"meta_description"`
	SlugCandidate   string   `json:"slug_candidate"`
	Keywords        []string `json:"keywords"`
	FocusKeywords   []string `json:"focus_keywords"`
}

func (s *Server) toolOptimizeSEO(_ context.Context, user *auth.User, raw json.RawMessage) (any, error) {
	var args autonomousArticleArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := s.autonomousArticle(user, &args); err != nil {
		return nil, err
	}
	title := strings.TrimSpace(args.Title)
	if title == "" {
		title = firstContentLine(args.ContentMarkdown)
	}
	if utf8.RuneCountInString(title) > 60 {
		title = truncateRunes(title, 60)
	}
	plain := strings.TrimSpace(stripMarkdown(args.ContentMarkdown))
	desc := firstNonEmptySentence(plain)
	desc = truncateRunes(desc, 120)
	kw := extractKeywords(title+" "+args.ContentMarkdown, 5)
	return seoResult{SEOTitle: title, MetaDescription: desc, SlugCandidate: article.NormalizeSlug(title), Keywords: kw, FocusKeywords: kw}, nil
}

type taxonomyCategory struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Slug  string `json:"slug"`
	Score int    `json:"score"`
}
type taxonomyResult struct {
	Categories            []taxonomyCategory `json:"categories"`
	RecommendedCategoryID *int64             `json:"recommended_category_id"`
	NewCategoryCandidates []string           `json:"new_category_candidates"`
	SuggestedTags         []string           `json:"suggested_tags"`
}

func (s *Server) toolSuggestTaxonomy(ctx context.Context, user *auth.User, raw json.RawMessage) (any, error) {
	var args autonomousArticleArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.ArticleID) != "" {
		if err := s.autonomousArticle(user, &args); err != nil {
			return nil, err
		}
	} else if strings.TrimSpace(args.Title) == "" && strings.TrimSpace(args.ContentMarkdown) == "" {
		return nil, toolErrorf("article_id または title/content_markdown を指定してください")
	}
	keywords := extractKeywords(args.Title+" "+args.ContentMarkdown, 8)
	var cats []taxonomyCategory
	if err := s.db.SelectContext(ctx, &cats, `SELECT id, name, slug, 0 AS score FROM categories ORDER BY name, id`); err != nil {
		return nil, err
	}
	text := strings.ToLower(args.Title + " " + stripMarkdown(args.ContentMarkdown))
	for i := range cats {
		name, slug := strings.ToLower(cats[i].Name), strings.ToLower(cats[i].Slug)
		if name != "" && strings.Contains(text, name) {
			cats[i].Score += 3
		}
		if slug != "" && strings.Contains(text, slug) {
			cats[i].Score += 2
		}
		for _, k := range keywords {
			if strings.Contains(strings.ToLower(k), name) || strings.Contains(name, strings.ToLower(k)) {
				cats[i].Score++
			}
		}
	}
	sort.SliceStable(cats, func(i, j int) bool {
		if cats[i].Score != cats[j].Score {
			return cats[i].Score > cats[j].Score
		}
		return cats[i].Name < cats[j].Name
	})
	newCats := []string{}
	for _, k := range keywords {
		if len(newCats) == 3 {
			break
		}
		found := false
		for _, c := range cats {
			if strings.EqualFold(k, c.Name) || strings.EqualFold(k, c.Slug) {
				found = true
				break
			}
		}
		if !found {
			newCats = append(newCats, k)
		}
	}
	var recommended *int64
	if len(cats) > 0 && cats[0].Score > 0 {
		id := cats[0].ID
		recommended = &id
	}
	return taxonomyResult{Categories: cats, RecommendedCategoryID: recommended, NewCategoryCandidates: newCats, SuggestedTags: keywords}, nil
}

func stripMarkdown(s string) string {
	s = mdImageRE.ReplaceAllString(s, " ")
	s = mdLinkRE.ReplaceAllString(s, "$1")
	s = mdMarkRE.ReplaceAllString(s, "")
	s = markupRE.ReplaceAllString(s, "")
	return html.UnescapeString(s)
}
func firstContentLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(strings.TrimLeft(l, "# "))
		if l != "" {
			return l
		}
	}
	return ""
}
func firstNonEmptySentence(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	if loc := sentenceEndRE.FindStringIndex(s); loc != nil {
		return strings.TrimSpace(s[:loc[1]])
	}
	return s
}
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
func extractKeywords(s string, limit int) []string {
	s = strings.ToLower(stripMarkdown(s))
	counts := map[string]int{}
	for _, w := range wordRE.FindAllString(s, -1) {
		if !englishStopWords[w] {
			counts[w]++
		}
	}
	for _, w := range jpKeywordRE.FindAllString(s, -1) {
		counts[w]++
	}
	words := make([]string, 0, len(counts))
	for w := range counts {
		words = append(words, w)
	}
	sort.Slice(words, func(i, j int) bool {
		if counts[words[i]] != counts[words[j]] {
			return counts[words[i]] > counts[words[j]]
		}
		return words[i] < words[j]
	})
	if len(words) > limit {
		words = words[:limit]
	}
	return words
}
