package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/jmoiron/sqlx"
	"golang.org/x/crypto/acme/autocert"

	"sorus/internal/ai"
	"sorus/internal/article"
	"sorus/internal/auth"
	"sorus/internal/backup"
	"sorus/internal/database"
	"sorus/internal/desktop"
	"sorus/internal/importer"
	"sorus/internal/rag"
	"sorus/internal/search"
	"sorus/internal/server"
	"sorus/internal/theme"
	"sorus/internal/version"
	"sorus/static"
)

// desktopListen は最初のサイトの待ち受けを選ぶ。テストでは OS が選ぶポートに差し替える。
var desktopListen = desktop.Listen

// desktopListenSite は2つ目以降のサイトの待ち受けを選ぶ。
var desktopListenSite = desktop.ListenSite

// buildMode はビルド時に -ldflags "-X main.buildMode=desktop" で埋め込む既定の動作モード。desktop なら
// フラグを付けずに起動してもデスクトップモードになる（desktop.SelectMode）。配布用のデスクトップ版で使う。
var buildMode string

// desktopMode はデスクトップモードで動いているか。main が起動時に desktop.SelectMode で決める。
var desktopMode bool

func main() {
	if isVersionArgs(os.Args[1:]) {
		fmt.Println(version.Version)
		return
	}
	mode := desktop.SelectMode(os.Args[1:], buildMode, os.Getenv)
	if isHelpArgs(mode.Args) {
		writeUsage(os.Stdout)
		return
	}
	if mode.Reason != "" {
		log.Println(mode.Reason)
	}
	args := mode.Args
	var desktopPaths desktop.AppPaths
	if mode.Desktop {
		// デスクトップモードはカレントディレクトリの .env や ENV_FILE に依存せず、データを OS 標準の場所に置く。
		// サブコマンド（backup・restore・export・import）も同じ場所を使う。
		p, err := useDesktopPaths()
		if err != nil {
			desktop.ShowError("sorus を起動できませんでした。データを保存する場所を用意できません。\n\n" + err.Error())
			os.Exit(1)
		}
		desktopMode, desktopPaths = true, p
	} else {
		loadEnvFile(envOr("ENV_FILE", ".env"))
	}

	if cfg, ok, err := parseTokenArgs(args); ok {
		exitOnParseError("token", err)
		if err := runTokenCreate(cfg, os.Stdout, os.Stderr); err != nil {
			log.Fatalf("Token creation failed: %v", err)
		}
		return
	}

	if ok, err := parseSitesArgs(args); ok {
		exitOnParseError("sites", err)
		if err := runSites(desktopPaths, os.Stdout); err != nil {
			log.Fatalf("Listing sites failed: %v", err)
		}
		return
	}

	if cfg, ok, err := parseBackupArgs(args); ok {
		exitOnParseError("backup", err)
		exitOnParseError("backup", useDesktopSite(desktopPaths, cfg.site))
		if err := runBackup(cfg, os.Stdout); err != nil {
			log.Fatalf("Backup failed: %v", err)
		}
		return
	}

	if cfg, ok, err := parseRestoreArgs(args); ok {
		exitOnParseError("restore", err)
		exitOnParseError("restore", useDesktopSite(desktopPaths, cfg.site))
		if err := runRestore(cfg, os.Stdin, os.Stdout); errors.Is(err, errRestoreCancelled) {
			fmt.Fprintln(os.Stderr, "Restore cancelled. Nothing was changed.")
			os.Exit(1)
		} else if err != nil {
			log.Fatalf("Restore failed: %v", err)
		}
		return
	}

	if cfg, ok, err := parseExportArgs(args); ok {
		exitOnParseError("export", err)
		exitOnParseError("export", useDesktopSite(desktopPaths, cfg.site))
		if err := runExport(cfg); err != nil {
			log.Fatalf("Export failed: %v", err)
		}
		return
	}

	if cfg, ok, err := parseImportArgs(args); ok {
		exitOnParseError("import", err)
		exitOnParseError("import", useDesktopSite(desktopPaths, cfg.site))
		if err := runImport(cfg, os.Stdout, os.Stderr); err != nil {
			log.Fatalf("Import failed: %v", err)
		}
		return
	}

	if desktopMode {
		if err := runDesktop(desktopPaths, args, nil); err != nil {
			log.Printf("Desktop mode: %v", err)
			desktop.ShowError("sorus を起動できませんでした。\n\n" + err.Error() + "\n\n詳しい記録: " + desktopPaths.Log)
			os.Exit(1)
		}
		return
	}

	serveCfg, err := parseServeArgs(args)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "sorus: %v\n", err)
		os.Exit(2)
	}

	log.Printf("Starting sorus %s...", version.Version)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	uploadDir := envOr("UPLOAD_DIR", "uploads")
	// 待ち受けアドレスは、要求を受け付ける前（DB を開く前）に確かめ、書き間違いなら起動しない。
	addr, err := listenAddrFromEnv(port)
	if err != nil {
		log.Fatalf("Failed to read the listen address: %v", err)
	}
	acfg, useAutocert := autocertConfigFromEnv()
	plainAddr := ""
	if useAutocert {
		if plainAddr, err = autocertPlainAddr(addr, port); err != nil {
			log.Fatalf("Failed to read the listen address: %v", err)
		}
	}
	// autocert で HTTPS 配信するなら、明示的に無効化されない限り Cookie に Secure を付ける。
	secureCookie := useAutocert
	switch s := strings.ToLower(os.Getenv("SECURE_COOKIE")); s {
	case "true", "1":
		secureCookie = true
	case "false", "0":
		secureCookie = false
	}

	// 署名鍵は同期 API での招待の発行（issue_invite）に使う。ログには出さない。
	syncSigner, err := syncSigningKey(serveCfg)
	if err != nil {
		log.Fatalf("Failed to load capability key: %v", err)
	}
	var (
		syncKey  ed25519.PublicKey
		syncPath string
	)
	if syncSigner != nil {
		syncKey = syncSigner.Public().(ed25519.PublicKey)
		if syncPath, err = syncAPIPath(serveCfg); err != nil {
			log.Fatalf("Failed to load sync API path: %v", err)
		}
	}

	// MCP サーバーのパス。公開専用モードは推測できない秘匿パス（MCP_API_PATH か mcp_path.txt）だけに置く。
	mcpPath, err := mcpAPIPath(serveCfg)
	if err != nil {
		log.Fatalf("Failed to load MCP API path: %v", err)
	}

	// 公開専用モードには管理画面が無いため、ログインできない初期管理者は作らない。
	db := connectDatabase()
	defer db.Close()
	if !serveCfg.publicOnly {
		ensureInitialAdmin(db)
	}

	searchIndex, err := search.OpenOrCreateIndex(envOr("SEARCH_INDEX_DIR", "search_index"))
	if err != nil {
		log.Fatalf("Failed to open search index: %v", err)
	}
	defer searchIndex.Close()

	aiProvider, ragService := newAIServices(db)

	srv, err := server.New(server.Config{
		DB:              db,
		UploadDir:       uploadDir,
		SecureCookie:    secureCookie,
		SiteName:        os.Getenv("SITE_NAME"),
		SiteDescription: os.Getenv("SITE_DESCRIPTION"),
		BaseURL:         os.Getenv("BASE_URL"),
		AllowedHosts:    splitEnvList("ALLOWED_HOSTS"),
		TrustedProxies:  splitEnvList("TRUSTED_PROXIES"),
		Search:          searchIndex,
		AI:              aiProvider,
		RAG:             ragService,
		AttachmentDir:   envOr("ATTACHMENT_DIR", server.DefaultAttachmentDir),
		ThemesDir:       envOr("THEMES_DIR", server.DefaultThemesDir),
		PublicOnly:      serveCfg.publicOnly,
		SyncPublicKey:   syncKey,
		SyncSigningKey:  syncSigner,
		SyncAPIPath:     syncPath,
		MCPAPIPath:      mcpPath,
	})
	if err != nil {
		log.Fatalf("Failed to initialize server: %v", err)
	}
	if serveCfg.publicOnly {
		log.Printf("Public-only mode: the admin UI (/admin) is disabled; MCP API at %s (OAuth with confirmation codes from a connected PC)", mcpPath)
	}

	// グレースフルシャットダウンの準備
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	serveErr := make(chan error, 3)
	var stopAutoCert func(context.Context) error
	if useAutocert {
		stopAutoCert = startServerWithAutoCert(srv.Handler(), acfg, plainAddr, serveErr)
		log.Printf("Server listening on https://%s (autocert, HTTP %s / HTTPS %s / health check %s, SecureCookie=%v)",
			acfg.domains[0], acfg.httpAddr, acfg.httpsAddr, plainAddr, secureCookie)
	} else {
		go func() {
			log.Printf("Server listening on %s (SecureCookie=%v)", listenURL(addr), secureCookie)
			serveErr <- srv.Start(addr)
		}()
	}

	select {
	case <-quit:
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Server error: %v", err)
		}
	}
	log.Println("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if stopAutoCert != nil {
		if err := stopAutoCert(ctx); err != nil {
			log.Printf("HTTPS server shutdown error: %v", err)
		}
	}
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("Server shutdown error: %v", err)
	}
	log.Println("Server gracefully stopped.")
}

// isVersionArgs は args が版の表示（sorus --version・sorus -version・sorus version）だけかを返す。
// 版はリリースのビルドで -ldflags "-X sorus/internal/version.Version=<タグ名>" により埋め込む。
func isVersionArgs(args []string) bool {
	return len(args) == 1 && slices.Contains([]string{"--version", "-version", "version"}, args[0])
}

// isHelpArgs は args が使い方の表示（sorus help・sorus -h・sorus --help）だけかを返す。
func isHelpArgs(args []string) bool {
	return len(args) == 1 && slices.Contains([]string{"help", "-h", "-help", "--help"}, args[0])
}

// exitOnParseError は、サブコマンドの引数の解析が err で失敗していれば終了する。-h・--help（flag.ErrHelp）なら
// 使い方は表示済みのため 0 で、それ以外は誤りを name を付けて標準エラー出力に書き 2 で終わる。
func exitOnParseError(name string, err error) {
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		os.Exit(2)
	}
}

// loadEnvFile は path（通常 ".env"）が存在すれば読み込み、未設定の環境変数をセットする。
// 外部ライブラリを使わず純Goで実装する。
func loadEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // ファイルが存在しない場合は何もしない
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// export KEY=VALUE 形式にも対応
		line = strings.TrimPrefix(line, "export ")
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		// 前後の引用符 (" または ') を除去
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		// 既存の環境変数は上書きしない
		if os.Getenv(key) == "" {
			_ = os.Setenv(key, val)
		}
	}
}

// envOr は環境変数 key の値を返す。未設定なら def を返す。
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// openDatabase は DB_PATH の DB に接続してマイグレーションを適用し、初期管理者を用意する。
// デスクトップモードでは最初のユーザーを「はじめに」画面で作るため、初期管理者は作らない。
func openDatabase() *sqlx.DB {
	db := connectDatabase()
	if !desktopMode {
		ensureInitialAdmin(db)
	}
	return db
}

// newAIServices は環境変数から AI 執筆アシスタントと RAG の埋め込みサービスを用意し、その状態をログに出す。
func newAIServices(db *sqlx.DB) (ai.Provider, *rag.Service) {
	aiProvider, err := ai.NewProviderFromEnv()
	if err != nil {
		log.Printf("AI assistant disabled: %v", err)
	} else if ai.IsConfigured(aiProvider) {
		log.Printf("AI assistant: %s", aiProvider.Name())
	} else {
		log.Println("AI assistant: not configured (set GEMINI_API_KEY to enable)")
	}

	ragEmbedder, ragErr := rag.NewEmbedderFromEnv()
	if ragErr != nil && !errors.Is(ragErr, rag.ErrNotConfigured) {
		log.Printf("RAG indexing disabled: %v", ragErr)
	}
	ragService := rag.NewService(db, ragEmbedder)
	if ragEmbedder == nil {
		log.Println("RAG embeddings: not configured (related articles use any stored embeddings)")
	}
	return aiProvider, ragService
}

