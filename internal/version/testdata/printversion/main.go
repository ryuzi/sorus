// printversion は -X による版の埋め込みを確かめるためのテスト用プログラム。
package main

import (
	"fmt"

	"sorus/internal/version"
)

func main() {
	fmt.Print(version.Version)
}
