package theme

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// 取り込みの上限（Zip ボム・DoS 対策）。
const (
	MaxArchiveSize   = 10 << 20 // 展開前の Zip（ダウンロードを含む）の上限
	MaxExtractedSize = 50 << 20 // 展開後の合計サイズの上限
	MaxArchiveFiles  = 500      // 展開するファイル数の上限
	maxArchiveDirs   = 1000     // ディレクトリのエントリ数の上限
)

var (
	// ErrArchiveTooLarge は Zip が上限（サイズ・展開後サイズ・ファイル数）を超えたときのエラー。
	ErrArchiveTooLarge = errors.New("theme: archive exceeds the limit")
	// ErrUnsafeArchive は Zip に危険なエントリ（親ディレクトリへの脱出・絶対パス・シンボリックリンクなど）があるときのエラー。
	ErrUnsafeArchive = errors.New("theme: unsafe archive")
	// ErrBuiltinTheme はビルトインテーマと同じ ID を取り込む・ビルトインテーマを削除しようとしたときのエラー。
	ErrBuiltinTheme = errors.New("theme: built-in theme")
	// ErrThemeNotFound は削除するカスタムテーマが無いときのエラー。
	ErrThemeNotFound = errors.New("theme: custom theme not found")
	// ErrUnsupportedURL は取り込み元として解釈できない URL のときのエラー。
	ErrUnsupportedURL = errors.New("theme: unsupported URL")
)

// InstallFromZip は Zip のテーマを destParentDir/<id>/ に展開・検証・配置する。
//
// テーマ ID は、Zip が1つのディレクトリで包まれていればその名前（GitHub の repo-main のような -main・-master は除く）、
// そうでなければ theme.json の name.en から作る。同じ ID のカスタムテーマがあれば置き換える（更新）。
// ビルトインテーマと同じ ID は ErrBuiltinTheme で拒む。検証に失敗したら作ったディレクトリをすべて消す。
func InstallFromZip(r io.ReaderAt, size int64, destParentDir string) (*Theme, error) {
	return installZip(r, size, destParentDir, "", "")
}

// DownloadedArchive は URL からダウンロードしたテーマのアーカイブ。
type DownloadedArchive struct {
	Data      []byte
	Hint      string
	SourceURL string
}

// Install はアーカイブを destParentDir/<id>/ に展開・検証・配置する。
func (a *DownloadedArchive) Install(destParentDir string) (*Theme, error) {
	return installZip(bytes.NewReader(a.Data), int64(len(a.Data)), destParentDir, a.Hint, a.SourceURL)
}

// DownloadArchive は rawURL（GitHub・GitLab のリポジトリ URL、または .zip の URL）からテーマアーカイブをダウンロードする。
// 取得は https のみ、プライベートなネットワークへの接続は拒む（ssrf.go）。展開やファイル配置は行わない。
func DownloadArchive(ctx context.Context, rawURL string) (*DownloadedArchive, error) {
	return downloadArchive(ctx, newSafeClient(CheckPublicAddr), CheckPublicAddr, rawURL)
}

func downloadArchive(ctx context.Context, client *http.Client, ipCheck func(netip.Addr) error, rawURL string) (*DownloadedArchive, error) {
	candidates, hint, err := resolveArchiveURLs(rawURL)
	if err != nil {
		return nil, err
	}
	data, err := downloadFirst(ctx, client, ipCheck, candidates)
	if err != nil {
		return nil, err
	}
	return &DownloadedArchive{
		Data:      data,
		Hint:      hint,
		SourceURL: strings.TrimSpace(rawURL),
	}, nil
}

// InstallFromURL は rawURL（GitHub・GitLab のリポジトリ URL、または .zip の URL）からテーマを取り込む。
// 取得は https のみ、プライベートなネットワークへの接続は拒む（ssrf.go）。取り込み元は theme.json の source_url に記録する。
func InstallFromURL(ctx context.Context, rawURL string, destParentDir string) (*Theme, error) {
	return installFromURL(ctx, newSafeClient(CheckPublicAddr), CheckPublicAddr, rawURL, destParentDir)
}