// connectDatabase は DB_PATH の DB に接続してマイグレーションを適用する。
func connectDatabase() *sqlx.DB {
	db, err := database.Connect(envOr("DB_PATH", "database.db"))
	if err != nil {
		log.Fatalf("Failed to connect database: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		db.Close()
		log.Fatalf("Failed to migrate database: %v", err)
	}
	log.Println("Database initialized successfully.")
	return db
}

// ensureInitialAdmin は、ユーザーが1件も存在しなければ初期管理者を作成する。
// インポートで著者が先に作られると初期管理者が作られなくなるため、インポート前にも呼ぶ。
func ensureInitialAdmin(db *sqlx.DB) {
	var userCount int
	if err := db.Get(&userCount, "SELECT COUNT(*) FROM users"); err == nil && userCount == 0 {
		adminPass := os.Getenv("ADMIN_PASSWORD")
		generated := false
		if adminPass == "" {
			buf := make([]byte, 8)
			if _, err := rand.Read(buf); err != nil {
				log.Fatalf("Failed to generate random admin password: %v", err)
			}
			adminPass = hex.EncodeToString(buf)
			generated = true
		}

		hash, err := auth.HashPassword(adminPass)
		if err != nil {
			log.Fatalf("Failed to hash initial admin password: %v", err)
		}
		_, err = db.Exec(
			"INSERT INTO users (id, username, display_name, password_hash, role) VALUES (?, ?, ?, ?, ?)",
			"admin-default", "admin", "管理者", hash, auth.RoleAdmin,
		)
		if err != nil {
			log.Fatalf("Failed to create initial admin user: %v", err)
		}

		if generated {
			log.Printf("==================================================================")
			log.Printf("  Initial admin user created!")
			log.Printf("  Username: admin")
			log.Printf("  Password: %s", adminPass)
			log.Printf("  IMPORTANT: Please change this password after your first login!")
			log.Printf("==================================================================")
		} else {
			log.Println("Created initial admin user: username=admin (password from ADMIN_PASSWORD)")
		}
	}
}

// desktopShutdownTimeout はデスクトップモードを止めるとき、処理中の要求を待つ時間の上限。
const desktopShutdownTimeout = 10 * time.Second

// desktopStartWait は、別の sorus がロックを持っているがまだ応答しない（起動の途中か終了の途中）とき、応答するか
// ロックが空くのを待つ時間の上限。
const desktopStartWait = 15 * time.Second

// useDesktopPaths はデスクトップモードのデータディレクトリを用意し、既存の処理（サーバーとサブコマンド）が環境変数で
// 受け取るデータの場所を、1つ目のサイト（データディレクトリそのもの）に向ける。コマンドの --site で別のサイトを
// 選んだときは、useDesktopSite が向け直す。
func useDesktopPaths() (desktop.AppPaths, error) {
	p, err := desktop.DefaultAppPaths()
	if err != nil {
		return p, err
	}
	if err := p.Ensure(); err != nil {
		return p, err
	}
	return p, useSitePaths(desktop.SitePathsIn(p.Dir))
}

// useSitePaths は、既存の処理が環境変数で受け取るデータの場所をサイト sp に向ける。利用者の環境変数に同じ名前が
// あっても上書きする。
func useSitePaths(sp desktop.SitePaths) error {
	for k, v := range map[string]string{
		"DB_PATH":          sp.DB,
		"UPLOAD_DIR":       sp.Uploads,
		"ATTACHMENT_DIR":   sp.Attachments,
		"SEARCH_INDEX_DIR": sp.SearchIndex,
		"BACKUP_DIR":       sp.Backups,
		"THEMES_DIR":       sp.Themes,
	} {
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	return nil
}

// useDesktopSite は、デスクトップモードのコマンド（backup・restore・export・import）が扱うサイトを --site の値 id から
// 選び（desktopSiteFor）、データの場所をそのサイトに向ける。サーバーモードでは何もしない。
func useDesktopSite(p desktop.AppPaths, id string) error {
	if !desktopMode {
		return nil
	}
	site, err := desktopSiteFor(p, id)
	if err != nil {
		return err
	}
	return useSitePaths(p.Site(site))
}

// desktopSiteFor は、コマンドの --site の値 id から扱うサイトを選ぶ。sites.json は読むだけで書かない（書くのは
// 動いているアプリだけ）。id を省いたときは、サイトが1つならそれを選び、2つ以上なら id を指定するよう求める。
func desktopSiteFor(p desktop.AppPaths, id string) (desktop.Site, error) {
	reg, _, err := desktop.LoadRegistry(p)
	if err != nil {
		return desktop.Site{}, err
	}
	if id == "" {
		if len(reg.Sites) == 1 {
			return reg.Sites[0], nil
		}
		return desktop.Site{}, fmt.Errorf("サイトが %d つあるため、どのサイトかを --site <id> で指定してください（使える id: %s）。"+
			"サイトの名前と id は sorus sites で確かめられます", len(reg.Sites), strings.Join(reg.IDs(), ", "))
	}
	if i := reg.Index(id); i >= 0 {
		return reg.Sites[i], nil
	}
	return desktop.Site{}, fmt.Errorf("id が %q のサイトはありません。使える id は sorus sites で確かめてください", id)
}

// addSiteFlag は、デスクトップモードのときだけ、コマンドの fs に扱うサイトを選ぶ --site を加える。
func addSiteFlag(fs *flag.FlagSet, site *string) {
	if desktopMode {
		fs.StringVar(site, "site", "", "扱うサイトの id（サイトが2つ以上あるときに指定する。一覧は sorus sites）")
	}
}

// parseSitesArgs は "sites" の引数を解析する。サイトの一覧の呼び出しでなければ ok=false を返す。
func parseSitesArgs(args []string) (ok bool, err error) {
	if len(args) == 0 || args[0] != "sites" {
		return false, nil
	}
	fs := flag.NewFlagSet("sites", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "使い方: sorus sites")
		fmt.Fprintln(fs.Output(), "  この PC の sorus のサイトを、1行に1つずつ表示する: id、サイト名、アドレス、フォルダ。")
		fmt.Fprintln(fs.Output(), "  id は sorus backup・restore・export・import の --site に使う。")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return true, err
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return true, fmt.Errorf("extra argument: %s", fs.Arg(0))
	}
	if !desktopMode {
		return true, errors.New("sorus sites は、この PC で使う sorus（デスクトップモード）のサイトの一覧です。--desktop を付けて実行してください")
	}
	return true, nil
}

// runSites はデスクトップモードのサイトの一覧を、1行に1つずつ w に書く: id（sites.json がまだ無いときは -）、サイト名
// （「はじめに」の前なら「名前の無いサイト（作成日）」）、アドレス、フォルダ。sites.json とサイトの DB は読むだけで書かない。
func runSites(p desktop.AppPaths, w io.Writer) error {
	reg, saved, err := desktop.LoadRegistry(p)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, site := range reg.Sites {
		id := site.ID
		if id == "" {
			id = "-"
		}
		port := site.Port
		if port == 0 {
			// sites.json がまだ無いときは、動いている（最後に動いた）ときのポートか、最初に試すポート。
			port = desktop.PreferredPort
			if inst, err := desktop.ReadInstance(p.Instance); err == nil && !saved {
				port = inst.Port
			}
		}
		sp := p.Site(site)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", id, desktopSiteName(sp, site.Created), desktop.Instance{Port: port}.AdminURL(), sp.Dir)
	}
	return tw.Flush()
}

// desktopSiteName は、コマンドに表示するサイト sp の名前を返す。「はじめに」の前（DB が無い・読めないときを含む）なら
// 「名前の無いサイト（作成日）」にする。
func desktopSiteName(sp desktop.SitePaths, created time.Time) string {
	unnamed := "名前の無いサイト（" + created.Local().Format("2006年1月2日") + "）"
	// Connect は無い DB を作るため、先に確かめる。
	if _, err := os.Stat(sp.DB); err != nil {
		return unnamed
	}
	db, err := database.Connect(sp.DB)
	if err != nil {
		return unnamed
	}
	defer db.Close()
	if name, err := server.SiteNameOf(db); err == nil && name != "" {
		return name
	}
	return unnamed
}

// errDesktopSetupNotDone は、デスクトップモードで「はじめに」を済ませる前（アカウントが無いとき）に取り込もうとしたときの
// エラー。何をすればよいかを利用者に伝える。ユーザーがいないときも、古い版の取り込みで作られた著者のユーザーしか
// いないときも、起動すれば「はじめに」が出る（auth.SetupDone）。
var errDesktopSetupNotDone = errors.New("まだ「はじめに」が済んでいないため、取り込めません。sorus を一度起動して「はじめに」を済ませてから、もう一度取り込んでください。")

// checkDesktopSetupDone は、デスクトップモードでまだ「はじめに」を済ませていない（管理者のアカウントがいない）なら
// errDesktopSetupNotDone を返す。記事を書いた人になるアカウントが無いため。判定は管理画面が「はじめに」を出すかと
// 同じ auth.SetupDone で行う。
func checkDesktopSetupDone(db *sqlx.DB) error {
	if !desktopMode {
		return nil
	}
	done, err := auth.SetupDone(db)
	if err != nil {
		return err
	}
	if !done {
		return errDesktopSetupNotDone
	}
	return nil
}

// importCurrentAccount は CLI の current モードで使う管理者アカウントを返す。
// 古い版の取り込みで作られた著者のユーザーが残っていても、そちらは選ばない。
func importCurrentAccount(db *sqlx.DB) (*auth.User, error) {
	users, err := auth.ListUsers(db)
	if err != nil {
		return nil, err
	}
	for i := range users {
		if users[i].Role == auth.RoleAdmin {
			return &users[i], nil
		}
	}
	return nil, errors.New("current mode requires an administrator account")
}

// runDesktop はデスクトップモードで sorus を動かす。sites.json のすべてのサイトを、サイトごとに 127.0.0.1 の別のポート
// （1つ目のサイトはできれば desktop.PreferredPort、ほかのサイトは記録したポート）で同時に動かし、Host ヘッダーが
// 127.0.0.1・localhost 以外の要求を拒否する。データディレクトリに instance.json を書いて、最後に使っていたサイトの
// 管理画面を sorus のウィンドウ（desktop.OpenWindow）で開く。すでに起動していれば、そのインスタンス（最後に使っていた
// サイト）をウィンドウで開いて nil を返す。開けないサイトがあってもほかのサイトは開き、1つも開けなければエラーを返す。
// 画面がすべて閉じた（windowWatcher）、管理画面の「sorus を終了」、SIGINT・SIGTERM、または stop が閉じられる
// （テスト用。通常は nil）と、すべてのサイトが処理中の要求を返し終えてから止まり、instance.json を消して nil を返す。
// ログはデータディレクトリの sorus.log にも書く。
func runDesktop(p desktop.AppPaths, args []string, stop <-chan struct{}) error {
	if len(args) > 0 {
		return fmt.Errorf("unknown argument for desktop mode: %q (add --server to run as a server)", args[0])
	}
	logFile, err := desktop.OpenLog(p.Log)
	if err != nil {
		return err
	}
	defer logFile.Close()
	// Windows の配布版にはコンソールが無いため、記録は sorus.log に残す（標準エラー出力に書けなくても止まらないよう先に書く）。
	logOut := io.MultiWriter(logFile, os.Stderr)
	prevLog := log.Writer()
	log.SetOutput(logOut)
	defer log.SetOutput(prevLog)

	ctx := context.Background()
	// 2回目の起動では、起動中のものをウィンドウで開くだけにする。起動中のものは、画面がつながると終了の待ち（予約を
	// 反映してから終了する）をやめる。
	if inst, ok := desktop.FindRunning(ctx, p.Instance); ok {
		log.Printf("Desktop mode: sorus is already running at %s; opening it", inst.BaseURL())
		openDesktopWindow(inst.AdminURL())
		return nil
	}
	unlock, running, err := lockOrFindDesktop(ctx, p, desktopStartWait)
	if err != nil {
		return err
	}
	if running != nil {
		log.Printf("Desktop mode: sorus is already running at %s; opening it", running.BaseURL())
		openDesktopWindow(running.AdminURL())
		return nil
	}
	defer unlock()

	log.Printf("Starting sorus %s (desktop mode, data: %s)...", version.Version, p.Dir)
	app, err := newDesktopApp(p, logOut)
	if err != nil {
		return err
	}
	if err := app.openSites(); err != nil {
		return err
	}
	// 新しい版の確認は、サイトがいくつあってもアプリで1つだけ動かす。
	updateCtx, stopUpdates := context.WithCancel(context.Background())
	defer stopUpdates()
	go app.update.Run(updateCtx)
	adminURL := app.start()
	// 画面の見張りは、ウィンドウを開く前に始める（開く時刻から、最初の画面を待つ）。
	app.watch.start(adminURL)
	watchCtx, stopWatch := context.WithCancel(context.Background())
	defer stopWatch()
	go app.watch.run(watchCtx)
	openDesktopWindow(adminURL)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	var serveFailed error
	select {
	case <-sig:
	case <-stop:
	case <-app.quit: // 理由は windowWatcher がログに残す
	case err := <-app.failed:
		serveFailed = err
	}
	stopWatch()
	log.Println("Shutting down sorus...")
	app.beginStop()
	stopUpdates()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), desktopShutdownTimeout)
	defer cancel()
	app.shutdown(shutdownCtx)
	if serveFailed != nil {
		return serveFailed
	}
	log.Println("sorus stopped.")
	return nil
}

// errDesktopStopping は、終了の途中にサイトの追加・削除を頼まれたときのエラー。
var errDesktopStopping = errors.New("sorus を終了しているところのため、できませんでした")

