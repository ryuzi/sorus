#!/usr/bin/env bash
set -e

CHROME="/mnt/c/Program Files/Google/Chrome/Application/chrome.exe"
SESSION="666f76f11bd0f2d601adaa2ab69e8d4a9f33bc47b4bfd9ad5cdea1b1e299977c"

OUT_DIR="$(wslpath -w "$PWD")"

echo "Taking screenshots..."

# 1. トップページ
"$CHROME" --headless --disable-gpu --window-size=1440,1100 --screenshot="$OUT_DIR\\screenshot_top_final.png" "http://localhost:8080/"

# 2. 記事一覧 (新着記事カードの整列確認)
"$CHROME" --headless --disable-gpu --window-size=1440,1100 --screenshot="$OUT_DIR\\screenshot_articles_final.png" "http://localhost:8080/articles"

# 3. お問い合わせ・アクセス (地図2枚配置確認)
"$CHROME" --headless --disable-gpu --window-size=1440,1200 --screenshot="$OUT_DIR\\screenshot_contact_final.png" "http://localhost:8080/contact"

# 4. 管理画面 記事一覧 (HTML取得 -> Chrome撮影)
curl -s -b "cms_session=$SESSION" "http://localhost:8080/admin/articles" > admin_articles.html
# パスを絶対URLに書き換え
sed -i 's|href="/static/|href="http://localhost:8080/static/|g; s|src="/static/|src="http://localhost:8080/static/|g' admin_articles.html
"$CHROME" --headless --disable-gpu --window-size=1440,1000 --screenshot="$OUT_DIR\\screenshot_admin_articles.png" "file:///$OUT_DIR\\admin_articles.html"

# 5. 管理画面 固定ページ一覧
curl -s -b "cms_session=$SESSION" "http://localhost:8080/admin/articles?kind=page" > admin_pages.html
sed -i 's|href="/static/|href="http://localhost:8080/static/|g; s|src="/static/|src="http://localhost:8080/static/|g' admin_pages.html
"$CHROME" --headless --disable-gpu --window-size=1440,1000 --screenshot="$OUT_DIR\\screenshot_admin_pages.png" "file:///$OUT_DIR\\admin_pages.html"

rm -f admin_articles.html admin_pages.html

echo "Screenshots taken successfully."
