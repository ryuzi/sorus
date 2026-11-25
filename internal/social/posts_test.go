package social

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestXLength(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"hello", 5},
		{"こんにちは", 10},
		{"新歓 2026", 9},
		{"詳しくは https://example.ac.jp/articles/%E6%96%B0%E6%AD%93-2026 へ", 4*2 + 1 + XURLLength + 1 + 2},
		{"漢", 2},
		{"a\uFE0Fb", 2}, // 異体字セレクターは数えない
		{"“引用”", 1 + 4 + 1},
		{"a\nb", 3},
	}
	for _, tt := range tests {
		if got := XLength(tt.in); got != tt.want {
			t.Errorf("XLength(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestClipXKeepsURLs(t *testing.T) {
	url := "https://media.example.ac.jp/articles/long-slug"
	s := strings.Repeat("学生メディアの記事です。", 30) + "\n" + url
	got := clipX(s, XMaxLength)
	if XLength(got) > XMaxLength {
		t.Errorf("clipped length %d > %d", XLength(got), XMaxLength)
	}
	if !strings.HasSuffix(got, "\n"+url) || !strings.Contains(got, ellipsis) {
		t.Errorf("clipX should keep the URL and add an ellipsis: %q", got)
	}
	if short := "短い投稿"; clipX(short, XMaxLength) != short {
		t.Error("short post was changed")
	}
}

func TestNormalizeHashtag(t *testing.T) {
	tests := map[string]string{
		"研究":           "#研究",
		"#キャンパス ライフ":   "#キャンパスライフ",
		"＃Open-Campus": "#OpenCampus",
		"2026":         "",
		"新歓2026":       "#新歓2026",
		"!!!":          "",
		"ゼミ_紹介":        "#ゼミ_紹介",
		"":             "",
	}
	for in, want := range tests {
		if got := NormalizeHashtag(in); got != want {
			t.Errorf("NormalizeHashtag(%q) = %q, want %q", in, got, want)
		}
	}
	if got := normalizeHashtags([]string{"#研究", "研究", "#AI", "#ai", "", "x"}, 3); strings.Join(got, " ") != "#研究 #AI #x" {
		t.Errorf("normalizeHashtags = %q", got)
	}
	if got := DefaultHashtags("お知らせ", "サンプルサイト"); strings.Join(got, " ") != "#お知らせ #サンプルサイト" {
		t.Errorf("DefaultHashtags = %q", got)
	}
	if got := DefaultHashtags("", ""); len(got) != 0 {
		t.Errorf("DefaultHashtags(empty) = %q, want empty", got)
	}
}

func TestTemplatePosts(t *testing.T) {
	in := PostInput{
		Title:    "文学部の学生が「春の図書館」で出会った一冊",
		Category: "キャンパス ライフ",
		SiteName: "学生メディア",
		URL:      "https://media.example.ac.jp/articles/spring-library",
		Summary:  strings.Repeat("図書館の新しい読書スペースを取材しました。", 20),
	}
	p := TemplatePosts(in)
	if len(p.XThread) != 2 {
		t.Fatalf("x thread = %q, want 2 posts", p.XThread)
	}
	for i, post := range p.XThread {
		if XLength(post) > XMaxLength {
			t.Errorf("post %d length %d > %d", i, XLength(post), XMaxLength)
		}
	}
	if !strings.HasPrefix(p.XThread[0], "【キャンパス ライフ】") || !strings.Contains(p.XThread[0], "#キャンパスライフ #学生メディア") {
		t.Errorf("first post = %q", p.XThread[0])
	}
	if !strings.Contains(p.XThread[1], in.URL) {
		t.Errorf("link post = %q", p.XThread[1])
	}
	if strings.Contains(p.Instagram, "https://") || !strings.Contains(p.Instagram, "プロフィールのリンク") || !strings.Contains(p.Instagram, "#学生メディア") {
		t.Errorf("instagram = %q", p.Instagram)
	}
	if !strings.Contains(p.LINE, "「"+in.Title+"」") || !strings.HasSuffix(p.LINE, in.URL) {
		t.Errorf("line = %q", p.LINE)
	}
	if strings.Join(p.Hashtags, " ") != "#キャンパスライフ #学生メディア" {
		t.Errorf("hashtags = %q", p.Hashtags)
	}

	// 未公開（URL 無し）の記事は、リンクの投稿・案内を付けない。
	in.URL = ""
	p = TemplatePosts(in)
	if len(p.XThread) != 1 || strings.Contains(p.LINE, "記事を読む") {
		t.Errorf("unpublished posts = %+v", p)
	}

	// カテゴリが無くサイト名もハッシュタグにならないときは、ハッシュタグも末尾の空行も付けない。
	p = TemplatePosts(PostInput{
		Title:    "記事タイトル",
		Category: "",
		SiteName: "2026",
		URL:      "https://example.com/articles/post",
		Summary:  "記事の抜粋です。",
	})
	if len(p.Hashtags) != 0 {
		t.Errorf("hashtags = %q, want empty", p.Hashtags)
	}
	if strings.ContainsAny(p.XThread[0], "#＃") || strings.HasSuffix(p.XThread[0], "\n") {
		t.Errorf("first post = %q, want no # and no trailing newline", p.XThread[0])
	}
	if strings.ContainsAny(p.Instagram, "#＃") || strings.HasSuffix(p.Instagram, "\n") {
		t.Errorf("instagram = %q, want no # and no trailing newline", p.Instagram)
	}
}

func TestFinalizeEnforcesPlatformRules(t *testing.T) {
	in := PostInput{Title: "タイトル", Category: "研究", SiteName: "学生メディア", URL: "https://media.example.ac.jp/articles/x"}
	p := Finalize(Posts{
		XThread:   []string{"  ", strings.Repeat("長い投稿", 100), "2", "3", "4", "5", "6"},
		Instagram: "キャプション https://media.example.ac.jp/articles/x を見てね",
		LINE:      strings.Repeat("あ", 1200),
		Hashtags:  []string{"研究", "#研究", "2026"},
	}, in)
	if len(p.XThread) != maxThreadPosts {
		t.Fatalf("thread has %d posts, want %d", len(p.XThread), maxThreadPosts)
	}
	if XLength(p.XThread[0]) > XMaxLength {
		t.Errorf("post 0 length %d", XLength(p.XThread[0]))
	}
	if last := p.XThread[len(p.XThread)-1]; !strings.Contains(last, in.URL) {
		t.Errorf("last post %q should link the article", last)
	}
	if strings.Contains(p.Instagram, "https://") || !strings.Contains(p.Instagram, "#研究") {
		t.Errorf("instagram = %q", p.Instagram)
	}
	if !strings.HasSuffix(p.LINE, in.URL) || utf8.RuneCountInString(p.LINE) > maxLINERunes+len("\n\n▼記事を読む\n")+len(in.URL) {
		t.Errorf("line (%d runes) = %.40q...", utf8.RuneCountInString(p.LINE), p.LINE)
	}
	if strings.Join(p.Hashtags, " ") != "#研究" {
		t.Errorf("hashtags = %q", p.Hashtags)
	}

	// 空の応答でも、タイトルとハッシュタグで最低限の告知文にする。
	p = Finalize(Posts{}, in)
	if len(p.XThread) != 2 || p.XThread[0] != "タイトル" || p.Instagram == "" || p.LINE == "" || len(p.Hashtags) == 0 {
		t.Errorf("empty posts = %+v", p)
	}
}
