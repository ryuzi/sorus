package social

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SNS ごとの文字数の上限と、告知文に付けるハッシュタグの数。
const (
	// XMaxLength は X の1投稿の上限（重み付きの文字数。全角は2、半角は1、URL は一律 XURLLength）。
	XMaxLength = 280
	// XURLLength は X が URL を数える長さ（t.co の短縮 URL の長さ）。
	XURLLength = 23
	// maxThreadPosts は X のスレッドの最大投稿数。
	maxThreadPosts = 5
	// maxXHashtags は X の1投稿目に付けるハッシュタグの最大数。
	maxXHashtags = 3
	// maxInstagramHashtags は Instagram のキャプションに付けるハッシュタグの最大数。
	maxInstagramHashtags = 10
	// maxInstagramRunes は Instagram のキャプションの上限。
	maxInstagramRunes = 2200
	// maxLINERunes は LINE の配信文（URL を除く）の上限。LINE の上限（5000字）より短く、読み切れる長さにする。
	maxLINERunes = 1000
	// maxHashtagRunes はハッシュタグ1つの最大文字数（# を除く）。
	maxHashtagRunes = 30
)

// Posts は SNS ごとの告知文。
type Posts struct {
	// XThread は X（旧 Twitter）のスレッド。1要素が1投稿。
	XThread []string `json:"x_thread"`
	// Instagram はリード文（キャプション）。Instagram はキャプションのリンクを開けないため URL を含めない。
	Instagram string `json:"instagram"`
	// LINE は LINE 公式アカウントの配信文。
	LINE string `json:"line"`
	// Hashtags は記事に付けるハッシュタグ（# 付き）。
	Hashtags []string `json:"hashtags"`
}

// PostInput は告知文の材料。
type PostInput struct {
	Title    string
	Category string
	SiteName string
	// URL は記事の公開 URL（絶対 URL）。未公開の記事なら空にする。
	URL string
	// Summary は記事の抜粋・リード文（プレーンテキスト）。
	Summary string
}

// urlPattern は X が URL として数える文字列（http(s):// で始まり空白までの部分）。
var urlPattern = regexp.MustCompile(`https?://[^\s　]+`)

// XLength は X が数える s の重み付きの文字数を返す（twitter-text v3 の規則に沿った目安）。
// ラテン文字・一般句読点の一部は1、それ以外（和文・絵文字など）は2、URL は長さによらず XURLLength。
func XLength(s string) int {
	n, last := 0, 0
	for _, loc := range urlPattern.FindAllStringIndex(s, -1) {
		n += weightedLength(s[last:loc[0]]) + XURLLength
		last = loc[1]
	}
	return n + weightedLength(s[last:])
}

func weightedLength(s string) int {
	n := 0
	for _, r := range s {
		n += runeWeight(r)
	}
	return n
}

// runeWeight は X での1文字の重み。絵文字の異体字セレクターとゼロ幅接合子は数えない。
func runeWeight(r rune) int {
	switch {
	case r == 0xFE0F || r == 0x200D:
		return 0
	case r <= 0x10FF, r >= 0x2000 && r <= 0x200D, r >= 0x2010 && r <= 0x201F, r >= 0x2032 && r <= 0x2037:
		return 1
	}
	return 2
}

// clipX は s を X の重み付きの文字数 limit 以内に切り詰め、切り詰めたら末尾を省略記号にする。
// URL は途中で切らず、本文を詰めたうえで末尾の行に残す。
func clipX(s string, limit int) string {
	s = strings.TrimSpace(s)
	if XLength(s) <= limit {
		return s
	}
	urls := urlPattern.FindAllString(s, -1)
	text := strings.TrimSpace(urlPattern.ReplaceAllString(s, ""))
	// URL と、その前の改行の分を本文の上限から除く。
	budget := limit - len(urls)*(XURLLength+1)
	budget -= runeWeight([]rune(ellipsis)[0])
	var b strings.Builder
	used := 0
	for _, r := range text {
		w := runeWeight(r)
		if used+w > budget {
			break
		}
		b.WriteRune(r)
		used += w
	}
	out := strings.TrimRightFunc(b.String(), unicode.IsSpace) + ellipsis
	for _, u := range urls {
		out += "\n" + u
	}
	return out
}

// clipRunes は s を max 文字以内に切り詰め、切り詰めたら末尾を省略記号にする。
func clipRunes(s string, max int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return strings.TrimRightFunc(string([]rune(s)[:max-1]), unicode.IsSpace) + ellipsis
}

// NormalizeHashtag は s をハッシュタグ（# 付き）にする。空白・記号を除き、使える文字が無ければ空を返す。
// 数字だけのハッシュタグは SNS で機能しないため空を返す。
func NormalizeHashtag(s string) string {
	s = strings.TrimLeft(strings.TrimSpace(s), "#＃")
	var b strings.Builder
	digitsOnly := true
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsMark(r):
			digitsOnly = false
		case unicode.IsDigit(r):
		case r == '_' || r == 'ー' || r == '々' || r == '〆':
			digitsOnly = false
		default:
			continue
		}
		b.WriteRune(r)
	}
	tag := b.String()
	if tag == "" || digitsOnly {
		return ""
	}
	return "#" + string([]rune(tag)[:min(utf8.RuneCountInString(tag), maxHashtagRunes)])
}

