package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"
)

// PreferredPort はデスクトップモードで最初に試すポート。使用中なら OS が選んだ空きポートを使う。
const PreferredPort = 8780

// ListenHost はデスクトップモードで待ち受けるアドレス。同じ PC からの接続だけを受ける。
const ListenHost = "127.0.0.1"

// healthPath は起動中のインスタンスの確認に使うヘルスチェックの URL パス（server の /healthz）。
const healthPath = "/healthz"

// ErrLocked は、同じデータディレクトリを別の sorus が使っている（ロックを取れなかった）ことを示す。
var ErrLocked = errors.New("desktop: another sorus is using the data directory")

// Instance は instance.json の内容。起動中のインスタンスのプロセス ID と待ち受けポート。
type Instance struct {
	PID  int `json:"pid"`
	Port int `json:"port"`
}

// BaseURL はインスタンスの URL（http://127.0.0.1:<port>）を返す。
func (i Instance) BaseURL() string {
	return "http://" + net.JoinHostPort(ListenHost, strconv.Itoa(i.Port))
}

// AdminURL はインスタンスの管理画面の URL を返す。ブラウザで開く先。
func (i Instance) AdminURL() string { return i.BaseURL() + "/admin" }

// WriteInstance は inst を path（instance.json）に書く。読み手が書きかけのファイルを読まないよう、
// 同じディレクトリの一時ファイルに書いてから置き換える。
func WriteInstance(path string, inst Instance) error {
	data, err := json.Marshal(inst)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".instance-*.json")
	if err != nil {
		return fmt.Errorf("desktop: write instance file: %w", err)
	}
	_, werr := tmp.Write(data)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("desktop: write instance file: %w", werr)
	}
	return nil
}

// ReadInstance は path（instance.json）を読む。ファイルが無ければ os.ErrNotExist を包んだエラーを返す。
func ReadInstance(path string) (Instance, error) {
	var inst Instance
	data, err := os.ReadFile(path)
	if err != nil {
		return inst, err
	}
	if err := json.Unmarshal(data, &inst); err != nil {
		return inst, fmt.Errorf("desktop: %s: %w", path, err)
	}
	if inst.Port <= 0 || inst.Port > 65535 {
		return inst, fmt.Errorf("desktop: %s: invalid port %d", path, inst.Port)
	}
	return inst, nil
}

// RemoveInstance は path（instance.json）を消す。無くてもエラーにしない。
func RemoveInstance(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// FindRunning は path（instance.json）に記録されたインスタンスが応答するかをヘルスチェックで確かめ、
// 応答すればそのインスタンスと true を返す。ファイルが無い・壊れている・応答しない（前回の異常終了の残り）
// ときは false を返す。
func FindRunning(ctx context.Context, path string) (Instance, bool) {
	inst, err := ReadInstance(path)
	if err != nil {
		return inst, false
	}
	return inst, Healthy(ctx, inst)
}

// Healthy は inst がヘルスチェック（/healthz）に sorus として応答するかを返す。データベースに接続できない
// （503 unhealthy）ときも、インスタンス自体は起動しているため true を返す。
func Healthy(ctx context.Context, inst Instance) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, inst.BaseURL()+healthPath, nil)
	if err != nil {
		return false
	}
	// プロキシの環境変数に左右されないよう、既定のクライアントではなくプロキシを使わない接続で確かめる。
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	res, err := client.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusServiceUnavailable {
		return false
	}
	var body struct {
		Status string `json:"status"`
	}
	return json.NewDecoder(res.Body).Decode(&body) == nil && body.Status != ""
}

// WaitRunning は、ロックを持つ別のインスタンスが起動を終えて instance.json を書き、ヘルスチェックに応答するまで
// 最大 timeout 待つ。応答すればそのインスタンスと true を返す。
func WaitRunning(ctx context.Context, path string, timeout time.Duration) (Instance, bool) {
	deadline := time.Now().Add(timeout)
	for {
		if inst, ok := FindRunning(ctx, path); ok {
			return inst, true
		}
		if time.Now().After(deadline) {
			return Instance{}, false
		}
		select {
		case <-ctx.Done():
			return Instance{}, false
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Listen は 127.0.0.1 の PreferredPort で待ち受ける。使用中なら OS が選んだ空きポートで待ち受ける。
func Listen() (net.Listener, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(ListenHost, strconv.Itoa(PreferredPort)))
	if err == nil {
		return ln, nil
	}
	ln, err2 := net.Listen("tcp", net.JoinHostPort(ListenHost, "0"))
	if err2 != nil {
		return nil, errors.Join(err, err2)
	}
	return ln, nil
}

// siteSearchPorts は、2つ目からのサイトに新しいポートを選ぶとき、PreferredPort の次から順に試すポートの数。
// 覚えやすい近い番号を選び、どれも使えなければ OS が選んだ空きポートを使う。
const siteSearchPorts = 20

// ListenSite は2つ目からのサイトの待ち受けを 127.0.0.1 で始める。port（sites.json に記録したポート）が 0 より大きければ
// まずそれを試す。使えないか 0 なら、PreferredPort の次から siteSearchPorts 個のうち avoid（ほかのサイトのポート）に
// 無いものを順に試し、どれも使えなければ OS が選んだ空きポートで待ち受ける。どのポートになったかは呼び出し側が
// 待ち受けのアドレスで確かめ、記録したものと違えば記録し直す。
func ListenSite(port int, avoid []int) (net.Listener, error) {
	if port > 0 {
		if ln, err := net.Listen("tcp", net.JoinHostPort(ListenHost, strconv.Itoa(port))); err == nil {
			return ln, nil
		}
	}
	for p := PreferredPort + 1; p <= PreferredPort+siteSearchPorts; p++ {
		if p == port || slices.Contains(avoid, p) {
			continue
		}
		if ln, err := net.Listen("tcp", net.JoinHostPort(ListenHost, strconv.Itoa(p))); err == nil {
			return ln, nil
		}
	}
	return net.Listen("tcp", net.JoinHostPort(ListenHost, "0"))
}

// Lock はデータディレクトリのロックファイル path（instance.lock）を排他的にロックする。別のプロセスがロック中なら
// 待たずに ErrLocked を返す。ロックはプロセスが異常終了しても OS が解放するため、古いロックが残って起動できなく
// なることはない。返す関数でロックを解放する。
func Lock(path string) (unlock func() error, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("desktop: open lock file: %w", err)
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, err
	}
	return func() error {
		return errors.Join(unlockFile(f), f.Close())
	}, nil
}