// desktopApp はデスクトップモードのアプリ（1つ）。データディレクトリのロック・instance.json・ログ・終了・新しい版の
// 確認・サイトの一覧（sites.json）を持ち、一覧のすべてのサイト（desktopSite）を同時に動かす。sites.json を書くのは
// アプリだけ（コマンドは読むだけ）。
type desktopApp struct {
	paths  desktop.AppPaths
	logOut io.Writer
	// update は新しい版の確認。すべてのサイトの server.Config.UpdateCheck に渡し、Run はアプリで1回だけ動かす。
	update *server.UpdateCheck
	// quit は、終了すると決めたとき（画面がすべて閉じた・どれかのサイトで「sorus を終了」が押された。watch が決める）に
	// 値を受け取る。failed は、待ち受けが思いがけず止まったサイトのエラーを受け取る。
	quit   chan struct{}
	failed chan error
	// watch はすべてのサイトの開いている画面を見張り、終了するかを決める。
	watch *windowWatcher

	// mu は以下を守る。
	mu sync.Mutex
	// reg はサイトの一覧。saved は sites.json があるか（2つ目のサイトを作ったことがあるか）。無いあいだは書かない。
	reg   desktop.Registry
	saved bool
	// sites は reg.Sites と同じ順の、各サイトの実行の状態（開けなかったサイトと削除の途中のサイトを含む）。
	sites []*desktopSite
	// instancePort は instance.json に書いたポート（最後に使ったサイト）。0 なら書いていない。
	instancePort int
	// stopping はアプリが止まり始めたか。止まり始めたら instance.json を書かず、サイトの追加・削除を断る。
	// closed は shutdown を呼んだか（2回目は何もしない）。
	stopping, closed bool
	// deletions は削除の後片付け（別の goroutine）を数える。止まるときに終わるのを待つ。
	deletions sync.WaitGroup
	// schedulerInterval は予約投稿を確認する周期。0 は既定（1分）。テストが、サイトを開く前に短くする。
	schedulerInterval time.Duration
}

// desktopSite は1つのサイトの実行の状態（待ち受け・DB・検索インデックス・サーバー）。
type desktopSite struct {
	// entry は sites.json の記録。1つ目のサイトの id は、2つ目のサイトを作るときに付く（desktopApp.mu で守る）。
	entry desktop.Site
	paths desktop.SitePaths
	ln    net.Listener
	port  int
	db    *sqlx.DB
	index *search.Index
	srv   *server.Server
	// openErr は開けなかった理由。開けたサイトは nil。
	openErr error
	// deleting は削除を始めたか（ほかの削除と、最後に残ったサイトの数え方に使う。削除の途中のサイトはアプリの
	// shutdown が止めない）。gone は削除の前のバックアップができて、一覧から外したか。どちらも desktopApp.mu で守る。
	deleting, gone bool
}

// newDesktopApp はデータディレクトリ p のサイトの一覧を読み、まだどのサイトも開いていないアプリを返す。
// sites.json が壊れていれば、サイトを失わないよう起動せずにエラーを返す。
func newDesktopApp(p desktop.AppPaths, logOut io.Writer) (*desktopApp, error) {
	reg, saved, err := desktop.LoadRegistry(p)
	if err != nil {
		return nil, err
	}
	a := &desktopApp{
		paths:  p,
		logOut: logOut,
		update: server.NewUpdateCheck(),
		quit:   make(chan struct{}, 1),
		failed: make(chan error, 1),
		reg:    reg,
		saved:  saved,
	}
	a.watch = newWindowWatcher(a)
	return a, nil
}

// openSites は一覧のすべてのサイトを開く。開けないサイトはログに残して理由を覚え、ほかのサイトは開く。記録した
// ポートが使えず別のポートにしたサイトは、sites.json に記録し直す。1つも開けなければ、その理由をまとめたエラーを返す。
func (a *desktopApp) openSites() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var errs []error
	moved := false
	for i, entry := range a.reg.Sites {
		site := a.openSite(entry)
		a.sites = append(a.sites, site)
		if site.openErr != nil {
			errs = append(errs, site.openErr)
			continue
		}
		if entry.Port != site.port {
			if !entry.First() && entry.Port != 0 {
				log.Printf("Desktop mode: site %s: port %d is not available; using port %d from now on", entry.ID, entry.Port, site.port)
			}
			a.reg.Sites[i].Port, site.entry.Port = site.port, site.port
			moved = true
		}
	}
	if moved && a.saved {
		if err := desktop.SaveRegistry(a.paths, a.reg); err != nil {
			log.Printf("Desktop mode: %v", err)
		}
	}
	if len(errs) == len(a.sites) {
		return errors.Join(errs...)
	}
	return nil
}

// openSite はサイト entry を開く（待ち受け・DB・検索インデックス・AI・サーバー）。開けなければ、開いたものを閉じて
// 理由を openErr に入れ、ログに残す。待ち受けはまだ動かさない（serve）。a.mu を持って呼ぶ。
func (a *desktopApp) openSite(entry desktop.Site) *desktopSite {
	site := &desktopSite{entry: entry, paths: a.paths.Site(entry)}
	if err := site.open(a); err != nil {
		site.close()
		site.openErr = err
		log.Printf("Desktop mode: could not open the site in %s: %v", site.paths.Dir, err)
	}
	return site
}

func (s *desktopSite) open(a *desktopApp) error {
	var err error
	if s.entry.First() {
		s.ln, err = desktopListen()
	} else {
		if err := os.MkdirAll(s.paths.Dir, 0o700); err != nil {
			return fmt.Errorf("create site directory: %w", err)
		}
		s.ln, err = desktopListenSite(s.entry.Port, a.usedPorts(s))
	}
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	s.port = s.ln.Addr().(*net.TCPAddr).Port

	if s.db, err = database.Connect(s.paths.DB); err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	if err := database.Migrate(s.db); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	if s.index, err = search.OpenOrCreateIndex(s.paths.SearchIndex); err != nil {
		return fmt.Errorf("open search index: %w", err)
	}
	aiProvider, ragService := newAIServices(s.db)
	s.srv, err = server.New(server.Config{
		DB:            s.db,
		UploadDir:     s.paths.Uploads,
		AttachmentDir: s.paths.Attachments,
		ThemesDir:     s.paths.Themes,
		// http://127.0.0.1 で配信するため、Cookie に Secure を付けない。
		SecureCookie:      false,
		SiteName:          os.Getenv("SITE_NAME"),
		SiteDescription:   os.Getenv("SITE_DESCRIPTION"),
		BaseURL:           os.Getenv("BASE_URL"),
		Search:            s.index,
		AI:                aiProvider,
		RAG:               ragService,
		Desktop:           true,
		SuggestedUsername: desktop.OSUsername(),
		OnQuit:            a.watch.quitButton,
		LogOutput:         a.logOut,
		SiteKey:           s.entry.Key(),
		Sites:             desktopSiteView{app: a, site: s},
		UpdateCheck:       a.update,
		SchedulerInterval: a.schedulerInterval,
		MCPAPIPath:        desktopMCPAPIPath(),
	})
	if err != nil {
		return fmt.Errorf("initialize server: %w", err)
	}
	return nil
}

// stop はサイト s のサーバーを、待ち受けを閉じて処理中の要求を返し終えてから（desktopShutdownTimeout まで待つ）
// 止め、開いたものを閉じる。
func (s *desktopSite) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), desktopShutdownTimeout)
	defer cancel()
	if err := s.srv.Shutdown(ctx); err != nil {
		log.Printf("Desktop mode: site %s: shutdown: %v", s.entry.ID, err)
	}
	s.close()
}

// close はサイトの開いたもの（検索インデックス・DB・待ち受け）を閉じる。サーバーは先に Shutdown で止めておく。
func (s *desktopSite) close() {
	if s.index != nil {
		s.index.Close()
	}
	if s.db != nil {
		s.db.Close()
	}
	if s.ln != nil {
		s.ln.Close()
	}
}

// info はサイト s の server.SiteInfo を返す（Name は除く）。current は一覧を返すサーバーのサイト。a.mu を持って呼ぶ。
func (s *desktopSite) info(current *desktopSite) server.SiteInfo {
	info := server.SiteInfo{ID: s.entry.ID, Created: s.entry.Created, Current: s == current, Dir: s.paths.Dir}
	if s.openErr != nil {
		info.OpenError = s.openErr.Error()
		info.OpenKind = server.ClassifyOpenError(s.openErr)
	} else {
		info.AdminURL = desktop.Instance{Port: s.port}.AdminURL()
	}
	return info
}

// usedPorts は、ほかのサイトが記録した・使っているポートを返す（新しいポートを選ぶときに避ける）。a.mu を持って呼ぶ。
func (a *desktopApp) usedPorts(self *desktopSite) []int {
	var ports []int
	for _, e := range a.reg.Sites {
		if e.Dir != self.entry.Dir && e.Port != 0 {
			ports = append(ports, e.Port)
		}
	}
	for _, s := range a.sites {
		if s != self && s.port != 0 {
			ports = append(ports, s.port)
		}
	}
	return ports
}

// serve はサイト s の待ち受けを別の goroutine で動かす。Host ヘッダーはサイトのポートで確かめ（desktop.HostGuard）、
// 管理画面のページを開いた要求で s を最後に使ったサイトにする（useSite）。
func (a *desktopApp) serve(s *desktopSite) {
	srv, ln, port := s.srv, s.ln, s.port
	go func() {
		err := srv.Serve(ln, func(h http.Handler) http.Handler { return desktop.HostGuard(port, a.trackUse(s, h)) })
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			select {
			case a.failed <- fmt.Errorf("server: %w", err):
			default:
			}
		}
	}()
}

// start は開けたすべてのサイトの待ち受けを動かし、instance.json を最後に使っていたサイトに向けて書き、そのサイトの
// 管理画面の URL（ブラウザで開く先）を返す。
func (a *desktopApp) start() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.sites {
		if s.srv == nil {
			continue
		}
		a.serve(s)
		if s.entry.ID == "" {
			log.Printf("sorus is running at %s", s.info(nil).AdminURL)
		} else {
			log.Printf("sorus is running at %s (site %s)", s.info(nil).AdminURL, s.entry.ID)
		}
	}
	last := a.instanceSite()
	a.writeInstance(last)
	return last.info(nil).AdminURL
}

// instanceSite は instance.json が指すサイトを返す: 最後に使ったサイト。それが開けていなければ、開けた最初のサイト。
// a.mu を持って呼ぶ。
func (a *desktopApp) instanceSite() *desktopSite {
	last := a.reg.LastSite()
	var first *desktopSite
	for _, s := range a.sites {
		if s.srv == nil || s.deleting {
			continue
		}
		if s.entry.Dir == last.Dir {
			return s
		}
		if first == nil {
			first = s
		}
	}
	return first
}

// writeInstance は instance.json をサイト s に向けて書く（2回目の起動はそのサイトを開く）。向きが変わらないとき、
// 止まり始めたあとは書かない。a.mu を持って呼ぶ。
func (a *desktopApp) writeInstance(s *desktopSite) {
	if s == nil || a.stopping || a.instancePort == s.port {
		return
	}
	if err := desktop.WriteInstance(a.paths.Instance, desktop.Instance{PID: os.Getpid(), Port: s.port}); err != nil {
		// 2回目の起動が起動中のものを見つけられないだけで、この sorus は使える。
		log.Printf("Desktop mode: %v", err)
		return
	}
	a.instancePort = s.port
}

// trackUse は、サイト s で管理画面のページを開いた要求で、s を最後に使ったサイトにしてから next に渡す。
func (a *desktopApp) trackUse(s *desktopSite, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAdminPageRequest(r) {
			a.useSite(s)
		}
		next.ServeHTTP(w, r)
	})
}

// isAdminPageRequest は r がブラウザで管理画面のページを開く要求か（画面の部分の読み込み・API・フォームの送信では
// ないか）を返す。開いたままの別のタブが裏で読み込む部分で、最後に使ったサイトが変わらないようにする。
func isAdminPageRequest(r *http.Request) bool {
	p := r.URL.Path
	return r.Method == http.MethodGet && (p == "/admin" || strings.HasPrefix(p, "/admin/")) &&
		r.Header.Get("HX-Request") == "" && strings.Contains(r.Header.Get("Accept"), "text/html")
}

// useSite はサイト s を最後に使ったサイトとして記録する。変わったときだけ sites.json（あれば）と instance.json を書く。
func (a *desktopApp) useSite(s *desktopSite) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopping || s.deleting {
		return
	}
	if a.saved && a.reg.Last != s.entry.ID {
		next := a.reg.Clone()
		next.Last = s.entry.ID
		if err := desktop.SaveRegistry(a.paths, next); err != nil {
			log.Printf("Desktop mode: %v", err)
		}
		a.reg = next
	}
	a.writeInstance(s)
}

// requestQuit は終了を始めさせる（windowWatcher が終了すると決めたときに呼ぶ）。終了は runDesktop が別の goroutine で行う。
func (a *desktopApp) requestQuit() {
	select {
	case a.quit <- struct{}{}:
	default:
	}
}

// beginStop は止まり始めたことを記録し、instance.json を消す。終了の途中に起動し直されたとき、応答しなくなった
// このインスタンスを開こうとせず、ロックが空くのを待って自分で起動するよう、止まり始める時点で消す。
func (a *desktopApp) beginStop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopping = true
	if err := desktop.RemoveInstance(a.paths.Instance); err != nil {
		log.Printf("Desktop mode: %v", err)
	}
	a.instancePort = 0
}

// shutdown はすべてのサイトを同時に、処理中の要求を返し終えてから止める（ctx まで待つ）。削除の途中のサイトは
// 削除の側が止めて閉じる（finishDelete・cancelDelete）ため、それが終わるのも待ってから、DB と検索インデックスを
// 閉じる。2回目からは何もしない。
func (a *desktopApp) shutdown(ctx context.Context) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.stopping, a.closed = true, true
	var running []*desktopSite
	for _, s := range a.sites {
		if s.srv != nil && !s.deleting {
			running = append(running, s)
		}
	}
	a.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range running {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.srv.Shutdown(ctx); err != nil {
				log.Printf("Server shutdown error: %v", err)
			}
		}()
	}
	wg.Wait()
	a.deletions.Wait()
	for _, s := range running {
		s.close()
	}
}

