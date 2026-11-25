package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultUpdateURL は最新のリリースを調べる GitHub の API。
	DefaultUpdateURL = "https://api.github.com/repos/ryuzi/sorus/releases/latest"
	// UpdateURLEnv は DefaultUpdateURL を差し替える環境変数（テスト・自前の配布向け）。
	UpdateURLEnv = "SORUS_UPDATE_URL"
	// NoUpdateCheckEnv を 1 にすると新しい版を調べない。
	NoUpdateCheckEnv = "SORUS_NO_UPDATE_CHECK"
	// UpdateCheckInterval は新しい版を調べる間隔。これより短い間隔では調べない。
	UpdateCheckInterval = 24 * time.Hour

	// devVersion は go run や通常の go build のときの版（internal/version.Version の既定値）。
	devVersion = "dev"
	// maxReleaseBody は API の応答として読む上限バイト数。
	maxReleaseBody = 1 << 20
	// updateTimeout は1回の問い合わせに許す時間。
	updateTimeout = 15 * time.Second
)

// Release は公開されている最新のリリース。
type Release struct {
	// Version はリリースのタグ名（例: v1.2.3）。
	Version string
	// PageURL はリリースのページ。
	PageURL string
}

// UpdateCheckEnabled は、版 current の sorus が新しい版を調べてよいかを返す。開発中の版（"dev"）と、
// 環境変数 SORUS_NO_UPDATE_CHECK が 1 のときは調べない。
func UpdateCheckEnabled(current string, getenv func(string) string) bool {
	return current != "" && current != devVersion && getenv(NoUpdateCheckEnv) != "1"
}

// UpdateURL は最新のリリースを調べる URL（環境変数 SORUS_UPDATE_URL があればそれ）を返す。
func UpdateURL(getenv func(string) string) string {
	if u := strings.TrimSpace(getenv(UpdateURLEnv)); u != "" {
		return u
	}
	return DefaultUpdateURL
}

// LatestRelease は url（GitHub の releases/latest と同じ形の JSON を返す API）から最新のリリースを取得する。
// 下書き・プレリリースや、タグ名が版の形でないものはエラーにする。client が nil なら http.DefaultClient を使う。
func LatestRelease(ctx context.Context, client *http.Client, url, current string) (Release, error) {
	if client == nil {
		client = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Release{}, fmt.Errorf("desktop: update check: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "sorus/"+current)
	resp, err := client.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("desktop: update check: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("desktop: update check: %s returned %s", url, resp.Status)
	}
	var body struct {
		TagName    string `json:"tag_name"`
		HTMLURL    string `json:"html_url"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReleaseBody)).Decode(&body); err != nil {
		return Release{}, fmt.Errorf("desktop: update check: parse response: %w", err)
	}
	if body.Draft || body.Prerelease {
		return Release{}, errors.New("desktop: update check: latest release is a draft or pre-release")
	}
	if _, ok := parseSemver(body.TagName); !ok {
		return Release{}, fmt.Errorf("desktop: update check: tag %q is not a version", body.TagName)
	}
	return Release{Version: body.TagName, PageURL: body.HTMLURL}, nil
}

// Newer は版 latest が版 current より新しいかを返す（セマンティック バージョニングで比べる）。
// どちらかが版の形（v1.2.3 または 1.2.3。-rc.1 などのプレリリースと +build を付けてもよい）でなければ false。
func Newer(latest, current string) bool {
	l, ok := parseSemver(latest)
	if !ok {
		return false
	}
	c, ok := parseSemver(current)
	if !ok {
		return false
	}
	return compareSemver(l, c) > 0
}

// InstallCommand は goos の OS で最新版を入れ直す1行のコマンド（配布ページと同じ）を返す。
func InstallCommand(goos string) string {
	if goos == "windows" {
		return "irm https://github.com/ryuzi/sorus/releases/latest/download/install.ps1 | iex"
	}
	return "curl -fsSL https://github.com/ryuzi/sorus/releases/latest/download/install.sh | sh"
}

// semver は解析した版。
type semver struct {
	core [3]int
	pre  []string
}

func parseSemver(v string) (semver, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	v, _, _ = strings.Cut(v, "+")
	core, pre, hasPre := strings.Cut(v, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	var s semver
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" || (len(p) > 1 && p[0] == '0') {
			return semver{}, false
		}
		s.core[i] = n
	}
	if hasPre {
		if pre == "" {
			return semver{}, false
		}
		s.pre = strings.Split(pre, ".")
		for _, id := range s.pre {
			if id == "" {
				return semver{}, false
			}
		}
	}
	return s, true
}

// compareSemver は a と b を比べ、a が新しければ正、古ければ負、同じなら 0 を返す。
// プレリリース（1.2.3-rc.1）は同じ番号の正式版（1.2.3）より古い。
func compareSemver(a, b semver) int {
	for i := range a.core {
		if a.core[i] != b.core[i] {
			return a.core[i] - b.core[i]
		}
	}
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0:
		return 1
	case len(b.pre) == 0:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		x, y := a.pre[i], b.pre[i]
		xn, xerr := strconv.Atoi(x)
		yn, yerr := strconv.Atoi(y)
		switch {
		case xerr == nil && yerr == nil:
			if xn != yn {
				return xn - yn
			}
		case xerr == nil:
			return -1 // 数字だけの識別子は英字を含む識別子より前
		case yerr == nil:
			return 1
		default:
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		}
	}
	return len(a.pre) - len(b.pre)
}
