package ai

import (
	"strings"
	"testing"
)

func TestMaskAcademicSyntax(t *testing.T) {
	input := "# 研究タイトル\n\n" +
		"本研究では、アインシュタインの質量エネルギー方程式 $E=mc^2$ および以下の式を用いる。\n\n" +
		"$$\n\\int_{-\\infty}^{\\infty} e^{-x^2} dx = \\sqrt{\\pi}\n$$\n\n" +
		"また、アーキテクチャ図は以下の通りである。\n" +
		"![実験構成図](/static/images/diagram.png \"構成図\")\n\n" +
		"実行コマンドは `go run main.go` である。\n\n" +
		"```python\n" +
		"def calculate():\n" +
		"    # $E=mc^2$ in comment should not be double-masked\n" +
		"    return \"ok\"\n" +
		"```\n"

	masked, restore := MaskAcademicSyntax(input)

	// マスク結果の確認: 数式やコードや画像がトークンに置き換わっていること
	if strings.Contains(masked, "$E=mc^2$") {
		t.Errorf("inline math was not masked: %s", masked)
	}
	if strings.Contains(masked, `\int_{-\infty}^{\infty}`) {
		t.Errorf("display math was not masked: %s", masked)
	}
	if strings.Contains(masked, "![実験構成図](/static/images/diagram.png \"構成図\")") {
		t.Errorf("markdown image was not masked: %s", masked)
	}
	if strings.Contains(masked, "def calculate():") {
		t.Errorf("code block was not masked: %s", masked)
	}
	if !strings.Contains(masked, "__SORUS_MASK_0__") {
		t.Errorf("expected mask tokens, got: %s", masked)
	}

	// 復元の確認
	restored := mustRestore(t, restore, masked)
	if restored != input {
		t.Errorf("restored text does not match original.\nGot:\n%s\nWant:\n%s", restored, input)
	}

	// LLMがトークンの前後に空白を入れた場合の寛容復元テスト
	tampered := strings.ReplaceAll(masked, "__SORUS_MASK_0__", "__ SORUS_MASK_0 __")
	restoredLoose := mustRestore(t, restore, tampered)
	if strings.Contains(restoredLoose, "__") {
		t.Errorf("loose restore failed to restore tampered token: %s", restoredLoose)
	}

	// Minor 9: LLMが小文字化（__sorus_mask_0__）して返した場合の復元テスト
	tamperedLower := strings.ReplaceAll(masked, "__SORUS_MASK_0__", "__sorus_mask_0__")
	restoredLower := mustRestore(t, restore, tamperedLower)
	if strings.Contains(restoredLower, "__") {
		t.Errorf("lowercase restore failed to restore tampered token: %s", restoredLower)
	}
}

// Minor 8: 通貨記号（$）の数式誤検知防止テスト
func TestMaskAcademicSyntaxCurrencyPreserved(t *testing.T) {
	currencyText := "Price is $100 and discount is $20. Total fee: $50."
	masked, restore := MaskAcademicSyntax(currencyText)

	// 金額表記が数式として誤ってトークン化されていないこと
	if strings.Contains(masked, "__SORUS_MASK_") {
		t.Errorf("currency expression was mistakenly masked: %s", masked)
	}
	if masked != currencyText {
		t.Errorf("expected masked text to match original currency text, got: %s", masked)
	}
	if restored := mustRestore(t, restore, masked); restored != currencyText {
		t.Errorf("restored does not match: got %s, want %s", restored, currencyText)
	}

	// 金額と正規の数式が混在するケース
	mixedText := "Price is $100 and $200, while the formula is $E=mc^2$ and $x$."
	maskedMixed, restoreMixed := MaskAcademicSyntax(mixedText)
	if !strings.Contains(maskedMixed, "$100 and $200") {
		t.Errorf("expected currency to be preserved, got: %s", maskedMixed)
	}
	if strings.Contains(maskedMixed, "$E=mc^2$") {
		t.Errorf("expected formula to be masked, got: %s", maskedMixed)
	}
	restoredMixed := mustRestore(t, restoreMixed, maskedMixed)
	if restoredMixed != mixedText {
		t.Errorf("restoredMixed mismatch: got %s, want %s", restoredMixed, mixedText)
	}
}

func TestMaskAcademicSyntaxEmpty(t *testing.T) {
	masked, restore := MaskAcademicSyntax("")
	if masked != "" {
		t.Errorf("expected empty string, got %q", masked)
	}
	if restored := mustRestore(t, restore, ""); restored != "" {
		t.Errorf("expected empty string, got %q", restored)
	}
}

func TestMaskSpecialSyntaxGalleryAndLinkURL(t *testing.T) {
	input := "# ギャラリーとリンクのテスト\n\n" +
		":::gallery tag=\"campus\" limit=\"4\" :::\n\n" +
		"詳細情報は [公式サイト](https://example.com/info?param=1&lang=ja \"タイトル\") を参照。\n" +
		"また [Google](https://google.com) で検索してください。\n"

	masked, restore := MaskAcademicSyntax(input)

	// gallery ディレクティブ全体がマスクされていること
	if strings.Contains(masked, ":::gallery") {
		t.Errorf("gallery directive was not masked: %s", masked)
	}
	// リンクのURL部分がマスクされていること（リンクテキストは残っている）
	if !strings.Contains(masked, "[公式サイト]") || !strings.Contains(masked, "[Google]") {
		t.Errorf("link text was accidentally masked: %s", masked)
	}
	if strings.Contains(masked, "https://example.com/info") || strings.Contains(masked, "https://google.com") {
		t.Errorf("link URLs were not masked: %s", masked)
	}

	// 復元テスト
	restored := mustRestore(t, restore, masked)
	if restored != input {
		t.Errorf("restored text does not match original.\nGot:\n%s\nWant:\n%s", restored, input)
	}
}

func TestMaskNestedInlineCodeAndLink(t *testing.T) {
	// Markdown 記法の解説記事などで生じるコード内のリンク記法
	input := "Markdown では `[リンクテキスト](https://example.com)` のように記述します。"
	masked, restore := MaskAcademicSyntax(input)

	// マスク復元が LIFO で正しく完了し、__SORUS_MASK_ が残留しないこと
	restored := mustRestore(t, restore, masked)
	if restored != input {
		t.Fatalf("nested tokens failed to restore.\nGot:  %q\nWant: %q", restored, input)
	}
}

func TestMaskAcademicSyntaxLooseRestoreWithDollar(t *testing.T) {
	// $1 や $2 などの正規表現後方参照と誤認されうる記号を含むインライン数式
	input := "式 $x_1 = 10$ および $y_2 = 20$ の展開式。"
	masked, restore := MaskAcademicSyntax(input)

	// LLM がトークンを小文字化・スペース混入させた場合（緩い一致パス）
	tampered := masked
	tampered = strings.Replace(tampered, "__SORUS_MASK_0__", "__ sorus_mask_0 __", 1)
	tampered = strings.Replace(tampered, "__SORUS_MASK_1__", "__  SORUS_MASK_1  __", 1)

	restored := mustRestore(t, restore, tampered)
	if restored != input {
		t.Fatalf("loose restore with dollar formula failed.\nGot:  %q\nWant: %q", restored, input)
	}
}

func mustRestore(t *testing.T, restore func(string) (string, error), text string) string {
	t.Helper()
	got, err := restore(text)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	return got
}
