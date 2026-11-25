package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// SocialPostWriter は記事の SNS 告知文を作る。API を呼び出すプロバイダー（Gemini・OpenAI・Claude・custom）が満たす。
// Provider に含めないのは、Provider だけを実装した既存の実装（テスト用の偽物など）を壊さないため。
// 呼び出し側は p.(SocialPostWriter) で取り出す。
type SocialPostWriter interface {
	// GenerateSocialPosts は記事から X のスレッド・Instagram のリード文・LINE の配信文・ハッシュタグを作る。
	// 文字数の上限や URL の有無などの整形は呼び出し側（social.Finalize）で行う。
	GenerateSocialPosts(ctx context.Context, req SocialPostRequest) (*SocialPosts, error)
}

// SocialPostRequest は告知文の材料。
type SocialPostRequest struct {
	Title    string
	Category string
	SiteName string
	// URL は記事の公開 URL。未公開なら空。
	URL string
	// Text は記事本文（Markdown またはプレーンテキスト）。
	Text string
}

// SocialPosts は AI が作った告知文。
type SocialPosts struct {
	XThread   []string `json:"x_thread"`
	Instagram string   `json:"instagram"`
	LINE      string   `json:"line"`
	Hashtags  []string `json:"hashtags"`
}

var (
	_ SocialPostWriter = (*Gemini)(nil)
	_ SocialPostWriter = (*OpenAI)(nil)
	_ SocialPostWriter = (*Claude)(nil)
)

const socialPostPrompt = "あなたはWebサイトの SNS 担当編集者です。以下の記事を、読者に届けるための告知文を作成してください。\n" +
	"出力は次の形の JSON オブジェクトだけにしてください。\n" +
	`{"x_thread": ["1投稿目", "2投稿目"], "instagram": "キャプション", "line": "配信文", "hashtags": ["#タグ1", "#タグ2"]}` + "\n" +
	"- x_thread: X（旧Twitter）のスレッド。2〜4投稿。1投稿目で記事の要点と読みどころを伝え、続く投稿で補足する。" +
	"各投稿は全角140字以内。1投稿目にハッシュタグを2〜3個付ける。記事URLがあれば最後の投稿にそのまま含める。\n" +
	"- instagram: Instagram のキャプション（リード文）。300〜600字。改行と絵文字を適度に使い、" +
	"最後に「記事の全文はプロフィールのリンクから」と案内してハッシュタグを5〜10個付ける。URL は書かない。\n" +
	"- line: LINE 公式アカウントの配信文。150字前後で、親しみやすく簡潔に。記事URLがあれば末尾に置く。\n" +
	"- hashtags: 記事に合うハッシュタグ（# 付き）を5〜10個。\n" +
	"- 記事に書かれていない事実・数値・人名を加えないでください。誇張や釣り見出しにしないでください。\n" +
	"- JSON 以外（解説・前置き・コードブロックの囲み）は出力しないでください。"

func (a *assistant) GenerateSocialPosts(ctx context.Context, req SocialPostRequest) (*SocialPosts, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "メディア名: %s\n", req.SiteName)
	if req.Category != "" {
		fmt.Fprintf(&b, "カテゴリ: %s\n", req.Category)
	}
	fmt.Fprintf(&b, "タイトル: %s\n", req.Title)
	if req.URL != "" {
		fmt.Fprintf(&b, "記事URL: %s\n", req.URL)
	} else {
		b.WriteString("記事URL: なし（未公開のため URL は書かないでください）\n")
	}
	b.WriteString("\n本文:\n" + req.Text)

	out, err := a.ask(ctx, socialPostPrompt, b.String())
	if err != nil {
		return nil, err
	}
	posts, err := parseSocialPosts(out)
	if err != nil {
		return nil, err
	}
	return posts, nil
}

// parseSocialPosts は応答から JSON オブジェクトを取り出して読む。前後の説明文やコードブロックの囲みは無視する。
func parseSocialPosts(out string) (*SocialPosts, error) {
	s := stripCodeFence(strings.TrimSpace(out))
	start, end := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if start < 0 || end < start {
		return nil, fmt.Errorf("%w: social posts are not JSON", ErrEmptyResponse)
	}
	var posts SocialPosts
	if err := json.Unmarshal([]byte(s[start:end+1]), &posts); err != nil {
		return nil, fmt.Errorf("%w: parse social posts: %v", ErrEmptyResponse, err)
	}
	if len(posts.XThread) == 0 && strings.TrimSpace(posts.Instagram) == "" && strings.TrimSpace(posts.LINE) == "" {
		return nil, ErrEmptyResponse
	}
	return &posts, nil
}
