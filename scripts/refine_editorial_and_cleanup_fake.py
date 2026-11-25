#!/usr/bin/env python3
"""
scripts/refine_editorial_and_cleanup_fake.py

1. 初期シードデータの架空記事4件（art-pub-1, art-pub-2, art-news-1, art-life-1）を完全削除。
2. 固定ページ（JA & EN）のハイプ・誇大表現を点検し、学術的・客観的・誠実な表現へ改訂。
"""

import sqlite3

DB_PATH = "database.db"

def main():
    conn = sqlite3.connect(DB_PATH)
    c = conn.cursor()

    # 1. 架空記事4件の削除
    fake_ids = ['art-pub-1', 'art-pub-2', 'art-news-1', 'art-life-1']
    print(f"Deleting fake articles: {fake_ids}")
    for fid in fake_ids:
        c.execute("DELETE FROM article_revisions WHERE article_id = ?", (fid,))
        c.execute("DELETE FROM article_comments WHERE article_id = ?", (fid,))
        c.execute("DELETE FROM attachments WHERE article_id = ?", (fid,))
        c.execute("DELETE FROM articles WHERE id = ?", (fid,))
    print(f"Deleted {len(fake_ids)} fake articles.")

    # 2. 誇大表現の点検と改訂
    # 置換対象の定義 (ターゲットID/slug, タイトル・抜粋・本文の置換ペア)
    replacements = [
        # (slug, [ (old, new), ... ])
        (
            "publications",
            [
                ("研究業績・代表ブレークスルー論文アーカイブ", "主要論文・学術論文アーカイブ（Selected Publications）"),
                ("代表ブレークスルー論文（Selected Breakthrough Papers）", "主要論文（Selected Publications）"),
                ("代表ブレークスルー論文 (Selected Breakthrough Papers)", "主要論文 (Selected Publications)"),
                ("代表ブレークスルー論文", "主要論文"),
                ("ブレークスルー", "画期的成果"),
            ]
        ),
        (
            "publications-en",
            [
                ("Selected Publications & Academic Breakthroughs", "Selected Publications & Research Archive"),
                ("Selected breakthrough publications", "Selected peer-reviewed publications"),
                ("Breakthrough Publications", "Selected Publications"),
                ("Breakthrough Papers", "Selected Publications"),
                ("breakthrough", "innovative"),
            ]
        ),
        (
            "research-environmental",
            [
                ("分析化学的ブレークスルーを数多く創出してきました", "新しい分析手法の開発・実用化を数多く進めてきました"),
                ("分析化学的ブレークスルー", "分析手法の開発・実用化"),
                ("ブレークスルー", "開発手法"),
            ]
        ),
        (
            "research-environmental-en",
            [
                ("This historic breakthrough was featured", "This novel analytical methodology was featured"),
                ("historic breakthrough", "novel analytical methodology"),
                ("breakthrough", "novel methodology"),
            ]
        ),
        (
            "research-separation",
            [
                ("世界最高峰の分析精度を確立し続けています", "極微量領域における高度な分析精度を確立し続けています"),
                ("世界最高峰の分析精度", "極微量領域における高度な分析精度"),
                ("最高峰", "高度な水準"),
            ]
        ),
        (
            "research-nanomaterials",
            [
                ("究極の分子認識システムの構築に取り組んでいます", "高選択的な分子認識システムの構築に取り組んでいます"),
                ("究極の分子認識システム", "高選択的な分子認識システム"),
                ("究極の", "高度な"),
            ]
        ),
        (
            "patents-en",
            [
                ("translates academic breakthroughs into practical industrial applications", "translates innovative analytical chemistry into practical industrial applications"),
                ("breakthroughs", "innovations"),
            ]
        ),
        (
            "movies-en",
            [
                ("Focusing on our breakthrough mass spectrometry methodology", "Focusing on our rapid mass spectrometry methodology"),
                ("breakthrough mass spectrometry", "rapid mass spectrometry"),
            ]
        ),
        (
            "research-en",
            [
                ("The ultimate barrier in ultra-trace measurement", "The primary challenge in ultra-trace measurement"),
                ("ultimate barrier", "primary challenge"),
            ]
        ),
    ]

    for slug, rep_list in replacements:
        row = c.execute("SELECT id, title, excerpt, content_markdown FROM articles WHERE slug = ?", (slug,)).fetchone()
        if not row:
            print(f"Warning: Article with slug '{slug}' not found.")
            continue
        art_id, title, excerpt, content = row
        new_title = title
        new_excerpt = excerpt
        new_content = content

        for old, new in rep_list:
            if old in new_title:
                print(f"[{slug}] Title: '{old}' -> '{new}'")
                new_title = new_title.replace(old, new)
            if excerpt and old in new_excerpt:
                print(f"[{slug}] Excerpt: '{old}' -> '{new}'")
                new_excerpt = new_excerpt.replace(old, new)
            if old in new_content:
                print(f"[{slug}] Content: '{old}' -> '{new}'")
                new_content = new_content.replace(old, new)

        c.execute("""
            UPDATE articles
            SET title = ?, excerpt = ?, content_markdown = ?
            WHERE id = ?
        """, (new_title, new_excerpt, new_content, art_id))

    conn.commit()
    conn.close()
    print("Database refinement completed successfully.")

if __name__ == "__main__":
    main()
