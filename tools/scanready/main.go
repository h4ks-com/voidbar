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
	fields := []string{"guildJoinRequests", "consents", "analyticsToken", "users", "presences", "relationships", "privateChannels", "sessions", "experiments", "connectedAccounts", "userGuildSettings", "readState", "guilds", "notes", "linkedUsers", "mergedPresences", "mergedMembers", "gameRelationships", "geoOrderedRtcRegions", "friendSuggestionCount"}
	for _, f := range fields {
		re := regexp.MustCompile(`\be\.` + f + `\.(map|forEach|filter)\b`)
		ms := re.FindAllStringIndex(c, -1)
		unguarded := 0
		for _, m := range ms {
			s := m[0] - 80
			if s < 0 {
				s = 0
			}
			end := m[1] + 40
			if end > len(c) {
				end = len(c)
			}
			ctx := c[s:end]
			if !strings.Contains(ctx, "null!=") && !strings.Contains(ctx, "||[]") && !strings.Contains(ctx, "||[]") && !strings.Contains(ctx, "!=null") {
				unguarded++
				fmt.Printf("%s: UNGUARDED ctx=...%s...\n", f, ctx)
			}
		}
		if len(ms) > 0 {
			fmt.Printf("%s: total=%d unguarded=%d\n", f, len(ms), unguarded)
		}
	}
}
