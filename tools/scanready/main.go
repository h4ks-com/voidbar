package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

func main() {
	b, err := os.ReadFile(`C:\Users\A2D1A~1\AppData\Local\Temp\opencode\mirror-2022-06\assets\d6a2d679515510432278.js`)
	if err != nil {
		panic(err)
	}
	c := string(b)
	// Find all imports of module 389726 (the shallow-equal comparator) and
	// show how the imported symbol is used (comparator call sites).
	re := regexp.MustCompile(`.{0,300}=([a-zA-Z_$][\w$]*)\(n\(389726\)\).{0,300}`)
	for _, m := range re.FindAllString(c, -1) {
		fmt.Println(">>>", m)
		fmt.Println()
	}
	_ = strings.TrimSpace
}