func installFromURL(ctx context.Context, client *http.Client, ipCheck func(netip.Addr) error, rawURL, destParentDir string) (*Theme, error) {
	arc, err := downloadArchive(ctx, client, ipCheck, rawURL)
	if err != nil {
		return nil, err
	}
	return arc.Install(destParentDir)
}

var repoSegment = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// resolveArchiveURLs は rawURL からダウンロードする Zip の URL の候補（先頭から試す）と、テーマ ID の候補を返す。
//   - https://github.com/{owner}/{repo}[.git] → refs/heads/main.zip、無ければ master.zip
//   - https://gitlab.com/{group...}/{repo}[.git] → /-/archive/main/{repo}-main.zip、無ければ master
//   - .zip で終わる https の URL → そのまま
func resolveArchiveURLs(rawURL string) ([]string, string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrUnsupportedURL, err)
	}
	if err := checkFetchURL(u, func(netip.Addr) error { return nil }); err != nil {
		return nil, "", err
	}
	if strings.HasSuffix(strings.ToLower(u.Path), ".zip") {
		u.Fragment = ""
		return []string{u.String()}, strings.TrimSuffix(path.Base(u.Path), path.Ext(u.Path)), nil
	}
	segs := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	for _, s := range segs {
		if !repoSegment.MatchString(s) || s == "." || s == ".." {
			return nil, "", fmt.Errorf("%w: %s is not a repository URL", ErrUnsupportedURL, rawURL)
		}
	}
	host := strings.ToLower(u.Hostname())
	if u.Port() != "" {
		return nil, "", fmt.Errorf("%w: %s is not a repository URL", ErrUnsupportedURL, rawURL)
	}
	switch host {
	case "github.com", "www.github.com":
		if len(segs) != 2 {
			return nil, "", fmt.Errorf("%w: use https://github.com/{owner}/{repo}", ErrUnsupportedURL)
		}
		owner, repo := segs[0], strings.TrimSuffix(segs[1], ".git")
		base := "https://github.com/" + owner + "/" + repo + "/archive/refs/heads/"
		return []string{base + "main.zip", base + "master.zip"}, repo, nil
	case "gitlab.com", "www.gitlab.com":
		if len(segs) < 2 {
			return nil, "", fmt.Errorf("%w: use https://gitlab.com/{owner}/{repo}", ErrUnsupportedURL)
		}
		segs[len(segs)-1] = strings.TrimSuffix(segs[len(segs)-1], ".git")
		project, repo := strings.Join(segs, "/"), segs[len(segs)-1]
		base := "https://gitlab.com/" + project + "/-/archive/"
		return []string{base + "main/" + repo + "-main.zip", base + "master/" + repo + "-master.zip"}, repo, nil
	}
	return nil, "", fmt.Errorf("%w: %s (use a GitHub/GitLab repository URL or a .zip URL)", ErrUnsupportedURL, rawURL)
}

// downloadFirst は候補を順に試し、最初に取れた Zip の中身を返す。404 のときだけ次の候補へ進む。
func downloadFirst(ctx context.Context, client *http.Client, ipCheck func(netip.Addr) error, candidates []string) ([]byte, error) {
	var lastErr error
	for i, c := range candidates {
		data, status, err := download(ctx, client, ipCheck, c)
		if err == nil {
			return data, nil
		}
		lastErr = err
		if status != http.StatusNotFound || i == len(candidates)-1 {
			break
		}
	}
	return nil, lastErr
}

// download は rawURL を MaxArchiveSize まで読み込む。超えたらその場で打ち切る。
func download(ctx context.Context, client *http.Client, ipCheck func(netip.Addr) error, rawURL string) ([]byte, int, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrUnsupportedURL, err)
	}
	if err := checkFetchURL(u, ipCheck); err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", "sorus-theme-installer")
	req.Header.Set("Accept", "application/zip, application/octet-stream;q=0.9, */*;q=0.1")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("theme: download %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, fmt.Errorf("theme: download %s: %s", rawURL, resp.Status)
	}
	if resp.ContentLength > MaxArchiveSize {
		return nil, resp.StatusCode, fmt.Errorf("%w: download is %d bytes (limit %d)", ErrArchiveTooLarge, resp.ContentLength, MaxArchiveSize)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxArchiveSize+1))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("theme: download %s: %w", rawURL, err)
	}
	if len(data) > MaxArchiveSize {
		return nil, resp.StatusCode, fmt.Errorf("%w: download exceeds %d bytes", ErrArchiveTooLarge, MaxArchiveSize)
	}
	return data, resp.StatusCode, nil
}

