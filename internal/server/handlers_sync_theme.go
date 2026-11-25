package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"sorus/internal/theme"
	"sorus/static"
)

// 公開サーバー側の、独自テーマのファイルの受け取り（sync_settings の ThemePackage）。
//
// PC で取り込んだ独自テーマは、PC の「公開サーバーへ反映する」で、設定とともにテーマのファイルごと送られてくる
// （handlers_remote_theme.go）。公開サーバーには管理画面が無いため、テーマの取り込み（handlers_theme_custom.go）と
// 同じ検査・上限・置き場所（Config.ThemesDir）で受け取り、再起動せずにカタログを読み直して使う。
// 公開サーバーが外部の URL へテーマを取りに行くことはない（送られてきた内容だけを使う）。

// 失敗したとき: 受け取りの検査・展開・配置・カタログの読み直し・設定の保存のどれかに失敗したら、配置したテーマを
// 元に戻し、設定とテーマを変えていないことを PC に伝える。元に戻せなかったとき（theme.ErrNotRestored）と、途中で
// プロセスが止まったときは、このテーマのファイルだけが無いか新しい版になることがある。元のデータは PC にあるため、
// PC から「公開サーバーへ反映する」をもう一度押して揃える（公開サーバーは自動の復旧をしない。前の版を含む
// 作業ディレクトリは、起動時の theme.CleanupStaleStaging も消さずに残す）。

// syncThemeDigest は独自テーマの Zip の SHA-256（16進数の小文字）を返す。
func syncThemeDigest(archive []byte) string {
	sum := sha256.Sum256(archive)
	return hex.EncodeToString(sum[:])
}

// stageSyncTheme は sync_settings で送られた独自テーマ pkg を検査し、作業ディレクトリに展開・検証する。
// 配置はまだ行わない（commitSyncTheme）。theme（Settings.Theme）と ID が違う・空・上限超え・壊れている・危険な
// エントリがある・テーマの形式が正しくない・ビルトインテーマの ID の場合は、何も置かずに 422 を返す。
// 呼び出し側は themeMu を持ち、成功したら Close する。
func (s *Server) stageSyncTheme(c echo.Context, themeID string, pkg *SyncThemePackage) (*theme.StagedTheme, error) {
	id := strings.TrimSpace(pkg.ID)
	if id == "" || id != strings.TrimSpace(themeID) {
		return nil, echo.NewHTTPError(http.StatusUnprocessableEntity, "送られた独自テーマ（theme_package）の ID が theme と一致しません")
	}
	if len(pkg.Archive) == 0 {
		return nil, echo.NewHTTPError(http.StatusUnprocessableEntity, fmt.Sprintf("独自テーマ「%s」のファイルが空です", id))
	}
	if len(pkg.Archive) > theme.MaxArchiveSize {
		return nil, syncThemeTooLarge(id)
	}
	if !strings.EqualFold(strings.TrimSpace(pkg.SHA256), syncThemeDigest(pkg.Archive)) {
		return nil, echo.NewHTTPError(http.StatusUnprocessableEntity,
			fmt.Sprintf("独自テーマ「%s」のファイルが途中で壊れました。もう一度「公開サーバーへ反映する」を押してください", id))
	}
	st, err := theme.StageZipAs(bytes.NewReader(pkg.Archive), int64(len(pkg.Archive)), s.cfg.ThemesDir, id)
	switch {
	case err == nil:
		return st, nil
	case errors.Is(err, theme.ErrBuiltinTheme):
		return nil, echo.NewHTTPError(http.StatusUnprocessableEntity,
			fmt.Sprintf("独自テーマ「%s」は標準テーマと同じ ID のため、公開サーバーで使えません。テーマのフォルダ名を変えて取り込み直してください", id))
	case errors.Is(err, theme.ErrArchiveTooLarge):
		return nil, syncThemeTooLarge(id)
	case errors.Is(err, theme.ErrUnsafeArchive):
		c.Logger().Warnf("sync settings: theme %q: %v", id, err)
		return nil, echo.NewHTTPError(http.StatusUnprocessableEntity,
			fmt.Sprintf("独自テーマ「%s」に不正なファイルパス（親フォルダへの移動・絶対パス・シンボリックリンクなど）が含まれているため、公開サーバーで受け付けませんでした", id))
	case errors.Is(err, theme.ErrInvalidTheme):
		c.Logger().Warnf("sync settings: theme %q: %v", id, err)
		return nil, echo.NewHTTPError(http.StatusUnprocessableEntity,
			fmt.Sprintf("独自テーマ「%s」の形式（theme.json・theme.css）が正しくないため、公開サーバーで受け付けませんでした", id))
	}
	c.Logger().Errorf("sync settings: stage theme %q: %v", id, err)
	return nil, echo.NewHTTPError(http.StatusInternalServerError,
		fmt.Sprintf("公開サーバーで独自テーマ「%s」のファイルを保存できませんでした。設定とテーマは変更していません。公開サーバーの空き容量を確かめてください", id))
}

