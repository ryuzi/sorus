package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"

	"sorus/internal/ai"
)

// isolateAIBaseURLPolicy は、サーバーモードのカスタム AI の送り先の制限（restrictAIBaseURL）を確かめる試験のために、
// AI の環境変数と、制限を外す環境変数（ALLOW_PRIVATE_AI_BASE_URL）を消す。
func isolateAIBaseURLPolicy(t *testing.T) {
	t.Helper()
	clearAIEnv(t)
	t.Setenv(allowPrivateAIBaseURLEnv, "")
}

// aiSettingsTest は「設定」→「AI」の接続テストを form で呼び、結果を返す。
func aiSettingsTest(t *testing.T, c *client, form url.Values) aiTestResponse {
	t.Helper()
	res := c.postAPI(aiSettingsTestPath, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	expectStatus(t, res, http.StatusOK)
	var out aiTestResponse
	if err := json.Unmarshal([]byte(res.Body), &out); err != nil {
		t.Fatalf("not JSON: %s", res.Body)
	}
	return out
}

// TestRestrictAIBaseURLPolicy は、カスタム AI の送り先を公開されたアドレスに限るのが、サーバーモードで
// ALLOW_PRIVATE_AI_BASE_URL=1 が無く、運用者が環境変数 AI_BASE_URL で指定した送り先でもないときだけであることを確かめる。
func TestRestrictAIBaseURLPolicy(t *testing.T) {
	isolateAIBaseURLPolicy(t)
	server, desktop := &Server{cfg: Config{}}, &Server{cfg: Config{Desktop: true}}
	const local = "http://localhost:11434/v1"
	if !server.restrictAIBaseURL(local) {
		t.Error("server mode must restrict the custom AI base URL by default")
	}
	if desktop.restrictAIBaseURL(local) {
		t.Error("desktop mode must allow a local LLM")
	}
	for _, v := range []string{"true", "yes", "0", " 2"} {
		t.Setenv(allowPrivateAIBaseURLEnv, v)
		if !server.restrictAIBaseURL(local) {
			t.Errorf("%s=%q must not lift the restriction (only 1 does)", allowPrivateAIBaseURLEnv, v)
		}
	}
	t.Setenv(allowPrivateAIBaseURLEnv, "1")
	if server.restrictAIBaseURL(local) {
		t.Errorf("%s=1 must lift the restriction", allowPrivateAIBaseURLEnv)
	}

	// 運用者が環境変数で指定した送り先は、管理画面から入力された送り先ではないため制限しない。ほかの送り先は制限する。
	t.Setenv(allowPrivateAIBaseURLEnv, "")
	t.Setenv("AI_BASE_URL", local+"/")
	for _, u := range []string{local, local + "/"} {
		if server.restrictAIBaseURL(u) {
			t.Errorf("restrictAIBaseURL(%q) with the same AI_BASE_URL = true, want false", u)
		}
	}
	for _, u := range []string{"http://localhost:11435/v1", "http://169.254.169.254/latest", ""} {
		if !server.restrictAIBaseURL(u) {
			t.Errorf("restrictAIBaseURL(%q) with another AI_BASE_URL = false, want true", u)
		}
	}
}

// サーバーモードでは、管理画面からカスタム AI のベース URL にこのサーバー自身・社内ネットワーク・クラウドのメタデータの
// アドレスを登録できない（保存も接続テストもしない）。公開されたホストの https は従来どおり登録できる。
func TestAISettingsServerModeRejectsPrivateBaseURL(t *testing.T) {
	isolateAIBaseURLPolicy(t)
	mock := newCompatMock(t, "読書会のお知らせ\n図書館で読書会")
	env := newTestEnv(t)
	adm := env.loggedIn(admin)

	for _, u := range []string{
		"http://localhost:11434/v1", "http://127.0.0.1:11434/v1", mock.URL + "/v1",
		"http://169.254.169.254/latest", "https://169.254.169.254/v1", "https://[::1]:8443/v1",
		"http://192.168.1.50:8000/v1", "https://10.0.0.5/v1", "https://172.16.0.2/v1", "https://100.64.0.1/v1",
		"https://ollama.localhost/v1", "https://[::ffff:127.0.0.1]/v1", "https://0.0.0.0/v1",
	} {
		form := url.Values{"provider": {"custom"}, "base_url_custom": {u}, "model_custom": {"llama3.1"}}
		res := adm.postForm(aiSettingsPath, form)
		expectStatus(t, res, http.StatusUnprocessableEntity)
		expectContains(t, res.Body, allowPrivateAIBaseURLEnv)
		out := aiSettingsTest(t, adm, url.Values{"test_provider": {"custom"}, "base_url_custom": {u}, "model_custom": {"llama3.1"}})
		if out.OK || !strings.Contains(out.Message, allowPrivateAIBaseURLEnv) {
			t.Errorf("connection test for %s = %+v, want the private address message", u, out)
		}
	}
	if _, saved := savedAI(t, env); saved {
		t.Fatal("a private base URL must not be saved")
	}
	if n := len(mock.authorizations()); n != 0 {
		t.Fatalf("the private AI server received %d requests, want 0", n)
	}

	// 外部ホストへの平文 http は、従来どおり https を求める文で断る。
	res := adm.postForm(aiSettingsPath, url.Values{"provider": {"custom"}, "base_url_custom": {"http://llm.example.com/v1"}})
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, "https://")

	expectRedirect(t, adm.postForm(aiSettingsPath, url.Values{"provider": {"custom"}, "base_url_custom": {"https://llm.example.com/v1/"},
		"model_custom": {"llama3.1"}}), aiSettingsPath+"?flash=ai_saved")
	if saved, _ := savedAI(t, env); saved.Provider != ai.KindCustom || saved.Providers[ai.KindCustom].BaseURL != "https://llm.example.com/v1" {
		t.Fatalf("saved = %+v, want the public https base URL", saved)
	}
}