// desktopSiteView は、サイト site のサーバーに渡す server.Sites（server.Config.Sites）。一覧の Current は site。
type desktopSiteView struct {
	app  *desktopApp
	site *desktopSite
}

func (v desktopSiteView) List() []server.SiteInfo          { return v.app.listSites(v.site) }
func (v desktopSiteView) Create() (server.SiteInfo, error) { return v.app.createSite(v.site) }
func (v desktopSiteView) Delete(id string) (string, error) { return v.app.deleteSite(id) }
func (v desktopSiteView) Forget(id string) error           { return v.app.forgetSite(id) }

// listSites は server.Sites.List。サイト名は、一覧を守る鍵を放してから各サイトの DB から読む。
func (a *desktopApp) listSites(current *desktopSite) []server.SiteInfo {
	a.mu.Lock()
	var sites []*desktopSite
	var infos []server.SiteInfo
	for _, s := range a.sites {
		if !s.gone {
			sites = append(sites, s)
			infos = append(infos, s.info(current))
		}
	}
	a.mu.Unlock()
	for i, s := range sites {
		if s.db == nil {
			continue
		}
		name, err := server.SiteNameOf(s.db)
		if err != nil {
			// 読めなかったことは、「はじめに」が済んでいない（Name が空でエラーなし）こととは別。
			log.Printf("Desktop mode: read the name of the site in %s: %v", s.paths.Dir, err)
			infos[i].NameError = err.Error()
			continue
		}
		infos[i].Name = name
	}
	return infos
}

// createSite は server.Sites.Create。sites.json を書けるまで、一覧（a.reg・a.sites）は変えない。
func (a *desktopApp) createSite(current *desktopSite) (server.SiteInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopping {
		return server.SiteInfo{}, errDesktopStopping
	}
	next := a.reg.Clone()
	// sites.json を初めて作るときの1つ目のサイトも、今のポートを記録する。
	for i, s := range a.sites {
		if s.openErr == nil {
			next.Sites[i].Port = s.port
		}
	}
	entry, err := next.AddSite(a.paths, time.Now())
	if err != nil {
		return server.SiteInfo{}, err
	}
	if i := slices.Index(a.sites, current); next.Last == "" && i >= 0 {
		next.Last = next.Sites[i].ID
	}
	site := a.openSite(entry)
	if site.openErr != nil {
		a.removeNewSite(entry)
		return server.SiteInfo{}, fmt.Errorf("新しいサイトを作れませんでした: %w", site.openErr)
	}
	next.Sites[len(next.Sites)-1].Port, site.entry.Port = site.port, site.port
	if err := desktop.SaveRegistry(a.paths, next); err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), desktopShutdownTimeout)
		defer cancel()
		if serr := site.srv.Shutdown(ctx); serr != nil {
			log.Printf("Server shutdown error: %v", serr)
		}
		site.close()
		a.removeNewSite(entry)
		return server.SiteInfo{}, fmt.Errorf("新しいサイトを作れませんでした: %w", err)
	}
	for i, s := range a.sites {
		s.entry = next.Sites[i]
	}
	a.reg, a.saved = next, true
	a.sites = append(a.sites, site)
	a.serve(site)
	log.Printf("Desktop mode: created site %s in %s; it is running at %s", entry.ID, site.paths.Dir, site.info(nil).AdminURL)
	return site.info(current), nil
}

// removeNewSite は、作りかけでやめた新しいサイト entry のフォルダを消す。
func (a *desktopApp) removeNewSite(entry desktop.Site) {
	if err := a.paths.RemoveSite(entry); err != nil {
		log.Printf("Desktop mode: %v", err)
	}
}

// deleteSite は server.Sites.Delete（手順はそちらを参照）。バックアップを作るあいだは一覧を守る鍵を持たない
// （ほかのサイトの画面を待たせない）。
func (a *desktopApp) deleteSite(id string) (string, error) {
	site, err := a.beginDelete(id)
	if err != nil {
		return "", err
	}
	return a.backupAndDelete(site)
}

// beginDelete はサイト id を削除できるかを確かめ、削除の途中（deleting）にして返す（server.Sites.Delete の手順1）。
// 削除の途中のサイトは、アプリの shutdown では止めない。削除をやめて戻す（cancelDelete）か、止めて閉じる
// （finishDelete・cancelDelete）までは、この削除が受け持つ。
func (a *desktopApp) beginDelete(id string) (*desktopSite, error) {
	a.mu.Lock()
	var site *desktopSite
	// remaining は削除の途中でない、開いているサイトの数。開けなかったサイトは数えない（それだけが残ると、開いて
	// いるサイトが無くなり、instance.json も閉じたサイトを指したままになる）。
	remaining := 0
	for _, s := range a.sites {
		if s.deleting {
			continue
		}
		if s.srv != nil {
			remaining++
		}
		if id != "" && s.entry.ID == id {
			site = s
		}
	}
	var err error
	switch {
	case a.stopping:
		err = errDesktopStopping
	case site == nil:
		err = fmt.Errorf("id が %q のサイトはありません", id)
	case site.srv == nil:
		err = fmt.Errorf("開けなかったサイトは削除できません（%v）", site.openErr)
	case remaining <= 1:
		err = errors.New("最後に残ったサイトは削除できません")
	}
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}
	site.deleting = true
	a.deletions.Add(1)
	a.mu.Unlock()
	return site, nil
}

// backupAndDelete は beginDelete で削除の途中にしたサイト site の、server.Sites.Delete の手順2〜4。手順5〜7は
// 別の goroutine で行う（finishDelete）。
func (a *desktopApp) backupAndDelete(site *desktopSite) (string, error) {
	id := site.entry.ID
	ctx, cancel := context.WithTimeout(context.Background(), desktopShutdownTimeout)
	defer cancel()
	if err := site.srv.StopBackground(ctx); err != nil {
		log.Printf("Desktop mode: site %s: %v", id, err)
	}
	file, err := site.srv.BackupBeforeDelete(a.paths.DeletedBackups, desktop.DeletedSiteBackupName(id, time.Now()))
	if err != nil {
		log.Printf("Desktop mode: site %s was not deleted because the backup failed: %v", id, err)
		a.cancelDelete(site)
		return "", fmt.Errorf("削除の前のバックアップを作れなかったため、削除しませんでした: %w", err)
	}
	a.mu.Lock()
	site.gone = true
	a.mu.Unlock()
	log.Printf("Desktop mode: deleting site %s (backup: %s)", id, file)
	go a.finishDelete(site)
	return file, nil
}

// cancelDelete は、バックアップを作れずにやめた削除のサイト site を何も消さずに戻す。アプリが止まり始めていなければ、
// 止めた仕事を動かし直し、サイトはアプリに戻す（このあと止まり始めても、shutdown がほかのサイトと一緒に止める）。
// 止まり始めていれば、shutdown はこのサイトを止めない（削除の途中だったため）ので、動かし直さずに、削除を頼んだ
// 要求を返し終えてから別の goroutine で止めて閉じる（shutdown はそれも待つ）。
func (a *desktopApp) cancelDelete(site *desktopSite) {
	a.mu.Lock()
	stopping := a.stopping
	if !stopping {
		site.deleting = false
	}
	a.mu.Unlock()
	if stopping {
		go func() {
			defer a.deletions.Done()
			site.stop()
		}()
		return
	}
	site.srv.ResumeBackground()
	a.deletions.Done()
}

// finishDelete は削除の後片付け（server.Sites.Delete の手順5〜7）。削除を頼んだ要求（完了の画面）を返し終えてから
// サイトを止めて閉じ、sites.json の保存が成功してからフォルダを消す。保存に失敗したらファイルは残し、
// そのサイトは開けなかったサイトとして一覧に残す（一覧に残ったサイトを、次の起動で空のデータベースとして
// 開き直さないため）。instance.json は、開いているサイトを指すようにする。
func (a *desktopApp) finishDelete(site *desktopSite) {
	defer a.deletions.Done()
	id := site.entry.ID
	// stop は処理中の要求を待つ。要求は a.mu を取ることがあるので、鍵を持たずに止める。
	site.stop()

	a.mu.Lock()
	i := slices.Index(a.sites, site)
	next := a.reg.Clone()
	ri := next.Index(id)
	if i < 0 || ri < 0 || len(next.Sites) <= 1 {
		a.mu.Unlock()
		log.Printf("Desktop mode: site %s was not removed from the site list (files were kept)", id)
		return
	}
	next.Sites = slices.Delete(next.Sites, ri, ri+1)
	if next.Last == id {
		next.Last = next.Sites[0].ID
	}
	if err := desktop.SaveRegistry(a.paths, next); err != nil {
		log.Printf("Desktop mode: site %s was not removed because the site list could not be saved (files were kept): %v", id, err)
		site.openErr = err
		site.srv = nil
		site.db = nil
		site.gone = false
		site.deleting = false
		a.instancePort = 0
		a.writeInstance(a.instanceSite())
		a.mu.Unlock()
		return
	}
	a.reg = next
	a.sites = slices.Delete(a.sites, i, i+1)
	a.pointInstanceAway(site)
	a.mu.Unlock()

	if err := a.paths.RemoveSite(site.entry); err != nil {
		log.Printf("Desktop mode: site %s: %v (the backup is kept; remove what is left of %s yourself)", id, err, site.paths.Dir)
	}
	log.Printf("Desktop mode: deleted site %s", id)
}

// forgetSite は server.Sites.Forget。開けなかったサイトを一覧から外し、フォルダには触れない。
func (a *desktopApp) forgetSite(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopping {
		return errDesktopStopping
	}
	var site *desktopSite
	openOther := false
	for _, s := range a.sites {
		if s.gone || s.deleting {
			continue
		}
		if id != "" && s.entry.ID == id {
			site = s
			continue
		}
		if s.srv != nil {
			openOther = true
		}
	}
	switch {
	case id == "" || site == nil || a.reg.Index(id) < 0:
		return fmt.Errorf("id が %q のサイトはありません", id)
	case site.srv != nil:
		return errors.New("開いているサイトは一覧から外せません")
	case len(a.reg.Sites) <= 1:
		return errors.New("最後に残ったサイトは一覧から外せません")
	case site.entry.First() && !openOther:
		return errors.New("開いているサイトがほかにないため、このサイトは一覧から外せません")
	}
	next := a.reg.Clone()
	ri := next.Index(id)
	next.Sites = slices.Delete(next.Sites, ri, ri+1)
	if next.Last == id {
		next.Last = next.Sites[0].ID
	}
	if err := desktop.SaveRegistry(a.paths, next); err != nil {
		return fmt.Errorf("サイトの一覧を保存できなかったため、外しませんでした: %w", err)
	}
	a.reg = next
	a.saved = true
	for i, s := range a.sites {
		if s == site {
			a.sites = slices.Delete(a.sites, i, i+1)
			break
		}
	}
	a.pointInstanceAway(site)
	log.Printf("Desktop mode: removed site %s from the site list (the folder was kept: %s)", id, site.paths.Dir)
	return nil
}

// pointInstanceAway は、instance.json がサイト site を指しているとき、開いているサイトへ向け直す。
// 待ち受けに失敗して site.port が 0 でも、記録したポート（site.entry.Port）で指していれば向け直す。
// 向ける先が変わらないときは何も書かない。a.mu を持って呼ぶ。
func (a *desktopApp) pointInstanceAway(site *desktopSite) {
	if a.instancePort == site.port || a.instancePort == site.entry.Port {
		a.instancePort = 0
	}
	a.writeInstance(a.instanceSite())
}

