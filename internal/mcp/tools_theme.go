package mcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"sorus/internal/auth"
	"sorus/internal/theme"
	"sorus/static"
)

// themeInstallTimeout は URL からテーマを取り込むときの、ダウンロードを含む上限時間。
const themeInstallTimeout = 45 * time.Second

// themeDownloadArchive は URL からアーカイブをダウンロードする（テストで差し替え可能）。
var themeDownloadArchive = theme.DownloadArchive

// themeInfo は cms_list_themes・cms_install_theme が返すテーマ1つの情報。
type themeInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	NameEN        string `json:"name_en"`
	Description   string `json:"description"`
	DescriptionEN string `json:"description_en"`
	IsCustom      bool   `json:"is_custom"`
	SourceURL     string `json:"source_url"`
	ColorScheme   string `json:"color_scheme"`
	// Author は作者。theme.json は未知の項目を受け付けない（theme.ValidateThemeDir）ため、現状は常に空文字。
	Author string `json:"author"`
}

type listThemesResult struct {
	ActiveThemeID string      `json:"active_theme_id"`
	Themes        []themeInfo `json:"themes"`
}

type installThemeArgs struct {
	URL       string `json:"url"`
	ZipBase64 string `json:"zip_base64"`
}

type installThemeResult struct {
	Status  string    `json:"status"`
	Theme   themeInfo `json:"theme"`
	Message string    `json:"message"`
}

type themeIDArgs struct {
	ThemeID string `json:"theme_id"`
}

type themeIDResult struct {
	Status  string `json:"status"`
	ThemeID string `json:"theme_id"`
	Message string `json:"message"`
}

// themeTools はテーマ管理のツールを返す。
func (s *Server) themeTools() []*tool {
	return []*tool{
		{
			info: toolInfo{
				Name:        "cms_list_themes",
				Title:       "テーマの一覧",
				Description: "公開サイトに使えるテーマ（ビルトインと取り込んだ独自テーマ）の一覧と、現在適用中のテーマ ID（active_theme_id）を返します。",
				InputSchema: objectSchema(map[string]any{}),
				Annotations: &toolAnnotations{Title: "テーマの一覧", ReadOnlyHint: true, IdempotentHint: true},
			},
			run: s.toolListThemes,
		},
		{
			info: toolInfo{
				Name:  "cms_install_theme",
				Title: "テーマの取り込み",
				Description: "GitHub・GitLab のリポジトリ URL、https の .zip の URL、または Base64 で符号化した Zip から独自テーマを取り込みます。" +
					"url と zip_base64 のどちらか一方を指定してください。同じ ID の独自テーマは置き換えます（適用はしません。適用は cms_activate_theme）。管理者のみ実行できます。",
				InputSchema: objectSchema(map[string]any{
					"url":        stringProp("取り込み元の URL（GitHub・GitLab のリポジトリ、または https の .zip）"),
					"zip_base64": stringProp("テーマの Zip を Base64 で符号化した文字列（theme.json と theme.css を含むこと）"),
				}),
				Annotations: &toolAnnotations{Title: "テーマの取り込み", OpenWorldHint: true},
			},
			run: s.toolInstallTheme,
		},
		{
			info: toolInfo{
				Name:        "cms_activate_theme",
				Title:       "テーマの適用",
				Description: "公開サイトに適用するテーマを切り替えます。theme_id は cms_list_themes で確認してください。管理者のみ実行できます。",
				InputSchema: objectSchema(map[string]any{
					"theme_id": stringProp("適用するテーマの ID"),
				}, "theme_id"),
				Annotations: &toolAnnotations{Title: "テーマの適用", IdempotentHint: true},
			},
			run: s.toolActivateTheme,
		},
		{
			info: toolInfo{
				Name:        "cms_uninstall_theme",
				Title:       "テーマの削除",
				Description: "取り込んだ独自テーマを削除します。ビルトインテーマと適用中のテーマは削除できません。管理者のみ実行できます。",
				InputSchema: objectSchema(map[string]any{
					"theme_id": stringProp("削除する独自テーマの ID"),
				}, "theme_id"),
				Annotations: &toolAnnotations{Title: "テーマの削除", DestructiveHint: true},
			},
			run: s.toolUninstallTheme,
		},
	}
}

// themesDir は独自テーマの保存先を返す。Config.ThemesDir が空ならカタログの保存先を使う。
func (s *Server) themesDir() string {
	if d := strings.TrimSpace(s.cfg.ThemesDir); d != "" {
		return d
	}
	return s.themes.CustomDir()
}

// storedThemeID は settings に保存したテーマ ID を返す。未設定なら theme.Default。
func (s *Server) storedThemeID(ctx context.Context) (string, error) {
	var v string
	err := s.db.GetContext(ctx, &v, `SELECT value FROM settings WHERE key = 'theme'`)
	if errors.Is(err, sql.ErrNoRows) {
		return theme.Default, nil
	}
	if err != nil {
		return "", fmt.Errorf("read theme setting: %w", err)
	}
	if v = strings.TrimSpace(v); v == "" {
		return theme.Default, nil
	}
	return v, nil
}

