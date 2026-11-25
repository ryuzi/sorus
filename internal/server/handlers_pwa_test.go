package server

import (
	"net/http"
	"testing"
)

func TestPWAOfflinePage(t *testing.T) {
	env := newTestEnv(t)
	res := env.newClient().get("/admin/offline")
	expectStatus(t, res, http.StatusOK)
	expectContains(t, res.Body,
		"<title>オフライン | sorus</title>",
		"オフライン",
		"通信が切断されています",
		"再接続を試す",
		"前の画面に戻る",
		"window.location.reload()",
	)

	t.Run("English", func(t *testing.T) {
		res := env.newClient().getAs("/admin/offline", "", map[string]string{"Accept-Language": "en"})
		expectStatus(t, res, http.StatusOK)
		expectContains(t, res.Body, "<title>Offline | sorus</title>", "Offline", "Connection lost", "Retry Connection", "Go back")
	})
}