// lockOrFindDesktop はデータディレクトリのロックを取る。別の sorus がロックを持っていれば、wait の間、
// そのインスタンスが応答するようになる（起動の途中）か、ロックが空く（終了の途中）まで待つ。応答する
// インスタンスが見つかればそれを返し、ロックが取れれば解放する関数を返す。wait を過ぎてもロックが空かず
// 応答も無ければエラーを返す。
func lockOrFindDesktop(ctx context.Context, p desktop.AppPaths, wait time.Duration) (unlock func() error, running *desktop.Instance, err error) {
	deadline := time.Now().Add(wait)
	for {
		unlock, err := desktop.Lock(p.Lock)
		if err == nil {
			return unlock, nil, nil
		}
		if !errors.Is(err, desktop.ErrLocked) {
			return nil, nil, err
		}
		if inst, ok := desktop.FindRunning(ctx, p.Instance); ok {
			return nil, &inst, nil
		}
		if time.Now().After(deadline) {
			return nil, nil, errors.New("sorus はすでに起動していますが、応答がありません。しばらく待ってから、もう一度起動してください")
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// importConfig は import サブコマンドの設定。
type importConfig struct {
	file           string
	downloadImages bool
	accountMode    string
	defaultAuthor  string
	// defaultAuthorSet は --default-author を指定したか（current モードでは使わないことを伝えるため）。
	defaultAuthorSet bool
	dryRun           bool
	skipPages        bool
	timezone         string
	// site はデスクトップモードで取り込む先のサイトの id（--site）。
	site string
}

// parseImportArgs は "import [flags] <file>" または "--import=<file> [flags]" の引数を解析する。
// インポートの呼び出しでなければ ok=false を返す。フラグはファイル名の前後どちらにも置ける。
func parseImportArgs(args []string) (cfg importConfig, ok bool, err error) {
	switch {
	case len(args) > 0 && args[0] == "import":
		args = args[1:]
	case slices.ContainsFunc(args, func(a string) bool {
		return a == "-import" || a == "--import" || strings.HasPrefix(a, "-import=") || strings.HasPrefix(a, "--import=")
	}):
	default:
		return cfg, false, nil
	}

	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.StringVar(&cfg.file, "import", "", "取り込む WordPress WXR ファイル")
	fs.BoolVar(&cfg.downloadImages, "download-images", true, "外部画像をダウンロードして WebP 化する")
	fs.StringVar(&cfg.accountMode, "account-mode", "current", "著者の扱い: current（管理者のアカウントにまとめる。既定）または authors（WordPress の著者ごとにユーザーを作る）")
	fs.StringVar(&cfg.defaultAuthor, "default-author", importer.DefaultAuthor, "authors モードで、著者が空の記事を割り当てるユーザー名")
	fs.BoolVar(&cfg.dryRun, "dry-run", false, "変更をコミットせず検証だけ行う")
	fs.BoolVar(&cfg.skipPages, "skip-pages", false, "固定ページ (page) を取り込まない")
	fs.StringVar(&cfg.timezone, "timezone", "UTC", "GMT 日時の無い記事の日時を解釈するタイムゾーン（例: Asia/Tokyo）")
	addSiteFlag(fs, &cfg.site)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "使い方: sorus import [flags] <wordpress.xml>  または  sorus --import=<wordpress.xml> [flags]")
		fs.PrintDefaults()
	}

	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return cfg, true, err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(positional) > 1 || (len(positional) == 1 && cfg.file != "") {
		return cfg, true, fmt.Errorf("import file specified more than once")
	}
	if len(positional) == 1 {
		cfg.file = positional[0]
	}
	if cfg.file == "" {
		fs.Usage()
		return cfg, true, fmt.Errorf("missing WXR file")
	}
	if cfg.accountMode != "" && cfg.accountMode != "current" && cfg.accountMode != "authors" {
		return cfg, true, fmt.Errorf("invalid account mode %q (want current or authors)", cfg.accountMode)
	}
	fs.Visit(func(f *flag.Flag) { cfg.defaultAuthorSet = cfg.defaultAuthorSet || f.Name == "default-author" })
	return cfg, true, nil
}

// runImport は WXR を DB に取り込み、サマリーを stdout に表示する。
// 検索インデックスはサーバーと共有するため、サーバー停止中に実行すること。
func runImport(cfg importConfig, stdout, stderr io.Writer) error {
	loc, err := time.LoadLocation(cfg.timezone)
	if err != nil {
		return fmt.Errorf("invalid timezone %q: %w", cfg.timezone, err)
	}

	syncPath, err := importSyncAPIPath()
	if err != nil {
		return err
	}

	db := openDatabase()
	defer db.Close()
	if err := checkDesktopSetupDone(db); err != nil {
		return err
	}
	accountMode := cfg.accountMode
	if accountMode == "" {
		accountMode = "current"
	}
	var account string
	if accountMode == "current" {
		u, err := importCurrentAccount(db)
		if err != nil {
			return err
		}
		account = u.ID
		if cfg.defaultAuthorSet {
			fmt.Fprintln(stderr, "お知らせ: current モードでは --default-author を使いません。著者ごとにユーザーを作るには --account-mode=authors を指定してください。")
		}
	}

	searchIndex, err := search.OpenOrCreateIndex(envOr("SEARCH_INDEX_DIR", "search_index"))
	if err != nil {
		return fmt.Errorf("open search index: %w", err)
	}
	defer searchIndex.Close()

	// 固定ページのスラッグは、サーバーでの保存と同じく同期 API のパスの先頭部分も予約語として扱う。
	articles := article.NewService(db, article.WithReservedSlugs(article.PathReservedSlug(syncPath)))
	im := importer.New(db, searchIndex, importer.Options{
		DefaultAuthor:  cfg.defaultAuthor,
		Account:        account,
		DownloadImages: cfg.downloadImages,
		DryRun:         cfg.dryRun,
		SkipPages:      cfg.skipPages,
		UploadDir:      envOr("UPLOAD_DIR", "uploads"),
		Location:       loc,
		// 取り込んだページの /<slug> がサーバーの同期 API のパスを隠さないようにする。
		ReservedPageSlug: articles.IsReservedPageSlug,
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("Importing %s ...", cfg.file)
	sum, err := im.ImportFile(ctx, cfg.file)
	if err != nil {
		return err
	}
	sum.Write(stdout)
	return nil
}

// importSyncAPIPath はインポート先のサーバーが使う同期 API のパスを返す。SYNC_API_PATH があればそれを、
// 無ければ署名鍵（CAPABILITY_KEY_FILE）と同じディレクトリの sync_path.txt を読む（作成はしない）。
// どちらも無ければ DefaultSyncAPIPath を返す。
func importSyncAPIPath() (string, error) {
	explicit, err := parseSyncPathOption(os.Getenv("SYNC_API_PATH"))
	if err != nil || explicit != "" {
		return explicit, err
	}
	p, err := loadSyncAPIPath(syncPathFile(envOr("CAPABILITY_KEY_FILE", defaultCapabilityKeyFile)))
	if errors.Is(err, os.ErrNotExist) {
		return server.DefaultSyncAPIPath, nil
	}
	return p, err
}

// autocertConfig は Let's Encrypt による自動 HTTPS 配信の設定。
type autocertConfig struct {
	domains   []string // 証明書を取得するドメイン（これ以外の SNI には証明書を発行しない）
	email     string   // ACME アカウントの連絡先（任意）
	cacheDir  string   // 証明書・アカウント鍵のキャッシュ先（0700 で作成される）
	httpAddr  string   // HTTP-01 チャレンジと HTTPS リダイレクトを受けるアドレス
	httpsAddr string   // TLS 配信のアドレス
}

// autocertConfigFromEnv は AUTOCERT_DOMAIN（カンマ区切りで複数可）が設定されていれば
// 自動 HTTPS の設定を返す。未設定なら ok=false で、従来どおり PORT で HTTP 配信する。
func autocertConfigFromEnv() (cfg autocertConfig, ok bool) {
	for _, d := range strings.Split(os.Getenv("AUTOCERT_DOMAIN"), ",") {
		if d = strings.TrimSpace(d); d != "" {
			cfg.domains = append(cfg.domains, d)
		}
	}
	if len(cfg.domains) == 0 {
		return cfg, false
	}
	cfg.email = os.Getenv("AUTOCERT_EMAIL")
	cfg.cacheDir = envOr("AUTOCERT_CACHE_DIR", "certs")
	cfg.httpAddr = envOr("AUTOCERT_HTTP_ADDR", ":80")
	cfg.httpsAddr = envOr("AUTOCERT_HTTPS_ADDR", ":443")
	return cfg, true
}

// listenAddrFromEnv は --server・--public-only の待ち受けアドレスを返す。LISTEN_ADDR（無ければ BIND_ADDR）を指定すれば
// そのアドレス（例: 127.0.0.1:8080 なら、この PC の中からの要求だけを受け付け、前段のリバースプロキシだけに見せる）、
// 無ければ従来どおりすべてのアドレスの PORT（:8080）。ポートを書かずにアドレスだけ（例: 127.0.0.1）を書けば PORT を使う。
// 読めない値はエラーにする（意図せずすべてのアドレスで待ち受けないよう、起動を止める）。
func listenAddrFromEnv(port string) (string, error) {
	for _, key := range []string{"LISTEN_ADDR", "BIND_ADDR"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			addr, err := normalizeListenAddr(v, port)
			if err != nil {
				return "", fmt.Errorf("invalid %s %q: %w", key, v, err)
			}
			return addr, nil
		}
	}
	return ":" + port, nil
}

// autocertPlainAddr は自動 HTTPS（AUTOCERT_DOMAIN）のときの平文ポート（/healthz にだけ答え、ほかは HTTPS へ送る）の
// 待ち受けアドレスを返す。AUTOCERT_PLAIN_ADDR を指定すればそのアドレス（例: 127.0.0.1:8080 なら、この PC・コンテナの中の
// ヘルスチェックだけに見せる）、無ければ listenAddr（LISTEN_ADDR・BIND_ADDR、それも無ければ :PORT）。
func autocertPlainAddr(listenAddr, port string) (string, error) {
	v := strings.TrimSpace(os.Getenv("AUTOCERT_PLAIN_ADDR"))
	if v == "" {
		return listenAddr, nil
	}
	addr, err := normalizeListenAddr(v, port)
	if err != nil {
		return "", fmt.Errorf("invalid AUTOCERT_PLAIN_ADDR %q: %w", v, err)
	}
	return addr, nil
}

// errInvalidListenAddr は待ち受けアドレスとして読めない値のエラー。
var errInvalidListenAddr = errors.New("use an address and port such as 127.0.0.1:8080")

// normalizeListenAddr は host:port・:port・host（127.0.0.1・localhost・::1・[::1]）の形の v を、net.Listen に渡せる
// host:port にする。ポートを書かなければ port を使う。
func normalizeListenAddr(v, port string) (string, error) {
	host, p, err := net.SplitHostPort(v)
	if err != nil {
		host, p = strings.TrimSuffix(strings.TrimPrefix(v, "["), "]"), port
	}
	if n, err := strconv.Atoi(p); err != nil || n < 0 || n > 65535 {
		return "", errInvalidListenAddr
	}
	if host != "" && net.ParseIP(host) == nil && !validListenHostname(host) {
		return "", errInvalidListenAddr
	}
	return net.JoinHostPort(host, p), nil
}

// validListenHostname は host が待ち受けに使えるホスト名（英数字・ハイフン・ドット。例: localhost）かを返す。
func validListenHostname(host string) bool {
	if host == "" || len(host) > 253 || strings.HasPrefix(host, ".") || strings.HasPrefix(host, "-") {
		return false
	}
	for _, r := range host {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

// listenURL はログに出す、待ち受けアドレス addr を開く URL（すべてのアドレスで待ち受けるなら localhost）を返す。
func listenURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// splitEnvList は環境変数 key のカンマ区切りの値を、前後の空白を除いて返す（空の値は除く）。
func splitEnvList(key string) []string {
	var out []string
	for _, v := range strings.Split(os.Getenv(key), ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// newAutocertServers は handler を TLS で配信する HTTPS サーバーと、ACME の HTTP-01 チャレンジに
// 応答しつつそれ以外を HTTPS へリダイレクトする HTTP サーバーを組み立てる。
// 証明書は cfg.cacheDir にキャッシュされ、有効期限前に自動更新される。
func newAutocertServers(handler http.Handler, cfg autocertConfig) (httpSrv, httpsSrv *http.Server) {
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(cfg.domains...),
		Cache:      autocert.DirCache(cfg.cacheDir),
		Email:      cfg.email,
	}
	httpSrv = &http.Server{
		Addr:              cfg.httpAddr,
		Handler:           m.HTTPHandler(nil), // nil: チャレンジ以外は https:// へリダイレクト
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	httpsSrv = &http.Server{
		Addr:      cfg.httpsAddr,
		Handler:   handler,
		TLSConfig: m.TLSConfig(),
	}
	// 通常の配信と同じ待ち時間の上限を付ける。長くかかる要求はハンドラーが期限を延ばす（HTTP/2 でも同じく延ばせる）。
	server.SetTimeouts(httpsSrv)
	return httpSrv, httpsSrv
}

// newPlainHealthCheckHandler は平文ポート用の安全なハンドラーを返す。
// コンテナ内やローカルのヘルスチェック（/healthz）のみ直接応答し、
// それ以外のすべてのパスは HTTPS（primaryDomain）へリダイレクトして平文での認証情報送信を防ぐ。
func newPlainHealthCheckHandler(handler http.Handler, primaryDomain string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			handler.ServeHTTP(w, r)
			return
		}
		host := primaryDomain
		if host == "" {
			host = r.Host
		}
		reqURI := r.URL.RequestURI()
		if reqURI == "" {
			reqURI = "/"
		}
		target := "https://" + host + reqURI
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	})
}

// newPlainHealthCheckServer は自動 HTTPS のときの平文ポートのサーバー（plainAddr で待ち受け、/healthz にだけ答えて、
// ほかは cfg の最初のドメインの HTTPS へ送る）を組み立てる。
func newPlainHealthCheckServer(handler http.Handler, cfg autocertConfig, plainAddr string) *http.Server {
	primaryDomain := ""
	if len(cfg.domains) > 0 {
		primaryDomain = cfg.domains[0]
	}
	return &http.Server{
		Addr:              plainAddr,
		Handler:           newPlainHealthCheckHandler(handler, primaryDomain),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// startServerWithAutoCert は autocert の HTTP / HTTPS サーバーと平文ヘルスチェックポート（plainAddr。autocertPlainAddr）を
// バックグラウンドで起動する。各サーバーが終了すると、その結果（Shutdown による停止なら http.ErrServerClosed）を errc に送る。
// errc は 3 件を受け取れるバッファを持つこと。返す関数ですべてのサーバーを停止する。
func startServerWithAutoCert(handler http.Handler, cfg autocertConfig, plainAddr string, errc chan<- error) (shutdown func(context.Context) error) {
	httpSrv, httpsSrv := newAutocertServers(handler, cfg)
	plainSrv := newPlainHealthCheckServer(handler, cfg, plainAddr)
	go func() { errc <- httpSrv.ListenAndServe() }()
	go func() { errc <- httpsSrv.ListenAndServeTLS("", "") }() // 証明書は TLSConfig.GetCertificate が供給する
	go func() { errc <- plainSrv.ListenAndServe() }()
	return func(ctx context.Context) error {
		return errors.Join(httpSrv.Shutdown(ctx), httpsSrv.Shutdown(ctx), plainSrv.Shutdown(ctx))
	}
}

// backupConfig は backup サブコマンドの設定。
type backupConfig struct {
	// dest はバックアップ先。空なら BACKUP_DIR（既定 backups）に sorus_backup_YYYYMMDD_HHMMSS.zip
	// （dbOnly なら backup_YYYYMMDD_HHMMSS.db）を作る。
	dest string
	// dbOnly は DB だけを SQLite のファイルとして書き出すか（--db-only）。dest の拡張子が .db のときも DB だけにする。
	dbOnly bool
	// site はデスクトップモードでバックアップするサイトの id（--site）。
	site string
}

// parseBackupArgs は "backup [--db-only] [dest_path]" の引数を解析する。
// バックアップの呼び出しでなければ ok=false を返す。
func parseBackupArgs(args []string) (cfg backupConfig, ok bool, err error) {
	if len(args) == 0 || args[0] != "backup" {
		return cfg, false, nil
	}

	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.BoolVar(&cfg.dbOnly, "db-only", false, "画像を含めず、データベースだけを SQLite のファイル（.db）として書き出す")
	addSiteFlag(fs, &cfg.site)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "使い方: sorus backup [--db-only] [dest_path]")
		fmt.Fprintln(fs.Output(), "  データベースとアップロード画像（$UPLOAD_DIR）・添付資料（$ATTACHMENT_DIR）をまとめた完全バックアップ（Zip）を作る。")
		fmt.Fprintln(fs.Output(), "  dest_path を省略すると $BACKUP_DIR（既定 backups）/sorus_backup_YYYYMMDD_HHMMSS.zip に書き出す。")
		fmt.Fprintln(fs.Output(), "  --db-only を付けるか dest_path の拡張子が .db なら、データベースだけを書き出す（既定の名前は backup_YYYYMMDD_HHMMSS.db）。")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args[1:]); err != nil {
		return cfg, true, err
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return cfg, true, fmt.Errorf("too many arguments: %q", fs.Args())
	}
	cfg.dest = fs.Arg(0)
	return cfg, true, nil
}

// runBackup は DB_PATH の DB（と UPLOAD_DIR の画像・ATTACHMENT_DIR の添付資料）のバックアップを cfg.dest に作り、
// サマリーを w に書く。
// DB は VACUUM INTO で複製するため、サーバー稼働中でも読み書きを止めずに実行できる。
func runBackup(cfg backupConfig, w io.Writer) error {
	dbPath := envOr("DB_PATH", "database.db")
	// Connect は存在しない DB ファイルを新規作成するため、空の DB をバックアップしないよう先に確かめる。
	if err := checkSourceDatabaseExists(dbPath); err != nil {
		return err
	}
	dbOnly := cfg.dbOnly || strings.EqualFold(filepath.Ext(cfg.dest), ".db")
	dest := cfg.dest
	if dest == "" {
		name := backup.FileName(time.Now())
		if dbOnly {
			name = database.BackupFileName(time.Now())
		}
		dest = filepath.Join(envOr("BACKUP_DIR", "backups"), name)
	}
	dest, err := filepath.Abs(dest)
	if err != nil {
		return err
	}

	db, err := database.Connect(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	start := time.Now()
	uploadDir := envOr("UPLOAD_DIR", "uploads")
	attachDir := envOr("ATTACHMENT_DIR", server.DefaultAttachmentDir)
	if dbOnly {
		err = database.Backup(db, dest)
	} else {
		err = backup.CreateWith(db, dest, backup.CreateOptions{UploadDir: uploadDir, AttachmentDir: attachDir})
	}
	if err != nil {
		return err
	}
	elapsed := time.Since(start)
	info, err := os.Stat(dest)
	if err != nil {
		return err
	}

	fmt.Fprintln(w, "Backup completed")
	fmt.Fprintln(w, "  Warning: This backup contains passwords and API keys. Do not share it.")
	fmt.Fprintln(w, "  To share the site, download the static site as a Zip.")
	fmt.Fprintf(w, "  Source:      %s\n", dbPath)
	if !dbOnly {
		m, err := backup.ReadManifest(dest)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "  Uploads:     %s (%d files, %d bytes)\n", uploadDir, m.Uploads.FileCount, m.Uploads.TotalSize)
		if m.Attachments != nil {
			fmt.Fprintf(w, "  Attachments: %s (%d files, %d bytes)\n", attachDir, m.Attachments.FileCount, m.Attachments.TotalSize)
		}
	}
	fmt.Fprintf(w, "  Destination: %s\n", dest)
	fmt.Fprintf(w, "  Size:        %d bytes\n", info.Size())
	fmt.Fprintf(w, "  Elapsed:     %s\n", elapsed.Round(time.Millisecond))
	return nil
}

// errRestoreCancelled は restore の確認で yes 以外が入力されたことを表す。
var errRestoreCancelled = errors.New("restore cancelled")

// restoreConfig は restore サブコマンドの設定。
type restoreConfig struct {
	src  string // 復元する完全バックアップ（sorus_backup_YYYYMMDD_HHMMSS.zip）
	yes  bool   // 確認の入力を省く（--yes）
	site string // デスクトップモードで復元する先のサイトの id（--site）
}

// parseRestoreArgs は "restore [--yes] <backup.zip>" の引数を解析する。
// 復元の呼び出しでなければ ok=false を返す。
func parseRestoreArgs(args []string) (cfg restoreConfig, ok bool, err error) {
	if len(args) == 0 || args[0] != "restore" {
		return cfg, false, nil
	}

	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.BoolVar(&cfg.yes, "yes", false, "確認の入力を省いて復元する（スクリプトから実行する場合）")
	addSiteFlag(fs, &cfg.site)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "使い方: sorus restore [--yes] <backup.zip>")
		fmt.Fprintln(fs.Output(), "  sorus backup や管理画面で作成した完全バックアップを、$DB_PATH と $UPLOAD_DIR・$ATTACHMENT_DIR に復元する。")
		fmt.Fprintln(fs.Output(), "  添付資料を含まない以前のバックアップでは、$ATTACHMENT_DIR はそのまま残す。")
		fmt.Fprintln(fs.Output(), "  sorus のサーバーを停止してから実行する（稼働中のサーバーへは管理画面の「バックアップ」から復元する）。")
		fmt.Fprintln(fs.Output(), "  置き換える前の DB と画像・添付資料は、元の場所に "+backup.PreRestoreInfix+"YYYYMMDD_HHMMSS を付けた名前で残す。")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args[1:]); err != nil {
		return cfg, true, err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return cfg, true, fmt.Errorf("specify exactly one backup file, got %q", fs.Args())
	}
	cfg.src = fs.Arg(0)
	return cfg, true, nil
}

// runRestore は cfg.src の完全バックアップを検査して内容を out に表示し、確認のうえで DB_PATH と UPLOAD_DIR・
// ATTACHMENT_DIR に復元する。添付資料を含まない形式のバージョン1のアーカイブでは ATTACHMENT_DIR に触れない。
// cfg.yes でなければ in から1行読み、yes でなければ何も変えずに errRestoreCancelled を返す。
func runRestore(cfg restoreConfig, in io.Reader, out io.Writer) error {
	dbPath := envOr("DB_PATH", "database.db")
	if strings.HasPrefix(dbPath, "file:") || strings.Contains(dbPath, "?") || strings.Contains(dbPath, ":memory:") {
		return fmt.Errorf("DB_PATH must be a plain file path to restore into, got %q", dbPath)
	}
	uploadDir := envOr("UPLOAD_DIR", "uploads")
	attachDir := envOr("ATTACHMENT_DIR", server.DefaultAttachmentDir)
	m, err := backup.ReadManifest(cfg.src)
	if err != nil {
		return err
	}

	fmt.Fprintln(out, "Backup to restore")
	fmt.Fprintf(out, "  File:        %s\n", cfg.src)
	fmt.Fprintf(out, "  Created at:  %s\n", m.CreatedAt.Local().Format(time.RFC3339))
	fmt.Fprintf(out, "  Created by:  sorus %s (schema v%d)\n", m.AppVersion, m.SchemaVersion)
	fmt.Fprintf(out, "  Database:    %d bytes\n", m.Database.Size)
	fmt.Fprintf(out, "  Uploads:     %d files, %d bytes\n", m.Uploads.FileCount, m.Uploads.TotalSize)
	if m.Attachments != nil {
		fmt.Fprintf(out, "  Attachments: %d files, %d bytes\n", m.Attachments.FileCount, m.Attachments.TotalSize)
	}
	fmt.Fprintln(out, "Restore into")
	fmt.Fprintf(out, "  Database:    %s\n", dbPath)
	fmt.Fprintf(out, "  Uploads:     %s\n", uploadDir)
	if m.Attachments != nil {
		fmt.Fprintf(out, "  Attachments: %s\n", attachDir)
	}
	if !cfg.yes {
		fmt.Fprintln(out, "The current database and files will be replaced. They are kept next to the originals")
		fmt.Fprintln(out, "with \""+backup.PreRestoreInfix+"<timestamp>\" added to their names. Stop the sorus server before continuing.")
		fmt.Fprint(out, "Type yes to restore: ")
		line, _ := bufio.NewReader(in).ReadString('\n')
		if strings.TrimSpace(line) != "yes" {
			return errRestoreCancelled
		}
	}

	start := time.Now()
	if err := backup.RestoreWith(cfg.src, backup.RestoreTargets{DBPath: dbPath, UploadDir: uploadDir, AttachmentDir: attachDir}); err != nil {
		return err
	}
	fmt.Fprintln(out, "Restore completed")
	fmt.Fprintf(out, "  Elapsed:     %s\n", time.Since(start).Round(time.Millisecond))
	fmt.Fprintln(out, "  The previous data was kept with \""+backup.PreRestoreInfix+"<timestamp>\" added to the names. Start sorus to use the restored data.")
	return nil
}

// checkSourceDatabaseExists はバックアップ元 DB の実ファイルが存在することを確かめる。
// URI 指定（file:path?mode=... 等）やインメモリ DB（:memory:）も考慮する。
func checkSourceDatabaseExists(dbPath string) error {
	if strings.Contains(dbPath, ":memory:") {
		return nil
	}
	raw := dbPath
	if strings.HasPrefix(raw, "file:") {
		raw = strings.TrimPrefix(raw, "file:")
		if idx := strings.Index(raw, "?"); idx >= 0 {
			raw = raw[:idx]
		}
	}
	if raw == "" {
		return nil
	}
	if _, err := os.Stat(raw); err != nil {
		return fmt.Errorf("source database: %w", err)
	}
	return nil
}

// exportConfig は export サブコマンドの設定。
type exportConfig struct {
	outDir string // 出力先ディレクトリ（既定: dist）
	theme  string // テーマ上書き（任意）
	clean  bool   // 前回の書き出しの残りを消して入れ替えるか（--clean）
	site   string // デスクトップモードで書き出すサイトの id（--site）
}

// parseExportArgs は "export [out_dir]" の引数を解析する。
// エクスポートの呼び出しでなければ ok=false を返す。
func parseExportArgs(args []string) (cfg exportConfig, ok bool, err error) {
	if len(args) == 0 || args[0] != "export" {
		return cfg, false, nil
	}

	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.StringVar(&cfg.theme, "theme", "", "公開テーマを "+strings.Join(theme.Builtin.IDs(), ", ")+" のいずれかで一時上書きする")
	// 既定は従来どおり上書きだけにする（out_dir に利用者が置いたファイルを黙って消さない）。
	fs.BoolVar(&cfg.clean, "clean", false, "隣の一時フォルダに書き出してから out_dir と入れ替え、前回の書き出しの残り（下書きに戻した記事など）を消す")
	addSiteFlag(fs, &cfg.site)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "使い方: sorus export [--clean] [--theme "+exportThemeChoices()+"] [out_dir]")
		fmt.Fprintln(fs.Output(), "  out_dir を省略すると $EXPORT_DIR（既定 dist）に静的HTML・アセット一式を書き出す")
		fmt.Fprintln(fs.Output(), "  --clean は out_dir の中身を新しい書き出しと入れ替える（out_dir に自分で置いたファイルも消える）")
		if desktopMode {
			fmt.Fprintln(fs.Output(), "  --site <id> は書き出すサイト（サイトが2つ以上あるときに指定する。一覧は sorus sites）")
		}
	}
	if err := fs.Parse(args[1:]); err != nil {
		return cfg, true, err
	}

	switch fs.NArg() {
	case 0:
		cfg.outDir = envOr("EXPORT_DIR", "dist")
	case 1:
		cfg.outDir = fs.Arg(0)
	default:
		return cfg, true, fmt.Errorf("extra argument: %s", fs.Arg(1))
	}
	// DB を開く前に、管理画面・同期 API と同じカタログ（ビルトインと取り込んだカスタムテーマ）でテーマを確かめる。
	// デスクトップモードで --site を指定したときは、そのサイトのカスタムテーマをまだ読めないため、書き出しで確かめる。
	if cfg.theme != "" && !(desktopMode && cfg.site != "") {
		themes := exportThemeCatalog()
		if _, ok := themes.Get(cfg.theme); !ok {
			return cfg, true, fmt.Errorf("unknown theme %q: use one of %s", cfg.theme, strings.Join(themes.IDs(), ", "))
		}
	}
	return cfg, true, nil
}