// themeChanged は Config.OnThemeChanged を呼ぶ。テーマの排他を解いた後に呼ぶこと。
func (s *Server) themeChanged() {
	if s.cfg.OnThemeChanged != nil {
		s.cfg.OnThemeChanged()
	}
}

func newThemeInfo(t theme.Theme) themeInfo {
	return themeInfo{
		ID:            t.ID,
		Name:          t.Label("ja"),
		NameEN:        t.Name["en"],
		Description:   t.Summary("ja"),
		DescriptionEN: t.Description["en"],
		IsCustom:      t.IsCustom,
		SourceURL:     t.SourceURL,
		ColorScheme:   t.ColorScheme,
	}
}

func (s *Server) toolListThemes(ctx context.Context, _ *auth.User, raw json.RawMessage) (any, error) {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return nil, err
	}
	stored, err := s.storedThemeID(ctx)
	if err != nil {
		return nil, err
	}
	all := s.themes.All()
	out := listThemesResult{ActiveThemeID: s.themes.Resolve(stored).ID, Themes: make([]themeInfo, 0, len(all))}
	for _, t := range all {
		out.Themes = append(out.Themes, newThemeInfo(t))
	}
	return out, nil
}

func adminOnly(user *auth.User, message string) error {
	if user == nil || !user.HasRole(auth.RoleAdmin) {
		return toolErrorf("%s", message)
	}
	return nil
}

func (s *Server) toolInstallTheme(ctx context.Context, user *auth.User, raw json.RawMessage) (any, error) {
	if err := adminOnly(user, "テーマを取り込む権限がありません（管理者のみ）"); err != nil {
		return nil, err
	}
	var args installThemeArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	rawURL := strings.TrimSpace(args.URL)
	encoded := strings.TrimSpace(args.ZipBase64)
	if (rawURL == "") == (encoded == "") {
		return nil, toolErrorf("url と zip_base64 のどちらか一方を指定してください")
	}
	dir := s.themesDir()
	if dir == "" {
		return nil, toolErrorf("独自テーマの保存先が設定されていないため、テーマを取り込めません")
	}
	var install func() (*theme.Theme, error)
	if rawURL != "" {
		dlCtx, cancel := context.WithTimeout(ctx, themeInstallTimeout)
		defer cancel()
		arc, err := themeDownloadArchive(dlCtx, rawURL)
		if err != nil {
			return nil, s.themeInstallError(err, true)
		}
		install = func() (*theme.Theme, error) {
			return arc.Install(dir)
		}
	} else {
		if base64.StdEncoding.DecodedLen(len(encoded)) > theme.MaxArchiveSize+3 {
			return nil, toolErrorf("Zip が大きすぎます（上限 %dMB）", theme.MaxArchiveSize>>20)
		}
		b, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, toolErrorf("zip_base64 を Base64 として読めません")
		}
		if len(b) == 0 {
			return nil, toolErrorf("zip_base64 が空です")
		}
		if len(b) > theme.MaxArchiveSize {
			return nil, toolErrorf("Zip が大きすぎます（上限 %dMB）", theme.MaxArchiveSize>>20)
		}
		install = func() (*theme.Theme, error) {
			return theme.InstallFromZip(bytes.NewReader(b), int64(len(b)), dir)
		}
	}

	s.themeMu.Lock()
	t, err := install()
	if err == nil {
		if rerr := s.themes.Reload(static.FS); rerr != nil {
			s.themeMu.Unlock()
			return nil, fmt.Errorf("reload themes after install: %w", rerr)
		}
	} else if errors.Is(err, theme.ErrNotRestored) {
		// 前の版を元の場所に戻せなかった（配置先にテーマが無い）ので、カタログをディスクの状態に合わせる。
		if rerr := s.themes.Reload(static.FS); rerr != nil {
			s.logger.Printf("mcp: reload themes after a failed install: %v", rerr)
		}
	}
	var info themeInfo
	if err == nil {
		if got, ok := s.themes.Get(t.ID); ok {
			info = newThemeInfo(got)
		} else {
			info = newThemeInfo(*t)
		}
	}
	s.themeMu.Unlock()
	if err != nil {
		return nil, s.themeInstallError(err, rawURL != "")
	}
	s.themeChanged()
	return installThemeResult{
		Status:  "installed",
		Theme:   info,
		Message: fmt.Sprintf("テーマ「%s」（%s）を取り込みました。公開サイトに適用するには cms_activate_theme を使ってください。", info.Name, info.ID),
	}, nil
}