// archiveEntry は展開するファイル1つ。rel はテーマのディレクトリからの相対パス（/ 区切り、Clean 済み）。
type archiveEntry struct {
	file *zip.File
	rel  string
}

// installZip は InstallFromZip の本体。hint はテーマ ID の候補、sourceURL は theme.json に記録する取り込み元。
func installZip(r io.ReaderAt, size int64, destParentDir, hint, sourceURL string) (*Theme, error) {
	entries, root, err := openArchive(r, size)
	if err != nil {
		return nil, err
	}
	id, err := chooseID(root, hint, entries)
	if err != nil {
		return nil, err
	}
	st, err := stage(entries, destParentDir, id)
	if err != nil {
		return nil, err
	}
	// 成功時は置き換え前のテーマを含む作業ディレクトリ、失敗時は展開したものを消す（前の版を元の場所に
	// 戻せなかったときは残す）。
	defer st.Close()
	if sourceURL != "" {
		t := st.Theme
		t.SourceURL = sourceURL
		if err := writeManifest(filepath.Join(st.src, "theme.json"), t); err != nil {
			return nil, err
		}
		if st.Theme, err = ValidateThemeDir(os.DirFS(st.staging), id); err != nil {
			return nil, err
		}
	}
	if err := st.Commit(); err != nil {
		return nil, err
	}
	t := st.Theme
	return &t, nil
}

// 作業ディレクトリの名前。destParentDir の直下に作る（カタログは . で始まる名前を読まない）。
const (
	// installPrefix は取り込み・受け取りの作業ディレクトリ。<id>/ に展開し、置き換え前のテーマを oldDirName に退避する。
	installPrefix = ".install-"
	// uninstallPrefix は削除するテーマを移すゴミ箱。
	uninstallPrefix = ".uninstall-"
	// oldDirName は作業ディレクトリの中の、置き換え前のテーマの退避先。
	oldDirName = ".old"
)

// renameFunc は配置・退避に使う os.Rename（テストで失敗させるために差し替える）。
var renameFunc = os.Rename

// ErrNotRestored は、テーマの置き換え（StagedTheme の Commit）またはその取り消し（Rollback）が途中で失敗し、
// 配置先を置き換えの前の状態に戻せなかったときのエラー。配置先にはテーマが無いか、新しい版が残っている。
// 前の版は消さずに作業ディレクトリ（.install-*/.old）に残し、エラーにその場所を書く。自動では戻さない
// （起動時の CleanupStaleStaging も、前の版を含む作業ディレクトリを消さず、戻しもしない）。
var ErrNotRestored = errors.New("theme: the theme directory could not be put back to its state before the replacement")

// StagedTheme は展開・検証を終え、まだ配置していないカスタムテーマ（StageZipAs）。Commit で destParentDir/<id>/ に
// 配置し、Rollback で配置の前に戻せる。最後に必ず Close で作業ディレクトリを片付ける。StageZipAs〜Close は
// 呼び出し側が排他する（サーバーの themeMu）。
type StagedTheme struct {
	// Theme は検証したテーマ。IsCustom と Dir は Commit の後に配置先のものになる。
	Theme Theme
	// staging は destParentDir の直下の作業ディレクトリ（installPrefix）。
	staging string
	// src は展開したテーマ（staging/<id>）、dest は配置先（destParentDir/<id>）、backup は置き換え前のテーマの退避先。
	src, dest, backup string
	// committed は配置済みか、hadOld は配置の前に同じ ID のテーマがあったか（Rollback で戻す）。
	committed, hadOld bool
	// keep は、配置先を置き換えの前に戻せなかった（ErrNotRestored）ため、前の版を含む作業ディレクトリを残すか。
	keep bool
}