// exportThemeCatalog は書き出すサイトのテーマのカタログ（ビルトインと $THEMES_DIR のカスタムテーマ）を返す。
// カスタムテーマの置き場所を読めなければビルトインのみのカタログを返す。
func exportThemeCatalog() *theme.Catalog {
	if c, err := theme.LoadComposite(static.FS, envOr("THEMES_DIR", server.DefaultThemesDir)); err == nil {
		return c
	}
	return theme.Builtin
}

// exportThemeChoices は使い方の表示に出す --theme の選択肢（例: modern|tech|news）。
func exportThemeChoices() string { return strings.Join(theme.Builtin.IDs(), "|") }

// runExport は静的サイト生成を実行する。
func runExport(cfg exportConfig) error {
	db := openDatabase()
	defer db.Close()

	srv, err := server.New(server.Config{
		DB:            db,
		UploadDir:     envOr("UPLOAD_DIR", "uploads"),
		AttachmentDir: envOr("ATTACHMENT_DIR", "attachments"),
		ThemesDir:     envOr("THEMES_DIR", server.DefaultThemesDir),
		SiteName:      envOr("SITE_NAME", "sorus"),
		BaseURL:       envOr("BASE_URL", "http://localhost:8080"),
	})
	if err != nil {
		return fmt.Errorf("init server for export: %w", err)
	}

	rep, err := srv.Export(context.Background(), server.ExportOptions{OutDir: cfg.outDir, Theme: cfg.theme, Clean: cfg.clean})
	if err != nil {
		return err
	}

	log.Printf("Export completed successfully into %q in %v:", rep.OutDir, rep.Duration.Round(time.Millisecond))
	log.Printf("  - Articles: %d", rep.ArticlesCount)
	log.Printf("  - Pages:    %d", rep.PagesCount)
	log.Printf("  - Files:    %d", rep.FilesCount)
	for _, w := range rep.Warnings {
		log.Printf("注意: %s", w)
	}
	return nil
}

