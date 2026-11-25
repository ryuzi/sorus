#!/usr/bin/env bash
set -e

CHROME="/mnt/c/Program Files/Google/Chrome/Application/chrome.exe"
SESSION="788909582d48df9cebeda5c720110e69ca09a5716de42f3cb65feacdd7fafd9e"
OUT_DIR="$(wslpath -w "$PWD")"
WSL_IP="172.18.15.192"

echo "Capturing screenshots via HTML snapshot..."

# 1. トップページ
curl -s "http://localhost:8080/" > temp_top.html
sed -i "s|href=\"/static/|href=\"http://$WSL_IP:8080/static/|g; s|src=\"/static/|src=\"http://$WSL_IP:8080/static/|g; s|href=\"/themes/|href=\"http://$WSL_IP:8080/themes/|g; s|src=\"/uploads/|src=\"http://$WSL_IP:8080/uploads/|g" temp_top.html
"$CHROME" --headless --disable-gpu --window-size=1440,1100 --screenshot="$OUT_DIR\\screenshot_1_top_refined.png" "file:///$OUT_DIR\\temp_top.html"

# 2. 研究室紹介 (/about)
curl -s "http://localhost:8080/about" > temp_about.html
sed -i "s|href=\"/static/|href=\"http://$WSL_IP:8080/static/|g; s|src=\"/static/|src=\"http://$WSL_IP:8080/static/|g; s|href=\"/themes/|href=\"http://$WSL_IP:8080/themes/|g; s|src=\"/uploads/|src=\"http://$WSL_IP:8080/uploads/|g" temp_about.html
"$CHROME" --headless --disable-gpu --window-size=1440,1100 --screenshot="$OUT_DIR\\screenshot_2_about_asterisks_fixed.png" "file:///$OUT_DIR\\temp_about.html"

# 3. 研究紹介 (/research)
curl -s "http://localhost:8080/research" > temp_research.html
sed -i "s|href=\"/static/|href=\"http://$WSL_IP:8080/static/|g; s|src=\"/static/|src=\"http://$WSL_IP:8080/static/|g; s|href=\"/themes/|href=\"http://$WSL_IP:8080/themes/|g; s|src=\"/uploads/|src=\"http://$WSL_IP:8080/uploads/|g" temp_research.html
"$CHROME" --headless --disable-gpu --window-size=1440,1100 --screenshot="$OUT_DIR\\screenshot_3_research_toc.png" "file:///$OUT_DIR\\temp_research.html"

# 4. 管理画面 記事一覧
curl -s -b "cms_session=$SESSION" "http://localhost:8080/admin/articles" > temp_admin_articles.html
sed -i "s|href=\"/static/|href=\"http://$WSL_IP:8080/static/|g; s|src=\"/static/|src=\"http://$WSL_IP:8080/static/|g" temp_admin_articles.html
"$CHROME" --headless --disable-gpu --window-size=1440,1000 --screenshot="$OUT_DIR\\screenshot_4_admin_articles_search.png" "file:///$OUT_DIR\\temp_admin_articles.html"

# 5. 管理画面 AI設定
curl -s -b "cms_session=$SESSION" "http://localhost:8080/admin/settings/ai" > temp_admin_ai.html
sed -i "s|href=\"/static/|href=\"http://$WSL_IP:8080/static/|g; s|src=\"/static/|src=\"http://$WSL_IP:8080/static/|g" temp_admin_ai.html
"$CHROME" --headless --disable-gpu --window-size=1440,1100 --screenshot="$OUT_DIR\\screenshot_5_admin_ai_settings.png" "file:///$OUT_DIR\\temp_admin_ai.html"

# 6. 記事一覧 (/articles)
curl -s "http://localhost:8080/articles" > temp_articles.html
sed -i "s|href=\"/static/|href=\"http://$WSL_IP:8080/static/|g; s|src=\"/static/|src=\"http://$WSL_IP:8080/static/|g; s|href=\"/themes/|href=\"http://$WSL_IP:8080/themes/|g; s|src=\"/uploads/|src=\"http://$WSL_IP:8080/uploads/|g" temp_articles.html
"$CHROME" --headless --disable-gpu --window-size=1440,1100 --screenshot="$OUT_DIR\\screenshot_6_articles_cards.png" "file:///$OUT_DIR\\temp_articles.html"

rm -f temp_top.html temp_about.html temp_research.html temp_admin_articles.html temp_admin_ai.html temp_articles.html

echo "Screenshots successfully captured!"