// StageZipAs は Zip のテーマを destParentDir の作業ディレクトリに展開・検証する。配置は Commit で行う。
//
// テーマ ID は id に固定する（Zip を包むディレクトリ名や theme.json からは決めない）。ほかの sorus から送られた
// テーマを同じ ID のまま受け取るために使う。上限・危険なエントリ・ビルトインテーマの ID は InstallFromZip と
// 同じく拒む。ID の形式に合わない id は ErrInvalidTheme で拒む。
func StageZipAs(r io.ReaderAt, size int64, destParentDir, id string) (*StagedTheme, error) {
	if !idPattern.MatchString(id) {
		return nil, fmt.Errorf("%w: invalid theme ID %q (use %s)", ErrInvalidTheme, id, idPattern)
	}
	if _, ok := Builtin.Get(id); ok {
		return nil, fmt.Errorf("%w: %q is a built-in theme ID", ErrBuiltinTheme, id)
	}
	entries, _, err := openArchive(r, size)
	if err != nil {
		return nil, err
	}
	return stage(entries, destParentDir, id)
}

// openArchive は Zip を開いて上限と危険なエントリを調べ、展開するファイルとテーマのルート（Zip の中のディレクトリ）を返す。
func openArchive(r io.ReaderAt, size int64) ([]archiveEntry, string, error) {
	if size > MaxArchiveSize {
		return nil, "", fmt.Errorf("%w: archive is %d bytes (limit %d)", ErrArchiveTooLarge, size, MaxArchiveSize)
	}
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, "", fmt.Errorf("%w: not a zip archive: %v", ErrInvalidTheme, err)
	}
	root, entries, err := planArchive(zr)
	if err != nil {
		return nil, "", err
	}
	return entries, root, nil
}

// stage は entries を destParentDir の作業ディレクトリの <id>/ に展開して検証する。ビルトインテーマの ID は拒む。
// 失敗したら作業ディレクトリをすべて消す。
func stage(entries []archiveEntry, destParentDir, id string) (*StagedTheme, error) {
	if _, ok := Builtin.Get(id); ok {
		return nil, fmt.Errorf("%w: %q is a built-in theme ID; rename the theme directory", ErrBuiltinTheme, id)
	}
	if err := os.MkdirAll(destParentDir, 0o755); err != nil {
		return nil, fmt.Errorf("theme: create %s: %w", destParentDir, err)
	}
	staging, err := os.MkdirTemp(destParentDir, installPrefix)
	if err != nil {
		return nil, fmt.Errorf("theme: create staging dir: %w", err)
	}
	st := &StagedTheme{
		staging: staging,
		src:     filepath.Join(staging, id),
		backup:  filepath.Join(staging, oldDirName),
	}
	if st.dest, err = filepath.Abs(filepath.Join(destParentDir, id)); err != nil {
		st.dest = filepath.Join(destParentDir, id)
	}
	if err := extract(entries, st.src); err != nil {
		st.Close()
		return nil, err
	}
	if st.Theme, err = ValidateThemeDir(os.DirFS(staging), id); err != nil {
		st.Close()
		return nil, err
	}
	return st, nil
}

