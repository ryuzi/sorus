// Package importer は WordPress のエクスポートファイル (WXR) を本 CMS に取り込む。
// WXR の解析、HTML から GFM Markdown への変換、外部画像の WebP 化、
// 記事・著者・カテゴリ・301 リダイレクトの DB 登録、検索インデックスへの登録を行う。
package importer

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// wpDateLayout は wp:post_date / wp:post_date_gmt の書式。
const wpDateLayout = "2006-01-02 15:04:05"

// ErrNotWXR は入力が WXR (RSS 2.0 の channel) でないことを示す。
var ErrNotWXR = errors.New("importer: input is not a WordPress WXR export")

// Export は WXR ファイル全体の解析結果。
type Export struct {
	SiteURL     string // wp:base_site_url（無ければ channel の link）
	Authors     []Author
	Categories  []Term
	Tags        []Term
	Posts       []Post            // post_type が post / page の項目（ファイル内の順）
	Attachments []Post            // post_type が attachment の項目
	LocalImages map[string][]byte // Markdown Zip 内の画像（Zip 内正規化パス → バイト列）
}

// Author は wp:author。
type Author struct {
	Login       string
	Email       string
	DisplayName string
}

// Term はカテゴリまたはタグ。Slug はパーセントデコード済み。
type Term struct {
	Name string
	Slug string
}

// Post は WXR の item 1件。
type Post struct {
	ID              int64
	Title           string
	Link            string // 旧パーマリンク（絶対 URL または /?p=123）
	Creator         string // dc:creator（著者のログイン名）
	ContentHTML     string
	ContentMarkdown string
	ExcerptHTML     string
	Date            time.Time // UTC。wp:post_date_gmt を優先し、無効なら wp:post_date を Location で解釈する
	Slug            string    // wp:post_name（パーセントデコード済み）
	Status          string    // wp:status（publish, draft, private, future, ...）
	Type            string    // wp:post_type（post, page, attachment, ...）
	ParentID        int64
	AttachmentURL   string
	Categories      []Term
	Tags            []Term
	Lang            string
	Description     string
	SourcePath      string            // Zip 内の元ファイルパス（相対画像パスの解決用）
	Meta            map[string]string // wp:postmeta（_thumbnail_id など）
}

// ParseOptions は Parse の設定。
type ParseOptions struct {
	// Location は wp:post_date_gmt が無いときに wp:post_date を解釈するタイムゾーン。既定は UTC。
	Location *time.Location
}

// xml 構造体。名前空間はバージョン（export/1.0〜1.2）で異なるため、ローカル名だけで照合する。
// content:encoded と excerpt:encoded はローカル名が同じなので XMLName の名前空間で振り分ける。
type xmlText struct {
	XMLName xml.Name
	Value   string `xml:",chardata"`
}

type xmlItemCategory struct {
	Domain   string `xml:"domain,attr"`
	Nicename string `xml:"nicename,attr"`
	Value    string `xml:",chardata"`
}

type xmlMeta struct {
	Key   string `xml:"meta_key"`
	Value string `xml:"meta_value"`
}

type xmlItem struct {
	Title         string            `xml:"title"`
	Link          string            `xml:"link"`
	GUID          string            `xml:"guid"`
	Creator       string            `xml:"creator"`
	Encoded       []xmlText         `xml:"encoded"`
	PostID        string            `xml:"post_id"`
	PostDate      string            `xml:"post_date"`
	PostDateGMT   string            `xml:"post_date_gmt"`
	PostName      string            `xml:"post_name"`
	Status        string            `xml:"status"`
	PostType      string            `xml:"post_type"`
	PostParent    string            `xml:"post_parent"`
	AttachmentURL string            `xml:"attachment_url"`
	Categories    []xmlItemCategory `xml:"category"`
	Meta          []xmlMeta         `xml:"postmeta"`
}

type xmlAuthor struct {
	Login       string `xml:"author_login"`
	Email       string `xml:"author_email"`
	DisplayName string `xml:"author_display_name"`
}

type xmlCategory struct {
	Nicename string `xml:"category_nicename"`
	Name     string `xml:"cat_name"`
}

type xmlTag struct {
	Slug string `xml:"tag_slug"`
	Name string `xml:"tag_name"`
}

// Parse は WXR をストリームで読み、channel 直下の著者・カテゴリ・タグと item を抽出する。
// item は1件ずつ復号するため、ファイル全体の DOM を保持しない。
// encoding/xml は外部実体を展開しないため、XXE の心配はない。
func Parse(r io.Reader, opts ParseOptions) (*Export, error) {
	loc := opts.Location
	if loc == nil {
		loc = time.UTC
	}
	d := xml.NewDecoder(r)
	exp := &Export{}
	sawChannel := false
	depth := 0 // channel 内の要素の深さ（channel 自身が 1）

	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("importer: parse wxr: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth == 0 {
				if t.Name.Local == "channel" {
					sawChannel = true
					depth = 1
				}
				continue
			}
			if depth > 1 {
				depth++
				continue
			}
			// channel 直下の要素。
			if err := exp.decodeChannelChild(d, t, loc); err != nil {
				return nil, err
			}
		case xml.EndElement:
			if depth > 0 {
				depth--
			}
		}
	}
	if !sawChannel {
		return nil, ErrNotWXR
	}
	return exp, nil
}