// themeInstallError は取り込みの失敗を、AI エージェントに返せるメッセージにする（内部のパスは含めない）。
func (s *Server) themeInstallError(err error, fromURL bool) error {
	switch {
	case errors.Is(err, theme.ErrNotRestored):
		s.logger.Printf("mcp: theme install: %v", err)
		return toolErrorf("テーマを置き換えられず、前の版も元の場所に戻せませんでした。このテーマはいま使えません。前の版のファイルは消さずに、テーマのフォルダの中の作業用フォルダに残しています（自動では戻りません）。もう一度取り込むと、このテーマを使えるようになります")
	case errors.Is(err, theme.ErrBuiltinTheme):
		return toolErrorf("ビルトインテーマと同じ ID のテーマは取り込めません。Zip の最上位フォルダ名か theme.json の name.en を変えてください")
	case errors.Is(err, theme.ErrArchiveTooLarge):
		return toolErrorf("Zip が上限（%dMB、展開後 %dMB、%d ファイル）を超えています", theme.MaxArchiveSize>>20, theme.MaxExtractedSize>>20, theme.MaxArchiveFiles)
	case errors.Is(err, theme.ErrUnsafeArchive):
		return toolErrorf("Zip に安全でないエントリ（親フォルダへの脱出・絶対パス・シンボリックリンクなど）が含まれているため取り込めません")
	case errors.Is(err, theme.ErrInvalidTheme):
		// 検証の理由（Zip でない・theme.json の不備・CSS の外部参照など）はテーマを直すのに要るため返す。
		// 理由はアーカイブ内の相対パスとテーマ ID だけで、サーバーのパスは含まない。
		return toolErrorf("テーマの内容が正しくありません（Zip 形式・theme.json・theme.css を確認してください）: %v", err)
	case errors.Is(err, theme.ErrUnsupportedURL), errors.Is(err, theme.ErrBlockedURL):
		return toolErrorf("この URL からは取り込めません。GitHub・GitLab のリポジトリ URL か、https の .zip の URL を指定してください（内部ネットワークの URL は使えません）")
	case errors.Is(err, context.DeadlineExceeded):
		return toolErrorf("テーマのダウンロードが時間内に終わりませんでした")
	}
	s.logger.Printf("mcp: theme install: %v", err)
	if fromURL {
		return toolErrorf("URL からテーマを取得できませんでした。URL が公開されているか確認してください")
	}
	return toolErrorf("Zip ファイルを取り込めませんでした")
}

func (s *Server) toolActivateTheme(ctx context.Context, user *auth.User, raw json.RawMessage) (any, error) {
	if err := adminOnly(user, "テーマを切り替える権限がありません（管理者のみ）"); err != nil {
		return nil, err
	}
	var args themeIDArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(args.ThemeID)
	if id == "" {
		return nil, toolErrorf("theme_id を指定してください")
	}
	// 取り込み・削除と同じ排他の中で、カタログにあることの確認と保存を行う（削除中のテーマを適用しない）。
	s.themeMu.Lock()
	t, ok := s.themes.Get(id)
	if !ok {
		s.themeMu.Unlock()
		return nil, toolErrorf("テーマ %q はありません。cms_list_themes で ID を確認してください", id)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES ('theme', ?, CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, t.ID)
	s.themeMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("save theme setting: %w", err)
	}
	s.themeChanged()
	return themeIDResult{
		Status:  "activated",
		ThemeID: t.ID,
		Message: fmt.Sprintf("公開サイトのテーマを「%s」（%s）に切り替えました。", t.Label("ja"), t.ID),
	}, nil
}

func (s *Server) toolUninstallTheme(ctx context.Context, user *auth.User, raw json.RawMessage) (any, error) {
	if err := adminOnly(user, "テーマを削除する権限がありません（管理者のみ）"); err != nil {
		return nil, err
	}
	var args themeIDArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(args.ThemeID)
	if id == "" {
		return nil, toolErrorf("theme_id を指定してください")
	}
	s.themeMu.Lock()
	err := s.uninstallThemeLocked(ctx, id)
	s.themeMu.Unlock()
	if err != nil {
		return nil, err
	}
	s.themeChanged()
	return themeIDResult{
		Status:  "uninstalled",
		ThemeID: id,
		Message: fmt.Sprintf("独自テーマ %s を削除しました。", id),
	}, nil
}

// uninstallThemeLocked は themeMu を持った状態で独自テーマ id を削除し、カタログを読み直す。
func (s *Server) uninstallThemeLocked(ctx context.Context, id string) error {
	t, ok := s.themes.Get(id)
	if !ok {
		return toolErrorf("テーマ %q はありません。cms_list_themes で ID を確認してください", id)
	}
	if !t.IsCustom {
		return toolErrorf("ビルトインテーマ %q は削除できません", t.ID)
	}
	stored, err := s.storedThemeID(ctx)
	if err != nil {
		return err
	}
	if stored == t.ID {
		return toolErrorf("テーマ %q は公開サイトに適用中のため削除できません。先に cms_activate_theme で別のテーマに切り替えてください", t.ID)
	}
	if err := theme.UninstallCustomTheme(s.themesDir(), t.ID); err != nil {
		switch {
		case errors.Is(err, theme.ErrBuiltinTheme):
			return toolErrorf("ビルトインテーマ %q は削除できません", t.ID)
		case errors.Is(err, theme.ErrThemeNotFound):
			return toolErrorf("独自テーマ %q が見つかりません", t.ID)
		}
		return fmt.Errorf("uninstall theme %s: %w", t.ID, err)
	}
	if err := s.themes.Reload(static.FS); err != nil {
		return fmt.Errorf("reload themes after uninstall: %w", err)
	}
	return nil
}