// Commit は検証したテーマを destParentDir/<id>/ に配置する。同じ ID のテーマがあれば置き換え（更新）、
// 置き換え前のテーマは Close まで作業ディレクトリに残す（Rollback で戻すため）。
//
// 失敗したときは配置の前のままにする。新しい版を置けず、前の版も元の場所に戻せなかったときだけ、前の版を
// 作業ディレクトリに残して ErrNotRestored を返す（配置先にはテーマが無い）。
func (s *StagedTheme) Commit() error {
	if s.committed {
		return nil
	}
	hadOld := false
	info, err := os.Lstat(s.dest)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return fmt.Errorf("theme: install %s: %w", s.dest, err)
	case !info.IsDir():
		return fmt.Errorf("theme: install %s: the path exists and is not a directory", s.dest)
	default:
		hadOld = true
	}
	if hadOld {
		if err := renameFunc(s.dest, s.backup); err != nil {
			return fmt.Errorf("theme: replace %s: %w", s.dest, err)
		}
	}
	if err := renameFunc(s.src, s.dest); err != nil {
		if !hadOld {
			return fmt.Errorf("theme: install %s: %w", s.dest, err)
		}
		if rerr := renameFunc(s.backup, s.dest); rerr != nil {
			return s.notRestored(true, fmt.Errorf("place the new version: %w; put the previous version back: %w", err, rerr))
		}
		return fmt.Errorf("theme: replace %s: %w", s.dest, err)
	}
	s.committed, s.hadOld = true, hadOld
	s.Theme.IsCustom = true
	s.Theme.Dir = s.dest
	return nil
}

// Rollback は Commit で配置したテーマを作業ディレクトリへ戻し、置き換え前のテーマがあれば元に戻す。Commit して
// いなければ何もしない。途中で失敗したときは、前の版を作業ディレクトリに残して ErrNotRestored を返す（配置先は
// 新しい版のままか、テーマが無い）。
func (s *StagedTheme) Rollback() error {
	if !s.committed {
		return nil
	}
	if err := renameFunc(s.dest, s.src); err != nil {
		return s.notRestored(s.hadOld, fmt.Errorf("remove the new version: %w", err))
	}
	s.committed = false
	if s.hadOld {
		if err := renameFunc(s.backup, s.dest); err != nil {
			return s.notRestored(true, fmt.Errorf("put the previous version back: %w", err))
		}
	}
	return nil
}

// notRestored は、配置先を置き換えの前に戻せなかったことを表す ErrNotRestored を返し、Close が作業ディレクトリを
// 消さないようにする。前の版があれば（hadOld）、エラーにその場所を書く（元のデータの在りかをログに残す）。
func (s *StagedTheme) notRestored(hadOld bool, cause error) error {
	s.keep = true
	err := fmt.Errorf("%w (%s): %w", ErrNotRestored, s.dest, cause)
	if hadOld {
		err = fmt.Errorf("%w (the previous version is kept in %s)", err, s.backup)
	}
	return err
}

// Close は作業ディレクトリ（置き換え前のテーマ・取り除いたテーマを含む）を消す。配置したテーマには触れない。
// 配置先を置き換えの前に戻せなかったとき（ErrNotRestored）は、前の版を消さないよう作業ディレクトリを残す。
func (s *StagedTheme) Close() error {
	if s.keep {
		return nil
	}
	return os.RemoveAll(s.staging)
}

