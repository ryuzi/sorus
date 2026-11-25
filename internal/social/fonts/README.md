# OGP 画像用フォント

`NotoSansJP-Bold-subset.ttf.gz` は動的 OGP 画像（`internal/social/ogp.go`）のタイトル・カテゴリ・サイト名の描画に使う
日本語フォント。シングルバイナリでどの環境でも同じ見た目になるよう `go:embed` で埋め込む。

- 元フォント: Noto Sans JP（可変フォント `NotoSansJP-VF.ttf` Version 2.04、SIL Open Font License 1.1 → `OFL.txt`）
- 加工: `golang.org/x/image/font` は可変フォントの軸を扱えない（既定のインスタンスは Thin）ため、
  wght=700（Bold）の静的インスタンスを作り、`charset.py` の文字集合（JIS X 0208・CP932 拡張・ASCII・
  ラテン補助・一般句読点・記号・全角形 ほか約1万字）にサブセット化して gzip で圧縮した。

再生成の手順（fontTools が必要。開発時のみ）:

```sh
python3 charset.py   # charset.txt を作る
fonttools varLib.instancer NotoSansJP-VF.ttf wght=700 -o NotoSansJP-Bold.ttf
pyftsubset NotoSansJP-Bold.ttf --text-file=charset.txt --layout-features='' --no-hinting \
  --drop-tables+=GPOS,GSUB,GDEF,BASE,vhea,vmtx,STAT,DSIG --name-IDs='0,1,2,3,4,5,6,13,14' \
  --output-file=NotoSansJP-Bold-subset.ttf
gzip -9 NotoSansJP-Bold-subset.ttf
```