// normalizeHashtags は tags を正規化し、重複（大文字小文字の違いを含む）を除いて最大 max 個にする。
func normalizeHashtags(tags []string, max int) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, t := range tags {
		tag := NormalizeHashtag(t)
		key := strings.ToLower(tag)
		if tag == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, tag)
		if len(out) == max {
			break
		}
	}
	return out
}

// DefaultHashtags はカテゴリ名・サイト名から作るハッシュタグ。
func DefaultHashtags(category, siteName string) []string {
	return normalizeHashtags([]string{category, siteName}, maxInstagramHashtags)
}

// TemplatePosts は AI を使わずに、タイトル・抜粋・カテゴリから定型の告知文を作る。
// AI のAPIキーが未設定のメディアでも、告知文の下書きとしてすぐに使えるようにする。
func TemplatePosts(in PostInput) Posts {
	in = cleanInput(in)
	tags := DefaultHashtags(in.Category, in.SiteName)
	xTags := strings.Join(tags[:min(len(tags), maxXHashtags)], " ")

	head := "【記事】" + in.Title
	if in.Category != "" {
		head = "【" + in.Category + "】" + in.Title
	}
	// 1投稿目: 見出し・要約・ハッシュタグ。要約は収まる長さに詰める。
	first := head
	if in.Summary != "" {
		budget := XMaxLength - XLength(head+"\n\n\n\n"+xTags)
		if budget > 20 {
			first += "\n\n" + clipX(in.Summary, budget)
		}
	}
	if xTags != "" {
		first += "\n\n" + xTags
	}
	thread := []string{first}
	if in.URL != "" {
		thread = append(thread, "記事の全文はこちらから読めます。\n"+in.URL)
	}

	var ig strings.Builder
	ig.WriteString("【記事】" + in.Title)
	if in.Summary != "" {
		ig.WriteString("\n\n" + clipRunes(in.Summary, 400))
	}
	ig.WriteString("\n\n記事の全文はプロフィールのリンクから読めます。")
	if len(tags) > 0 {
		ig.WriteString("\n\n" + strings.Join(tags, " "))
	}

	var line strings.Builder
	line.WriteString("【新着】記事のお知らせ\n\n「" + in.Title + "」")
	if in.Summary != "" {
		line.WriteString("\n" + clipRunes(in.Summary, 120))
	}
	if in.URL != "" {
		line.WriteString("\n\n▼記事を読む\n" + in.URL)
	}

	return Finalize(Posts{XThread: thread, Instagram: ig.String(), LINE: line.String(), Hashtags: tags}, in)
}

// Finalize は AI が作った告知文を各 SNS の制約に合わせて整える。
//   - X: 空の投稿を除き、最大 maxThreadPosts 投稿・各投稿 XMaxLength 以内にする。記事 URL が無ければ最後に加える。
//   - Instagram: 開けない URL を除き、ハッシュタグが無ければ加え、maxInstagramRunes 以内にする。
//   - LINE: maxLINERunes 以内にし、記事 URL が無ければ末尾に加える。
//   - Hashtags: 正規化して重複を除く。空ならカテゴリ名・メディア名から作る。
func Finalize(p Posts, in PostInput) Posts {
	in = cleanInput(in)
	out := Posts{Hashtags: normalizeHashtags(p.Hashtags, maxInstagramHashtags)}
	if len(out.Hashtags) == 0 {
		out.Hashtags = DefaultHashtags(in.Category, in.SiteName)
	}

	thread := []string{}
	for _, post := range p.XThread {
		if post = strings.TrimSpace(post); post != "" {
			thread = append(thread, post)
		}
	}
	if len(thread) > maxThreadPosts {
		thread = thread[:maxThreadPosts]
	}
	if len(thread) == 0 {
		thread = append(thread, in.Title)
	}
	if in.URL != "" && !containsAny(thread, in.URL) {
		linkPost := "記事の全文はこちらから読めます。\n" + in.URL
		if len(thread) < maxThreadPosts {
			thread = append(thread, linkPost)
		} else {
			thread[len(thread)-1] = linkPost
		}
	}
	for i, post := range thread {
		thread[i] = clipX(post, XMaxLength)
	}
	out.XThread = thread

	ig := strings.TrimSpace(urlPattern.ReplaceAllString(p.Instagram, ""))
	if ig == "" {
		ig = in.Title
	}
	if !strings.ContainsAny(ig, "#＃") && len(out.Hashtags) > 0 {
		ig += "\n\n" + strings.Join(out.Hashtags, " ")
	}
	out.Instagram = clipRunes(ig, maxInstagramRunes)

	line := strings.TrimSpace(p.LINE)
	if line == "" {
		line = in.Title
	}
	if in.URL != "" && !strings.Contains(line, in.URL) {
		line = clipRunes(line, maxLINERunes) + "\n\n▼記事を読む\n" + in.URL
	} else if utf8.RuneCountInString(line) > maxLINERunes {
		line = clipRunes(urlPattern.ReplaceAllString(line, ""), maxLINERunes)
		if in.URL != "" {
			line += "\n\n▼記事を読む\n" + in.URL
		}
	}
	out.LINE = line
	return out
}

func cleanInput(in PostInput) PostInput {
	in.Title = clip(in.Title, maxTitleRunes)
	in.Category = clip(in.Category, maxCategoryRunes)
	in.SiteName = clip(in.SiteName, maxSiteRunes)
	in.URL = strings.TrimSpace(in.URL)
	in.Summary = clip(in.Summary, 2000)
	return in
}

func containsAny(posts []string, sub string) bool {
	for _, p := range posts {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}