// planArchive は Zip のエントリを調べ、テーマのルート（Zip の中のディレクトリ。直下なら ""）と展開するファイルを返す。
// 危険なパス・シンボリックリンク・上限超えはここで拒む（1バイトも書く前に判定する）。
func planArchive(zr *zip.Reader) (string, []archiveEntry, error) {
	type item struct {
		f    *zip.File
		name string
	}
	var files []item
	dirs := 0
	var declared uint64
	tops := map[string]bool{}
	hasRootManifest := false
	for _, f := range zr.File {
		name, err := safeEntryName(f.Name)
		if err != nil {
			return "", nil, err
		}
		if name == "" || isJunk(name) {
			continue
		}
		mode := f.Mode()
		if mode&fs.ModeSymlink != 0 || (!mode.IsRegular() && !mode.IsDir()) {
			return "", nil, fmt.Errorf("%w: %q is not a regular file", ErrUnsafeArchive, f.Name)
		}
		top, _, _ := strings.Cut(name, "/")
		tops[top] = true
		if mode.IsDir() || strings.HasSuffix(f.Name, "/") {
			if dirs++; dirs > maxArchiveDirs {
				return "", nil, fmt.Errorf("%w: more than %d directories", ErrArchiveTooLarge, maxArchiveDirs)
			}
			continue
		}
		if len(files) >= MaxArchiveFiles {
			return "", nil, fmt.Errorf("%w: more than %d files", ErrArchiveTooLarge, MaxArchiveFiles)
		}
		declared += f.UncompressedSize64
		if declared > MaxExtractedSize {
			return "", nil, fmt.Errorf("%w: extracted size exceeds %d bytes", ErrArchiveTooLarge, MaxExtractedSize)
		}
		if name == "theme.json" {
			hasRootManifest = true
		}
		files = append(files, item{f, name})
	}

	root := ""
	if !hasRootManifest {
		// GitHub・GitLab の Zip はトップに repo-main/ のような1つのディレクトリを持つ。その直下の theme.json を探す。
		found := false
		if len(tops) == 1 {
			for top := range tops {
				for _, it := range files {
					if it.name == top+"/theme.json" {
						root, found = top, true
						break
					}
				}
			}
		}
		if !found {
			return "", nil, fmt.Errorf("%w: theme.json is not at the top of the archive (or inside its single top-level directory)", ErrInvalidTheme)
		}
	}

	var out []archiveEntry
	seen := map[string]bool{}
	for _, it := range files {
		rel := it.name
		if root != "" {
			rel = strings.TrimPrefix(rel, root+"/")
		}
		key := strings.ToLower(rel) // 大文字小文字を区別しないファイルシステムでの上書きを防ぐ
		if seen[key] {
			return "", nil, fmt.Errorf("%w: duplicate entry %q", ErrUnsafeArchive, it.f.Name)
		}
		seen[key] = true
		out = append(out, archiveEntry{file: it.f, rel: rel})
	}
	return root, out, nil
}

// safeEntryName は Zip のエントリ名を検査し、Clean した / 区切りの相対パスを返す。
// 絶対パス・ドライブ名・バックスラッシュ・親ディレクトリへの脱出（..）を含むものは拒む（Zip Slip）。
func safeEntryName(name string) (string, error) {
	bad := func() (string, error) {
		return "", fmt.Errorf("%w: entry %q escapes the theme directory", ErrUnsafeArchive, name)
	}
	if name == "" || strings.ContainsAny(name, "\\:\x00") || strings.HasPrefix(name, "/") {
		return bad()
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return bad()
		}
	}
	clean := path.Clean(name)
	if clean == "." {
		return "", nil
	}
	if !filepath.IsLocal(filepath.FromSlash(clean)) {
		return bad()
	}
	return clean, nil
}

// isJunk は macOS などが Zip に入れる不要なファイルか。
func isJunk(name string) bool {
	if name == "__MACOSX" || strings.HasPrefix(name, "__MACOSX/") {
		return true
	}
	base := path.Base(name)
	return base == ".DS_Store" || base == "Thumbs.db"
}

// chooseID はテーマ ID を決める。Zip を包むディレクトリ名、hint（リポジトリ名など）、theme.json の name.en の順に試す。
func chooseID(root, hint string, entries []archiveEntry) (string, error) {
	for _, c := range []string{root, hint} {
		if id := slugID(c); id != "" {
			return id, nil
		}
	}
	for _, e := range entries {
		if e.rel != "theme.json" {
			continue
		}
		var m struct {
			Name map[string]string `json:"name"`
		}
		rc, err := e.file.Open()
		if err == nil {
			_ = json.NewDecoder(io.LimitReader(rc, 1<<20)).Decode(&m)
			rc.Close()
		}
		if id := slugID(m.Name["en"]); id != "" {
			return id, nil
		}
	}
	return "", fmt.Errorf("%w: cannot derive a theme ID; wrap the files in a directory named after the theme (e.g. my-theme/theme.json)", ErrInvalidTheme)
}

var slugInvalid = regexp.MustCompile(`[^a-z0-9_-]+`)

// slugID は s をテーマ ID の形式に直す。ブランチ名の接尾辞（-main・-master）を除き、使えない文字は - にする。
func slugID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, suffix := range []string{"-main", "-master"} {
		if strings.HasSuffix(s, suffix) && len(s) > len(suffix) {
			s = strings.TrimSuffix(s, suffix)
			break
		}
	}
	s = slugInvalid.ReplaceAllString(s, "-")
	s = strings.TrimLeft(s, "-_")
	if len(s) > 41 {
		s = s[:41]
	}
	s = strings.TrimRight(s, "-_")
	if !idPattern.MatchString(s) {
		return ""
	}
	return s
}