// defaultCapabilityKeyFile はケーパビリティ・トークンの署名鍵（Ed25519 秘密鍵）の既定の保存先。
// CAPABILITY_KEY_FILE で変更できる。
const defaultCapabilityKeyFile = "capability.key"

// syncPathFileName は自動生成した同期 API の秘匿パスを保存するファイルの名前。署名鍵と同じディレクトリに置く。
const syncPathFileName = "sync_path.txt"

// maxTokenTTL は発行できるトークンの有効期間の上限（10年）。
const maxTokenTTL = 3650 * 24 * time.Hour

// serveConfig はサーバー起動時のオプション。
type serveConfig struct {
	publicOnly bool   // 公開専用モード（--public-only / PUBLIC_ONLY=true）
	keyFile    string // 署名鍵ファイル（CAPABILITY_KEY_FILE）
	syncPath   string // 同期 API のパス（--sync-path / SYNC_API_PATH）。空なら署名鍵と同じディレクトリの sync_path.txt を使う
}

// parseServeArgs はサーバー起動時の引数を解析する。--public-only の既定値は環境変数 PUBLIC_ONLY、
// --sync-path を指定しなければ環境変数 SYNC_API_PATH を使う（syncPathOrEnv）。
// PUBLIC_ONLY が真偽値として読めなければ、意図せず管理画面を公開しないよう起動を止めるためエラーを返す。
func parseServeArgs(args []string) (cfg serveConfig, err error) {
	cfg.keyFile = envOr("CAPABILITY_KEY_FILE", defaultCapabilityKeyFile)
	if v := strings.TrimSpace(os.Getenv("PUBLIC_ONLY")); v != "" {
		if cfg.publicOnly, err = strconv.ParseBool(v); err != nil {
			return cfg, fmt.Errorf("invalid PUBLIC_ONLY %q: use true or false", v)
		}
	}

	fs := newServeFlagSet(&cfg)
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return cfg, fmt.Errorf("unknown command or argument: %q", fs.Arg(0))
	}
	if cfg.syncPath, err = parseSyncPathOption(syncPathOrEnv(fs, cfg.syncPath)); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// newServeFlagSet はサーバー起動時のフラグを cfg に結び付けた FlagSet を返す。各フラグの既定値は cfg の今の値。
// 使い方（Usage）には起動の仕方とすべてのサブコマンドを出す。
func newServeFlagSet(cfg *serveConfig) *flag.FlagSet {
	fs := flag.NewFlagSet("sorus", flag.ContinueOnError)
	fs.BoolVar(&cfg.publicOnly, "public-only", cfg.publicOnly, "公開専用モードで起動する（管理画面を無効にし、公開ページと同期 API だけを配信する）")
	fs.StringVar(&cfg.syncPath, "sync-path", cfg.syncPath,
		"同期 API のパス（例: /api/sync-<ランダムな16進数>）。省略すると環境変数 SYNC_API_PATH を、それも無ければ署名鍵と同じディレクトリの "+syncPathFileName+" に保存した推測できないパスを使う（無ければ生成する）")
	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintln(out, "使い方: sorus [--desktop | --server] [--public-only] [--sync-path /api/sync-...]")
		fmt.Fprintln(out, "       sorus token create --role writer|editor|admin --sub <名前> [--ttl 90d] [--key-file path] [--sync-path path] [--base-url https://example.com]")
		fmt.Fprintln(out, "       sorus import [flags] <wordpress.xml>  または  sorus --import=<wordpress.xml> [flags]")
		fmt.Fprintln(out, "       sorus export [--clean] [--theme "+exportThemeChoices()+"] [out_dir]")
		fmt.Fprintln(out, "       sorus backup [--db-only] [dest_path]")
		fmt.Fprintln(out, "       sorus restore [--yes] <backup.zip>")
		fmt.Fprintln(out, "       sorus sites")
		fmt.Fprintln(out, "       sorus --version")
		fmt.Fprintln(out, "       sorus help")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "動作モード:")
		fmt.Fprintln(out, "  --desktop  自分の PC で使う（デスクトップモード）。データは PC の決まった場所に置き、管理画面をブラウザで開く")
		fmt.Fprintln(out, "  --server   サーバーとして動かす（デスクトップ版でもサーバーモードにする）")
		fmt.Fprintln(out, "  --desktop を付けると（デスクトップ版では付けなくても）import・export・backup・restore もデスクトップモードのデータを使う")
		fmt.Fprintln(out, "  デスクトップモードでサイトが2つ以上あるときは、import・export・backup・restore に --site <id> を付けて、どのサイトかを指定する")
		fmt.Fprintln(out, "  sorus sites  デスクトップモードのサイトの一覧（id・サイト名・アドレス・フォルダ）を表示する")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "その他:")
		fmt.Fprintln(out, "  --version  版を表示する")
		fmt.Fprintln(out, "  help       この使い方を表示する（-h・--help も同じ）")
		fmt.Fprintln(out, "  各サブコマンドの詳しい使い方は -h で表示する（例: sorus import -h）")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "サーバーモードのオプション:")
		fs.PrintDefaults()
	}
	return fs
}

// writeUsage は sorus の使い方（起動の仕方とすべてのサブコマンド）を w に書く。sorus help・-h・--help で使う。
// 環境変数に設定した同期 API のパスを画面に出さないよう、既定値は空のまま表示する。
func writeUsage(w io.Writer) {
	fs := newServeFlagSet(&serveConfig{})
	fs.SetOutput(w)
	fs.Usage()
}

// syncPathOrEnv は、fs で --sync-path を指定していればその値 p を、していなければ環境変数 SYNC_API_PATH を返す。
// SYNC_API_PATH は推測されないよう秘匿するパスのため、フラグの既定値にはせず（-h や引数の誤りで出す使い方に
// 表示しない）、解析のあとで補う。
func syncPathOrEnv(fs *flag.FlagSet, p string) string {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "sync-path" {
			set = true
		}
	})
	if set {
		return p
	}
	return os.Getenv("SYNC_API_PATH")
}

// parseSyncPathOption は --sync-path・SYNC_API_PATH の値を検査して正規化する。空白だけなら未指定として空を返す。
func parseSyncPathOption(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	p, err := server.NormalizeSyncAPIPath(raw)
	if err != nil {
		return "", fmt.Errorf("invalid --sync-path (SYNC_API_PATH): %w", err)
	}
	return p, nil
}

// syncPublicKey は同期 API の署名検証に使う公開鍵を返す（syncSigningKey の公開鍵）。
func syncPublicKey(cfg serveConfig) (ed25519.PublicKey, error) {
	priv, err := syncSigningKey(cfg)
	if priv == nil || err != nil {
		return nil, err
	}
	return priv.Public().(ed25519.PublicKey), nil
}

// syncSigningKey は同期 API の署名鍵を返す。公開専用モードでは署名鍵ファイルが無ければ作成する。
// それ以外では署名鍵ファイルがあるときだけ同期 API を有効にし、無ければ nil を返す。
func syncSigningKey(cfg serveConfig) (ed25519.PrivateKey, error) {
	var (
		priv    ed25519.PrivateKey
		created bool
		err     error
	)
	if cfg.publicOnly {
		priv, created, err = loadOrCreateCapabilityKey(cfg.keyFile)
	} else {
		priv, err = loadCapabilityKey(cfg.keyFile)
		if errors.Is(err, os.ErrNotExist) {
			log.Println("Sync API: disabled (start with --public-only, or create a key with 'sorus token create')")
			return nil, nil
		}
	}
	if err != nil {
		return nil, err
	}
	if created {
		log.Printf("Created a new capability signing key: %s (keep it secret and back it up)", cfg.keyFile)
	}
	return priv, nil
}

// syncAPIPath は同期 API を有効にするときのパスを返す（resolveSyncAPIPath を参照）。
func syncAPIPath(cfg serveConfig) (string, error) {
	p, created, err := resolveSyncAPIPath(cfg.syncPath, cfg.keyFile)
	if err != nil {
		return "", err
	}
	if created {
		log.Printf("Created a new secret sync API path: %s (keep it with the signing key)", syncPathFile(cfg.keyFile))
	}
	log.Printf("Sync API: enabled at POST %s (signing key: %s)", p, cfg.keyFile)
	if cfg.publicOnly && p == server.DefaultSyncAPIPath {
		log.Printf("Sync API: %s is easy to guess; unset SYNC_API_PATH to use a secret path from %s", p, syncPathFileName)
	}
	return p, nil
}

// syncPathFile は署名鍵 keyFile と同じディレクトリの sync_path.txt のパスを返す。
func syncPathFile(keyFile string) string {
	return filepath.Join(filepath.Dir(keyFile), syncPathFileName)
}

// resolveSyncAPIPath は同期 API のパスを返す。explicit（正規化済みの --sync-path・SYNC_API_PATH）が空でなければそれを使う。
// 空なら署名鍵 keyFile と同じディレクトリの sync_path.txt から読み込み、無ければ推測できないパスを生成して保存する。
// 公開サーバーと sorus token create は同じ手順でパスを決めるため、案内するパスと公開サーバーのパスが一致する。
// created は sync_path.txt を新しく作成したか。
func resolveSyncAPIPath(explicit, keyFile string) (p string, created bool, err error) {
	if explicit != "" {
		return explicit, false, nil
	}
	return loadOrCreateSyncAPIPath(syncPathFile(keyFile))
}

// loadSyncAPIPath は path から同期 API のパスを読み込む。ファイルが無ければ os.ErrNotExist を包んだエラーを返す。
// パスを知られると固定のパスと同じくスキャンや大量の要求の的になるため、署名鍵と同じく権限を検査する（readPrivateFile）。
func loadSyncAPIPath(path string) (string, error) {
	data, err := readPrivateFile(path, "sync path")
	if err != nil {
		return "", err
	}
	p, err := server.NormalizeSyncAPIPath(string(data))
	if err != nil {
		return "", fmt.Errorf("sync path %s: %w (fix the file, or delete it to generate a new path and tell the new URL to every writer)", path, err)
	}
	return p, nil
}

// loadOrCreateSyncAPIPath は path から同期 API のパスを読み込む。ファイルが無ければ推測できないパスを生成し、
// 所有者だけが読み書きできる権限（0600）で保存する。壊れたファイルは作り直さずにエラーにする（作り直すと
// ローカル CMS に設定済みの URL がすべて使えなくなる）。created は新しく作成したか。
func loadOrCreateSyncAPIPath(path string) (p string, created bool, err error) {
	p, err = loadSyncAPIPath(path)
	if !errors.Is(err, os.ErrNotExist) {
		return p, false, err
	}
	if p, err = server.NewSyncAPIPath(); err != nil {
		return "", false, err
	}
	// 同時に別のプロセス（公開サーバーと token create など）が作成していれば、そのパスを使う。
	err = createPrivateFile(path, []byte(p+"\n"))
	if errors.Is(err, os.ErrExist) {
		p, err = loadSyncAPIPath(path)
		return p, false, err
	}
	if err != nil {
		return "", false, fmt.Errorf("sync path: %w", err)
	}
	return p, true, nil
}

// errInsecureKeyFile は署名鍵などの秘密のファイルを所有者以外も読み書きできることを示す。
var errInsecureKeyFile = errors.New("permissions are too open")