// syncThemeTooLarge は独自テーマのファイルが上限を超えたときの 422 を返す。
func syncThemeTooLarge(id string) error {
	return echo.NewHTTPError(http.StatusUnprocessableEntity,
		fmt.Sprintf("独自テーマ「%s」のファイルが上限（Zip で %dMB、展開後 %dMB、%d ファイル）を超えているため、公開サーバーで受け付けませんでした",
			id, theme.MaxArchiveSize>>20, theme.MaxExtractedSize>>20, theme.MaxArchiveFiles))
}

// commitSyncTheme は検証済みのテーマを配置し、カタログを読み直して、そのテーマがカタログに入ったことを確かめる。
// 失敗したら配置を元に戻す（rollbackSyncTheme）。設定のトランザクションを確定する前に呼ぶ。
func (s *Server) commitSyncTheme(c echo.Context, st *theme.StagedTheme) error {
	id := st.Theme.ID
	if err := st.Commit(); err != nil {
		c.Logger().Errorf("sync settings: install theme %q: %v", id, err)
		if errors.Is(err, theme.ErrNotRestored) {
			// 前の版も元の場所に戻せなかった（配置先にテーマが無い）。カタログをディスクに合わせる。
			s.reloadThemesAfterFailure(c, id)
			return echo.NewHTTPError(http.StatusInternalServerError,
				syncThemeNotRestoredMessage(id, "独自テーマ「%s」を配置できず、前の版も元の場所に戻せませんでした。"))
		}
		return echo.NewHTTPError(http.StatusInternalServerError,
			fmt.Sprintf("公開サーバーで独自テーマ「%s」を配置できませんでした。設定とテーマは変更していません。もう一度お試しください", id))
	}
	err := s.themeCatalog().Reload(static.FS)
	if err == nil {
		if t, ok := s.themeCatalog().Get(id); ok && t.IsCustom {
			return nil
		}
		err = fmt.Errorf("theme %q is not in the catalog after reload", id)
	}
	c.Logger().Errorf("sync settings: reload after installing theme %q: %v", id, err)
	return s.rollbackSyncTheme(c, st)
}

// rollbackSyncTheme は配置したテーマを元に戻してカタログを読み直し、設定もテーマも変えていないことを伝える 500 を返す。
// 元に戻せなかったときは、設定は変えていないこと・テーマのファイルが無いか新しい版になっていること・もう一度
// 反映すれば揃うことを伝える。
func (s *Server) rollbackSyncTheme(c echo.Context, st *theme.StagedTheme) error {
	id := st.Theme.ID
	rerr := st.Rollback()
	s.reloadThemesAfterFailure(c, id)
	if rerr != nil {
		c.Logger().Errorf("sync settings: roll back theme %q: %v", id, rerr)
		return echo.NewHTTPError(http.StatusInternalServerError,
			syncThemeNotRestoredMessage(id, "設定を保存できず、独自テーマ「%s」も元に戻せませんでした。"))
	}
	return echo.NewHTTPError(http.StatusInternalServerError,
		fmt.Sprintf("公開サーバーで設定を保存できませんでした。設定と独自テーマ「%s」は変更していません。もう一度お試しください", id))
}

// reloadThemesAfterFailure は、配置・取り消しに失敗したあと、カタログをディスクの状態に読み直す。
func (s *Server) reloadThemesAfterFailure(c echo.Context, id string) {
	if err := s.themeCatalog().Reload(static.FS); err != nil {
		c.Logger().Errorf("sync settings: reload after the failure of theme %q: %v", id, err)
	}
}

// syncThemeNotRestoredMessage は、配置したテーマを元に戻せなかった（theme.ErrNotRestored）ときの PC への説明を返す。
// what は何に失敗したか（%s にテーマ ID が入る）。設定のトランザクションは確定していない。
func syncThemeNotRestoredMessage(id, what string) string {
	return "公開サーバーで" + fmt.Sprintf(what, id) + "設定は変更していません。公開サーバーにあるこのテーマのファイルが無いか新しい版になっているため、" +
		"公開サイトの見た目が崩れることがあります。もう一度「公開サーバーへ反映する」を押すと、設定とテーマがこのPCの内容で揃います"
}