// extract は entries を dir の下に書き出す。実際に展開したバイト数でも MaxExtractedSize を守る。
func extract(entries []archiveEntry, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	var budget int64 = MaxExtractedSize
	for _, e := range entries {
		target := filepath.Join(absDir, filepath.FromSlash(e.rel))
		// Clean したパスが展開先の配下にあることを改めて確かめる（Zip Slip の二重の防御）。
		if rel, err := filepath.Rel(absDir, target); err != nil || !filepath.IsLocal(rel) {
			return fmt.Errorf("%w: entry %q escapes the theme directory", ErrUnsafeArchive, e.file.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		n, err := writeEntry(e.file, target, budget)
		if err != nil {
			return err
		}
		budget -= n
	}
	return nil
}

// writeEntry は1つのエントリを target に新規作成で書き、書いたバイト数を返す。budget を超えたら打ち切る。
func writeEntry(f *zip.File, target string, budget int64) (int64, error) {
	rc, err := f.Open()
	if err != nil {
		return 0, fmt.Errorf("%w: open %q: %v", ErrInvalidTheme, f.Name, err)
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, fmt.Errorf("theme: create %s: %w", target, err)
	}
	n, err := io.Copy(out, io.LimitReader(rc, budget+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, fmt.Errorf("%w: extract %q: %v", ErrInvalidTheme, f.Name, err)
	}
	if n > budget {
		return n, fmt.Errorf("%w: extracted size exceeds %d bytes", ErrArchiveTooLarge, MaxExtractedSize)
	}
	return n, nil
}

// writeManifest は t の内容で theme.json を書き直す（source_url の記録に使う）。
func writeManifest(p string, t Theme) error {
	data, err := json.MarshalIndent(manifest{
		Name: t.Name, Description: t.Description, ColorScheme: t.ColorScheme, Order: t.Order, SourceURL: t.SourceURL,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(data, '\n'), 0o644)
}

// UninstallCustomTheme は customDir/<id>/ のカスタムテーマを削除する。
// ビルトインテーマの ID は ErrBuiltinTheme、ID の形式に合わない（パストラバーサルを含む）ものや
// 無いもの・ディレクトリでないもの（シンボリックリンクなど）は ErrThemeNotFound で拒む。
func UninstallCustomTheme(customDir string, id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("%w: invalid theme ID %q", ErrThemeNotFound, id)
	}
	if _, ok := Builtin.Get(id); ok {
		return fmt.Errorf("%w: %q cannot be uninstalled", ErrBuiltinTheme, id)
	}
	if strings.TrimSpace(customDir) == "" {
		return fmt.Errorf("%w: no custom theme directory", ErrThemeNotFound)
	}
	target := filepath.Join(customDir, id)
	if rel, err := filepath.Rel(customDir, target); err != nil || rel != id {
		return fmt.Errorf("%w: invalid theme ID %q", ErrThemeNotFound, id)
	}
	info, err := os.Lstat(target)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%w: %q", ErrThemeNotFound, id)
	}
	// 先に作業用の名前へ移してから消す。途中で失敗しても半端なテーマがカタログに残らない。
	trash, err := os.MkdirTemp(customDir, uninstallPrefix)
	if err != nil {
		return fmt.Errorf("theme: uninstall %s: %w", id, err)
	}
	if err := os.Rename(target, filepath.Join(trash, id)); err != nil {
		_ = os.Remove(trash)
		return fmt.Errorf("theme: uninstall %s: %w", id, err)
	}
	// ディレクトリの退避に成功した時点で、カスタムテーマは customDir から実質的に削除されている。
	// その後のゴミ箱の削除が失敗しても呼び出し元を失敗させず、次回起動時または CleanupStaleStaging で掃除する。
	if err := os.RemoveAll(trash); err != nil {
		log.Printf("theme: warning: failed to remove trash %s: %v", trash, err)
	}
	return nil
}