// 以前に保存した（またはローカルを指す名前の）カスタム AI の送り先にも、サーバーモードでは接続しない。接続のたびに
// 解決したアドレスを確かめ（DNS リバインディング対策）、接続テストは理由と ALLOW_PRIVATE_AI_BASE_URL を案内する。
func TestAIServerModeRefusesPrivateConnection(t *testing.T) {
	isolateAIBaseURLPolicy(t)
	mock := newCompatMock(t, "読書会のお知らせ\n図書館で読書会")
	stored := `{"provider":"custom","providers":{"custom":{"base_url":"` + mock.URL + `/v1","model":"llama3.1"}}}`
	env := newTestEnvWith(t, Config{}, func(db *sqlx.DB) {
		if err := upsertSetting(db, aiSettingKey, stored); err != nil {
			t.Fatal(err)
		}
	})

	// 起動時に保存済みの設定から作ったプロバイダー（記事エディタの AI）は、私有アドレスへ接続しない。
	res, _ := env.loggedIn(studentA).postAI("suggest-titles", "本文")
	if res.StatusCode == http.StatusOK {
		t.Errorf("suggest-titles = %d, want an error", res.StatusCode)
	}
	saved, ok := savedAI(t, env)
	if !ok {
		t.Fatal("stored settings were not loaded")
	}
	p, _, err := env.server.buildAIProvider(saved)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.SuggestTitles(t.Context(), "本文"); !errors.Is(err, ai.ErrPrivateAddress) {
		t.Errorf("SuggestTitles err = %v, want ai.ErrPrivateAddress", err)
	}
	// 名前（localhost）で指定しても、解決したループバックのアドレスへは接続しない。
	host := strings.TrimPrefix(mock.URL, "http://127.0.0.1")
	p, _, err = env.server.buildAIProvider(savedAIConfig{
		Provider:  ai.KindCustom,
		Providers: map[string]savedProviderConfig{ai.KindCustom: {BaseURL: "http://localhost" + host + "/v1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.SuggestTitles(t.Context(), "本文"); !errors.Is(err, ai.ErrPrivateAddress) {
		t.Errorf("SuggestTitles via localhost err = %v, want ai.ErrPrivateAddress", err)
	}
	// 接続テスト（ベース URL の欄を送らず、保存済みの送り先で試す）。
	out := aiSettingsTest(t, env.loggedIn(admin), url.Values{"test_provider": {"custom"}})
	if out.OK || !strings.Contains(out.Message, allowPrivateAIBaseURLEnv) {
		t.Errorf("connection test = %+v, want the private address message", out)
	}
	if n := len(mock.authorizations()); n != 0 {
		t.Fatalf("the private AI server received %d requests, want 0", n)
	}
}

// デスクトップモードでは、この PC のローカル LLM（Ollama・LM Studio）へ従来どおり登録・接続できる。
func TestAISettingsDesktopModeAllowsLocalLLM(t *testing.T) {
	isolateAIBaseURLPolicy(t)
	mock := newCompatMock(t, "読書会のお知らせ\n図書館で読書会")
	env := newTestEnvWith(t, Config{Desktop: true}, nil)
	adm := env.loggedIn(admin)
	out := aiSettingsTest(t, adm, url.Values{"test_provider": {"custom"}, "base_url_custom": {mock.URL + "/v1"}, "model_custom": {"llama3.1"}})
	if !out.OK || len(out.Titles) == 0 {
		t.Fatalf("desktop connection test = %+v, want success", out)
	}
	expectRedirect(t, adm.postForm(aiSettingsPath, url.Values{"provider": {"custom"}, "base_url_custom": {"http://localhost:11434/v1"},
		"model_custom": {"llama3.1"}}), aiSettingsPath+"?flash=ai_saved")
	if n := len(mock.authorizations()); n != 1 {
		t.Fatalf("local AI server received %d requests, want 1", n)
	}
}

// サーバーモードでも、ALLOW_PRIVATE_AI_BASE_URL=1 なら同じサーバーで動かす LLM に登録・接続できる。運用者が環境変数
// AI_BASE_URL で指定したのと同じ送り先も使える（ほかの私有アドレスは引き続き断る）。
func TestAISettingsServerModePrivateOptIns(t *testing.T) {
	isolateAIBaseURLPolicy(t)
	mock := newCompatMock(t, "読書会のお知らせ\n図書館で読書会")
	env := newTestEnv(t)
	adm := env.loggedIn(admin)
	form := url.Values{"provider": {"custom"}, "test_provider": {"custom"}, "base_url_custom": {mock.URL + "/v1"}, "model_custom": {"llama3.1"}}

	t.Setenv(allowPrivateAIBaseURLEnv, "1")
	if out := aiSettingsTest(t, adm, form); !out.OK {
		t.Fatalf("connection test with %s=1 = %+v, want success", allowPrivateAIBaseURLEnv, out)
	}
	expectRedirect(t, adm.postForm(aiSettingsPath, form), aiSettingsPath+"?flash=ai_saved")

	t.Setenv(allowPrivateAIBaseURLEnv, "")
	t.Setenv("AI_PROVIDER", "custom")
	t.Setenv("AI_BASE_URL", mock.URL+"/v1")
	if out := aiSettingsTest(t, adm, form); !out.OK {
		t.Fatalf("connection test to the operator's AI_BASE_URL = %+v, want success", out)
	}
	expectRedirect(t, adm.postForm(aiSettingsPath, form), aiSettingsPath+"?flash=ai_saved")
	if n := len(mock.authorizations()); n != 2 {
		t.Fatalf("AI server received %d requests, want 2", n)
	}

	res := adm.postForm(aiSettingsPath, url.Values{"provider": {"custom"}, "base_url_custom": {"http://127.0.0.1:11434/v1"}})
	expectStatus(t, res, http.StatusUnprocessableEntity)
	expectContains(t, res.Body, allowPrivateAIBaseURLEnv)
}
