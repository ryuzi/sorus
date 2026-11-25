package server

import (
	"net/http"
	"testing"
)

// 管理画面の ?lang= は記事の言語（一覧の絞り込み・翻訳先）で、画面の表示言語ではない。表示言語は
// /admin/set-lang で決めた Cookie に従う（英語の記事を絞り込んだり翻訳を作ったりしても、画面が英語にならない）。
func TestAdminLangQueryDoesNotSwitchUILanguage(t *testing.T) {
	env := newTestEnv(t)
	ed := env.loggedIn(chief)
	for _, path := range []string{"/admin/articles?lang=en", "/admin/articles/new?lang=en"} {
		res := ed.get(path)
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, `<html lang="ja"`)
	}

	ed.useEnglish()
	res := ed.get("/admin/articles?lang=ja")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body, `<html lang="en"`)
}