// readPrivateFile は所有者だけが読み書きできるべき秘密のファイル（署名鍵・同期 API のパス）を読む。label はエラーの文言に使う。
// ファイルが無ければ os.ErrNotExist を包んだエラーを返す。所有者以外（グループ・その他）が読み書きできるファイルは
// 漏洩している恐れがあるとして、errInsecureKeyFile を包んだエラーで拒否する。Windows の権限ビットは
// 読み取り専用属性しか表さないため、Windows では権限を検査しない。
func readPrivateFile(path, label string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	defer f.Close()
	// パスを調べ直すと検査と読み込みの間にファイルを差し替えられるため、開いたファイル自体の属性を調べる。
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s %s: not a regular file", label, path)
	}
	if perm := st.Mode().Perm(); runtime.GOOS != "windows" && perm&0o077 != 0 {
		return nil, fmt.Errorf("%s %s: %w (mode %04o, other users can access the file); restrict it with: chmod 600 %s",
			label, path, errInsecureKeyFile, perm, path)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	return data, nil
}

// createPrivateFile は data を書いたファイル path を、所有者だけが読み書きできる権限（0600）で新しく作る。
// 既存のファイルは上書きせず、os.ErrExist を包んだエラーを返す。書き込みに失敗したら作りかけのファイルを消す。
func createPrivateFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(path)
		return fmt.Errorf("write %s: %w", path, werr)
	}
	return nil
}

// loadCapabilityKey は path から署名鍵を読み込む。ファイルが無ければ os.ErrNotExist を包んだエラーを返す。
// 鍵があれば任意の権限のトークンを発行できるため、所有者以外が読み書きできる鍵は errInsecureKeyFile を包んだ
// エラーで拒否する（readPrivateFile）。
func loadCapabilityKey(path string) (ed25519.PrivateKey, error) {
	data, err := readPrivateFile(path, "capability key")
	if err != nil {
		return nil, err
	}
	priv, err := auth.DecodePrivateKey(string(data))
	if err != nil {
		return nil, fmt.Errorf("capability key %s: %w", path, err)
	}
	return priv, nil
}

// loadOrCreateCapabilityKey は path から署名鍵を読み込む。ファイルが無ければ新しい鍵を生成し、
// 所有者だけが読み書きできる権限（0600）で保存する。created は新しく作成したか。
func loadOrCreateCapabilityKey(path string) (priv ed25519.PrivateKey, created bool, err error) {
	priv, err = loadCapabilityKey(path)
	if !errors.Is(err, os.ErrNotExist) {
		return priv, false, err
	}
	if _, priv, err = auth.GenerateKeyPair(); err != nil {
		return nil, false, fmt.Errorf("capability key: generate: %w", err)
	}
	// 既存の鍵を上書きしないよう新しいファイルとして作る。同時に別のプロセスが作成していれば、その鍵を使う。
	err = createPrivateFile(path, []byte(auth.EncodePrivateKey(priv)+"\n"))
	if errors.Is(err, os.ErrExist) {
		priv, err = loadCapabilityKey(path)
		return priv, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("capability key: %w", err)
	}
	return priv, true, nil
}

// tokenConfig は token create サブコマンドの設定。
type tokenConfig struct {
	role     string
	sub      string
	ttl      time.Duration
	keyFile  string
	syncPath string // 公開サーバーの同期 API のパス（--sync-path / SYNC_API_PATH）。空なら sync_path.txt を使う
	baseURL  string // 公開サーバーの URL（--base-url / BASE_URL）。指定すると同期 API の完全な URL を案内する
}

// parseTokenArgs は "token create --role <role> --sub <name> [--ttl 90d] [--key-file path] [--sync-path path]
// [--base-url url]" の引数を解析する。トークン発行の呼び出しでなければ ok=false を返す。
func parseTokenArgs(args []string) (cfg tokenConfig, ok bool, err error) {
	if len(args) == 0 || args[0] != "token" {
		return cfg, false, nil
	}

	fs := flag.NewFlagSet("token create", flag.ContinueOnError)
	fs.StringVar(&cfg.role, "role", "", "トークンの権限: writer（記事の提出）, editor（校閲・公開）, admin（サイト設定の反映）")
	fs.StringVar(&cfg.sub, "sub", "", "トークンを渡す相手の名前またはユーザー ID（公開サーバーでの執筆者名になる）")
	ttl := fs.String("ttl", "90d", "有効期間。日数（例: 30d）または Go の時間表記（例: 12h）")
	fs.StringVar(&cfg.keyFile, "key-file", envOr("CAPABILITY_KEY_FILE", defaultCapabilityKeyFile), "署名鍵ファイル。無ければ作成する。公開サーバーと同じファイルを使うこと")
	fs.StringVar(&cfg.syncPath, "sync-path", "",
		"公開サーバーの同期 API のパス。省略すると環境変数 SYNC_API_PATH を、それも無ければ署名鍵と同じディレクトリの "+syncPathFileName+" を使う（無ければ作成する）。公開サーバーと同じ設定にすること")
	fs.StringVar(&cfg.baseURL, "base-url", os.Getenv("BASE_URL"), "公開サーバーの URL（例: https://example.com）。指定すると同期 API の完全な URL を表示する")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "使い方: sorus token create --role writer|editor|admin --sub <名前> [--ttl 90d] [--key-file path] [--sync-path path] [--base-url url]")
		fmt.Fprintln(fs.Output(), "  公開サーバーの署名鍵でケーパビリティ・トークンを発行し、標準出力に書き出す。接続先の同期 API の URL は標準エラー出力に表示する")
		fs.PrintDefaults()
	}

	if len(args) < 2 || args[1] != "create" {
		fs.Usage()
		if len(args) < 2 {
			return cfg, true, errors.New("missing subcommand: create")
		}
		return cfg, true, fmt.Errorf("unknown subcommand: %q", args[1])
	}
	if err := fs.Parse(args[2:]); err != nil {
		return cfg, true, err
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return cfg, true, fmt.Errorf("extra argument: %s", fs.Arg(0))
	}
	switch auth.Role(cfg.role) {
	case auth.RoleWriter, auth.RoleEditor, auth.RoleAdmin:
	case "":
		fs.Usage()
		return cfg, true, errors.New("missing --role (writer, editor or admin)")
	default:
		return cfg, true, fmt.Errorf("invalid --role %q: use writer, editor or admin", cfg.role)
	}
	if cfg.sub = strings.TrimSpace(cfg.sub); cfg.sub == "" {
		fs.Usage()
		return cfg, true, errors.New("missing --sub (the name of the person who receives the token)")
	}
	if cfg.ttl, err = parseTokenTTL(*ttl); err != nil {
		return cfg, true, err
	}
	if cfg.syncPath, err = parseSyncPathOption(syncPathOrEnv(fs, cfg.syncPath)); err != nil {
		return cfg, true, err
	}
	if cfg.baseURL, err = parseTokenBaseURL(cfg.baseURL); err != nil {
		return cfg, true, err
	}
	return cfg, true, nil
}

// parseTokenBaseURL は --base-url・BASE_URL の値を検査し、末尾の / を除いた形にする。空白だけなら未指定として空を返す。
func parseTokenBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("invalid --base-url (BASE_URL) %q: use the public server URL such as https://example.com", raw)
	}
	return strings.TrimRight(raw, "/"), nil
}

// parseTokenTTL は "90d" のような日数、または time.ParseDuration の表記を有効期間に変換する。
// 1秒以上 maxTokenTTL 以下でなければエラーを返す。
func parseTokenTTL(s string) (time.Duration, error) {
	var ttl time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 1 || n > int(maxTokenTTL/(24*time.Hour)) {
			return 0, fmt.Errorf("invalid --ttl %q: use 1d to 3650d", s)
		}
		ttl = time.Duration(n) * 24 * time.Hour
	} else {
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("invalid --ttl %q: use days (e.g. 90d) or a duration (e.g. 12h)", s)
		}
		ttl = d
	}
	if ttl < time.Second || ttl > maxTokenTTL {
		return 0, fmt.Errorf("invalid --ttl %q: must be between 1s and 3650d", s)
	}
	return ttl, nil
}

// runTokenCreate は署名鍵（無ければ作成する）でトークンを発行する。トークンだけを stdout に、
// 発行内容・接続先の同期 API・招待コード・注意事項を stderr に書く（stdout をそのままファイルやクリップボードへ渡せるようにする）。
// 同期 API のパスは公開サーバーと同じ手順（resolveSyncAPIPath）で決め、無ければ sync_path.txt を作成する。
// 発行したトークンは公開サーバーの DB（DB_PATH）に記録し、管理画面の一覧から取り消せるようにする（recordIssuedToken）。
func runTokenCreate(cfg tokenConfig, stdout, stderr io.Writer) error {
	priv, created, err := loadOrCreateCapabilityKey(cfg.keyFile)
	if err != nil {
		return err
	}
	syncPath, pathCreated, err := resolveSyncAPIPath(cfg.syncPath, cfg.keyFile)
	if err != nil {
		return err
	}
	token, err := auth.IssueToken(priv, cfg.sub, cfg.role, cfg.ttl)
	if err != nil {
		return err
	}
	claims, err := auth.VerifyToken(priv.Public().(ed25519.PublicKey), token)
	if err != nil {
		return fmt.Errorf("verify issued token: %w", err)
	}

	if created {
		fmt.Fprintf(stderr, "Created a new signing key: %s\n", cfg.keyFile)
		fmt.Fprintln(stderr, "  Keep it secret and back it up. The public server must use this same key file.")
	}
	if pathCreated {
		fmt.Fprintf(stderr, "Created a new secret sync API path: %s\n", syncPathFile(cfg.keyFile))
		fmt.Fprintln(stderr, "  Keep it with the signing key. The public server reads this same file.")
	}
	fmt.Fprintln(stderr, "Capability token created")
	fmt.Fprintf(stderr, "  Subject:   %s\n", claims.Subject)
	fmt.Fprintf(stderr, "  Role:      %s\n", claims.Role)
	fmt.Fprintf(stderr, "  Expires:   %s\n", time.Unix(claims.ExpiresAt, 0).Format(time.RFC3339))
	if cfg.baseURL != "" {
		fmt.Fprintf(stderr, "  Sync URL:  %s\n", cfg.baseURL+syncPath)
		fmt.Fprintln(stderr, "  On the local sorus admin page /admin/settings/remote, enter the Sync URL above as the public server URL,")
	} else {
		fmt.Fprintf(stderr, "  Sync path: %s\n", syncPath)
		fmt.Fprintln(stderr, "  On the local sorus admin page /admin/settings/remote, enter the public server URL followed by the Sync path")
		fmt.Fprintf(stderr, "  above (e.g. https://example.com%s; pass --base-url to print the full URL),\n", syncPath)
	}
	fmt.Fprintln(stderr, "  and paste the token below. Share the URL and the token only with the person who receives the token.")
	printInviteCode(stderr, cfg.baseURL, syncPath, token)
	recordIssuedToken(claims, stderr)
	_, err = fmt.Fprintln(stdout, token)
	return err
}

// printInviteCode は同期 API の URL とトークンを束ねた招待コード（server.EncodeInviteCode）を w に書く。
// 「Invite code: 」の行は導入スクリプト（deploy/install-server.sh）が読み取る。--base-url が無い、または
// https でない（http はこの PC 内だけ）ときは、招待コードを作れない理由を書く。
func printInviteCode(w io.Writer, baseURL, syncPath, token string) {
	if baseURL == "" {
		fmt.Fprintln(w, "  Invite code: (pass --base-url https://<your domain> to also print an invite code)")
		return
	}
	code, err := server.EncodeInviteCode(baseURL+syncPath, token)
	if err != nil {
		fmt.Fprintln(w, "  Invite code: not available (an invite code needs an https:// --base-url)")
		return
	}
	fmt.Fprintf(w, "  Invite code: %s\n", code)
	fmt.Fprintf(w, "  Or send only the invite code above, with this note: %s。\n", server.InviteJoinHint)
}

// recordIssuedToken は発行したトークンを公開サーバーの DB（DB_PATH）に記録する。公開サーバーの稼働中でも
// 書き込めるよう、サーバーと同じ設定（WAL・busy_timeout）で接続する。DB_PATH が未設定、または DB が無ければ
// 作らずに知らせる（カレントディレクトリの別の DB に書かないため。記録の無いトークンも、初めて使われたときに
// 公開サーバーが記録する）。記録できなくてもトークンは使えるため、失敗は stderr に知らせるだけにする。
func recordIssuedToken(claims *auth.Claims, stderr io.Writer) {
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		fmt.Fprintln(stderr, "  Note: not recorded in the server database (DB_PATH is not set). The server records the token when it is first used.")
		return
	}
	if _, err := os.Stat(dbPath); err != nil {
		fmt.Fprintf(stderr, "  Note: not recorded in the server database (%s was not found). The server records the token when it is first used.\n", dbPath)
		return
	}
	db, err := database.Connect(dbPath)
	if err == nil {
		defer db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err = server.RecordIssuedToken(ctx, db, claims, server.TokenIssuedViaCLI, "")
	}
	if err != nil {
		fmt.Fprintf(stderr, "  Note: could not record the token in the server database (%v). The server records it when it is first used.\n", err)
		return
	}
	fmt.Fprintf(stderr, "  Recorded in: %s (the site admin can revoke it from the local sorus)\n", dbPath)
}
