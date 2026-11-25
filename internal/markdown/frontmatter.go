package markdown

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

const fmDelimiter = "---"

// ErrInvalidFrontmatter はフロントマターが YAML のマッピングとして解釈できない場合に返る。
var ErrInvalidFrontmatter = errors.New("markdown: invalid frontmatter")

// ParseFrontmatter は YAML フロントマターと本文 Markdown を分離してパースする
//
// フロントマターは先頭行の "---" から次の "---" または "..." の行までとする。
// 先頭が "---" でない、または閉じ区切りがない場合はフロントマターなしとみなし、
// 空の meta と content 全体を本文として返す（先頭の "---" は Markdown の水平線として有効なため）。
// 改行コードは LF に正規化される。
func ParseFrontmatter(content string) (meta map[string]interface{}, bodyMD string, err error) {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	meta = map[string]interface{}{}

	rest := strings.TrimPrefix(content, "\uFEFF")
	first, rest, _ := strings.Cut(rest, "\n")
	if strings.TrimRight(first, " \t") != fmDelimiter {
		return meta, content, nil
	}

	var yamlLines []string
	for rest != "" {
		var line string
		line, rest, _ = strings.Cut(rest, "\n")
		if t := strings.TrimRight(line, " \t"); t == fmDelimiter || t == "..." {
			src := strings.Join(yamlLines, "\n")
			if strings.TrimSpace(src) != "" {
				if err := yaml.Unmarshal([]byte(src), &meta); err != nil {
					return nil, "", fmt.Errorf("%w: %v", ErrInvalidFrontmatter, err)
				}
				if meta == nil {
					meta = map[string]interface{}{}
				}
			}
			return meta, rest, nil
		}
		yamlLines = append(yamlLines, line)
	}
	return meta, content, nil
}

// SerializeFrontmatter は meta を YAML フロントマターとして本文の先頭に付けた文字列を返す。
// meta が空の場合は本文のみを返す。ParseFrontmatter で元の meta と本文に戻せる。
func SerializeFrontmatter(meta map[string]interface{}, bodyMD string) (string, error) {
	if len(meta) == 0 {
		return bodyMD, nil
	}
	var buf bytes.Buffer
	buf.WriteString(fmDelimiter + "\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(meta); err != nil {
		return "", fmt.Errorf("markdown: encode frontmatter: %w", err)
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("markdown: encode frontmatter: %w", err)
	}
	buf.WriteString(fmDelimiter + "\n")
	buf.WriteString(bodyMD)
	return buf.String(), nil
}