// decodeChannelChild は channel 直下の要素 start を読み切る。
func (exp *Export) decodeChannelChild(d *xml.Decoder, start xml.StartElement, loc *time.Location) error {
	wp := isWPNamespace(start.Name.Space)
	switch {
	case start.Name.Local == "item":
		var it xmlItem
		if err := d.DecodeElement(&it, &start); err != nil {
			return fmt.Errorf("importer: parse wxr item: %w", err)
		}
		p := it.toPost(loc)
		switch p.Type {
		case "post", "page":
			exp.Posts = append(exp.Posts, p)
		case "attachment":
			exp.Attachments = append(exp.Attachments, p)
		}
		return nil
	case wp && start.Name.Local == "author":
		var a xmlAuthor
		if err := d.DecodeElement(&a, &start); err != nil {
			return fmt.Errorf("importer: parse wxr author: %w", err)
		}
		exp.Authors = append(exp.Authors, Author{
			Login: strings.TrimSpace(a.Login), Email: strings.TrimSpace(a.Email), DisplayName: strings.TrimSpace(a.DisplayName),
		})
		return nil
	case wp && start.Name.Local == "category":
		var c xmlCategory
		if err := d.DecodeElement(&c, &start); err != nil {
			return fmt.Errorf("importer: parse wxr category: %w", err)
		}
		exp.Categories = append(exp.Categories, Term{Name: strings.TrimSpace(c.Name), Slug: decodeSlug(c.Nicename)})
		return nil
	case wp && start.Name.Local == "tag":
		var t xmlTag
		if err := d.DecodeElement(&t, &start); err != nil {
			return fmt.Errorf("importer: parse wxr tag: %w", err)
		}
		exp.Tags = append(exp.Tags, Term{Name: strings.TrimSpace(t.Name), Slug: decodeSlug(t.Slug)})
		return nil
	case wp && start.Name.Local == "base_site_url":
		var s string
		if err := d.DecodeElement(&s, &start); err != nil {
			return fmt.Errorf("importer: parse wxr base_site_url: %w", err)
		}
		exp.SiteURL = strings.TrimSpace(s)
		return nil
	case start.Name.Space == "" && start.Name.Local == "link":
		var s string
		if err := d.DecodeElement(&s, &start); err != nil {
			return fmt.Errorf("importer: parse wxr link: %w", err)
		}
		if exp.SiteURL == "" {
			exp.SiteURL = strings.TrimSpace(s)
		}
		return nil
	}
	return d.Skip()
}

func (it *xmlItem) toPost(loc *time.Location) Post {
	p := Post{
		ID:            parseInt(it.PostID),
		Title:         strings.TrimSpace(it.Title),
		Link:          strings.TrimSpace(it.Link),
		Creator:       strings.TrimSpace(it.Creator),
		Slug:          decodeSlug(it.PostName),
		Status:        strings.TrimSpace(it.Status),
		Type:          strings.TrimSpace(it.PostType),
		ParentID:      parseInt(it.PostParent),
		AttachmentURL: strings.TrimSpace(it.AttachmentURL),
		Date:          parseWPDate(it.PostDateGMT, it.PostDate, loc),
	}
	if p.Link == "" {
		p.Link = strings.TrimSpace(it.GUID)
	}
	for _, e := range it.Encoded {
		if strings.Contains(e.XMLName.Space, "/excerpt") {
			p.ExcerptHTML = e.Value
		} else {
			p.ContentHTML = e.Value
		}
	}
	for _, c := range it.Categories {
		term := Term{Name: strings.TrimSpace(c.Value), Slug: decodeSlug(c.Nicename)}
		if term.Name == "" {
			continue
		}
		switch c.Domain {
		case "category":
			p.Categories = append(p.Categories, term)
		case "post_tag":
			p.Tags = append(p.Tags, term)
		}
	}
	if len(it.Meta) > 0 {
		p.Meta = make(map[string]string, len(it.Meta))
		for _, m := range it.Meta {
			p.Meta[strings.TrimSpace(m.Key)] = strings.TrimSpace(m.Value)
		}
	}
	return p
}

// isWPNamespace は WordPress の wp: 名前空間（http://wordpress.org/export/1.x/）かを判定する。
func isWPNamespace(space string) bool {
	return strings.Contains(space, "wordpress.org/export/") && !strings.Contains(space, "/excerpt")
}

// parseWPDate は GMT の日時を優先し、未設定（0000-00-00 00:00:00 など）ならローカル日時を loc で解釈する。
// どちらも解釈できなければゼロ値を返す。
func parseWPDate(gmt, local string, loc *time.Location) time.Time {
	if t, err := time.ParseInLocation(wpDateLayout, strings.TrimSpace(gmt), time.UTC); err == nil && t.Year() > 1 {
		return t.UTC()
	}
	if t, err := time.ParseInLocation(wpDateLayout, strings.TrimSpace(local), loc); err == nil && t.Year() > 1 {
		return t.UTC()
	}
	return time.Time{}
}

// decodeSlug は WordPress がパーセントエンコードで保存する日本語スラッグを復号する。
func decodeSlug(s string) string {
	s = strings.TrimSpace(s)
	if dec, err := url.PathUnescape(s); err == nil {
		return dec
	}
	return s
}

func parseInt(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n
}
