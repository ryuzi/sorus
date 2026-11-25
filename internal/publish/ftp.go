package publish

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/jlaffaye/ftp"
)

// DefaultTimeout は公開先への接続と1回のやりとりの制限時間。
const DefaultTimeout = 30 * time.Second

// ErrAuth は公開先にユーザー名・パスワード（鍵・API トークン）を受け入れてもらえなかったエラー。
var ErrAuth = errors.New("publish: authentication failed")

// ErrFTPNoEncryption は、FTP のサーバーが暗号化（AUTH TLS）を断り、暗号化しないで送ることも許されていないエラー。
// ユーザー名・パスワードは送っていない。
var ErrFTPNoEncryption = errors.New("publish: ftp server does not support encryption (AUTH TLS)")

// FTPConfig はレンタルサーバーへ FTP で接続する設定。暗号化（明示的 TLS: AUTH TLS）して接続する。サーバーが
// 暗号化に対応していなければ ErrFTPNoEncryption を返し、AllowPlaintext のときだけ暗号化せずに接続して Remote.Notes で知らせる。
type FTPConfig struct {
	Host string
	// Port は 0 なら 21。
	Port     int
	User     string
	Password string
	// Root はアップロード先のフォルダ（例: /public_html）。CleanRoot で検査する。
	Root string
	// Timeout は接続と1回のやりとりの制限時間。0 なら DefaultTimeout。
	Timeout time.Duration
	// TLSConfig は暗号化に使う設定。nil なら Host の証明書を OS の信頼できる証明書で確かめる。
	TLSConfig *tls.Config
	// AllowPlaintext は、サーバーが暗号化に対応していないときに暗号化せずに接続してよいか。パスワードが読み取られる
	// おそれがあるため、利用者が選んだときだけ true にする。
	AllowPlaintext bool
}

type ftpRemote struct {
	conn *ftp.ServerConn
	root string
	// made は作成を試みた（または、あると分かっている）フォルダ。
	made map[string]bool
	// plain は暗号化せずに接続したか。
	plain bool
	// stop は ctx の終了で接続を閉じる見張りを止める。
	stop func() bool
}

// DialFTP は cfg のサーバーに接続してログインする。ctx が終わると接続を閉じる（送信中の処理も止まる）。
func DialFTP(ctx context.Context, cfg FTPConfig) (Remote, error) {
	root, err := CleanRoot(cfg.Root)
	if err != nil {
		return nil, err
	}
	host := strings.TrimSpace(cfg.Host)
	if host == "" {
		return nil, errors.New("publish: ftp host is empty")
	}
	port := cfg.Port
	if port == 0 {
		port = 21
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	tlsConf := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if cfg.TLSConfig != nil {
		tlsConf = cfg.TLSConfig.Clone()
	}
	// データ接続で制御接続の TLS セッションを再利用するよう求めるサーバー（vsftpd の require_ssl_reuse など）に備える。
	if tlsConf.ClientSessionCache == nil {
		tlsConf.ClientSessionCache = tls.NewLRUClientSessionCache(16)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	opts := []ftp.DialOption{ftp.DialWithContext(ctx), ftp.DialWithTimeout(timeout), ftp.DialWithShutTimeout(timeout)}
	plain := false
	conn, err := ftp.Dial(addr, append(opts, ftp.DialWithExplicitTLS(tlsConf))...)
	var perr *textproto.Error
	if errors.As(err, &perr) && perr.Code >= 500 {
		// AUTH TLS に対応していないサーバー。許されているときだけ、暗号化せずに接続し直す。
		if !cfg.AllowPlaintext {
			return nil, fmt.Errorf("%w: %s: %v", ErrFTPNoEncryption, addr, err)
		}
		plain = true
		conn, err = ftp.Dial(addr, opts...)
	}
	if err != nil {
		return nil, fmt.Errorf("publish: connect %s: %w", addr, err)
	}
	if err := conn.Login(cfg.User, cfg.Password); err != nil {
		_ = conn.Quit()
		if errors.As(err, &perr) && perr.Code == ftp.StatusNotLoggedIn {
			return nil, fmt.Errorf("%w: %v", ErrAuth, err)
		}
		return nil, fmt.Errorf("publish: login %s: %w", addr, err)
	}
	r := &ftpRemote{conn: conn, root: root, made: map[string]bool{}, plain: plain}
	r.stop = context.AfterFunc(ctx, func() { _ = conn.Quit() })
	return r, nil
}

// full は相対パス rel のサーバー上のパス。
func (r *ftpRemote) full(rel string) string { return path.Join(r.root, rel) }

func (r *ftpRemote) Put(ctx context.Context, rel string, body io.Reader, size int64) error {
	p := r.full(rel)
	r.ensureDir(path.Dir(p))
	return r.conn.Stor(p, body)
}

// ensureDir は dir とその親フォルダを作る。すでにあるときのエラーは無視する（作れなかったときは続く STOR が失敗する）。
func (r *ftpRemote) ensureDir(dir string) {
	if dir == "/" || dir == "." || r.made[dir] {
		return
	}
	r.ensureDir(path.Dir(dir))
	_ = r.conn.MakeDir(dir)
	r.made[dir] = true
}

func (r *ftpRemote) Delete(ctx context.Context, rel string) error {
	p := r.full(rel)
	err := r.conn.Delete(p)
	if err == nil {
		return nil
	}
	// 550 は「無い」と「消す権限が無い」のどちらもあるため、まだあるかを確かめる。消せないまま進めると、
	// 非公開にした記事が公開先に残ってしまう。
	var perr *textproto.Error
	if errors.As(err, &perr) && perr.Code == ftp.StatusFileUnavailable {
		if _, serr := r.conn.FileSize(p); errors.As(serr, &perr) && perr.Code == ftp.StatusFileUnavailable {
			return nil
		}
	}
	return err
}

func (r *ftpRemote) RemoveDir(ctx context.Context, rel string) error {
	return r.conn.RemoveDir(r.full(rel))
}

func (r *ftpRemote) CountEntries(ctx context.Context) (int, error) {
	names, err := r.conn.NameList(r.root)
	var perr *textproto.Error
	if errors.As(err, &perr) && (perr.Code == ftp.StatusFileUnavailable || perr.Code == ftp.StatusFileActionIgnored) {
		// フォルダが無い（450・550）。
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, name := range names {
		if base := path.Base(name); base != "." && base != ".." {
			n++
		}
	}
	return n, nil
}

func (r *ftpRemote) Notes() []string {
	if r.plain {
		return []string{"このサーバーは暗号化に対応していないため、設定どおり暗号化せずに送りました。パスワードが読み取られるおそれがあるため、サーバー会社が SFTP に対応していれば SFTP をおすすめします。"}
	}
	return nil
}

func (r *ftpRemote) Close() error {
	r.stop()
	return r.conn.Quit()
}
